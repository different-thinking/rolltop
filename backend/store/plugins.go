// File overview: Plugin registry persistence. It stores plugin enablement,
// records plugin migration checksums, and applies compiled plugin migrations
// to the correct system or per-user database scope.

package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"

	"rolltop/backend/plugins"
)

// PluginSetting is the persisted admin enablement state for one plugin definition.
type PluginSetting struct {
	ID               string
	Name             string
	Description      string
	Enabled          bool
	EnabledByDefault bool
	Heavy            bool
	Experimental     bool
	CreatedAt        int64
	UpdatedAt        int64
}

func (s *Store) initPluginTables(ctx context.Context) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS plugin_settings (
			id TEXT PRIMARY KEY,
			name TEXT NOT NULL,
			description TEXT NOT NULL DEFAULT '',
			enabled INTEGER NOT NULL,
			enabled_by_default INTEGER NOT NULL,
			heavy INTEGER NOT NULL DEFAULT 0,
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS plugin_migrations (
			plugin_id TEXT NOT NULL,
			migration_id TEXT NOT NULL,
			applied_at INTEGER NOT NULL,
			app_version TEXT NOT NULL DEFAULT '',
			checksum TEXT NOT NULL,
			PRIMARY KEY(plugin_id, migration_id)
		)`,
	}
	for _, stmt := range stmts {
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			return err
		}
	}
	return s.seedPluginSettings(ctx)
}

func (s *Store) seedPluginSettings(ctx context.Context) error {
	return s.SyncPluginDefinitions(ctx, s.pluginDefinitions)
}

// SyncPluginDefinitions upserts admin-visible plugin metadata while preserving
// an existing enabled/disabled choice for previously known plugins.
func (s *Store) SyncPluginDefinitions(ctx context.Context, definitions []plugins.Definition) error {
	ts := nowUnix()
	for _, def := range definitions {
		def.ID = strings.TrimSpace(def.ID)
		if def.ID == "" {
			continue
		}
		_, err := s.db.ExecContext(ctx, `INSERT INTO plugin_settings
				(id, name, description, enabled, enabled_by_default, heavy, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(id) DO UPDATE SET
				name = excluded.name,
				description = excluded.description,
				enabled_by_default = excluded.enabled_by_default,
				heavy = excluded.heavy,
				updated_at = excluded.updated_at`,
			def.ID, def.Name, def.Description, boolInt(def.EnabledByDefault), boolInt(def.EnabledByDefault), boolInt(def.Heavy), ts, ts)
		if err != nil {
			return err
		}
	}
	return nil
}

// ListPluginSettings returns admin-visible plugin enablement rows.
func (s *Store) ListPluginSettings(ctx context.Context) ([]PluginSetting, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, name, description, enabled, enabled_by_default, heavy, created_at, updated_at
		FROM plugin_settings ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PluginSetting
	for rows.Next() {
		var setting PluginSetting
		var enabled, enabledByDefault, heavy int
		if err := rows.Scan(&setting.ID, &setting.Name, &setting.Description, &enabled, &enabledByDefault, &heavy, &setting.CreatedAt, &setting.UpdatedAt); err != nil {
			return nil, err
		}
		setting.Enabled = enabled != 0
		setting.EnabledByDefault = enabledByDefault != 0
		setting.Heavy = heavy != 0
		if def, ok := s.pluginDefinitionByID(setting.ID); ok {
			setting.Experimental = def.Experimental
		}
		out = append(out, setting)
	}
	return out, rows.Err()
}

// PluginEnabled reports whether a plugin is currently active.
func (s *Store) PluginEnabled(ctx context.Context, id string) (bool, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return false, nil
	}
	var enabled int
	err := s.db.QueryRowContext(ctx, `SELECT enabled FROM plugin_settings WHERE id = ?`, id).Scan(&enabled)
	if errors.Is(err, sql.ErrNoRows) {
		def, ok := s.pluginDefinitionByID(id)
		if !ok {
			return false, nil
		}
		return def.EnabledByDefault, nil
	}
	return enabled != 0, err
}

// SetPluginEnabled updates plugin enablement and records the change time.
func (s *Store) SetPluginEnabled(ctx context.Context, id string, enabled bool) error {
	def, static, ok, err := s.pluginDefinition(ctx, strings.TrimSpace(id))
	if err != nil {
		return err
	}
	if !ok {
		return ErrNotFound
	}
	if enabled && static {
		if err := s.ApplyPluginMigrations(ctx, def.ID); err != nil {
			return err
		}
	}
	ts := nowUnix()
	_, err = s.db.ExecContext(ctx, `INSERT INTO plugin_settings
			(id, name, description, enabled, enabled_by_default, heavy, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			name = excluded.name,
			description = excluded.description,
			enabled = excluded.enabled,
			enabled_by_default = excluded.enabled_by_default,
			heavy = excluded.heavy,
			updated_at = excluded.updated_at`,
		def.ID, def.Name, def.Description, boolInt(enabled), boolInt(def.EnabledByDefault), boolInt(def.Heavy), ts, ts)
	return err
}

// ApplyEnabledPluginMigrations applies migrations for every enabled plugin at startup.
func (s *Store) ApplyEnabledPluginMigrations(ctx context.Context) error {
	settings, err := s.ListPluginSettings(ctx)
	if err != nil {
		return err
	}
	for _, setting := range settings {
		if !setting.Enabled {
			continue
		}
		if err := s.ApplyPluginMigrations(ctx, setting.ID); err != nil {
			return err
		}
	}
	return nil
}

// ApplyPluginMigrations applies migrations for one plugin after it is enabled.
func (s *Store) ApplyPluginMigrations(ctx context.Context, pluginID string) error {
	pluginID = strings.TrimSpace(pluginID)
	if _, ok := s.pluginDefinitionByID(pluginID); !ok {
		if _, _, ok, err := s.pluginDefinition(ctx, pluginID); err != nil {
			return err
		} else if !ok {
			return ErrNotFound
		}
		return nil
	}
	for _, scope := range s.pluginMigrationScopes() {
		for _, migration := range s.pluginMigrationsForScope(scope) {
			if migration.PluginID != pluginID {
				continue
			}
			if err := s.applyPluginMigration(ctx, migration); err != nil {
				return err
			}
		}
	}
	return nil
}

// pluginMigrationScopes returns both scopes. The split between a system and a
// per-tenant database is what made this a choice; one database holds both, so a
// plugin's system and user migrations apply to the same place.
func (s *Store) pluginMigrationScopes() []string {
	return []string{plugins.ScopeSystem, plugins.ScopeUser}
}

func (s *Store) pluginDefinition(ctx context.Context, id string) (plugins.Definition, bool, bool, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return plugins.Definition{}, false, false, nil
	}
	if def, ok := s.pluginDefinitionByID(id); ok {
		return def, true, true, nil
	}
	var def plugins.Definition
	var enabledByDefault, heavy int
	err := s.db.QueryRowContext(ctx, `SELECT id, name, description, enabled_by_default, heavy FROM plugin_settings WHERE id = ?`, id).
		Scan(&def.ID, &def.Name, &def.Description, &enabledByDefault, &heavy)
	if errors.Is(err, sql.ErrNoRows) {
		return plugins.Definition{}, false, false, nil
	}
	if err != nil {
		return plugins.Definition{}, false, false, err
	}
	def.EnabledByDefault = enabledByDefault != 0
	def.Heavy = heavy != 0
	return def, false, true, nil
}

func (s *Store) applyPluginMigration(ctx context.Context, migration plugins.Migration) error {
	if err := s.ensurePluginMigrationTable(ctx); err != nil {
		return err
	}
	checksum := pluginMigrationChecksum(migration)
	var existing string
	err := s.db.QueryRowContext(ctx, `SELECT checksum FROM plugin_migrations WHERE plugin_id = ? AND migration_id = ?`,
		migration.PluginID, migration.ID).Scan(&existing)
	if err == nil {
		if !pluginMigrationRecognised(existing, migration) {
			return pluginMigrationChecksumMismatch(migration, existing, checksum)
		}
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, column := range migration.EnsureColumns {
		if strings.TrimSpace(column.Table) == "" || strings.TrimSpace(column.Column) == "" || strings.TrimSpace(column.DDL) == "" {
			continue
		}
		exists, err := pluginColumnExists(ctx, tx, column.Table, column.Column)
		if err != nil {
			return fmt.Errorf("apply plugin migration %s/%s: %w", migration.PluginID, migration.ID, err)
		}
		if exists {
			continue
		}
		if _, err := tx.ExecContext(ctx, column.DDL); err != nil {
			return fmt.Errorf("apply plugin migration %s/%s: %w", migration.PluginID, migration.ID, err)
		}
	}
	for _, stmt := range migration.Statements {
		if strings.TrimSpace(stmt) == "" {
			continue
		}
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("apply plugin migration %s/%s: %w", migration.PluginID, migration.ID, err)
		}
	}
	if migration.Apply != nil {
		if err := migration.Apply(ctx, tx); err != nil {
			return fmt.Errorf("apply plugin migration %s/%s: %w", migration.PluginID, migration.ID, err)
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO plugin_migrations (plugin_id, migration_id, applied_at, checksum)
		VALUES (?, ?, ?, ?)`, migration.PluginID, migration.ID, nowUnix(), checksum); err != nil {
		return err
	}
	return tx.Commit()
}

// pluginMigrationChecksum fingerprints what a migration does to the schema, so
// that editing a migration someone has already applied is caught rather than
// silently ignored.
//
// Statements go through normalizeSQL first, the same reading the core schema
// checksums use, so how a migration is laid out in the source is not part of
// what identifies it. sqlnorm.go has the incident that made that necessary.
func pluginMigrationChecksum(m plugins.Migration) string {
	return pluginMigrationDigest(m, normalizeSQL)
}

// pluginMigrationChecksumLegacy reproduces the byte-exact checksum that earlier
// builds recorded, so a row written by one can be recognised as this same
// migration instead of read as a conflict. Its output is never stored.
func pluginMigrationChecksumLegacy(m plugins.Migration) string {
	return pluginMigrationDigest(m, strings.TrimSpace)
}

func pluginMigrationDigest(m plugins.Migration, normalize func(string) string) string {
	h := sha256.New()
	_, _ = h.Write([]byte(m.PluginID))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(m.ID))
	for _, column := range m.EnsureColumns {
		_, _ = h.Write([]byte{0})
		_, _ = h.Write([]byte(normalize(column.Table)))
		_, _ = h.Write([]byte{0})
		_, _ = h.Write([]byte(normalize(column.Column)))
		_, _ = h.Write([]byte{0})
		_, _ = h.Write([]byte(normalize(column.DDL)))
	}
	for _, stmt := range m.Statements {
		_, _ = h.Write([]byte{0})
		_, _ = h.Write([]byte(normalize(stmt)))
	}
	if m.Apply != nil {
		_, _ = h.Write([]byte{0})
		_, _ = h.Write([]byte("custom"))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// pluginMigrationRecognised reports whether a recorded checksum stands for this
// migration: the checksum a start writes today, the byte-exact one an older
// build wrote for the same text, or one of the historical texts listed in
// supersededPluginMigrationChecksums. Anything else is a real disagreement
// about what ran, and stays a refusal.
//
// A recognised older checksum is left in the row rather than rewritten; see
// checksumRecognised for why.
func pluginMigrationRecognised(stored string, m plugins.Migration) bool {
	return checksumRecognised(stored, pluginMigrationChecksum(m), pluginMigrationChecksumLegacy(m),
		supersededPluginMigrationChecksums[m.PluginID+"/"+m.ID]...)
}

// supersededPluginMigrationChecksums lists checksums that shipped builds wrote
// for a migration whose source text has since been reformatted without its SQL
// changing. Normalised hashing means no future reformat needs an entry here;
// these are the ones recorded before it existed, and each is the hash of a
// specific historical text, so nothing else can slip through.
//
// Add an entry only for a formatting-only change to an already-released
// migration. A migration whose SQL genuinely changed needs a new migration id,
// not a line here.
var supersededPluginMigrationChecksums = map[string][]string{
	plugins.RemoteImageBlocklist + "/001_create_rules": {
		// Adding 002_seed_default_patterns to the slice literal made gofmt
		// indent this migration's CREATE TABLE by one more tab. Identical SQL,
		// different bytes: every install that had applied 001 under the
		// byte-exact checksum refused to start on the next release.
		"95c1b828f35a50c7593520f3e57903c31ab04d2b63b89c9d56d7807115a7a91a",
	},
}

// pluginMigrationChecksumMismatch reports a migration that was edited after it
// ran. It names both checksums because the operator's next question is whether
// the database or the build is the odd one out.
func pluginMigrationChecksumMismatch(m plugins.Migration, stored, want string) error {
	return fmt.Errorf("plugin migration checksum mismatch for %s/%s: recorded %s, compiled %s "+
		"(the migration was edited after it ran; give the change a new migration id)",
		m.PluginID, m.ID, stored, want)
}

func (s *Store) applyPluginMigrationsForScope(ctx context.Context, scope string) error {
	for _, migration := range s.pluginMigrationsForScope(scope) {
		if err := s.applyPluginMigration(ctx, migration); err != nil {
			return err
		}
	}
	return nil
}

// undefinedTable is PostgreSQL's SQLSTATE for a missing relation.
const undefinedTable = "42P01"

// pluginMigrationsUpToDate reports whether every compiled plugin migration is
// already recorded, in one query and without taking the schema lock.
//
// This is the ordinary case on every start after the first, and it is worth a
// fast path of its own: the alternative serialises each starting process behind
// the server-wide schema lock for a pass that changes nothing, which during a
// rolling deploy means the incoming process waiting on the outgoing one.
//
// A checksum that disagrees is reported here rather than deferred to the locked
// pass: it is a refusal, not work to be done, and taking a global lock to
// produce it helps nobody. A checksum an older build wrote for the same
// migration is neither: it is recognised here and left alone, so an upgraded
// server takes no lock and writes nothing on a database that is already
// migrated.
func (s *Store) pluginMigrationsUpToDate(ctx context.Context) (bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT plugin_id, migration_id, checksum FROM plugin_migrations`)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == undefinedTable {
			// No bookkeeping table yet, so nothing can be recorded.
			return false, nil
		}
		return false, err
	}
	defer rows.Close()
	type migrationKey struct{ pluginID, migrationID string }
	applied := make(map[migrationKey]string)
	for rows.Next() {
		var key migrationKey
		var checksum string
		if err := rows.Scan(&key.pluginID, &key.migrationID, &checksum); err != nil {
			return false, err
		}
		applied[key] = checksum
	}
	if err := rows.Err(); err != nil {
		return false, err
	}
	for _, scope := range s.pluginMigrationScopes() {
		for _, migration := range s.pluginMigrationsForScope(scope) {
			existing, ok := applied[migrationKey{pluginID: migration.PluginID, migrationID: migration.ID}]
			if !ok {
				return false, nil
			}
			if !pluginMigrationRecognised(existing, migration) {
				return false, pluginMigrationChecksumMismatch(migration, existing, pluginMigrationChecksum(migration))
			}
		}
	}
	return true, nil
}

// pluginColumnExists answers whether a plugin's table already carries a column.
//
// It exists because SQLite had no ADD COLUMN IF NOT EXISTS, and it survives the
// move because the interface plugins are written against does — EnsureColumns
// still takes a column list and adds what is missing.
//
// to_regclass resolves the table through search_path, which is deliberately the
// same resolution the ALTER TABLE that follows will use. Looking the column up
// in current_schema() instead asks about a different table whenever the two
// disagree — which they do on a server that has a schema named after the
// connecting role, where current_schema() is that schema while an unqualified
// table name still falls through to public.
func pluginColumnExists(ctx context.Context, tx *sql.Tx, table, column string) (bool, error) {
	var exists bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS (
			SELECT 1 FROM pg_attribute
			WHERE attrelid = to_regclass(?) AND attname = ? AND attnum > 0 AND NOT attisdropped
		)`, table, column).Scan(&exists)
	if err != nil {
		return false, err
	}
	return exists, nil
}

func (s *Store) pluginMigrationsForScope(scope string) []plugins.Migration {
	scope = strings.TrimSpace(scope)
	out := make([]plugins.Migration, 0, len(s.pluginMigrations))
	for _, migration := range s.pluginMigrations {
		if scope == "" || migration.Scope == scope {
			out = append(out, migration)
		}
	}
	return out
}

func (s *Store) pluginDefinitionByID(id string) (plugins.Definition, bool) {
	id = strings.TrimSpace(id)
	if id == "" {
		return plugins.Definition{}, false
	}
	for _, def := range s.pluginDefinitions {
		if def.ID == id {
			return def, true
		}
	}
	return plugins.Definition{}, false
}

// ensurePluginMigrationTable creates the bookkeeping table if it is missing.
//
// The baseline already contains it, so this is a fallback for a store opened
// against a database built some other way. It runs at most once per store:
// CREATE TABLE IF NOT EXISTS takes catalog locks even when it does nothing, and
// this used to run once per migration on every open.
func (s *Store) ensurePluginMigrationTable(ctx context.Context) error {
	if s.pluginMigrationTableReady.Load() {
		return nil
	}
	if err := s.createPluginMigrationTable(ctx); err != nil {
		return err
	}
	s.pluginMigrationTableReady.Store(true)
	return nil
}

func (s *Store) createPluginMigrationTable(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS plugin_migrations (
		plugin_id TEXT NOT NULL,
		migration_id TEXT NOT NULL,
		applied_at INTEGER NOT NULL,
		app_version TEXT NOT NULL DEFAULT '',
		checksum TEXT NOT NULL,
		PRIMARY KEY(plugin_id, migration_id)
	)`)
	return err
}
