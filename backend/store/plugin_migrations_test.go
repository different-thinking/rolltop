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
	args := append([]string{"build", "-a"}, plugins.GoBuildFlags()...)
	args = append(args, "-buildmode=plugin", "-o", filepath.Join(backendDir, "remote_image_blocklist.so"), "./plugins/remote_image_blocklist/backend")
	cmd := exec.Command("go", args...)
	cmd.Dir = repoRoot
	cmd.Env = append(os.Environ(), "GOCACHE=/tmp/rolltop-go-build")
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
