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
// session ends.
//
// "When that session ends" is the whole difficulty, and the reason this file is
// longer than the lock itself. PostgreSQL ends a session when it notices the
// client is gone, and on a killed container it can take hours to notice: the
// process dies without closing the socket, no FIN or RST reaches the server —
// an evicted pod's connections are cut at the network, not by the peer — and
// the backend sits `idle`, holding the lock, until TCP keepalives expire.
// Linux's defaults put that a little over two hours away. For those two hours
// every start says "another rolltop server is already running against this
// database" about a server that is dead, and the app cannot come up at all.
//
// So the lock is made to expire on three independent timers, in the order they
// normally fire:
//
//  1. Keepalives on the lock session (instanceKeepalive*), set on the server
//     side so PostgreSQL probes the client itself and reaps the backend about a
//     minute after the peer stops answering, instead of after two hours.
//  2. A ping the holder runs on that same connection every instancePingEvery.
//     It proves the holder is alive to anyone reading pg_stat_activity, and it
//     is what makes silence mean something.
//  3. A waiting server, once its wait has run out, reading who holds the lock.
//     A holder that carries this file's application_name and has not pinged for
//     instanceStaleAfter is a process that no longer exists — it would be
//     pinging if it did — and its backend is terminated so the lock is freed.
//
// Only the marked, provably-silent holder is taken over. A holder that is not
// marked (an older rolltop, or anything else that took this key) might be a
// live server this code cannot interrogate, so it is named in the error rather
// than terminated, and an operator who knows better sets BreakInstanceLock.
//
// It is still not a distributed lock and does not pretend to be. What it
// guards is a misconfiguration, caught at startup, not a fencing token.

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"rolltop/backend/pgbind"
)

// instanceAdvisoryLock is the key one running server holds for its database's
// lifetime. It shares PostgreSQL's single advisory-lock space with
// schemaAdvisoryLock and must differ from it: a server holding this one for
// hours would otherwise block every schema operation.
const instanceAdvisoryLock int64 = 0x726F6C6C746F7001 // "rolltop\1"

// instanceLockAppName marks the lock session in pg_stat_activity. It is what
// lets a waiting server tell a rolltop holder — which pings, and whose silence
// therefore means it is dead — from an unmarked session it must not touch.
//
// Changing this string changes that answer: a holder running the old value
// stops being recognised and is reported instead of taken over. It is part of
// the on-the-wire contract between two versions of this file, not a label.
const instanceLockAppName = "rolltop-instance-lock"

// instanceRetryEvery is how often the wait re-tries. A rolling deploy overlaps
// the two processes for seconds, so the poll is cheap and the resolution is
// what decides how quickly the new server takes over.
const instanceRetryEvery = time.Second

// instancePingEvery is how often the holder touches its own lock session. Each
// ping moves pg_stat_activity.state_change, which is the only evidence a
// waiting server has that the holder is still a running process.
const instancePingEvery = 15 * time.Second

// instanceStaleAfter is how long a marked holder may stay silent before a
// waiting server treats it as dead and terminates its backend. Five missed
// pings, so a loaded machine that drops one or two is never mistaken for a
// crashed one, and the keepalives above normally reap the session first.
const instanceStaleAfter = 75 * time.Second

// instanceClaimAttempts bounds how many times a spent wait may look at the
// holder and retry. Without a bound, a lock that is held every time it is tried
// and free every time it is looked up spins without sleeping.
const instanceClaimAttempts = 3

// The keepalive settings PostgreSQL applies to the lock session. They are
// server-side GUCs: the backend probes the client, which is the direction that
// matters here, because the case being solved is a client that vanished.
// Roughly a minute to notice, against the two hours Linux defaults to.
//
// On a Unix-socket connection PostgreSQL accepts and ignores them.
const (
	instanceKeepaliveIdle     = 30
	instanceKeepaliveInterval = 10
	instanceKeepaliveCount    = 3
)

// errInstanceLockHeld is what a waiting server returns when the lock is held by
// something it will not take over. It carries the holder so the message can
// name it instead of leaving the operator to find it in pg_stat_activity.
type errInstanceLockHeld struct {
	holder *instanceLockHolder
}

func (e *errInstanceLockHeld) Error() string {
	msg := "another rolltop server is already running against this database. " +
		"One database serves one server: two of them stamp each other's sync runs interrupted " +
		"and fetch every mailbox twice. Stop the other server, or give this one its own database"
	if e.holder == nil {
		return msg
	}
	return msg + ". It is held by " + e.holder.describe() +
		". If that session belongs to a server that no longer exists, set " +
		"ROLLTOP_BREAK_INSTANCE_LOCK=true for one start to take the lock from it"
}

