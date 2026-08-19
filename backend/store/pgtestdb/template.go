// File overview: A template database so each test's schema costs a file copy
// rather than a schema build.
//
// WP8 of docs/postgres-migration-plan.md deferred this until the suite was big
// enough to justify it. It is: applying the baseline takes roughly 360 ms, and
// the converted suite creates several hundred databases, which is minutes of
// pure DDL per run. `CREATE DATABASE … TEMPLATE …` copies the finished files
// instead and costs a small fraction of that.
//
// The template is built once per test binary (sync.Once) and, across binaries
// running at the same time, once per server: the advisory lock makes the second
// package wait for the first rather than both creating the same database and
// one of them failing. It is deliberately *not* dropped afterwards — a template
// left behind is reused by the next run, and dropping it would race the other
// packages still cloning from it.

package pgtestdb

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"rolltop/backend/pgdsn"
	"rolltop/backend/sqlident"
)

// templateAdvisoryLock serializes template creation across test binaries. The
// value is arbitrary but must not collide with the store's own schema lock.
const templateAdvisoryLock int64 = 0x726F6C6C74657374 // "rolltest"

// templatePrefix names the databases other test databases are cloned from. The
// full name carries a tag derived from the schema, which is what makes a
// template from an older schema simply unused rather than something that has to
// be dropped — dropping it races the packages still cloning from it, and a test
// binary that lost that race fails with "template database does not exist" for
// every one of its tests.
const templatePrefix = "rolltop_test_tmpl_"

var (
	templateOnce sync.Once
	templateErr  error
)

// NewFromTemplate returns a DSN for an empty database that already carries the
// schema `build` writes, without running build for each test.
//
// build is called at most once per process, against the template database. It
// receives a DSN and must leave the schema in place; anything it writes becomes
// visible to every test, which is why it must write schema and nothing else.
// tag identifies the schema build. Callers derive it from the schema itself, so
// a change to the schema selects a new template rather than reusing a stale one.
func NewFromTemplate(t *testing.T, tag string, build func(dsn string) error) string {
	t.Helper()
	adminDSN := adminDSNOrSkip(t)
	name := templatePrefix + tag
	templateOnce.Do(func() { templateErr = ensureTemplate(adminDSN, name, build) })
	if templateErr != nil {
		t.Fatalf("build the test template database: %v", pgdsn.Redact(templateErr.Error()))
	}
	return newDatabase(t, adminDSN, name)
}

// ensureTemplate creates and populates the template unless another process
// already did.
//
// The existence check is repeated under the lock. Two test binaries starting
// together both find it missing, and without the re-check the loser would try
// to create a database that now exists and fail the whole package.
func ensureTemplate(adminDSN, name string, build func(dsn string) error) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	admin, err := connect(ctx, adminDSN)
	if err != nil {
		return err
	}
	defer func() { _ = admin.Close(context.Background()) }()

	if _, err := admin.Exec(ctx, `SELECT pg_advisory_lock($1)`, templateAdvisoryLock); err != nil {
		return fmt.Errorf("take the template lock: %w", err)
	}
	defer func() {
		unlockCtx, unlockCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer unlockCancel()
		_, _ = admin.Exec(unlockCtx, `SELECT pg_advisory_unlock($1)`, templateAdvisoryLock)
	}()

	var exists bool
	if err := admin.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)`, name).Scan(&exists); err != nil {
		return err
	}
	if exists {
		// Reused as-is. The name already says which schema it holds, so an
		// existing one is by definition current, and leaving it alone is what
		// keeps concurrently running test binaries from pulling it out from
		// under each other.
		return nil
	}
	if _, err := admin.Exec(ctx, `CREATE DATABASE `+sqlident.Quote(name)); err != nil {
		return fmt.Errorf("create the template: %w", err)
	}
	dsn, err := withDatabase(adminDSN, name)
	if err != nil {
		return err
	}
	if err := build(dsn); err != nil {
		return fmt.Errorf("populate the template: %w", err)
	}
	// Marking it a template is what lets a non-superuser clone it, and it also
	// makes PostgreSQL refuse connections that would write to it.
	//
	// Failing here is not fatal: CREATE DATABASE … TEMPLATE works on an ordinary
	// database too, as long as nothing is connected to it.
	_, _ = admin.Exec(ctx, `UPDATE pg_database SET datistemplate = true WHERE datname = $1`, name)
	return nil
}
