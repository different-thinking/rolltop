// File overview: Environment-driven application configuration.

package config

import (
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
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
	logLevel := strings.ToLower(env("ROLLTOP_LOG_LEVEL", "info"))
	switch logLevel {
	case "info", "debug":
	default:
		return Config{}, fmt.Errorf("ROLLTOP_LOG_LEVEL must be \"info\" or \"debug\", got %q", logLevel)
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

		StartupIntegrityCheck: integrityCheck,
	}, nil
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