// instanceLockHolder is the session holding the lock, as pg_stat_activity
// describes it.
type instanceLockHolder struct {
	pid          int
	appName      string
	clientAddr   string
	backendStart time.Time
	state        string
	// idleFor is measured with the database's clock, not this process's: the
	// two containers need not agree on the time, and the whole judgement below
	// rests on this number.
	idleFor time.Duration
}

// marked reports whether the holder is a rolltop lock session of this vintage —
// one that pings, and whose silence is therefore evidence rather than absence
// of evidence.
func (h *instanceLockHolder) marked() bool { return h != nil && h.appName == instanceLockAppName }

// staleAfter reports whether a marked holder has stopped pinging for long
// enough that the process behind it cannot still be running.
//
// Only an idle session qualifies. An abandoned lock session is always idle —
// there is nobody left to run a query on it — while one stuck mid-query is a
// state this code cannot read, and the keepalives end it soon enough anyway.
func (h *instanceLockHolder) staleAfter(d time.Duration) bool {
	return h.marked() && h.state == "idle" && h.idleFor > d
}

// describe names the holding session the way an operator would have to look it
// up: a noun phrase, so the caller decides whether it is being reported or
// taken over.
func (h *instanceLockHolder) describe() string {
	if h == nil {
		return "no session"
	}
	where := h.clientAddr
	if where == "" {
		where = "a local connection"
	}
	name := h.appName
	if name == "" {
		name = "unnamed"
	}
	return fmt.Sprintf("backend pid %d (%s) from %s, connected since %s, %s for %s",
		h.pid, name, where, h.backendStart.UTC().Format(time.RFC3339),
		h.state, h.idleFor.Round(time.Second))
}

// instanceLockOptions carries what the caller decides about the claim.
type instanceLockOptions struct {
	// wait is how long to let a previous process finish letting go.
	wait time.Duration
	// breakHeld takes the lock from a holder this code would otherwise only
	// report: an older rolltop, or any session it cannot recognise. It is the
	// operator asserting that no other server is running, so it is honoured
	// only after the whole wait has passed without the lock coming free.
	breakHeld bool

	// pingEvery and staleAfter are the constants above, overridable so a test
	// can watch a holder go stale in a second rather than in a minute and a
	// quarter. Nothing outside the tests sets them; zero means the constant.
	pingEvery  time.Duration
	staleAfter time.Duration
}

func (o instanceLockOptions) ping() time.Duration {
	if o.pingEvery > 0 {
		return o.pingEvery
	}
	return instancePingEvery
}

func (o instanceLockOptions) stale() time.Duration {
	if o.staleAfter > 0 {
		return o.staleAfter
	}
	return instanceStaleAfter
}

// instanceLock is the held connection. Releasing it is what lets the next
// process in.
type instanceLock struct {
	db   *sql.DB
	conn *sql.Conn

	pingEvery  time.Duration
	staleAfter time.Duration

	// mu serialises the ping against release. They share one connection, and
	// database/sql does not allow two callers on a *sql.Conn at once.
	mu     sync.Mutex
	closed bool

	stopPing context.CancelFunc
	pingDone chan struct{}
}

