// File overview: One process per database.
//
// The SQLite file locks used to enforce this as a side effect: two servers
// could not open the same database because the operating system said so. On
// PostgreSQL nothing does, and the failure is quiet rather than loud — both
// processes start, each start stamps the other's in-flight sync runs
// "interrupted" (MarkRunningSyncRunsInterrupted updates every running row), and
// both runners then schedule syncs for every user, so the same mailboxes are
// fetched twice and the same IMAP flag changes are pushed twice.
//
// The data-directory lock in cmd/rolltop does not cover this. It is per volume,
// so it catches two containers sharing `/data` and misses the case that matters
// here: two deployments with their own volumes pointed at one DSN.
//
// A session-scoped advisory lock closes it. The lock lives on a connection held
// for as long as the store is open, and PostgreSQL releases it when that
// session ends — including when the process is killed, which is what makes this
// self-healing rather than a lock file somebody has to clear.
//
// It is not a distributed lock and does not pretend to be. If the connection
// holding it breaks, the server keeps running while the lock is gone, and
// another process could then start. That is the right trade for what this
// guards: a misconfiguration, caught at startup, not a fencing token.

package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"rolltop/backend/pgbind"
)

// instanceAdvisoryLock is the key one running server holds for its database's
// lifetime. It shares PostgreSQL's single advisory-lock space with
// schemaAdvisoryLock and must differ from it: a server holding this one for
// hours would otherwise block every schema operation.
const instanceAdvisoryLock int64 = 0x726F6C6C746F7001 // "rolltop\1"

// instanceRetryEvery is how often the wait re-tries. A rolling deploy overlaps
// the two processes for seconds, so the poll is cheap and the resolution is
// what decides how quickly the new server takes over.
const instanceRetryEvery = time.Second

// instanceLock is the held connection. Releasing it is what lets the next
// process in.
type instanceLock struct {
	db   *sql.DB
	conn *sql.Conn
}

// acquireInstanceLock takes the single-server lock, waiting up to wait for a
// previous process to let go.
//
// Waiting rather than failing outright is what makes a rolling deployment work:
// the outgoing container is still serving while the incoming one starts, and
// refusing immediately would turn every deploy into a crash loop. A wait that
// runs out means the other server is not going away, which is the
// misconfiguration this exists to name.
//
// The connection is its own pool of one rather than a slot borrowed from the
// store's: holding a pooled connection for the process lifetime would take a
// connection away from the work the pool was sized for, invisibly.
func acquireInstanceLock(ctx context.Context, dsnName string, wait time.Duration) (*instanceLock, error) {
	db, err := sql.Open(pgbind.DriverName, dsnName)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	// No lifetime and no idle timeout: recycling this connection would drop the
	// lock with it, silently, while the server kept running.
	db.SetConnMaxLifetime(0)
	db.SetConnMaxIdleTime(0)

	conn, err := db.Conn(ctx)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	lock := &instanceLock{db: db, conn: conn}

	deadline := time.Now().Add(wait)
	for {
		var acquired bool
		if err := conn.QueryRowContext(ctx, `SELECT pg_try_advisory_lock($1)`, instanceAdvisoryLock).Scan(&acquired); err != nil {
			lock.release()
			return nil, err
		}
		if acquired {
			return lock, nil
		}
		if !time.Now().Before(deadline) {
			lock.release()
			return nil, errors.New("another rolltop server is already running against this database. " +
				"One database serves one server: two of them stamp each other's sync runs interrupted and fetch every mailbox twice. " +
				"Stop the other server, or give this one its own database")
		}
		timer := time.NewTimer(instanceRetryEvery)
		select {
		case <-ctx.Done():
			timer.Stop()
			lock.release()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

// release gives the lock back and closes the connection behind it. Closing the
// session would release the lock on its own; unlocking first keeps the release
// explicit and survives a server that hangs on to the session.
func (l *instanceLock) release() {
	if l == nil {
		return
	}
	if l.conn != nil {
		unlockCtx, cancel := context.WithTimeout(context.Background(), schemaUnlockTimeout)
		_, _ = l.conn.ExecContext(unlockCtx, `SELECT pg_advisory_unlock($1)`, instanceAdvisoryLock)
		cancel()
		_ = l.conn.Close()
	}
	if l.db != nil {
		_ = l.db.Close()
	}
}
