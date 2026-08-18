// File overview: Store construction and tenant database routing. OpenServer
// opens only the system database; UserStore opens data/users/<id>/rolltop.db
// on demand and mirrors the system user row into it. Store methods that touch
// user-owned mail/contact/blob/search hydration metadata should call dataDB or
// mustDataDB so they automatically run against the per-user SQLite handle in
// split mode while tests can still use one combined database via Open.

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"rolltop/backend/plugins"
	_ "rolltop/plugins/catalog"
)

var ErrNotFound = sql.ErrNoRows

const (
	DefaultMessageBodyPreviewBytes = 4096
	databaseFilename               = "rolltop.db"
)

// Store is the SQLite access layer; in production the root store opens the system DB and caches per-user stores.
type Store struct {
	db                *sql.DB
	path              string
	dataDir           string
	schema            schemaKind
	split             bool
	pluginDefinitions []plugins.Definition
	pluginMigrations  []plugins.Migration
	access            AccessMode
	mu                sync.Mutex
	userStores        map[int64]*Store
	healthMu          sync.Mutex
	health            map[int64]DatabaseHealth
}

// Open creates a combined store in one SQLite file. It is mostly used by tests
// and small helpers that do not need the production system/user split.
func Open(path string) (*Store, error) {
	return open(path, "", false, schemaCombined, nil, defaultPluginCatalog(), AccessShared)
}

// OpenServer opens the production system store without progress reporting.
// cmd/rolltop usually calls OpenServerWithOptions instead.
func OpenServer(path string, dataDir string) (*Store, error) {
	return OpenServerWithProgress(path, dataDir, nil)
}

// OpenServerWithProgress opens the installation-level database only. Per-user
// databases are opened lazily through UserStore so tenant-owned data remains in
// data/users/<id>/rolltop.db.
func OpenServerWithProgress(path string, dataDir string, progress MigrationReporter) (*Store, error) {
	access, _ := ResolveAccessMode(AccessAuto, dataDir)
	return open(path, dataDir, true, schemaSystem, progress, defaultPluginCatalog(), access)
}

// OpenServerWithPluginManifests opens the production system store with a plugin
// catalog derived from scanned plugin manifests.
func OpenServerWithPluginManifests(path string, dataDir string, manifests []plugins.Manifest, progress MigrationReporter) (*Store, error) {
	return OpenServerWithOptions(path, ServerOptions{DataDir: dataDir, Manifests: manifests, Progress: progress})
}

// ServerOptions carries what the production system store needs beyond its path.
// It exists so the access mode can reach every database - the system store and
// each tenant opened later - without a second constructor per combination.
type ServerOptions struct {
	DataDir   string
	Manifests []plugins.Manifest
	Progress  MigrationReporter
	// Access selects how SQLite coordinates access to its files. The zero value
	// is AccessAuto, which is resolved from the filesystem before opening.
	Access AccessMode
}

// OpenServerWithOptions is the production entrypoint.
func OpenServerWithOptions(path string, opts ServerOptions) (*Store, error) {
	access := opts.Access
	if access == AccessAuto {
		access, _ = ResolveAccessMode(access, opts.DataDir)
	}
	return open(path, opts.DataDir, true, schemaSystem, opts.Progress, pluginCatalogFromManifests(opts.Manifests), access)
}

// open is the shared constructor behind all Store entrypoints. It creates the
// SQLite parent directory, opens the connection with foreign keys and a busy
// timeout, installs the right migration set, and configures split-mode user-store
// caching only for the production system database.
func open(path string, dataDir string, split bool, schema schemaKind, progress MigrationReporter, catalog pluginCatalog, access AccessMode) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	if access == AccessAuto {
		access = AccessShared
	}
	db, err := sql.Open("sqlite3", access.dataSourceName(path))
	if err != nil {
		return nil, err
	}
	access.applyConnectionLimits(db)
	s := &Store{
		db:                db,
		path:              path,
		dataDir:           dataDir,
		schema:            schema,
		split:             split,
		access:            access,
		pluginDefinitions: append([]plugins.Definition(nil), catalog.definitions...),
		pluginMigrations:  append([]plugins.Migration(nil), catalog.migrations...),
	}
	if split {
		s.userStores = make(map[int64]*Store)
	}
	if err := s.migrate(context.Background(), schema, progress); err != nil {
		_ = db.Close()
		// A corrupt file reports SQLITE_CORRUPT from the first migration
		// statement onward. Name the file and the offline repair command here
		// so startup never fails with a bare "database disk image is malformed".
		if IsCorrupt(err) {
			return nil, newCorruptionError(0, path, err)
		}
		return nil, err
	}
	return s, nil
}

