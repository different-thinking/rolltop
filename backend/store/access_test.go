// File overview: SQLite access mode selection and what each mode configures.

package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseAccessMode(t *testing.T) {
	for raw, want := range map[string]AccessMode{
		"":            AccessAuto,
		"auto":        AccessAuto,
		"shared":      AccessShared,
		"NORMAL":      AccessShared,
		"exclusive":   AccessExclusive,
		" Exclusive ": AccessExclusive,
	} {
		got, err := ParseAccessMode(raw)
		if err != nil || got != want {
			t.Fatalf("ParseAccessMode(%q) = %v, %v, want %v", raw, got, err, want)
		}
	}
	if _, err := ParseAccessMode("wal"); err == nil {
		t.Fatal("an unknown access mode was accepted")
	}
}

// Exclusive mode exists to keep SQLite away from the shared WAL index, and the
// pragma has to be in the DSN because SQLite can only leave shared memory out of
// a database it has not yet opened normally.
func TestExclusiveAccessAsksForLockingModeAndOneConnection(t *testing.T) {
	shared := AccessShared.dataSourceName("/data/rolltop.db")
	exclusive := AccessExclusive.dataSourceName("/data/rolltop.db")
	for _, required := range []string{"_journal_mode=WAL", "_busy_timeout=5000", "_txlock=immediate", "_foreign_keys=on"} {
		if !strings.Contains(shared, required) || !strings.Contains(exclusive, required) {
			t.Fatalf("%q missing from one of the DSNs", required)
		}
	}
	if strings.Contains(shared, "_locking_mode") {
		t.Fatalf("shared DSN sets a locking mode: %s", shared)
	}
	if !strings.Contains(exclusive, "_locking_mode=exclusive") {
		t.Fatalf("exclusive DSN = %s", exclusive)
	}

	db := &sql.DB{}
	AccessShared.applyConnectionLimits(db)
	if got := db.Stats().MaxOpenConnections; got != 4 {
		t.Fatalf("shared MaxOpenConnections = %d, want 4", got)
	}
	// One connection is not a tuning choice: the connection holds the file lock,
	// so a second one could only wait for a lock that is never released.
	AccessExclusive.applyConnectionLimits(db)
	if got := db.Stats().MaxOpenConnections; got != 1 {
		t.Fatalf("exclusive MaxOpenConnections = %d, want 1", got)
	}
	if AccessExclusive.SharesFiles() || !AccessShared.SharesFiles() {
		t.Fatal("SharesFiles does not distinguish the modes")
	}
}

func TestResolveAccessModeKeepsAnExplicitChoice(t *testing.T) {
	dir := t.TempDir()
	if got, _ := ResolveAccessMode(AccessShared, dir); got != AccessShared {
		t.Fatalf("explicit shared resolved to %v", got)
	}
	// An operator who knows their storage outranks a superblock lookup, in both
	// directions.
	if got, _ := ResolveAccessMode(AccessExclusive, dir); got != AccessExclusive {
		t.Fatalf("explicit exclusive resolved to %v", got)
	}
	got, report := ResolveAccessMode(AccessAuto, dir)
	if report.SharedMemorySafe && got != AccessShared {
		t.Fatalf("auto on a %s filesystem resolved to %v", report.Name, got)
	}
	if !report.SharedMemorySafe && got != AccessExclusive {
		t.Fatalf("auto on a %s filesystem resolved to %v", report.Name, got)
	}
}

// A database opened exclusively still has to behave like a database.
func TestExclusiveStoreReadsAndWrites(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "rolltop.db")
	db, err := open(path, "", false, schemaCombined, nil, defaultPluginCatalog(), AccessExclusive)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if got := db.AccessMode(); got != AccessExclusive {
		t.Fatalf("access mode = %v", got)
	}
	user, err := db.CreateUser(ctx, "exclusive@example.test", "Exclusive", "hash", true)
	if err != nil {
		t.Fatal(err)
	}
	found, err := db.GetUserByID(ctx, user.ID)
	if err != nil || found.Email != user.Email {
		t.Fatalf("GetUserByID = %+v, %v", found, err)
	}
	// quick_check and VACUUM INTO have to run through the handle that owns the
	// file, because in this mode nothing else can open it.
	problems, err := db.CheckDatabase(ctx, 0, path)
	if err != nil || len(problems) != 0 {
		t.Fatalf("CheckDatabase = %v, %v", problems, err)
	}
	dest := filepath.Join(t.TempDir(), "backup", "rolltop.db")
	size, err := db.BackupDatabase(ctx, 0, path, dest)
	if err != nil || size <= 0 {
		t.Fatalf("BackupDatabase = %d, %v", size, err)
	}
}

// A tenant that never opened - or that was latched as corrupt - has no live
// handle, so the file-based check has to stand in for it.
func TestCheckDatabaseFallsBackToTheFileWithoutALiveHandle(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	db, err := OpenServer(filepath.Join(dataDir, "rolltop.db"), dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	user, err := db.CreateUser(ctx, "tenant@example.test", "Tenant", "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.UserStore(ctx, user.ID); err != nil {
		t.Fatal(err)
	}
	path := UserDatabaseFilePath(dataDir, user.ID)
	if _, live := db.liveDatabase(user.ID); !live {
		t.Fatal("an opened tenant reports no live handle")
	}
	if _, live := db.liveDatabase(user.ID + 1); live {
		t.Fatal("an unopened tenant reports a live handle")
	}
	problems, err := db.CheckDatabase(ctx, user.ID+1, path)
	if err != nil || len(problems) != 0 {
		t.Fatalf("CheckDatabase without a live handle = %v, %v", problems, err)
	}
}
