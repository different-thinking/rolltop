package config

import (
	"path/filepath"
	"testing"

	"rolltop/backend/memlimit"
)

const testMasterKey = "12345678901234567890123456789012"

func TestLoadUsesRolltopDefaults(t *testing.T) {
	t.Setenv("ROLLTOP_MASTER_KEY", testMasterKey)

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DatabasePath != filepath.Join("/data", "rolltop.db") {
		t.Fatalf("database path = %q", cfg.DatabasePath)
	}
	if cfg.DataDir != "/data" {
		t.Fatalf("data dir = %q", cfg.DataDir)
	}
	wantPluginDir, err := filepath.Abs("plugins")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PluginDir != wantPluginDir {
		t.Fatalf("plugin dir = %q", cfg.PluginDir)
	}
}

func TestLoadUsesRolltopDatabasePath(t *testing.T) {
	t.Setenv("ROLLTOP_MASTER_KEY", testMasterKey)
	t.Setenv("ROLLTOP_DB_PATH", "/rolltop-data/custom.db")

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DatabasePath != "/rolltop-data/custom.db" {
		t.Fatalf("database path = %q", cfg.DatabasePath)
	}
}

func TestLoadUsesRolltopPluginDir(t *testing.T) {
	t.Setenv("ROLLTOP_MASTER_KEY", testMasterKey)
	t.Setenv("ROLLTOP_PLUGIN_DIR", "/rolltop-plugins")

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PluginDir != "/rolltop-plugins" {
		t.Fatalf("plugin dir = %q", cfg.PluginDir)
	}
}

func TestLoadValidatesLogLevel(t *testing.T) {
	t.Setenv("ROLLTOP_MASTER_KEY", testMasterKey)
	t.Setenv("ROLLTOP_LOG_LEVEL", "")

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.LogLevel != "info" {
		t.Fatalf("default log level = %q", cfg.LogLevel)
	}

	t.Setenv("ROLLTOP_LOG_LEVEL", "Debug")
	cfg, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.LogLevel != "debug" {
		t.Fatalf("log level = %q", cfg.LogLevel)
	}

	t.Setenv("ROLLTOP_LOG_LEVEL", "verbose")
	if _, err := Load(); err == nil {
		t.Fatal("expected error for unknown log level")
	}
}

// clearGoogleEnv keeps these tests independent of a developer or CI environment
// that already exports Google settings.
func clearGoogleEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		"ROLLTOP_GOOGLE_CLIENT_ID",
		"ROLLTOP_GOOGLE_CLIENT_SECRET",
		"ROLLTOP_GOOGLE_REDIRECT_URLS",
		"ROLLTOP_GOOGLE_SCOPES",
	} {
		t.Setenv(name, "")
	}
}

func TestLoadReadsGoogleSettings(t *testing.T) {
	clearGoogleEnv(t)
	t.Setenv("ROLLTOP_MASTER_KEY", testMasterKey)
	t.Setenv("ROLLTOP_GOOGLE_CLIENT_ID", " client-id ")
	t.Setenv("ROLLTOP_GOOGLE_CLIENT_SECRET", "client-secret")
	t.Setenv("ROLLTOP_GOOGLE_REDIRECT_URLS",
		"https://rolltop.example.test/api/google/callback, http://localhost:8080/api/google/callback\nhttps://rolltop.example.test/api/google/callback")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Google.Configured() || cfg.Google.ClientID != "client-id" {
		t.Fatalf("google config = %+v", cfg.Google)
	}
	if len(cfg.Google.RedirectURLs) != 2 {
		t.Fatalf("redirect URLs = %v, want two deduplicated entries", cfg.Google.RedirectURLs)
	}
}

func TestLoadRejectsHalfAGoogleCredential(t *testing.T) {
	// A typo in one of the two variables otherwise stays invisible until a user
	// clicks Connect and gets a 503.
	clearGoogleEnv(t)
	t.Setenv("ROLLTOP_MASTER_KEY", testMasterKey)
	t.Setenv("ROLLTOP_GOOGLE_CLIENT_ID", "client-id")
	t.Setenv("ROLLTOP_GOOGLE_CLIENT_SECRET", "")
	if _, err := Load(); err == nil {
		t.Fatal("client id without a secret was accepted")
	}
}

