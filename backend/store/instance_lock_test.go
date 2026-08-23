// File overview: One server per database, and what that must not break.

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"rolltop/backend/pgbind"
	"rolltop/backend/store/pgtestdb"
)

// TestExclusiveInstanceRefusesASecondServer is the guard the SQLite file locks
// used to provide for free. Without it both processes start, each start marks
// the other's in-flight sync runs interrupted, and both fetch every mailbox.
func TestExclusiveInstanceRefusesASecondServer(t *testing.T) {
	ctx := context.Background()
	dsn := pgtestdb.NewFromTemplate(t, SchemaTag(), buildTestTemplate)

	first, err := OpenPostgres(ctx, dsn, PostgresOptions{MaxConns: 2, DataDir: t.TempDir(), ExclusiveInstance: true})
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()

	second, err := OpenPostgres(ctx, dsn, PostgresOptions{MaxConns: 2, DataDir: t.TempDir(), ExclusiveInstance: true})
	if err == nil {
		second.Close()
		t.Fatal("a second server opened the same database")
	}
	if !strings.Contains(err.Error(), "already running") {
		t.Fatalf("the refusal does not say what is wrong: %v", err)
	}
}

// TestExclusiveInstanceIsReleasedOnClose is what makes a restart work at all:
// the lock has to be gone the moment the previous store closes, not whenever
// its connection is eventually recycled.
func TestExclusiveInstanceIsReleasedOnClose(t *testing.T) {
	ctx := context.Background()
	dsn := pgtestdb.NewFromTemplate(t, SchemaTag(), buildTestTemplate)

	first, err := OpenPostgres(ctx, dsn, PostgresOptions{MaxConns: 2, DataDir: t.TempDir(), ExclusiveInstance: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	second, err := OpenPostgres(ctx, dsn, PostgresOptions{MaxConns: 2, DataDir: t.TempDir(), ExclusiveInstance: true})
	if err != nil {
		t.Fatalf("the lock outlived the store that held it: %v", err)
	}
	defer second.Close()
}

// TestExclusiveInstanceWaitsForTheOutgoingServer covers the rolling deploy: the
// old container is still serving while the new one starts, so refusing at once
// would turn every deployment into a crash loop.
func TestExclusiveInstanceWaitsForTheOutgoingServer(t *testing.T) {
	ctx := context.Background()
	dsn := pgtestdb.NewFromTemplate(t, SchemaTag(), buildTestTemplate)

	outgoing, err := OpenPostgres(ctx, dsn, PostgresOptions{MaxConns: 2, DataDir: t.TempDir(), ExclusiveInstance: true})
	if err != nil {
		t.Fatal(err)
	}
	closed := make(chan error, 1)
	go func() {
		time.Sleep(1500 * time.Millisecond)
		closed <- outgoing.Close()
	}()

	incoming, err := OpenPostgres(ctx, dsn, PostgresOptions{
		MaxConns: 2, DataDir: t.TempDir(), ExclusiveInstance: true, InstanceLockWait: 30 * time.Second,
	})
	if err != nil {
		t.Fatalf("the incoming server did not wait for the outgoing one: %v", err)
	}
	defer incoming.Close()
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
}

// TestStoresWithoutExclusiveInstanceShareADatabase pins that the guard is
// opt-in. The test suite opens several stores against one database on purpose,
// and so does every test that reopens a store to check persistence.
func TestStoresWithoutExclusiveInstanceShareADatabase(t *testing.T) {
	ctx := context.Background()
	dsn := pgtestdb.NewFromTemplate(t, SchemaTag(), buildTestTemplate)

	first, err := OpenPostgres(ctx, dsn, PostgresOptions{MaxConns: 2, DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := OpenPostgres(ctx, dsn, PostgresOptions{MaxConns: 2, DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
}

// abandonInstanceLockSession leaves behind exactly what an OOM-killed container
// leaves behind: a session holding the lock that nothing will ever release,
// because the process that opened it is gone. The session outlives the helper
// on purpose — releasing it would be the one thing the real failure never does.
//
// appName is what the abandoned session calls itself, which is the whole
// question a waiting server has to answer about it.
func abandonInstanceLockSession(t *testing.T, dsn, appName string) int {
	t.Helper()
	pgbind.Register()
	db, err := sql.Open(pgbind.DriverName, dsn)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	db.SetConnMaxLifetime(0)
	db.SetConnMaxIdleTime(0)
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx, `SET application_name = '`+appName+`'`); err != nil {
		t.Fatal(err)
	}
	var acquired bool
	if err := conn.QueryRowContext(ctx, `SELECT pg_try_advisory_lock($1)`, instanceAdvisoryLock).Scan(&acquired); err != nil {
		t.Fatal(err)
	}
	if !acquired {
		t.Fatal("the abandoned session could not take the lock it is meant to be holding")
	}
	var pid int
	if err := conn.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	// Only the handles, never the lock: closing the pool would end the session
	// and hand back the lock, which is the situation this helper exists to
	// avoid. The session dies with the test's database.
	t.Cleanup(func() { _ = db.Close() })
	return pid
}

func instanceSessionExists(t *testing.T, dsn string, pid int) bool {
	t.Helper()
	pgbind.Register()
	db, err := sql.Open(pgbind.DriverName, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var alive bool
	if err := db.QueryRowContext(context.Background(),
		`SELECT EXISTS (SELECT 1 FROM pg_stat_activity WHERE pid = $1)`, pid).Scan(&alive); err != nil {
		t.Fatal(err)
	}
	return alive
}

// TestExclusiveInstanceTakesOverASilentHolder is the outage this guard caused
// and now resolves. A container killed by the kernel leaves its lock session
// behind with nothing to close it, and PostgreSQL's default keepalives take
// over two hours to notice - during which every start refused to run, naming a
// server that no longer existed.
//
// The holder is recognisable and has stopped pinging, so it is provably gone.
func TestExclusiveInstanceTakesOverASilentHolder(t *testing.T) {
	ctx := context.Background()
	dsn := pgtestdb.NewFromTemplate(t, SchemaTag(), buildTestTemplate)
	dead := abandonInstanceLockSession(t, dsn, instanceLockAppName)

	lock, err := acquireInstanceLock(ctx, dsn, instanceLockOptions{
		wait: 3 * time.Second, pingEvery: time.Second, staleAfter: 100 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("a lock held by a process that no longer exists still blocked the start: %v", err)
	}
	defer lock.release()
	if instanceSessionExists(t, dsn, dead) {
		t.Fatal("the abandoned session was left holding the lock")
	}
}

// TestExclusiveInstanceReportsAHolderItCannotVouchFor keeps the takeover narrow.
// A session that does not carry this build's marker may be a running server
// whose liveness this code has no way to read, and ending it would start the
// second server the guard exists to prevent.
func TestExclusiveInstanceReportsAHolderItCannotVouchFor(t *testing.T) {
	ctx := context.Background()
	dsn := pgtestdb.NewFromTemplate(t, SchemaTag(), buildTestTemplate)
	other := abandonInstanceLockSession(t, dsn, "some-other-rolltop")

	lock, err := acquireInstanceLock(ctx, dsn, instanceLockOptions{
		wait: 0, pingEvery: time.Second, staleAfter: 100 * time.Millisecond,
	})
	if err == nil {
		lock.release()
		t.Fatal("a lock held by an unrecognised session was taken anyway")
	}
	if !strings.Contains(err.Error(), "already running") {
		t.Fatalf("the refusal does not say what is wrong: %v", err)
	}
	// The point of naming the holder is that an operator can check it without
	// going to pg_stat_activity themselves.
	if !strings.Contains(err.Error(), fmt.Sprintf("pid %d", other)) {
		t.Fatalf("the refusal does not name the session holding the lock: %v", err)
	}
	if !strings.Contains(err.Error(), "ROLLTOP_BREAK_INSTANCE_LOCK") {
		t.Fatalf("the refusal does not say how to recover from it: %v", err)
	}
	if !instanceSessionExists(t, dsn, other) {
		t.Fatal("a session this code cannot vouch for was terminated")
	}
}

// TestExclusiveInstanceBreaksAHeldLockWhenAsked is the escape hatch the refusal
// above points at: the operator has established that nothing else is running,
// and says so once.
func TestExclusiveInstanceBreaksAHeldLockWhenAsked(t *testing.T) {
	ctx := context.Background()
	dsn := pgtestdb.NewFromTemplate(t, SchemaTag(), buildTestTemplate)
	other := abandonInstanceLockSession(t, dsn, "some-other-rolltop")

	lock, err := acquireInstanceLock(ctx, dsn, instanceLockOptions{
		wait: 0, breakHeld: true, pingEvery: time.Second, staleAfter: time.Hour,
	})
	if err != nil {
		t.Fatalf("the operator's override did not take the lock: %v", err)
	}
	defer lock.release()
	if instanceSessionExists(t, dsn, other) {
		t.Fatal("the override left the previous holder in place")
	}
}

// TestExclusiveInstanceWillNotTakeOverALiveHolder is the property the takeover
// must not cost: a server that is running keeps its database, because it keeps
// pinging. Without the ping every idle lock session would look abandoned, and
// the guard would hand the database to a second server instead of refusing it.
func TestExclusiveInstanceWillNotTakeOverALiveHolder(t *testing.T) {
	ctx := context.Background()
	dsn := pgtestdb.NewFromTemplate(t, SchemaTag(), buildTestTemplate)

	live, err := acquireInstanceLock(ctx, dsn, instanceLockOptions{
		pingEvery: 50 * time.Millisecond, staleAfter: 500 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer live.release()
	// Long enough that a holder which did not ping would be well past stale.
	time.Sleep(1500 * time.Millisecond)

	second, err := acquireInstanceLock(ctx, dsn, instanceLockOptions{
		wait: 0, pingEvery: 50 * time.Millisecond, staleAfter: 500 * time.Millisecond,
	})
	if err == nil {
		second.release()
		t.Fatal("a running server's database was taken from underneath it")
	}
	if !strings.Contains(err.Error(), "already running") {
		t.Fatalf("the refusal does not say what is wrong: %v", err)
	}
	// The live holder must still have the session it kept - asked from outside,
	// because that connection belongs to its ping goroutine.
	if !instanceSessionExists(t, dsn, live.pid) {
		t.Fatal("the running server's lock session was terminated")
	}
}

// TestInstanceLockOverrideWillNotTakeALiveServer keeps the escape hatch from
// becoming the failure it exists to recover from. An override left set in the
// environment meets a healthy server on the next slow rolling deploy, and
// honouring it there would start the second server this guard is for - by way
// of the guard's own recovery path.
func TestInstanceLockOverrideWillNotTakeALiveServer(t *testing.T) {
	ctx := context.Background()
	dsn := pgtestdb.NewFromTemplate(t, SchemaTag(), buildTestTemplate)

	live, err := acquireInstanceLock(ctx, dsn, instanceLockOptions{
		pingEvery: 50 * time.Millisecond, staleAfter: 500 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer live.release()
	time.Sleep(300 * time.Millisecond)

	second, err := acquireInstanceLock(ctx, dsn, instanceLockOptions{
		wait: 0, breakHeld: true, pingEvery: 50 * time.Millisecond, staleAfter: 500 * time.Millisecond,
	})
	if err == nil {
		second.release()
		t.Fatal("the override took the database from a server that was still answering")
	}
	if !strings.Contains(err.Error(), "already running") {
		t.Fatalf("the refusal does not say what is wrong: %v", err)
	}
	if !instanceSessionExists(t, dsn, live.pid) {
		t.Fatal("the override terminated a running server's lock session")
	}
}

// TestInstanceLockIgnoresAHolderInAnotherDatabase covers the difference between
// where the lock lives and where the catalog that lists it lives. Advisory
// locks are scoped to a database, so two deployments sharing a cluster hold
// this key at once quite legitimately - but pg_locks lists the whole cluster,
// and a lookup that does not say which database it means can pick the other
// deployment's session.
//
// Both directions are wrong and both are checked: the neighbour must not be
// terminated, and it must not stand in the way of a recovery that is this
// database's to make.
func TestInstanceLockIgnoresAHolderInAnotherDatabase(t *testing.T) {
	ctx := context.Background()
	// Two databases on one cluster, which is the whole point, so neither comes
	// from NewFromTemplate: that memoises one database per test. Empty ones do,
	// because the lock reads catalogs and takes an advisory lock and touches no
	// table of ours. The neighbour is created first so it is the older backend,
	// which is the one an unscoped lookup would settle on.
	neighbour := pgtestdb.New(t)
	mine := pgtestdb.New(t)

	// A neighbour that looks abandoned, next to a live holder of this database.
	// Unscoped, the neighbour is what the lookup finds and terminates.
	neighbourPID := abandonInstanceLockSession(t, neighbour, instanceLockAppName)
	live, err := acquireInstanceLock(ctx, mine, instanceLockOptions{
		pingEvery: 50 * time.Millisecond, staleAfter: 200 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(400 * time.Millisecond)

	second, err := acquireInstanceLock(ctx, mine, instanceLockOptions{
		wait: 0, pingEvery: 50 * time.Millisecond, staleAfter: 200 * time.Millisecond,
	})
	if err == nil {
		second.release()
		t.Fatal("a lock this database's own server was holding was taken anyway")
	}
	if !instanceSessionExists(t, neighbour, neighbourPID) {
		t.Fatal("a lock session belonging to another database was terminated")
	}
	live.release()

	// The other direction: this database's own holder is the abandoned one, and
	// the neighbour's is live. Unscoped, the live neighbour is what the lookup
	// finds, and the recovery this database is owed never happens.
	neighbourLive, err := acquireInstanceLock(ctx, neighbour, instanceLockOptions{
		pingEvery: 50 * time.Millisecond, staleAfter: 200 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer neighbourLive.release()
	abandoned := abandonInstanceLockSession(t, mine, instanceLockAppName)
	time.Sleep(400 * time.Millisecond)

	recovered, err := acquireInstanceLock(ctx, mine, instanceLockOptions{
		wait: time.Second, pingEvery: 50 * time.Millisecond, staleAfter: 200 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("an abandoned lock in this database was not recovered: %v", err)
	}
	defer recovered.release()
	if instanceSessionExists(t, mine, abandoned) {
		t.Fatal("the abandoned session was left holding the lock")
	}
	if !instanceSessionExists(t, neighbour, neighbourLive.pid) {
		t.Fatal("the neighbour's running lock session was terminated")
	}
}

// TestInstanceLockRefusalAlwaysSaysHowToRecover pins the one line an operator
// acts on. It has to survive the case where the catalogs cannot be read at all,
// which is exactly when the automatic recovery cannot run and the manual one is
// all that is left.
func TestInstanceLockRefusalAlwaysSaysHowToRecover(t *testing.T) {
	withHolder := (&errInstanceLockHeld{holder: &instanceLockHolder{
		pid: 41, appName: instanceLockAppName, state: "idle",
	}}).Error()
	withoutHolder := (&errInstanceLockHeld{}).Error()

	for _, msg := range []string{withHolder, withoutHolder} {
		if !strings.Contains(msg, "already running") {
			t.Fatalf("the refusal does not say what is wrong: %s", msg)
		}
		if !strings.Contains(msg, "ROLLTOP_BREAK_INSTANCE_LOCK") {
			t.Fatalf("the refusal does not say how to recover from it: %s", msg)
		}
	}
	if !strings.Contains(withHolder, "pid 41") {
		t.Fatalf("the refusal does not name the session it read: %s", withHolder)
	}
}

// TestInstanceLockSessionIsMarkedAndProbed pins the two things a lock session
// has to establish before it holds anything: a name the next server recognises,
// and keepalives short enough that PostgreSQL reaps it minutes rather than
// hours after the process behind it dies. Both are what the takeover above
// rests on.
func TestInstanceLockSessionIsMarkedAndProbed(t *testing.T) {
	ctx := context.Background()
	dsn := pgtestdb.NewFromTemplate(t, SchemaTag(), buildTestTemplate)

	lock, err := acquireInstanceLock(ctx, dsn, instanceLockOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer lock.release()

	var appName string
	if err := lock.conn.QueryRowContext(ctx, `SELECT current_setting('application_name')`).Scan(&appName); err != nil {
		t.Fatal(err)
	}
	if appName != instanceLockAppName {
		t.Fatalf("the lock session is not recognisable: application_name = %q", appName)
	}
	for setting, want := range map[string]int{
		"tcp_keepalives_idle":     instanceKeepaliveIdle,
		"tcp_keepalives_interval": instanceKeepaliveInterval,
		"tcp_keepalives_count":    instanceKeepaliveCount,
	} {
		var got int
		if err := lock.conn.QueryRowContext(ctx, `SELECT current_setting($1)::int`, setting).Scan(&got); err != nil {
			t.Fatal(err)
		}
		// A Unix-socket connection reports 0 for all three: PostgreSQL accepts
		// the setting and has nothing to apply it to, which is not a failure.
		if got != want && got != 0 {
			t.Fatalf("%s = %d, want %d", setting, got, want)
		}
	}
}

// TestExclusiveInstanceSaysWhenItWasStoppedWhileWaiting keeps a stop signal
// during startup out of the misconfiguration message. The two read alike in a
// container log and mean opposite things - one sends an operator hunting for a
// second server that was never there - and cmd/rolltop tells them apart by the
// cancellation this has to keep wrapping.
func TestExclusiveInstanceSaysWhenItWasStoppedWhileWaiting(t *testing.T) {
	dsn := pgtestdb.NewFromTemplate(t, SchemaTag(), buildTestTemplate)
	abandonInstanceLockSession(t, dsn, instanceLockAppName)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()
	lock, err := acquireInstanceLock(ctx, dsn, instanceLockOptions{
		wait: time.Minute, staleAfter: time.Hour,
	})
	if err == nil {
		lock.release()
		t.Fatal("the wait outlived the stop signal")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("a stopped startup is not reported as one: %v", err)
	}
	if strings.Contains(err.Error(), "already running") {
		t.Fatalf("a stop signal was reported as a second server: %v", err)
	}
}