// DatabasePath returns the SQLite file backing the receiver. On the root
// production store this is the system database; per-user stores return their
// own tenant file.
func (s *Store) DatabasePath() string {
	return s.path
}

// Close shuts down the root store and any cached per-user stores opened through
// UserStore. The first close error is returned after all handles are attempted.
func (s *Store) Close() error {
	s.mu.Lock()
	stores := make([]*Store, 0, len(s.userStores))
	for _, us := range s.userStores {
		stores = append(stores, us)
	}
	s.mu.Unlock()
	var first error
	for _, us := range stores {
		if err := us.Close(); err != nil && first == nil {
			first = err
		}
	}
	if err := s.db.Close(); err != nil && first == nil {
		first = err
	}
	return first
}

// UserDataDir returns the filesystem directory that owns one user's SQLite DB,
// blobs, and search index. An empty dataDir means the store is combined.
func (s *Store) UserDataDir(userID int64) string {
	if s.dataDir == "" {
		return ""
	}
	return filepath.Join(s.dataDir, "users", fmt.Sprintf("%d", userID))
}

// UserStore returns the per-user database handle for user-owned data. In split
// mode this opens and migrates the user database lazily; in combined mode it
// returns the receiver.
func (s *Store) UserStore(ctx context.Context, userID int64) (*Store, error) {
	return s.userStore(ctx, userID, nil)
}

// PrepareUserStores is called during process startup so existing users have
// their schemas migrated before background sync or HTTP requests touch them.
func (s *Store) PrepareUserStores(ctx context.Context, progress MigrationReporter) error {
	if !s.split {
		return nil
	}
	users, err := s.ListUsers(ctx)
	if err != nil {
		return err
	}
	for i, user := range users {
		reportMigration(progress, MigrationProgress{Scope: "user", Migration: "open user database", Step: fmt.Sprintf("user %d", user.ID), Done: i, Total: len(users)})
		if _, err := s.userStore(ctx, user.ID, progress); err != nil {
			// A corrupt tenant database must not keep the installation down.
			// userStore has already latched it, so its own requests fail with
			// the repair instructions while every other tenant is served — and
			// the admin UI that schedules the repair stays reachable.
			if IsCorrupt(err) {
				reportMigration(progress, MigrationProgress{Scope: "user", Migration: "open user database", Step: fmt.Sprintf("user %d is damaged", user.ID), Done: i + 1, Total: len(users)})
				continue
			}
			return err
		}
		reportMigration(progress, MigrationProgress{Scope: "user", Migration: "open user database", Step: fmt.Sprintf("user %d", user.ID), Done: i + 1, Total: len(users)})
	}
	return nil
}

// userStore resolves the SQLite handle for one tenant. The root store owns the
// cache and the system users table; each child store owns only the user's DB.
// The double-check around open avoids creating duplicate handles when concurrent
// requests touch a user for the first time.
func (s *Store) userStore(ctx context.Context, userID int64, progress MigrationReporter) (*Store, error) {
	if !s.split || userID == 0 {
		return s, nil
	}
	// UserStore and UserDB reach this without going through dataDB, so the
	// latch has to be honored here too; otherwise every request re-opens and
	// re-migrates a database that is known to be unreadable.
	if health, corrupt := s.databaseHealth(userID); corrupt {
		return nil, newCorruptionError(userID, health.Path, errors.New(health.Detail))
	}
	s.mu.Lock()
	if us := s.userStores[userID]; us != nil {
		s.mu.Unlock()
		return us, nil
	}
	s.mu.Unlock()
	user, err := s.GetUserByID(ctx, userID)
	if err != nil {
		return nil, err
	}
	userDir := s.UserDataDir(userID)
	if err := os.MkdirAll(userDir, 0o700); err != nil {
		return nil, err
	}
	userDBPath := filepath.Join(userDir, databaseFilename)
	us, err := open(userDBPath, "", false, schemaUser, progress, pluginCatalog{
		definitions: s.pluginDefinitions,
		migrations:  s.pluginMigrations,
	}, s.access)
	if err != nil {
		return nil, s.NoteError(userID, err)
	}
	if err := us.mirrorUser(ctx, user); err != nil {
		_ = us.Close()
		// Corruption surfaces on the first write into the tenant file as
		// readily as during its migrations. Without the latch on this path the
		// tenant is still skipped at startup, but nothing records why: the
		// admin database page stays empty and every later request reopens and
		// re-migrates a file that cannot answer.
		return nil, s.NoteError(userID, err)
	}
	s.mu.Lock()
	if existing := s.userStores[userID]; existing != nil {
		s.mu.Unlock()
		_ = us.Close()
		return existing, nil
	}
	s.userStores[userID] = us
	s.mu.Unlock()
	return us, nil
}

