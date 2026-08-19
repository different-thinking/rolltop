// File overview: Environment-driven application configuration.

package config

import (
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"rolltop/backend/googleauth"
	"rolltop/backend/logging"
	"rolltop/backend/memlimit"
	"rolltop/backend/pgdsn"
)

// Config is the validated runtime configuration assembled from environment variables.
type Config struct {
	Addr string
	// DataDir holds the blob store and the Bleve indexes. The relational data
	// lives in PostgreSQL and no longer touches this directory.
	DataDir string
	// DatabaseURL is the PostgreSQL DSN. It is required: there is no local
	// fallback to fall back to, and a default would only turn a missing
	// configuration into a connection error against somebody's localhost.
	DatabaseURL string
	// DatabaseMaxConns caps the connection pool, or is 0 when ROLLTOP_DB_MAX_CONNS
	// was not set and the store's own default applies. The number is not
	// restated here: the store owns pool sizing, and a copy of the default in
	// this package would silently win over a retuned one there. Ask the opened
	// store what it resolved to when the effective number is what you want.
	DatabaseMaxConns int
	// DatabaseConnectTimeout is how long startup waits for a database that is
	// not up yet, since the app container and the database start independently.
	DatabaseConnectTimeout time.Duration
	IndexPath              string
	PluginDir              string
	// SearchBackend selects where full-text search lives: "bleve" keeps the
	// per-tenant mmap'd indexes under IndexPath, "postgres" serves search from
	// the message_search table in the relational database
	// (docs/search-postgres-plan.md). Default "bleve" until the cutover.
	SearchBackend string

	MasterKey []byte

	SessionTTL        time.Duration
	CookieSecure      bool
	SyncInterval      time.Duration
	InboxPollInterval time.Duration
	BlobRetention     time.Duration
	WebhookToken      string
	LogLevel          string
	Google            GoogleConfig

	// MemoryLimit is the soft ceiling the Go runtime is given at startup so a
	// large sync collects instead of growing into the container's limit.
	MemoryLimit memlimit.Request

	// StartupLockWait is how long a starting server waits for a previous
	// process to release the data directory before giving up. Rolling
	// deployments overlap the two containers for exactly that long.
	StartupLockWait time.Duration
}

const defaultDataDir = "/data"

// The two ROLLTOP_SEARCH_BACKEND values. Bleve stays the default until the
// Postgres read path has been compared against it on real mail
// (docs/search-postgres-plan.md §7, phase D).
const (
	SearchBackendBleve    = "bleve"
	SearchBackendPostgres = "postgres"
)

// DataDirFromEnv resolves the data directory on its own, without loading or
// validating the rest of the configuration. Startup paths that must run before
// Load - crash reporting arms itself before anything can fail - use it to reach
// the same directory Load will report.
func DataDirFromEnv() string {
	return env("ROLLTOP_DATA_DIR", defaultDataDir)
}

