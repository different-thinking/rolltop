// File overview: Tests for the split-mode sweep fan-out. Background sweeps meet
// a damaged tenant far more often than the open path does: they run every minute
// against a handle that was opened while the file was still intact, so the
// failure arrives from a statement rather than from open(). These cover what the
// sweep does with that tenant, and what it must keep doing for every other one.

package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"rolltop/internal/testlog"
)

// openSweepStore returns a split store with the named tenants already created.
func openSweepStore(t *testing.T, emails ...string) (*Store, string, []User) {
	t.Helper()
	ctx := context.Background()
	dataDir := filepath.Join(t.TempDir(), "data")
	db, err := OpenServer(filepath.Join(dataDir, "rolltop.db"), dataDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	users := make([]User, 0, len(emails))
	for _, email := range emails {
		user, err := db.CreateUser(ctx, email, email, "hash", false)
		if err != nil {
			t.Fatal(err)
		}
		users = append(users, user)
	}
	return db, dataDir, users
}

func TestSweepLatchesAndSkipsATenantThatFailsWithCorruption(t *testing.T) {
	ctx := context.Background()
	db, _, users := openSweepStore(t, "damaged@example.test", "healthy@example.test")
	damaged, healthy := users[0], users[1]

	var visited []int64
	err := db.forEachServiceableUser(ctx, "test sweep", func(user User, _ *Store) error {
		visited = append(visited, user.ID)
		if user.ID == damaged.ID {
			// What a statement on an already-open handle reports once the file
			// underneath it has been damaged.
			return fmt.Errorf("update sync_runs: %w", ErrCorrupt)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("one damaged tenant failed the whole sweep: %v", err)
	}
	// Every tenant behind the damaged one used to go unvisited, which is how a
	// single broken file stopped reconciliation for the whole installation.
	if len(visited) != 2 || visited[0] != damaged.ID || visited[1] != healthy.ID {
		t.Fatalf("sweep visited %v, want both tenants in order", visited)
	}
	if !db.DatabaseCorrupt(damaged.ID) {
		t.Fatal("the damaged tenant was not latched")
	}
	if db.DatabaseCorrupt(healthy.ID) {
		t.Fatal("the healthy tenant was latched")
	}

	// The latch is what makes the next sweep quiet: the tenant is gone from the
	// serviceable set, so nothing reopens a file that cannot answer.
	visited = nil
	if err := db.forEachServiceableUser(ctx, "test sweep", func(user User, _ *Store) error {
		visited = append(visited, user.ID)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(visited) != 1 || visited[0] != healthy.ID {
		t.Fatalf("second sweep visited %v, want only the healthy tenant", visited)
	}
}

func TestSweepReportsADamagedTenantOnceWithItsRepairCommand(t *testing.T) {
	ctx := context.Background()
	logs := testlog.Capture(t)
	db, dataDir, users := openSweepStore(t, "damaged@example.test")
	damaged := users[0]

	for range 3 {
		if err := db.forEachServiceableUser(ctx, "reconcile stale sync runs", func(_ User, _ *Store) error {
			return fmt.Errorf("update sync_runs: %w", ErrCorrupt)
		}); err != nil {
			t.Fatalf("sweep reported the damaged tenant as a failure: %v", err)
		}
	}

	lines := strings.Count(logs.String(), "reconcile stale sync runs")
	if lines != 1 {
		t.Fatalf("sweep logged the damaged tenant %d times, want 1:\n%s", lines, logs.String())
	}
	// The point of the line is that it is actionable on its own: a bare driver
	// message tells an operator neither which file broke nor how to repair it.
	for _, want := range []string{
		UserDatabaseFilePath(dataDir, damaged.ID),
		fmt.Sprintf("rolltop recover-db --user-id %d --confirm-offline", damaged.ID),
	} {
		if !strings.Contains(logs.String(), want) {
			t.Fatalf("sweep log does not mention %q:\n%s", want, logs.String())
		}
	}
}

func TestSweepAbortsOnAnErrorThatIsNotCorruption(t *testing.T) {
	ctx := context.Background()
	db, _, users := openSweepStore(t, "first@example.test", "second@example.test")

	failure := errors.New("mailbox generation rebuild crossed tenant boundary")
	visited := 0
	err := db.forEachServiceableUser(ctx, "test sweep", func(_ User, _ *Store) error {
		visited++
		return failure
	})
	// A tenant is only dropped on evidence that its file cannot answer at all.
	// Skipping on any error would turn a bug in one tenant's data into a tenant
	// that quietly stops being swept.
	if !errors.Is(err, failure) {
		t.Fatalf("sweep returned %v, want the callback's failure", err)
	}
	if visited != 1 {
		t.Fatalf("sweep visited %d tenants after a hard failure, want 1", visited)
	}
	for _, user := range users {
		if db.DatabaseCorrupt(user.ID) {
			t.Fatalf("user %d was latched by an error that is not corruption", user.ID)
		}
	}
}

func TestSweepStopsEarlyWithoutReportingAFailure(t *testing.T) {
	ctx := context.Background()
	db, _, _ := openSweepStore(t, "first@example.test", "second@example.test")

	visited := 0
	err := db.forEachServiceableUser(ctx, "test sweep", func(_ User, _ *Store) error {
		visited++
		return errSweepDone
	})
	if err != nil {
		t.Fatalf("a budgeted sweep that filled its batch reported %v", err)
	}
	if visited != 1 {
		t.Fatalf("sweep visited %d tenants after finishing early, want 1", visited)
	}
}

func TestInterruptStaleSyncRunsKeepsGoingPastADamagedTenant(t *testing.T) {
	ctx := context.Background()
	db, dataDir, users := openSweepStore(t, "damaged@example.test", "healthy@example.test")
	damaged, healthy := users[0], users[1]

	// Give the healthy tenant a stale run, so the sweep has something to do
	// after the damaged tenant has turned it back.
	healthyStore, err := db.UserStore(ctx, healthy.ID)
	if err != nil {
		t.Fatal(err)
	}
	now := nowUnix()
	if _, err := healthyStore.db.ExecContext(ctx, `INSERT INTO mail_accounts
			(id, user_id, email, host, port, username, encrypted_password, created_at, updated_at)
		VALUES (1, ?, ?, 'imap.example.test', 993, 'healthy', 'secret', ?, ?)`,
		healthy.ID, healthy.Email, now, now); err != nil {
		t.Fatal(err)
	}
	stale := now - int64((2 * time.Hour).Seconds())
	if _, err := healthyStore.db.ExecContext(ctx, `INSERT INTO sync_runs (user_id, account_id, status, started_at, updated_at)
		VALUES (?, 1, 'running', ?, ?)`, healthy.ID, stale, stale); err != nil {
		t.Fatal(err)
	}
	// Replace the other tenant's file with something SQLite cannot read.
	if _, err := db.UserStore(ctx, damaged.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(UserDatabaseFilePath(dataDir, damaged.ID), []byte("this is not a database"), 0o600); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenServer(filepath.Join(dataDir, "rolltop.db"), dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()

	n, err := reopened.InterruptStaleSyncRuns(ctx, time.Hour)
	if err != nil {
		t.Fatalf("stale sync run reconciliation failed on one damaged tenant: %v", err)
	}
	if n != 1 {
		t.Fatalf("interrupted %d stale runs, want the healthy tenant's 1", n)
	}
	if !reopened.DatabaseCorrupt(damaged.ID) {
		t.Fatal("the damaged tenant was not latched")
	}
}