// UserDB exposes the concrete per-user database for plugin code that needs to
// run its own SQL. Normal store methods should prefer dataDB so tests and split
// mode keep the same call shape.
func (s *Store) UserDB(ctx context.Context, userID int64) (*sql.DB, error) {
	us, err := s.UserStore(ctx, userID)
	if err != nil {
		return nil, err
	}
	return us.db, nil
}

// dataDB is the central tenant-routing helper. Any method that reads or writes
// user-owned mail/contact/blob metadata should reach SQLite through this path.
// A tenant already known to be corrupt fails here without touching the file, so
// every caller stops retrying a database that cannot answer until it is
// repaired offline.
func (s *Store) dataDB(ctx context.Context, userID int64) (*sql.DB, error) {
	if !s.split || userID == 0 {
		return s.db, nil
	}
	return s.UserDB(ctx, userID)
}

// mustDataDB is used only in helpers that must satisfy database/sql callback
// shapes and cannot return an error at the point they resolve the tenant DB.
// It never panics: an unresolvable tenant yields a handle whose statements all
// fail, which every caller already handles as a query error.
func (s *Store) mustDataDB(ctx context.Context, userID int64) *sql.DB {
	db, err := s.dataDB(ctx, userID)
	if err != nil {
		// Corruption already reported itself when it was latched. Anything else
		// would be lost entirely, because the caller only sees the generic
		// statement failure the unavailable handle produces.
		if !IsCorrupt(err) {
			log.Printf("resolve user %d database: %v", userID, err)
		}
		return unavailableDB()
	}
	return db
}

// mirrorUser copies installation-level identity/display fields into the user DB.
// This lets older query helpers join against users locally without putting mail
// rows back into the high-level system database.
func (s *Store) mirrorUser(ctx context.Context, user User) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO users
			(id, email, name, password_hash, is_admin, date_locale, date_format, theme, search_preset, search_recency_bias, search_fuzzy, search_sender_boost, search_sender_history, search_contact_boost, search_attachment_weight, search_compact_splitting, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			email = excluded.email,
			name = excluded.name,
			password_hash = excluded.password_hash,
			is_admin = excluded.is_admin,
			date_locale = excluded.date_locale,
			date_format = excluded.date_format,
			theme = excluded.theme,
			search_preset = excluded.search_preset,
			search_recency_bias = excluded.search_recency_bias,
			search_fuzzy = excluded.search_fuzzy,
			search_sender_boost = excluded.search_sender_boost,
			search_sender_history = excluded.search_sender_history,
			search_contact_boost = excluded.search_contact_boost,
			search_attachment_weight = excluded.search_attachment_weight,
			search_compact_splitting = excluded.search_compact_splitting,
			updated_at = excluded.updated_at`,
		user.ID, user.Email, user.Name, user.PasswordHash, boolInt(user.IsAdmin), user.DateLocale, user.DateFormat, user.Theme, user.SearchPreset, user.SearchRecencyBias, user.SearchFuzzy, boolInt(user.SearchSenderBoost), user.SearchSenderHistory, user.SearchContactBoost, user.SearchAttachmentWeight, boolInt(user.SearchCompactSplitting), user.CreatedAt.UTC().Unix(), user.UpdatedAt.UTC().Unix())
	return err
}

// DB returns the receiver's SQLite handle. On the root production store this is
// the system DB; callers that need mail data should pass through UserDB/dataDB.
func (s *Store) DB() *sql.DB {
	return s.db
}

// Vacuum compacts the receiver's database only. In split mode callers must run
// it on the system store and any user store they explicitly want to compact.
func (s *Store) Vacuum(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `VACUUM`)
	return err
}

func nowUnix() int64 {
	return time.Now().UTC().Unix()
}

func unixTime(v int64) time.Time {
	if v == 0 {
		return time.Time{}
	}
	return time.Unix(v, 0).UTC()
}

func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}