// Load reads environment configuration, applies defaults, and validates values needed before services start.
func Load() (Config, error) {
	dataDir := DataDirFromEnv()
	databaseURL := strings.TrimSpace(os.Getenv("ROLLTOP_DATABASE_URL"))
	if databaseURL == "" {
		return Config{}, errors.New("ROLLTOP_DATABASE_URL is required (a PostgreSQL connection string, e.g. postgres://user:password@host:5432/rolltop?sslmode=require)")
	}
	// Parsed here so a malformed DSN fails at startup with the variable named,
	// rather than inside the driver with the password quoted back.
	if err := pgdsn.Validate(databaseURL); err != nil {
		return Config{}, fmt.Errorf("ROLLTOP_DATABASE_URL: %w", err)
	}
	// 0 means unset, which the store reads as "use your default".
	databaseMaxConns, err := parseInt("ROLLTOP_DB_MAX_CONNS", 0)
	if err != nil {
		return Config{}, err
	}
	if databaseMaxConns < 0 {
		return Config{}, fmt.Errorf("ROLLTOP_DB_MAX_CONNS must be at least 1, got %d", databaseMaxConns)
	}
	if os.Getenv("ROLLTOP_DB_MAX_CONNS") != "" && databaseMaxConns == 0 {
		return Config{}, errors.New("ROLLTOP_DB_MAX_CONNS must be at least 1, got 0")
	}
	databaseConnectTimeout, err := parseDuration("ROLLTOP_DB_CONNECT_TIMEOUT", 2*time.Minute)
	if err != nil {
		return Config{}, err
	}
	if databaseConnectTimeout < 0 {
		return Config{}, fmt.Errorf("ROLLTOP_DB_CONNECT_TIMEOUT must not be negative, got %s", databaseConnectTimeout)
	}
	indexPath := env("ROLLTOP_INDEX_PATH", filepath.Join(dataDir, "bleve"))
	searchBackend := strings.ToLower(strings.TrimSpace(env("ROLLTOP_SEARCH_BACKEND", SearchBackendBleve)))
	switch searchBackend {
	case SearchBackendBleve, SearchBackendPostgres:
	default:
		return Config{}, fmt.Errorf("ROLLTOP_SEARCH_BACKEND must be %q or %q, got %q", SearchBackendBleve, SearchBackendPostgres, searchBackend)
	}
	pluginDir := env("ROLLTOP_PLUGIN_DIR", "plugins")
	if abs, err := filepath.Abs(pluginDir); err == nil {
		pluginDir = abs
	}

	key, err := ParseMasterKey(os.Getenv("ROLLTOP_MASTER_KEY"))
	if err != nil {
		return Config{}, err
	}

	sessionTTL, err := parseDuration("ROLLTOP_SESSION_TTL", 30*24*time.Hour)
	if err != nil {
		return Config{}, err
	}
	syncInterval, err := parseDuration("ROLLTOP_SYNC_INTERVAL", 15*time.Minute)
	if err != nil {
		return Config{}, err
	}
	inboxPollInterval, err := parseDuration("ROLLTOP_INBOX_POLL_INTERVAL", time.Minute)
	if err != nil {
		return Config{}, err
	}
	blobRetention, err := parseDuration("ROLLTOP_BLOB_RETENTION", 14*24*time.Hour)
	if err != nil {
		return Config{}, err
	}
	cookieSecure, err := parseBool("ROLLTOP_COOKIE_SECURE", false)
	if err != nil {
		return Config{}, err
	}
	// Two minutes covers a previous process draining HTTP, closing its plugins
	// and index, with room for a slow volume.
	startupLockWait, err := parseDuration("ROLLTOP_STARTUP_LOCK_WAIT", 2*time.Minute)
	if err != nil {
		return Config{}, err
	}
	if startupLockWait < 0 {
		return Config{}, fmt.Errorf("ROLLTOP_STARTUP_LOCK_WAIT must not be negative, got %s", startupLockWait)
	}
	// The logging package owns what a level means, so an unknown value is
	// rejected here by the same parser the logger applies.
	logLevel, err := logging.ParseLevel(os.Getenv("ROLLTOP_LOG_LEVEL"))
	if err != nil {
		return Config{}, fmt.Errorf("ROLLTOP_LOG_LEVEL: %w", err)
	}
	google, err := loadGoogleConfig()
	if err != nil {
		return Config{}, err
	}
	// The memlimit package owns what a ceiling means, so a typo is rejected here
	// by the same parser the runtime setting applies.
	memoryLimit, err := memlimit.ParseRequest(os.Getenv("ROLLTOP_MEMORY_LIMIT"))
	if err != nil {
		return Config{}, fmt.Errorf("ROLLTOP_MEMORY_LIMIT: %w", err)
	}
	return Config{
		Addr:                   env("ROLLTOP_ADDR", ":8080"),
		DataDir:                dataDir,
		DatabaseURL:            databaseURL,
		DatabaseMaxConns:       databaseMaxConns,
		DatabaseConnectTimeout: databaseConnectTimeout,
		IndexPath:              indexPath,
		SearchBackend:          searchBackend,
		PluginDir:              pluginDir,
		MasterKey:              key,
		SessionTTL:             sessionTTL,
		CookieSecure:           cookieSecure,
		SyncInterval:           syncInterval,
		InboxPollInterval:      inboxPollInterval,
		BlobRetention:          blobRetention,
		WebhookToken:           os.Getenv("ROLLTOP_WEBHOOK_TOKEN"),
		LogLevel:               logLevel,
		Google:                 google,
		MemoryLimit:            memoryLimit,
		StartupLockWait:        startupLockWait,
	}, nil
}

// GoogleConfig carries the operator-supplied OAuth client. It lives here rather
// than being read straight from the environment where it is used, so a typo is
// a startup failure instead of a 503 the first time somebody clicks Connect.
type GoogleConfig struct {
	ClientID     string
	ClientSecret string
	RedirectURLs []string
	Scopes       []string
}

