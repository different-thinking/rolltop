// File overview: What a start that cannot reach its database has to say.

package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"rolltop/backend/pgbind"
)

// unreachableDB is a pool pointed at a port nothing listens on, so every
// connection attempt fails to dial. sql.Open is lazy, so nothing happens until
// the wait pings.
func unreachableDB(t *testing.T) *sql.DB {
	t.Helper()
	pgbind.Register()
	db, err := sql.Open(pgbind.DriverName, "postgres://rolltop:hunter2@127.0.0.1:1/rolltop?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// TestWaitForPostgresReportsWhyItWaitedWhenCancelled is the case a hosted
// deployment actually hits: the database is unreachable and the orchestrator
// restarts the container a second later, long before the connect budget runs
// out. Reporting only "context canceled" makes that silence permanent — every
// restart repeats it and the reason is never printed.
func TestWaitForPostgresReportsWhyItWaitedWhenCancelled(t *testing.T) {
	db := unreachableDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()

	started := time.Now()
	err := waitForPostgres(ctx, db, 2*time.Minute)
	if err == nil {
		t.Fatal("an unreachable database was reported as available")
	}
	if waited := time.Since(started); waited > 30*time.Second {
		t.Fatalf("the cancellation was ignored: waited %s", waited)
	}
	// Still recognisable as a shutdown, so callers that distinguish one from a
	// broken database keep working.
	if !errors.Is(err, context.Canceled) {
		t.Errorf("the cancellation was swallowed: %v", err)
	}
	// And it names the failure it was waiting on.
	if !strings.Contains(err.Error(), "last connection attempt") {
		t.Errorf("the error does not say what it was waiting for: %v", err)
	}
	if !strings.Contains(err.Error(), "127.0.0.1:1") {
		t.Errorf("the error does not name the target it could not reach: %v", err)
	}
}

// TestWaitForPostgresKeepsThePasswordOutOfTheError is why these go through
// postgresError. pgx quotes the whole connection string back from its dial and
// parse failures, and this error is printed straight into the container log.
func TestWaitForPostgresKeepsThePasswordOutOfTheError(t *testing.T) {
	db := unreachableDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()
	err := waitForPostgres(ctx, db, 2*time.Minute)
	if err == nil {
		t.Fatal("an unreachable database was reported as available")
	}
	if strings.Contains(err.Error(), "hunter2") {
		t.Fatalf("the error carries the password: %v", err)
	}
}

// TestWaitForPostgresReportsTheFailureWithoutABudget keeps the zero-budget path
// honest: one attempt, then the driver's own reason.
func TestWaitForPostgresReportsTheFailureWithoutABudget(t *testing.T) {
	err := waitForPostgres(context.Background(), unreachableDB(t), 0)
	if err == nil {
		t.Fatal("an unreachable database was reported as available")
	}
	if errors.Is(err, context.Canceled) {
		t.Errorf("a refused connection was reported as a cancellation: %v", err)
	}
	if !strings.Contains(err.Error(), "127.0.0.1:1") {
		t.Errorf("the error does not name the target: %v", err)
	}
}