func TestLoadRequiresRedirectURLsWhenGoogleIsConfigured(t *testing.T) {
	clearGoogleEnv(t)
	t.Setenv("ROLLTOP_MASTER_KEY", testMasterKey)
	t.Setenv("ROLLTOP_GOOGLE_CLIENT_ID", "client-id")
	t.Setenv("ROLLTOP_GOOGLE_CLIENT_SECRET", "client-secret")
	t.Setenv("ROLLTOP_GOOGLE_REDIRECT_URLS", "")
	if _, err := Load(); err == nil {
		t.Fatal("google credentials without a redirect URI were accepted")
	}
}

func TestLoadRejectsUnusableRedirectURLs(t *testing.T) {
	clearGoogleEnv(t)
	t.Setenv("ROLLTOP_MASTER_KEY", testMasterKey)
	t.Setenv("ROLLTOP_GOOGLE_CLIENT_ID", "client-id")
	t.Setenv("ROLLTOP_GOOGLE_CLIENT_SECRET", "client-secret")
	for _, value := range []string{
		"/api/google/callback",
		"ftp://rolltop.example.test/cb",
		"://nonsense",
		// Right host, wrong path: Google would accept the registration and the
		// flow would then land on a route this server does not serve.
		"https://rolltop.example.test/callback",
	} {
		t.Setenv("ROLLTOP_GOOGLE_REDIRECT_URLS", value)
		if _, err := Load(); err == nil {
			t.Fatalf("redirect URL %q was accepted", value)
		}
	}
}

func TestLoadLeavesGoogleUnconfiguredByDefault(t *testing.T) {
	clearGoogleEnv(t)
	t.Setenv("ROLLTOP_MASTER_KEY", testMasterKey)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Google.Configured() {
		t.Fatalf("google reported as configured without credentials: %+v", cfg.Google)
	}
}

func TestLoadValidatesStartupIntegrityCheck(t *testing.T) {
	t.Setenv("ROLLTOP_MASTER_KEY", testMasterKey)
	t.Setenv("ROLLTOP_STARTUP_INTEGRITY_CHECK", "")

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.StartupIntegrityCheck != IntegrityCheckAuto {
		t.Fatalf("default startup integrity check = %q", cfg.StartupIntegrityCheck)
	}

	t.Setenv("ROLLTOP_STARTUP_INTEGRITY_CHECK", "Always")
	cfg, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.StartupIntegrityCheck != IntegrityCheckAlways {
		t.Fatalf("startup integrity check = %q", cfg.StartupIntegrityCheck)
	}

	t.Setenv("ROLLTOP_STARTUP_INTEGRITY_CHECK", "sometimes")
	if _, err := Load(); err == nil {
		t.Fatal("expected error for unknown startup integrity check mode")
	}
}

func TestLoadReadsMemoryLimit(t *testing.T) {
	t.Setenv("ROLLTOP_MASTER_KEY", testMasterKey)

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MemoryLimit != memlimit.DefaultRequest() {
		t.Fatalf("default memory limit = %+v, want %+v", cfg.MemoryLimit, memlimit.DefaultRequest())
	}

	t.Setenv("ROLLTOP_MEMORY_LIMIT", "768MiB")
	cfg, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	if want := (memlimit.Request{Bytes: 768 << 20}); cfg.MemoryLimit != want {
		t.Fatalf("configured memory limit = %+v, want %+v", cfg.MemoryLimit, want)
	}

	t.Setenv("ROLLTOP_MEMORY_LIMIT", "off")
	cfg, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.MemoryLimit.Disabled {
		t.Fatalf("disabled memory limit = %+v", cfg.MemoryLimit)
	}

	// A typo in a ceiling is a startup failure, not a silently unbounded heap.
	t.Setenv("ROLLTOP_MEMORY_LIMIT", "lots")
	if _, err := Load(); err == nil {
		t.Fatal("unparsable memory limit was accepted")
	}
}
