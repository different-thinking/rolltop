// File overview: SQLite access mode selection and what each mode configures.

package store

import (
	"context"
	"database/sql"
	"os"
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
	for _, required := range []string{"_busy_timeout=5000", "_txlock=immediate", "_foreign_keys=on"} {
		if !strings.Contains(shared, required) || !strings.Contains(exclusive, required) {
			t.Fatalf("%q missing from one of the DSNs", required)
		}
	}
	if strings.Contains(shared, "_locking_mode") || !strings.Contains(shared, "_journal_mode=WAL") {
		t.Fatalf("shared DSN = %s", shared)
	}
	if AccessShared.driverName() != "sqlite3" {
		t.Fatalf("shared driver = %s", AccessShared.driverName())
	}
	// The driver applies journal_mode before locking_mode whatever the DSN
	// says, so exclusive mode asks for WAL from the connect hook instead.
	if !strings.Contains(exclusive, "_locking_mode=exclusive") || strings.Contains(exclusive, "_journal_mode") {
		t.Fatalf("exclusive DSN = %s", exclusive)
	}
	if AccessExclusive.driverName() != exclusiveDriverName {
		t.Fatalf("exclusive driver = %s", AccessExclusive.driverName())
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

// The whole point of exclusive mode is that SQLite never creates the shared
// index, and the case that matters is a database that is already in WAL - which
// is every database that has ever run. SQLite only leaves the index out when
// EXCLUSIVE locking is set before the first WAL access, and the driver applies
// DSN pragmas in its own order, so this asserts the outcome rather than the
// spelling.
func TestExclusiveAccessNeverCreatesTheSharedIndex(t *testing.T) {
	ctx := context.Background()
	for _, existing := range []bool{false, true} {
		name := "fresh database"
		if existing {
			name = "database already in WAL"
		}
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "rolltop.db")
			if existing {
				seeded, err := open(path, "", false, schemaCombined, nil, defaultPluginCatalog(), AccessShared)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := seeded.CreateUser(ctx, "seed@example.test", "Seed", "hash", false); err != nil {
					t.Fatal(err)
				}
				if err := seeded.Close(); err != nil {
					t.Fatal(err)
				}
				if _, err := os.Stat(path + "-shm"); err == nil {
					t.Fatal("the seeded database left a shared index behind, so this proves nothing")
				}
			}

			db, err := open(path, "", false, schemaCombined, nil, defaultPluginCatalog(), AccessExclusive)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if _, err := db.CreateUser(ctx, "exclusive@example.test", "Exclusive", "hash", false); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(path + "-shm"); !os.IsNotExist(err) {
				t.Fatalf("exclusive mode created %s-shm: %v", path, err)
			}
			// It still has to be a WAL database: the mode is about the index,
			// not about giving up the write-ahead log.
			var journal string
			if err := db.db.QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&journal); err != nil {
				t.Fatal(err)
			}
			if !strings.EqualFold(journal, "wal") {
				t.Fatalf("journal mode = %q, want wal", journal)
			}
			var locking string
			if err := db.db.QueryRowContext(ctx, `PRAGMA locking_mode`).Scan(&locking); err != nil {
				t.Fatal(err)
			}
			if !strings.EqualFold(locking, "exclusive") {
				t.Fatalf("locking mode = %q, want exclusive", locking)
			}
		})
	}
}

// Shared mode keeps the index it depends on.
func TestSharedAccessKeepsTheSharedIndex(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rolltop.db")
	db, err := open(path, "", false, schemaCombined, nil, defaultPluginCatalog(), AccessShared)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.CreateUser(context.Background(), "shared@example.test", "Shared", "hash", false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path + "-shm"); err != nil {
		t.Fatalf("shared mode did not create the WAL index: %v", err)
	}
}

// ROLLTOP_DB_PATH can put the system database on a different filesystem than the
// tenant databases, so an automatic mode has to be answered per file rather than
// once for the installation.
func TestAutomaticAccessIsResolvedForEachDatabaseFile(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	systemPath := filepath.Join(t.TempDir(), "elsewhere", "rolltop.db")
	db, err := OpenServerWithOptions(systemPath, ServerOptions{DataDir: dataDir, Access: AccessAuto})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if db.accessConfigured != AccessAuto {
		t.Fatalf("configured access = %v, want it retained for later files", db.accessConfigured)
	}
	wantSystem, _ := ResolveAccessMode(AccessAuto, filepath.Dir(systemPath))
	if db.AccessMode() != wantSystem {
		t.Fatalf("system access = %v, want %v from its own directory", db.AccessMode(), wantSystem)
	}

	user, err := db.CreateUser(ctx, "resolved@example.test", "Resolved", "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	tenant, err := db.UserStore(ctx, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	wantTenant, _ := ResolveAccessMode(AccessAuto, filepath.Dir(UserDatabaseFilePath(dataDir, user.ID)))
	if tenant.AccessMode() != wantTenant {
		t.Fatalf("tenant access = %v, want %v from the data directory", tenant.AccessMode(), wantTenant)
	}
}

// An explicit mode still reaches every database, including tenants opened later.
func TestExplicitAccessReachesTenantDatabases(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	db, err := OpenServerWithOptions(filepath.Join(dataDir, "rolltop.db"), ServerOptions{
		DataDir: dataDir, Access: AccessExclusive,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	user, err := db.CreateUser(ctx, "explicit@example.test", "Explicit", "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	tenant, err := db.UserStore(ctx, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if db.AccessMode() != AccessExclusive || tenant.AccessMode() != AccessExclusive {
		t.Fatalf("access modes = system %v, tenant %v, want exclusive for both", db.AccessMode(), tenant.AccessMode())
	}
	if _, err := os.Stat(UserDatabaseFilePath(dataDir, user.ID) + "-shm"); !os.IsNotExist(err) {
		t.Fatalf("tenant database created a shared index: %v", err)
	}
}
