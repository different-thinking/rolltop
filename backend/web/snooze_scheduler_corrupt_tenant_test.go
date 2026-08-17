// File overview: The snooze scheduler against a tenant database that has
// stopped answering. The scheduler runs every 30 seconds on failure, so a
// tenant it cannot read decides both how loud the log gets and how accurately
// every other tenant is woken.

package web

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"rolltop/backend/store"
	"rolltop/internal/testlog"
)

func TestSnoozeSchedulerLatchesADamagedTenantInsteadOfBackingOffForever(t *testing.T) {
	ctx := context.Background()
	logs := testlog.Capture(t)
	dataDir := filepath.Join(t.TempDir(), "data")
	databasePath := filepath.Join(dataDir, "rolltop.db")
	db, err := store.OpenServer(databasePath, dataDir)
	if err != nil {
		t.Fatal(err)
	}
	damaged, err := db.CreateUser(ctx, "a-damaged@example.test", "Damaged", "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	healthy, err := db.CreateUser(ctx, "b-healthy@example.test", "Healthy", "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.UserStore(ctx, damaged.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.UserStore(ctx, healthy.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(store.UserDatabaseFilePath(dataDir, damaged.ID), []byte("this is not a database"), 0o600); err != nil {
		t.Fatal(err)
	}

	reopened, err := store.OpenServer(databasePath, dataDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	server, err := New(Options{
		Store: reopened, DataDir: dataDir, DatabasePath: databasePath, PluginDir: t.TempDir(),
		DisableBackgroundWorkers: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })

	// Server startup warms mail lists and latches the tenant on the way past.
	// Release it so the scheduler is the path that discovers the damage, which
	// is what happens when a file breaks under a server that is already up.
	reopened.ClearCorruption(damaged.ID)

	for range 3 {
		// Returning the damaged tenant's error held the scheduler in its
		// 30-second error backoff, so every healthy tenant's reminder was woken
		// on that cadence instead of its own due time.
		if _, err := server.processDueSnoozes(ctx, time.Now().UTC()); err != nil {
			t.Fatalf("one damaged tenant failed the whole scheduler pass: %v", err)
		}
	}
	if !reopened.DatabaseCorrupt(damaged.ID) {
		t.Fatal("the damaged tenant was not latched")
	}
	if reopened.DatabaseCorrupt(healthy.ID) {
		t.Fatal("the healthy tenant was latched")
	}
	if lines := strings.Count(logs.String(), "snooze scheduler user_id="); lines != 1 {
		t.Fatalf("scheduler logged the damaged tenant %d times, want 1:\n%s", lines, logs.String())
	}
	if want := "recover-db --user-id"; !strings.Contains(logs.String(), want) {
		t.Fatalf("scheduler log does not carry the repair command:\n%s", logs.String())
	}
}