// acquireInstanceLock takes the single-server lock, waiting up to opts.wait for
// a previous process to let go.
//
// Waiting rather than failing outright is what makes a rolling deployment work:
// the outgoing container is still serving while the incoming one starts, and
// refusing immediately would turn every deploy into a crash loop. A wait that
// runs out is not the end of it — see the file overview: the wait running out
// is when this asks who the holder is, because "held" and "held by a process
// that died an hour ago" need different answers.
//
// The connection is its own pool of one rather than a slot borrowed from the
// store's: holding a pooled connection for the process lifetime would take a
// connection away from the work the pool was sized for, invisibly.
func acquireInstanceLock(ctx context.Context, dsnName string, opts instanceLockOptions) (*instanceLock, error) {
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
	lock := &instanceLock{db: db, conn: conn, pingEvery: opts.ping(), staleAfter: opts.stale()}
	if err := prepareInstanceSession(ctx, conn); err != nil {
		lock.release()
		return nil, err
	}

	deadline := time.Now().Add(opts.wait)
	attempts := 0
	for {
		var acquired bool
		if err := conn.QueryRowContext(ctx, `SELECT pg_try_advisory_lock($1)`, instanceAdvisoryLock).Scan(&acquired); err != nil {
			lock.release()
			return nil, err
		}
		if acquired {
			lock.startPing()
			return lock, nil
		}
		if !time.Now().Before(deadline) {
			// The wait is spent. Everything past here is about the holder
			// rather than about waiting longer.
			//
			// It is bounded because it must terminate: "held, but by nobody
			// this query can see" is a race the retry resolves, and a race that
			// keeps resolving the same way is a loop. Three passes is one for
			// the ordinary handover, one for the backend this just terminated,
			// and one to spare.
			if attempts++; attempts > instanceClaimAttempts {
				lock.release()
				return nil, &errInstanceLockHeld{}
			}

			if err := lock.claimFromHolder(ctx, opts.breakHeld); err != nil {
				lock.release()
				return nil, err
			}
			// pg_terminate_backend returns before the backend is gone, and a
			// holder that released on its own has a lock to give up. Either way
			// the retry belongs one poll later, not immediately.
		}
		timer := time.NewTimer(instanceRetryEvery)
		select {
		case <-ctx.Done():
			timer.Stop()
			lock.release()
			// Being told to stop while waiting is not the misconfiguration this
			// guard exists to name, and reporting it as one sends whoever reads
			// the log looking for a second server that was never there.
			return nil, fmt.Errorf("stopped while waiting for another server to release this database: %w", ctx.Err())
		case <-timer.C:
		}
	}
}

// prepareInstanceSession makes the lock session recognisable and reapable
// before it holds anything: named, so a waiting server can tell it apart from
// sessions it must not touch, and keepalive-probed, so PostgreSQL ends it
// minutes rather than hours after the process behind it dies.
//
// Both have to be in place before the lock is taken, not after. A process
// killed in the window between would leave exactly the unmarked, unprobed
// holder this whole file exists to avoid.
func prepareInstanceSession(ctx context.Context, conn *sql.Conn) error {
	settings := []string{
		fmt.Sprintf(`SET application_name = '%s'`, instanceLockAppName),
		fmt.Sprintf(`SET tcp_keepalives_idle = %d`, instanceKeepaliveIdle),
		fmt.Sprintf(`SET tcp_keepalives_interval = %d`, instanceKeepaliveInterval),
		fmt.Sprintf(`SET tcp_keepalives_count = %d`, instanceKeepaliveCount),
	}
	for _, stmt := range settings {
		if _, err := conn.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("prepare the instance lock session: %w", err)
		}
	}
	return nil
}

// claimFromHolder clears the way for one more try at a lock that is still held
// after the full wait, or returns the error that says why it will not.
//
// The three outcomes are the three things "still held" can mean: nobody holds
// it any more and the retry will simply succeed; a rolltop holder has stopped
// pinging, so the process is gone and its backend is terminated; or something
// this code cannot vouch for holds it, which is an error naming that session —
// unless the operator has said to take it anyway.
func (l *instanceLock) claimFromHolder(ctx context.Context, breakHeld bool) error {
	holder, err := l.lookupHolder(ctx)
	if err != nil {
		// Not fatal in its own right. Failing to read the catalogs costs the
		// detail in the message and the chance to recover automatically; what
		// the operator has to be told either way is that the lock is held, and
		// replacing that with a catalog error would bury it.
		log.Printf("instance lock: %v", err)
		return &errInstanceLockHeld{}
	}
	switch {
	case holder == nil:
		// Released between the failed try and this lookup. The retry takes it.
		return nil
	case holder.staleAfter(l.staleAfter):
		log.Printf("instance lock: taking it from pid %d, a rolltop lock session silent for %s "+
			"(its process is gone; a running one pings every %s)",
			holder.pid, holder.idleFor.Round(time.Second), l.pingEvery)
	case breakHeld:
		log.Printf("instance lock: ROLLTOP_BREAK_INSTANCE_LOCK is set, taking it from %s", holder.describe())
	default:
		return &errInstanceLockHeld{holder: holder}
	}
	// Terminating is the last word: from here the lock is either free or about
	// to be, and the caller retries.
	if err := l.terminateHolder(ctx, holder.pid); err != nil {
		// A refused termination — a role that may not signal that backend —
		// leaves the lock exactly where it was, so the answer is the message
		// that names it, with the reason the recovery did not happen alongside.
		log.Printf("instance lock: %v", err)
		return &errInstanceLockHeld{holder: holder}
	}
	return nil
}

