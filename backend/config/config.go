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
)

// Config is the validated runtime configuration assembled from environment variables.
type Config struct {
	Addr         string
	DataDir      string
	DatabasePath string
	IndexPath    string
	PluginDir    string

	MasterKey []byte

	SessionTTL        time.Duration
	CookieSecure      bool
	SyncInterval      time.Duration
	InboxPollInterval time.Duration
	BlobRetention     time.Duration
	WebhookToken      string
	LogLevel          string
	Google            GoogleConfig

	// StartupIntegrityCheck selects when SQLite files are verified during
	// startup: after an unclean shutdown, on every start, or never.
	StartupIntegrityCheck string
}

// Values accepted by ROLLTOP_STARTUP_INTEGRITY_CHECK.
const (
	IntegrityCheckAuto   = "auto"
	IntegrityCheckAlways = "always"
	IntegrityCheckNever  = "never"
)

const defaultDataDir = "/data"

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
	dbPath := env("ROLLTOP_DB_PATH", filepath.Join(dataDir, "rolltop.db"))
	indexPath := env("ROLLTOP_INDEX_PATH", filepath.Join(dataDir, "bleve"))
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
	// quick_check reads every page, so the default only pays that cost when the
	// previous run did not shut down cleanly and the files may be damaged.
	integrityCheck := strings.ToLower(env("ROLLTOP_STARTUP_INTEGRITY_CHECK", IntegrityCheckAuto))
	switch integrityCheck {
	case IntegrityCheckAuto, IntegrityCheckAlways, IntegrityCheckNever:
	default:
		return Config{}, fmt.Errorf("ROLLTOP_STARTUP_INTEGRITY_CHECK must be %q, %q, or %q, got %q",
			IntegrityCheckAuto, IntegrityCheckAlways, IntegrityCheckNever, integrityCheck)
	}

	return Config{
		Addr:              env("ROLLTOP_ADDR", ":8080"),
		DataDir:           dataDir,
		DatabasePath:      dbPath,
		IndexPath:         indexPath,
		PluginDir:         pluginDir,
		MasterKey:         key,
		SessionTTL:        sessionTTL,
		CookieSecure:      cookieSecure,
		SyncInterval:      syncInterval,
		InboxPollInterval: inboxPollInterval,
		BlobRetention:     blobRetention,
		WebhookToken:      os.Getenv("ROLLTOP_WEBHOOK_TOKEN"),
		LogLevel:          logLevel,
		Google:            google,

		StartupIntegrityCheck: integrityCheck,
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
