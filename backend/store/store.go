// File overview: Store construction. One PostgreSQL database holds every
// tenant's rows, scoped by user_id, so there is no per-tenant routing left to
// do: dataDB and mustDataDB resolve to the one pool.
//
// They are still the call shape every user-owned query goes through. The
// indirection no longer picks a database, but it keeps the tenant argument in
// the signature of every method that touches user-owned data, which is what
// makes a query that forgot to scope by user_id visible in review. AGENTS.md
// makes that scoping the only tenant isolation layer there is now.

package store

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"sync/atomic"

	"rolltop/backend/plugins"
	_ "rolltop/plugins/catalog"
)

var ErrNotFound = sql.ErrNoRows

const (
	DefaultMessageBodyPreviewBytes = 4096
)

// Store is the database access layer. It wraps one connection pool against the
// PostgreSQL database named by ROLLTOP_DATABASE_URL.
type Store struct {
	db      *sql.DB
	dataDir string
	// registered owns the driver registration the pool was opened through, so
	// closing the store releases it.
	registered *registeredDSN
	// instance holds the single-server lock when the caller asked for one. Nil
	// in tests, which open several stores against one database on purpose.
	instance *instanceLock
	// maxConns is the pool cap this store resolved, whether from the caller or
	// from its own default. Kept so callers report the number in force rather
	// than restating the default and drifting from it.
	maxConns          int
	pluginDefinitions []plugins.Definition
	pluginMigrations  []plugins.Migration
	// pluginMigrationTableReady records that the bookkeeping table was created
	// or found, so the fallback CREATE TABLE IF NOT EXISTS runs once per store
	// rather than once per migration.
	pluginMigrationTableReady atomic.Bool
	// trigramSearch records that EnsureTrigramSearch found or installed
	// pg_trgm and its index, which is what lets search offer fuzzy matching.
	trigramSearch atomic.Bool
	// mailFootprint caches the per-table column layout the storage figure is
	// summed from. See storage_footprint.go.
	mailFootprint mailFootprintCache
}

// MaxConns is the pool cap in force, for the admin page and anything else that
// has to show what this process will actually open.
func (s *Store) MaxConns() int { return s.maxConns }

// DataDir returns the filesystem directory holding blobs and search indexes.
// The relational data no longer lives there; blobs and Bleve still do.
func (s *Store) DataDir() string { return s.dataDir }

// Close shuts down the connection pool, gives up the single-server lock, and
// releases the driver registration.
func (s *Store) Close() error {
	if s.db == nil {
		return nil
	}
	err := s.db.Close()
	// After the pool, so nothing is still running against the database when the
	// next process is let in.
	s.instance.release()
	s.registered.release()
	return err
}

// UserDataDir returns the filesystem directory that owns one user's blobs and
// search index. An empty dataDir means the store was opened without one, which
// is the case in tests that touch neither.
func (s *Store) UserDataDir(userID int64) string {
	if s.dataDir == "" {
		return ""
	}
	return filepath.Join(s.dataDir, "users", fmt.Sprintf("%d", userID))
}

// UserStore used to open a per-tenant database. Every tenant now lives in the
// one database, so it returns the receiver. It is kept because plugin code and
// several call sites are written around it, and removing it would be a rename
// across the tree rather than a change in behaviour.
func (s *Store) UserStore(ctx context.Context, userID int64) (*Store, error) {
	return s, nil
}

// UserDB exposes the database handle for plugin code that runs its own SQL.
// Plugin hooks receive *sql.DB, which is what keeps the compiled-plugin ABI
// unchanged across the PostgreSQL move.
func (s *Store) UserDB(ctx context.Context, userID int64) (*sql.DB, error) {
	return s.db, nil
}

// dataDB is the call shape every method that reads or writes user-owned data
// goes through. It resolves to the one pool; the userID argument survives
// because it is what keeps the tenant in view at the call site.
func (s *Store) dataDB(ctx context.Context, userID int64) (*sql.DB, error) {
	return s.db, nil
}

// mustDataDB is used by helpers that must satisfy database/sql callback shapes
// and cannot return an error where they resolve the handle.
func (s *Store) mustDataDB(ctx context.Context, userID int64) *sql.DB {
	return s.db
}

// DB returns the connection pool.
func (s *Store) DB() *sql.DB {
	return s.db
}

// ServiceableUsers lists the users background services may work on. Under
// SQLite this filtered out tenants whose database file was corrupt; one
// database has no such partial state, so it is ListUsers. The name stays
// because the callers read better for it: a sync loop asking for the users it
// may serve says more than one asking for all of them.
func (s *Store) ServiceableUsers(ctx context.Context) ([]User, error) {
	return s.ListUsers(ctx)
}

// Vacuum runs VACUUM against the database. PostgreSQL autovacuums, so this
// exists only for the admin page's explicit "reclaim space now" action.
func (s *Store) Vacuum(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `VACUUM`)
	return err
}
