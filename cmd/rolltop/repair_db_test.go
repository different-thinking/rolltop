package main

import (
	"context"
	"os"
	"path/filepath"
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
