package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
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
	corruptDatabaseFile(t, store.UserDatabaseFilePath(dataDir, userID))

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
	if len(records) != 1 || records[0].Path != store.UserDatabaseFilePath(dataDir, userID) {
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

func TestDamagedDatabaseWarningsNameTheFileAndTheRepairCommand(t *testing.T) {
	warnings := damagedDatabaseWarnings([]store.DatabaseHealth{
		{UserID: 9, Path: "/data/users/9/rolltop.db", Detail: "database disk image is malformed"},
		{UserID: 2, Path: "/data/users/2/rolltop.db", Detail: ""},
	})
	if len(warnings) != 2 {
		t.Fatalf("warnings = %#v", warnings)
	}
	// A map-ordered log would reshuffle on every start and defeat diffing.
	if !strings.HasPrefix(warnings[0], "user 2 ") || !strings.HasPrefix(warnings[1], "user 9 ") {
		t.Fatalf("warnings are not ordered by user: %#v", warnings)
	}
	// Without the path an operator cannot tell which file to back up, and
	// without the command they cannot act on the line at all.
	for _, want := range []string{
		"/data/users/9/rolltop.db",
		"database disk image is malformed",
		`"rolltop recover-db --user-id 9 --confirm-offline"`,
	} {
		if !strings.Contains(warnings[1], want) {
			t.Fatalf("warning %q is missing %q", warnings[1], want)
		}
	}
	if strings.Contains(warnings[0], ": ;") {
		t.Fatalf("a record without a detail produced a hole in the line: %q", warnings[0])
	}
	if damagedDatabaseWarnings(nil) != nil {
		t.Fatal("a healthy installation produced a warning")
	}
}

func TestDamagedTenantIsReportedAfterPrepareUserStores(t *testing.T) {
	ctx := context.Background()
	dataDir := filepath.Join(t.TempDir(), "data")
	db, err := store.OpenServer(filepath.Join(dataDir, "rolltop.db"), dataDir)
	if err != nil {
		t.Fatal(err)
	}
	user, err := db.CreateUser(ctx, "damaged@example.test", "Damaged", "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.UserStore(ctx, user.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(store.UserDatabaseFilePath(dataDir, user.ID), []byte("this is not a database"), 0o600); err != nil {
		t.Fatal(err)
	}

	reopened, err := store.OpenServer(filepath.Join(dataDir, "rolltop.db"), dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if err := reopened.PrepareUserStores(ctx, nil); err != nil {
		t.Fatalf("one damaged tenant kept the installation down: %v", err)
	}
	// The skip is what keeps the other accounts served; the log line is the
	// only thing that stops it from being silent.
	warnings := damagedDatabaseWarnings(reopened.CorruptDatabases())
	if len(warnings) != 1 {
		t.Fatalf("skipped tenant produced %d warnings: %#v", len(warnings), warnings)
	}
	if !strings.Contains(warnings[0], store.UserDatabaseFilePath(dataDir, user.ID)) {
		t.Fatalf("warning does not name the damaged file: %q", warnings[0])
	}
}