// lookupHolder reads the session holding the lock out of the catalogs.
//
// A bigint advisory key is stored split across two oid columns — the high half
// in classid, the low half in objid, with objsubid 1 marking it as the
// single-argument form. Reassembling it in SQL keeps the encoding in one place
// and out of Go's signed integers.
func (l *instanceLock) lookupHolder(ctx context.Context) (*instanceLockHolder, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return nil, errors.New("instance lock: session closed")
	}
	const query = `
		SELECT a.pid,
		       COALESCE(a.application_name, ''),
		       COALESCE(host(a.client_addr), ''),
		       a.backend_start,
		       COALESCE(a.state, ''),
		       EXTRACT(EPOCH FROM (now() - COALESCE(a.state_change, a.backend_start)))::double precision
		  FROM pg_locks l
		  JOIN pg_stat_activity a ON a.pid = l.pid
		 WHERE l.locktype = 'advisory'
		   AND l.granted
		   AND l.objsubid = 1
		   AND l.classid = (($1::bigint >> 32) & 4294967295)::bigint::oid
		   AND l.objid = ($1::bigint & 4294967295)::bigint::oid
		   AND a.pid <> pg_backend_pid()
		 ORDER BY a.backend_start
		 LIMIT 1`
	var (
		holder  instanceLockHolder
		idleFor float64
	)
	err := l.conn.QueryRowContext(ctx, query, instanceAdvisoryLock).Scan(
		&holder.pid, &holder.appName, &holder.clientAddr,
		&holder.backendStart, &holder.state, &idleFor)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read the holder of the instance lock: %w", err)
	}
	holder.idleFor = time.Duration(idleFor * float64(time.Second))
	return &holder, nil
}

// terminateHolder ends the holding backend, which releases every session lock
// it holds. Terminating a session of one's own role needs no elevated rights,
// which is what lets this work on a hosted database where nothing is superuser.
func (l *instanceLock) terminateHolder(ctx context.Context, pid int) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return errors.New("instance lock: session closed")
	}
	var terminated bool
	if err := l.conn.QueryRowContext(ctx, `SELECT pg_terminate_backend($1)`, pid).Scan(&terminated); err != nil {
		return fmt.Errorf("end the session holding the instance lock (pid %d): %w", pid, err)
	}
	if !terminated {
		// It exited on its own between the lookup and here, which is the same
		// outcome by a different route.
		log.Printf("instance lock: pid %d was already gone", pid)
	}
	// Termination is asynchronous; the lock is released as the backend exits.
	// The caller's next try picks it up on the next poll rather than racing it.
	return nil
}

// startPing runs the holder's proof of life for as long as the lock is held.
//
// Nothing in this process reads the ping. It exists for the next server to
// read: an idle session says nothing about whether a process is behind it, and
// a session that ran a query fifteen seconds ago says everything.
func (l *instanceLock) startPing() {
	ctx, cancel := context.WithCancel(context.Background())
	l.stopPing = cancel
	l.pingDone = make(chan struct{})
	go func() {
		defer close(l.pingDone)
		ticker := time.NewTicker(l.pingEvery)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				l.ping(ctx)
			}
		}
	}()
}

// ping touches the lock session, and says so when it cannot.
//
// A failure here means the connection carrying the lock is gone, and with it
// the lock — while this server keeps running and syncing. That is the one hole
// this design has always had, and the least it can do is stop being silent
// about it: the line below is what an operator has to see before two servers
// start writing over each other.
func (l *instanceLock) ping(ctx context.Context) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return
	}
	pingCtx, cancel := context.WithTimeout(ctx, l.pingEvery)
	defer cancel()
	var one int
	if err := l.conn.QueryRowContext(pingCtx, `SELECT 1`).Scan(&one); err != nil {
		if ctx.Err() != nil {
			return
		}
		log.Printf("instance lock: this server can no longer reach the session holding it (%v); "+
			"the single-server guard is not protecting this database until the next restart", err)
	}
}

// release gives the lock back and closes the connection behind it. Closing the
// session would release the lock on its own; unlocking first keeps the release
// explicit and survives a server that hangs on to the session.
func (l *instanceLock) release() {
	if l == nil {
		return
	}
	// Outside the mutex: the ping takes it, so waiting for the ping to finish
	// while holding it would deadlock.
	if l.stopPing != nil {
		l.stopPing()
		<-l.pingDone
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return
	}
	l.closed = true
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
