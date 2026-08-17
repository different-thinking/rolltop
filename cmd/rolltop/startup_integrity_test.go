package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"rolltop/backend/config"
	"rolltop/backend/store"
)

func TestClaimRunningMarkerReportsPreviousUncleanExit(t *testing.T) {
	dataDir := t.TempDir()
	if claimRunningMarker(dataDir) {
		t.Fatal("first start reported an unclean previous shutdown")
	}
	if _, err := os.Stat(runningMarkerPath(dataDir)); err != nil {
		t.Fatalf("running marker was not written: %v", err)
	}
	// A process that is killed never reaches releaseRunningMarker.
	if !claimRunningMarker(dataDir) {
		t.Fatal("start after a killed process did not report an unclean shutdown")
	}
	releaseRunningMarker(dataDir)
	if _, err := os.Stat(runningMarkerPath(dataDir)); !os.IsNotExist(err) {
		t.Fatalf("running marker survived a clean shutdown: %v", err)
	}
	if claimRunningMarker(dataDir) {
		t.Fatal("start after a clean shutdown reported an unclean shutdown")
	}
}

func TestStartupIntegrityCheckRequiredFollowsConfiguredMode(t *testing.T) {
	for _, testCase := range []struct {
		mode    string
		unclean bool
		want    bool
	}{
		{config.IntegrityCheckAuto, false, false},
		{config.IntegrityCheckAuto, true, true},
		{config.IntegrityCheckAlways, false, true},
		{config.IntegrityCheckNever, true, false},
		{"", true, true},
	} {
		if got := startupIntegrityCheckRequired(testCase.mode, testCase.unclean); got != testCase.want {
			t.Fatalf("startupIntegrityCheckRequired(%q, %v) = %v, want %v", testCase.mode, testCase.unclean, got, testCase.want)
		}
	}
}

func TestVerifyUserDatabasesLatchesDamagedTenant(t *testing.T) {
	ctx := context.Background()
	userID := writeMaintenanceFixture(t, 4000)
	dataDir := os.Getenv("ROLLTOP_DATA_DIR")
	corruptDatabaseFile(t, userDatabasePath(dataDir, userID))

	db, err := store.OpenServer(filepath.Join(dataDir, "rolltop.db"), dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	damaged, err := verifyUserDatabases(ctx, db, dataDir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(damaged) == 0 {
		t.Skip("SQLite did not report the injected page damage on this platform")
	}
	if len(damaged) != 1 || damaged[0] != userID {
		t.Fatalf("damaged user IDs = %v, want [%d]", damaged, userID)
	}
	if !db.DatabaseCorrupt(userID) {
		t.Fatal("damaged tenant was not latched as corrupt")
	}
	records := db.CorruptDatabases()
	if len(records) != 1 || records[0].Path != userDatabasePath(dataDir, userID) {
		t.Fatalf("corruption records = %+v", records)
	}
}

func TestVerifyUserDatabasesAcceptsIntactTenant(t *testing.T) {
	ctx := context.Background()
	userID := writeMaintenanceFixture(t, 50)
	dataDir := os.Getenv("ROLLTOP_DATA_DIR")

	db, err := store.OpenServer(filepath.Join(dataDir, "rolltop.db"), dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	damaged, err := verifyUserDatabases(ctx, db, dataDir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(damaged) != 0 {
		t.Fatalf("intact tenant reported as damaged: %v", damaged)
	}
	if db.DatabaseCorrupt(userID) {
		t.Fatal("intact tenant was latched as corrupt")
	}
}
