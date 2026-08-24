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
	assertPluginMigrationCount(t, ctx, db.DB(), plugins.RemoteImageBlocklist, 1)
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
