// File overview: Opening a store against a throwaway PostgreSQL database, for
// tests outside the store package.
//
// It replaces `store.Open(tempfile)`, which was a whole SQLite database in one
// file and cost nothing to make. A PostgreSQL equivalent cannot be that cheap,
// so the cost is paid once: the first call builds a template database carrying
// the baseline, and every later call clones it.
//
// The signature keeps the shape the SQLite helper had — `(db, err)` — so the
// several hundred call sites that check the error afterwards did not have to be
// rewritten around a different one.

package storetest

import (
	"context"
	"sync"
	"testing"

	"rolltop/backend/plugins"
	"rolltop/backend/store"
	"rolltop/backend/store/pgtestdb"
)

// testStoreDSNs remembers which database a test was given, so a test that
// closes its store and opens another one — the way the persistence tests check
// that state survived a restart — reaches the same database rather than a fresh
// clone of the template. Under SQLite that property came free from reopening
// the same file.
var testStoreDSNs sync.Map

// Open returns a store over an empty database carrying the current schema. The
// database is dropped when the test ends, and the store is closed with it.
//
// The test is skipped when TEST_DATABASE_URL is unset and failed when it is set
// but unusable, which is pgtestdb's contract rather than this function's.
func Open(t *testing.T) (*store.Store, error) {
	t.Helper()
	return OpenWithManifests(t, nil)
}

// OpenWithManifests is Open for tests that need the file-backed plugin
// migrations a manifest describes, rather than only the statically compiled
// catalog. Those migrations are applied to the test's own database on open, so
// they do not have to be part of the shared template.
func OpenWithManifests(t *testing.T, manifests []plugins.Manifest) (*store.Store, error) {
	t.Helper()
	db, err := store.OpenPostgres(context.Background(), DSN(t), store.PostgresOptions{
		// Small on purpose: a test that deadlocks on a held transaction should
		// do so quickly rather than after exhausting a production-sized pool,
		// and a package running tests in parallel would otherwise open far more
		// connections than the server allows.
		MaxConns:  4,
		DataDir:   t.TempDir(),
		Manifests: manifests,
	})
	if err != nil {
		return nil, err
	}
	t.Cleanup(func() { _ = db.Close() })
	return db, nil
}

// DSN returns the connection string for this test's database, creating it on
// first use. Tests that start a subprocess or drive a command which opens its
// own store — reset-search reads ROLLTOP_DATABASE_URL — need the string rather
// than the handle.
func DSN(t *testing.T) string {
	t.Helper()
	dsn, reused := testStoreDSNs.Load(t.Name())
	if !reused {
		dsn = pgtestdb.NewFromTemplate(t, store.SchemaTag(), buildTemplate)
		testStoreDSNs.Store(t.Name(), dsn)
		t.Cleanup(func() { testStoreDSNs.Delete(t.Name()) })
	}
	return dsn.(string)
}

// buildTemplate puts the schema into the template database, through the same
// code path production uses rather than a second definition of "the schema".
func buildTemplate(dsn string) error {
	return store.PrepareTestTemplate(context.Background(), dsn)
}