// Configured reports whether Google features can be offered at all.
func (g GoogleConfig) Configured() bool {
	return g.ClientID != "" && g.ClientSecret != ""
}

func loadGoogleConfig() (GoogleConfig, error) {
	google := GoogleConfig{
		ClientID:     strings.TrimSpace(os.Getenv("ROLLTOP_GOOGLE_CLIENT_ID")),
		ClientSecret: strings.TrimSpace(os.Getenv("ROLLTOP_GOOGLE_CLIENT_SECRET")),
		RedirectURLs: splitList(os.Getenv("ROLLTOP_GOOGLE_REDIRECT_URLS")),
		Scopes:       splitList(os.Getenv("ROLLTOP_GOOGLE_SCOPES")),
	}
	// Half a credential is always a mistake, and it is one that otherwise only
	// shows up as a failed consent long after startup.
	if (google.ClientID == "") != (google.ClientSecret == "") {
		return GoogleConfig{}, errors.New("ROLLTOP_GOOGLE_CLIENT_ID and ROLLTOP_GOOGLE_CLIENT_SECRET must be set together")
	}
	for _, raw := range google.RedirectURLs {
		parsed, err := url.Parse(raw)
		if err != nil {
			return GoogleConfig{}, fmt.Errorf("ROLLTOP_GOOGLE_REDIRECT_URLS: %q is not a URL: %w", raw, err)
		}
		if parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
			return GoogleConfig{}, fmt.Errorf("ROLLTOP_GOOGLE_REDIRECT_URLS: %q must be an absolute http or https URL", raw)
		}
		// A URI Google accepts but this server does not serve fails only at the
		// end of a consent round trip, which is a miserable way to learn about
		// a typo.
		if parsed.Path != googleauth.CallbackPath {
			return GoogleConfig{}, fmt.Errorf("ROLLTOP_GOOGLE_REDIRECT_URLS: %q must end in %s", raw, googleauth.CallbackPath)
		}
	}
	if google.Configured() && len(google.RedirectURLs) == 0 {
		return GoogleConfig{}, errors.New("ROLLTOP_GOOGLE_REDIRECT_URLS is required when Google client credentials are set")
	}
	return google, nil
}

// splitList accepts comma, whitespace, or newline separated values so operators
// can format multi-value environment variables however reads best for them.
func splitList(raw string) []string {
	fields := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == '\n' || r == '\r' || r == '\t' || r == ' '
	})
	out := make([]string, 0, len(fields))
	seen := map[string]bool{}
	for _, field := range fields {
		if field == "" || seen[field] {
			continue
		}
		seen[field] = true
		out = append(out, field)
	}
	return out
}

// ParseMasterKey decodes the encryption key used for IMAP/SMTP secrets and enforces the required key length.
func ParseMasterKey(value string) ([]byte, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, errors.New("ROLLTOP_MASTER_KEY is required")
	}

	decoders := []func(string) ([]byte, error){
		base64.StdEncoding.DecodeString,
		base64.RawStdEncoding.DecodeString,
		base64.URLEncoding.DecodeString,
		base64.RawURLEncoding.DecodeString,
		hex.DecodeString,
	}
	for _, decode := range decoders {
		if b, err := decode(value); err == nil && len(b) == 32 {
			return b, nil
		}
	}
	if len([]byte(value)) == 32 {
		return []byte(value), nil
	}
	return nil, errors.New("ROLLTOP_MASTER_KEY must decode to exactly 32 bytes")
}

func env(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func parseDuration(key string, fallback time.Duration) (time.Duration, error) {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	return d, nil
}

func parseInt(key string, fallback int) (int, error) {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	return n, nil
}

func parseBool(key string, fallback bool) (bool, error) {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, fmt.Errorf("%s: %w", key, err)
	}
	return b, nil
}

// SearchRoot is the directory holding the per-user Bleve indexes, one
// numbered subdirectory per tenant. It is derived here rather than rebuilt at
// each call site: a layout that moves must move for the server, the offline
// maintenance commands, and the startup memory check together, or the ones
// left behind silently measure and repair an empty directory.
func (c Config) SearchRoot() string { return SearchRootFor(c.DataDir) }

// SearchRootFor is the same derivation for callers that only have the data
// directory, such as the offline maintenance commands.
func SearchRootFor(dataDir string) string { return filepath.Join(dataDir, "users") }
