package store

import (
	"context"
	"database/sql"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"rolltop/backend/plugins"
	"rolltop/plugins/remote_image_blocklist/rules"
)

func TestBundledPluginMigrationsRespectDatabaseScope(t *testing.T) {
	ctx := context.Background()
	_, file, _, _ := runtime.Caller(0)
	repoRoot := filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
	pluginRoot := filepath.Join(t.TempDir(), "plugins")
	remoteRoot := filepath.Join(pluginRoot, plugins.RemoteImageBlocklist)
	backendDir := filepath.Join(remoteRoot, "backend")
	if err := os.MkdirAll(backendDir, 0o700); err != nil {
		t.Fatal(err)
	}
	manifest := `{
		"id": "remote_image_blocklist",
		"name": "Remote image blocklist",
		"description": "Test remote image blocklist",
		"backend": {"kind": "go-plugin", "binary": "backend/remote_image_blocklist.so"}
	}`
	if err := os.WriteFile(filepath.Join(remoteRoot, "manifest.json"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	// GoBuildFlags carries -race when this test binary is instrumented; without
	// it plugin.Open rejects the plugin over a mismatched build fingerprint.
	args := append([]string{"build"}, plugins.GoBuildFlags()...)
	args = append(args, "-buildmode=plugin", "-o", filepath.Join(backendDir, "remote_image_blocklist.so"), "./plugins/remote_image_blocklist/backend")
	cmd := exec.Command("go", args...)
	cmd.Dir = repoRoot
	// No GOCACHE override. This build shares the caller's build cache on
	// purpose: a private cache directory is cold on every CI run, which
	// turned a ~2s plugin link into a ~28s rebuild of the whole dependency
	// tree. The build fingerprint plugin.Open checks comes from the flags
	// above and the package sources, not from where the cache lives.
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	manifests, err := plugins.LoadManifests(pluginRoot)
	if err != nil {
		t.Fatal(err)
	}
	manager := plugins.NewBackendManager(pluginRoot, manifests)
	if _, ok, err := manager.Plugin(plugins.RemoteImageBlocklist); err != nil {
		t.Fatal(err)
	} else if !ok {
		t.Fatal("remote image blocklist backend plugin was not discovered")
	}
	db, err := openTestStore(t)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	user, err := db.CreateUser(ctx, "plugins@example.test", "Plugins", "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	userDB, err := db.UserDB(ctx, user.ID)
	if err != nil {
		t.Fatal(err)
	}

	// One database holds both scopes now, so a plugin's system and user tables
	// land in the same place. What is still worth asserting is that every
	// plugin's migrations ran and are recorded exactly once — the scope split
	// used to be the thing that could silently drop half of them.
	assertTableExists(t, ctx, db.DB(), "plugin_remote_image_blocklist_rules", true)
	assertTableExists(t, ctx, db.DB(), "identity_pgp_private_keys", true)
	assertTableExists(t, ctx, userDB, "identity_pgp_private_keys", true)
	assertPluginMigrationCount(t, ctx, db.DB(), plugins.RemoteImageBlocklist, 2)
	assertPluginMigrationCount(t, ctx, db.DB(), plugins.ClientSidePGP, 5)
}

func assertTableExists(t *testing.T, ctx context.Context, db *sql.DB, table string, want bool) {
	t.Helper()
	var count int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM information_schema.tables
		WHERE table_schema = current_schema() AND table_name = ?`, table).Scan(&count); err != nil {
		t.Fatal(err)
	}
	got := count != 0
	if got != want {
		t.Fatalf("table %s exists = %v, want %v", table, got, want)
	}
}

func assertPluginMigrationCount(t *testing.T, ctx context.Context, db *sql.DB, pluginID string, want int) {
	t.Helper()
	var count int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM plugin_migrations WHERE plugin_id = ?`, pluginID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != want {
		t.Fatalf("plugin_migrations count for %s = %d, want %d", pluginID, count, want)
	}
}

// TestPluginMigrationsFastPathSkipsTheSchemaLock pins the check that keeps an
// ordinary start off the server-wide schema lock. The pass it guards changes
// nothing on every start after the first, and taking a global lock for it
// serialises the incoming process of a rolling deploy behind the outgoing one.
func TestPluginMigrationsFastPathSkipsTheSchemaLock(t *testing.T) {
	ctx := context.Background()
	db := mustOpenTestStore(t)

	upToDate, err := db.pluginMigrationsUpToDate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !upToDate {
		t.Fatal("a freshly opened store still reports plugin migrations outstanding")
	}

	// A recorded checksum that disagrees with the compiled migration is a
	// refusal, and it has to come out of the unlocked check rather than being
	// deferred to the locked pass.
	var migrations []plugins.Migration
	for _, scope := range db.pluginMigrationScopes() {
		migrations = append(migrations, db.pluginMigrationsForScope(scope)...)
	}
	if len(migrations) == 0 {
		t.Skip("no plugin migrations are compiled in")
	}
	if _, err := db.db.ExecContext(ctx, `UPDATE plugin_migrations SET checksum = ? WHERE plugin_id = ? AND migration_id = ?`,
		"not-the-checksum", migrations[0].PluginID, migrations[0].ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.pluginMigrationsUpToDate(ctx); err == nil {
		t.Fatal("a checksum mismatch was not reported")
	}

	// A migration that has no row at all is work, not a refusal.
	if _, err := db.db.ExecContext(ctx, `DELETE FROM plugin_migrations WHERE plugin_id = ? AND migration_id = ?`,
		migrations[0].PluginID, migrations[0].ID); err != nil {
		t.Fatal(err)
	}
	upToDate, err = db.pluginMigrationsUpToDate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if upToDate {
		t.Fatal("a missing migration row was reported as up to date")
	}
}

// TestPluginMigrationTableIsEnsuredOnce pins that the fallback DDL does not run
// per migration. CREATE TABLE IF NOT EXISTS takes catalog locks even when it
// changes nothing.
func TestPluginMigrationTableIsEnsuredOnce(t *testing.T) {
	ctx := context.Background()
	db := mustOpenTestStore(t)
	if !db.pluginMigrationTableReady.Load() {
		// Nothing has needed the table yet in this store; the first call marks it.
		if err := db.ensurePluginMigrationTable(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if !db.pluginMigrationTableReady.Load() {
		t.Fatal("the table was not recorded as ready")
	}
	// Dropping it and asking again must be a no-op: the store already knows.
	if _, err := db.db.ExecContext(ctx, `DROP TABLE plugin_migrations`); err != nil {
		t.Fatal(err)
	}
	if err := db.ensurePluginMigrationTable(ctx); err != nil {
		t.Fatal(err)
	}
	var exists bool
	if err := db.db.QueryRowContext(ctx, `SELECT to_regclass('plugin_migrations') IS NOT NULL`).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Fatal("ensurePluginMigrationTable re-created the table, so it runs its DDL every call")
	}
}

// TestPluginMigrationChecksumIgnoresFormatting pins that a migration's identity
// is its SQL, not its indentation. It is the regression test for a start-up
// refusal: adding a second migration to a slice literal re-indented the first
// one's CREATE TABLE by a tab, and the byte-exact checksum read that as a
// migration edited after it had run.
func TestPluginMigrationChecksumIgnoresFormatting(t *testing.T) {
	shallow := plugins.Migration{
		Scope:    plugins.ScopeSystem,
		PluginID: "example",
		ID:       "001_create",
		Statements: []string{
			`CREATE TABLE IF NOT EXISTS example (
	id bigint GENERATED BY DEFAULT AS IDENTITY PRIMARY KEY,
	pattern TEXT NOT NULL UNIQUE
)`,
		},
	}
	indented := shallow
	indented.Statements = []string{
		`CREATE TABLE IF NOT EXISTS example (
			id bigint GENERATED BY DEFAULT AS IDENTITY PRIMARY KEY,
			pattern TEXT NOT NULL UNIQUE
		)`,
	}
	if pluginMigrationChecksum(shallow) != pluginMigrationChecksum(indented) {
		t.Fatal("re-indenting a migration changed its checksum, which refuses startup on every install that applied it")
	}

	// The whitespace between words still separates them: a normalisation that
	// dropped it would collide statements that differ in what they say.
	glued := shallow
	glued.Statements = []string{`CREATE TABLE IF NOT EXISTS example (id bigint GENERATED BY DEFAULT AS IDENTITYPRIMARY KEY, pattern TEXT NOT NULL UNIQUE)`}
	if pluginMigrationChecksum(shallow) == pluginMigrationChecksum(glued) {
		t.Fatal("two different statements share a checksum")
	}

	// A real content change is still a different migration.
	changed := shallow
	changed.Statements = []string{`CREATE TABLE IF NOT EXISTS example (id bigint PRIMARY KEY, pattern TEXT NOT NULL)`}
	if pluginMigrationChecksum(shallow) == pluginMigrationChecksum(changed) {
		t.Fatal("changing a migration's SQL did not change its checksum")
	}
	if pluginMigrationRecognised(pluginMigrationChecksum(changed), shallow) {
		t.Fatal("an edited migration's checksum was recognised as the one that ran")
	}
}

// TestSupersededPluginChecksumsAreAcceptedAndLeftAlone covers the upgrade path
// for a database whose rows were written by a build that hashed migration text
// byte-exactly: the row is recognised, the start does no work, and — the part
// that matters for a rollback — the recorded checksum is left exactly as the
// older build wrote it, so that build can still read it.
func TestSupersededPluginChecksumsAreAcceptedAndLeftAlone(t *testing.T) {
	ctx := context.Background()
	db := mustOpenTestStore(t)

	migration, ok := reformattableMigration(db)
	if !ok {
		t.Skip("no compiled migration distinguishes the two checksum algorithms")
	}
	legacy := pluginMigrationChecksumLegacy(migration)
	if _, err := db.db.ExecContext(ctx, `UPDATE plugin_migrations SET checksum = ? WHERE plugin_id = ? AND migration_id = ?`,
		legacy, migration.PluginID, migration.ID); err != nil {
		t.Fatal(err)
	}

	// The unlocked check answers "nothing to do": recognising a checksum is not
	// work, so an upgraded server neither takes the schema lock nor writes.
	upToDate, err := db.pluginMigrationsUpToDate(ctx)
	if err != nil {
		t.Fatalf("a recognisable older checksum was reported as a mismatch: %v", err)
	}
	if !upToDate {
		t.Fatal("a database an older build had migrated was reported as outstanding")
	}
	if err := db.applyPluginMigration(ctx, migration); err != nil {
		t.Fatal(err)
	}
	var stored string
	if err := db.db.QueryRowContext(ctx, `SELECT checksum FROM plugin_migrations WHERE plugin_id = ? AND migration_id = ?`,
		migration.PluginID, migration.ID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != legacy {
		t.Fatalf("the recorded checksum was rewritten to %s; the previous release can no longer read this database", stored)
	}
}

// TestRemoteImageBlocklistSupersededChecksum pins the one entry the superseded
// list was written for. The hash is what shipped builds recorded for
// 001_create_rules before its literal was re-indented; an install carrying it
// must upgrade, not refuse to start.
func TestRemoteImageBlocklistSupersededChecksum(t *testing.T) {
	const shipped = "95c1b828f35a50c7593520f3e57903c31ab04d2b63b89c9d56d7807115a7a91a"
	var migration plugins.Migration
	for _, candidate := range rules.Migrations() {
		if candidate.ID == "001_create_rules" {
			migration = candidate
			break
		}
	}
	if migration.ID == "" {
		t.Fatal("the remote image blocklist no longer declares 001_create_rules")
	}
	if !pluginMigrationRecognised(shipped, migration) {
		t.Fatal("the checksum shipped builds recorded for 001_create_rules is no longer recognised, so those installs refuse to start")
	}
	// The SQL itself is unchanged since that build, which is the whole reason
	// the row may be rewritten rather than refused.
	if got := "03c34a231d93ff986536f4a0ae0552aa4a089d2c8a7d8fce37f34054fc64ea76"; pluginMigrationChecksum(migration) != got {
		t.Fatalf("001_create_rules now hashes to %s: its SQL changed, so an install that ran the old one must not be told it matches",
			pluginMigrationChecksum(migration))
	}
}

// TestStartupAcceptsChecksumsWrittenByAnOlderBuild is the end-to-end form of the
// reported failure: a server that had applied its plugin migrations refused to
// start after an upgrade with "check plugin migrations: plugin migration
// checksum mismatch". Nothing about the database was wrong, so opening it has to
// succeed — and has to leave the database openable by the release being
// upgraded from.
func TestStartupAcceptsChecksumsWrittenByAnOlderBuild(t *testing.T) {
	ctx := context.Background()
	db := mustOpenTestStore(t)

	migration, ok := reformattableMigration(db)
	if !ok {
		t.Skip("no compiled migration distinguishes the two checksum algorithms")
	}
	// Put the database in the state an older build left it in.
	legacy := pluginMigrationChecksumLegacy(migration)
	if _, err := db.db.ExecContext(ctx, `UPDATE plugin_migrations SET checksum = ? WHERE plugin_id = ? AND migration_id = ?`,
		legacy, migration.PluginID, migration.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := openTestStore(t)
	if err != nil {
		t.Fatalf("startup refused a database an older build had migrated: %v", err)
	}
	var stored string
	if err := reopened.db.QueryRowContext(ctx, `SELECT checksum FROM plugin_migrations WHERE plugin_id = ? AND migration_id = ?`,
		migration.PluginID, migration.ID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != legacy {
		t.Fatalf("startup rewrote the checksum to %s, closing the door on a rollback to the previous release", stored)
	}
}

// reformattableMigration returns a compiled migration whose SQL spans lines, so
// the two checksum algorithms disagree about it and the upgrade path has
// something to repair. A single-line migration hashes the same either way and
// would let these tests pass without exercising anything.
func reformattableMigration(db *Store) (plugins.Migration, bool) {
	for _, scope := range db.pluginMigrationScopes() {
		for _, candidate := range db.pluginMigrationsForScope(scope) {
			if pluginMigrationChecksumLegacy(candidate) != pluginMigrationChecksum(candidate) {
				return candidate, true
			}
		}
	}
	return plugins.Migration{}, false
}
