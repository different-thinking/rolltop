package main

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"rolltop/backend/store"
)

func TestScheduledRepairsRebuildMarkedTenantAndClearTheMarker(t *testing.T) {
	ctx := context.Background()
	userID := writeMaintenanceFixture(t, 4000)
	dataDir := os.Getenv("ROLLTOP_DATA_DIR")
	databasePath := store.UserDatabaseFilePath(dataDir, userID)
	corruptDatabaseFile(t, databasePath)
	if problems, err := store.CheckDatabaseFile(ctx, databasePath); err != nil || len(problems) == 0 {
		t.Skip("SQLite did not report the injected page damage on this platform")
	}

	if err := store.ScheduleUserDatabaseRepair(dataDir, userID, "admin@example.test", time.Now()); err != nil {
		t.Fatal(err)
	}
	outcomes, err := runScheduledDatabaseRepairs(ctx, dataDir, nil, time.Now(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(outcomes) != 1 || !outcomes[0].Succeeded {
		t.Fatalf("repair outcomes = %+v", outcomes)
	}

	if _, found, err := store.UserDatabaseRepairRequest(dataDir, userID); err != nil || found {
		t.Fatalf("repair marker survived: %v %v", found, err)
	}
	report, found, err := store.UserDatabaseRepairReport(dataDir, userID)
	if err != nil || !found {
		t.Fatalf("repair report missing: %v %v", found, err)
	}
	if report.Report.RowsCopied == 0 || report.QuarantinePath == "" {
		t.Fatalf("persisted report = %+v", report)
	}
	if _, err := os.Stat(report.QuarantinePath); err != nil {
		t.Fatalf("damaged file was not quarantined: %v", err)
	}
	if problems, err := store.CheckDatabaseFile(ctx, databasePath); err != nil || len(problems) != 0 {
		t.Fatalf("repaired database still reports problems: %v %v", problems, err)
	}
	if recovered := countMaintenanceMessages(t, databasePath); recovered == 0 || recovered >= 4000 {
		t.Fatalf("recovered %d messages from a damaged file holding 4000", recovered)
	}
}

func TestScheduledRepairsIgnoreUnmarkedTenants(t *testing.T) {
	ctx := context.Background()
	userID := writeMaintenanceFixture(t, 20)
	dataDir := os.Getenv("ROLLTOP_DATA_DIR")

	outcomes, err := runScheduledDatabaseRepairs(ctx, dataDir, nil, time.Now(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(outcomes) != 0 {
		t.Fatalf("unmarked tenant was repaired: %+v", outcomes)
	}
	quarantined, err := filepath.Glob(store.UserDatabaseFilePath(dataDir, userID) + ".corrupt-*")
	if err != nil || len(quarantined) != 0 {
		t.Fatalf("unmarked tenant database was moved: %v %v", quarantined, err)
	}
}

func TestScheduledRepairClearsMarkerWhenTheRepairFails(t *testing.T) {
	ctx := context.Background()
	dataDir := filepath.Join(t.TempDir(), "data")
	// A marker for a tenant whose database does not exist cannot be repaired.
	if err := os.MkdirAll(filepath.Join(dataDir, "users", "7"), 0o700); err != nil {
		t.Fatal(err)
	}
	markerPath := store.UserDatabaseFilePath(dataDir, 7) + ".repair-requested"
	if err := os.WriteFile(markerPath, []byte(`{"user_id":7}`), 0o600); err != nil {
		t.Fatal(err)
	}

	outcomes, err := runScheduledDatabaseRepairs(ctx, dataDir, nil, time.Now(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(outcomes) != 1 || outcomes[0].Succeeded || outcomes[0].Error == "" {
		t.Fatalf("failed repair outcome = %+v", outcomes)
	}
	// Retrying on every boot would turn one damaged tenant into a restart loop.
	if _, err := os.Stat(markerPath); !os.IsNotExist(err) {
		t.Fatalf("marker survived a failed repair: %v", err)
	}
	report, found, err := store.UserDatabaseRepairReport(dataDir, 7)
	if err != nil || !found || report.Succeeded {
		t.Fatalf("failure was not recorded: %+v %v %v", report, found, err)
	}
}

func TestRestoreQuarantinedDatabasePutsTheOriginalBack(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	databasePath := filepath.Join(root, "rolltop.db")
	quarantinePath := databasePath + ".corrupt-test"

	// The state after a repair installed a copy that then failed verification:
	// the original sits in quarantine, the rejected copy is live.
	writeMinimalDatabase(t, quarantinePath)
	original, err := os.ReadFile(quarantinePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(databasePath, []byte("not a database"), 0o600); err != nil {
		t.Fatal(err)
	}

	outcome := store.RepairOutcome{QuarantinePath: quarantinePath}
	cause := errors.New("recovered database is still damaged")
	err = restoreQuarantinedDatabase(quarantinePath, databasePath, cause, &outcome)
	if err == nil {
		t.Fatal("rollback swallowed the failure that caused it")
	}
	if !errors.Is(err, cause) {
		t.Fatalf("rollback error lost its cause: %v", err)
	}
	if !strings.Contains(err.Error(), "restored") {
		t.Fatalf("error does not say the original was restored: %v", err)
	}

	restored, err := os.ReadFile(databasePath)
	if err != nil {
		t.Fatalf("the original database is gone: %v", err)
	}
	if !bytes.Equal(original, restored) {
		t.Fatal("the live database is not the original that was quarantined")
	}
	if outcome.QuarantinePath != "" {
		t.Fatalf("outcome still points at a quarantine: %q", outcome.QuarantinePath)
	}
	if leftovers, err := filepath.Glob(databasePath + ".rejected-*"); err != nil || len(leftovers) != 0 {
		t.Fatalf("the rejected recovery was left behind: %v %v", leftovers, err)
	}
	if problems, err := store.CheckDatabaseFile(ctx, databasePath); err != nil || len(problems) != 0 {
		t.Fatalf("restored database is not sound: %v %v", problems, err)
	}
}

// writeMinimalDatabase creates a small but genuinely valid SQLite file.
func writeMinimalDatabase(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE kept (id INTEGER PRIMARY KEY, note TEXT)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO kept (note) VALUES ('original')`); err != nil {
		t.Fatal(err)
	}
}

func TestQuarantineRollbackKeepsTheDatabaseAndItsWALTogether(t *testing.T) {
	root := t.TempDir()
	databasePath := filepath.Join(root, "rolltop.db")
	quarantinePath := filepath.Join(root, "rolltop.db.corrupt-test")
	for suffix, content := range map[string]string{"": "database", "-wal": "write ahead log"} {
		if err := os.WriteFile(databasePath+suffix, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// Non-empty directories at both the source and target -shm paths make its
	// rename fail (target not empty) and its removal fail (source not empty),
	// which forces the rollback after the -wal has already moved.
	for _, dir := range []string{databasePath + "-shm", quarantinePath + "-shm"} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "blocker"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	if err := quarantineSQLiteFileSet(databasePath, quarantinePath); err == nil {
		t.Fatal("quarantine reported success despite an unmovable sidecar")
	}
	// A database restored without its WAL has silently lost every transaction
	// that log still held.
	for suffix, want := range map[string]string{"": "database", "-wal": "write ahead log"} {
		got, err := os.ReadFile(databasePath + suffix)
		if err != nil {
			t.Fatalf("rollback did not restore %s: %v", databasePath+suffix, err)
		}
		if string(got) != want {
			t.Fatalf("restored %s = %q, want %q", databasePath+suffix, got, want)
		}
	}
	if _, err := os.Stat(quarantinePath); !os.IsNotExist(err) {
		t.Fatalf("rollback left the database at the quarantine path: %v", err)
	}
	if _, err := os.Stat(quarantinePath + "-wal"); !os.IsNotExist(err) {
		t.Fatalf("rollback left the WAL at the quarantine path: %v", err)
	}
}

func TestClaimRunningMarkerSurvivesWithoutBufferedWrites(t *testing.T) {
	dataDir := t.TempDir()
	if claimRunningMarker(dataDir) {
		t.Fatal("first start reported an unclean previous shutdown")
	}
	// The marker only helps if it reached the disk before the power cut it is
	// meant to detect, so the write path must fsync the file and its directory.
	raw, err := os.ReadFile(runningMarkerPath(dataDir))
	if err != nil {
		t.Fatalf("marker was not written: %v", err)
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		t.Fatal("marker is empty")
	}
}
