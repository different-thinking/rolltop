// File overview: Throwaway PostgreSQL databases for tests. TEST_DATABASE_URL
// names a server the test run may create databases on; New hands out one empty
// database per test and drops it afterwards, so tests that write schema or rows
// cannot see each other's state.
//
// The server must be a test-local container with CREATEDB rights. Pointing this
// at the hosted database does not work and is not meant to: that role has
// CREATEDB=false, and New says so rather than failing with a bare permission
// error.

package pgtestdb

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"rolltop/backend/pgdsn"
)

// EnvVar names the server tests may create databases on.
const EnvVar = "TEST_DATABASE_URL"

// maxDatabaseNameLen is PostgreSQL's identifier limit. Names are truncated to
// fit rather than silently colliding after the server truncates them itself.
const maxDatabaseNameLen = 63

var counter atomic.Uint64

// New creates an empty database and returns a DSN pointing at it.
//
// It skips the test when TEST_DATABASE_URL is unset, and fails it when the
// variable is set but the server cannot be used. Skipping the second case is
// what makes a broken CI service report a green suite that verified nothing.
func New(t *testing.T) string {
	t.Helper()
	adminDSN := strings.TrimSpace(os.Getenv(EnvVar))
	if adminDSN == "" {
		t.Skipf("%s not set", EnvVar)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	admin, err := pgx.Connect(ctx, adminDSN)
	if err != nil {
		t.Fatalf("%s is set but unusable: %v", EnvVar, err)
	}
	defer func() { _ = admin.Close(context.Background()) }()

	name := databaseName(t.Name())
	// A previous run killed mid-test leaves its database behind; reusing the
	// name would then apply a schema on top of an existing one.
	if _, err := admin.Exec(ctx, `DROP DATABASE IF EXISTS `+quoteIdentifier(name)); err != nil {
		t.Fatalf("drop stale test database %s: %v", name, err)
	}
	if _, err := admin.Exec(ctx, `CREATE DATABASE `+quoteIdentifier(name)); err != nil {
		t.Fatalf("cannot create a test database on %s (the server must be a test-local instance whose role has CREATEDB): %v", EnvVar, err)
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		// A fresh connection, because the one above is closed by the time
		// cleanups run. Reusing it drops nothing and leaves a database per test
		// behind on the server.
		cleaner, err := pgx.Connect(cleanupCtx, adminDSN)
		if err != nil {
			t.Errorf("connect to drop test database %s: %v", name, err)
			return
		}
		defer func() { _ = cleaner.Close(context.Background()) }()
		// Connections the test left open block DROP DATABASE; FORCE closes them
		// rather than failing the run over a handle the test forgot.
		if _, err := cleaner.Exec(cleanupCtx, `DROP DATABASE IF EXISTS `+quoteIdentifier(name)+` WITH (FORCE)`); err != nil {
			t.Errorf("drop test database %s: %v", name, err)
		}
	})
	dsn, err := withDatabase(adminDSN, name)
	if err != nil {
		t.Fatal(err)
	}
	return dsn
}

// databaseName derives a unique, valid identifier from the test's name. The
// counter keeps subtests and t.Parallel siblings apart; the process id keeps
// concurrently running test binaries apart.
func databaseName(testName string) string {
	sanitized := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_':
			return r
		case r >= 'A' && r <= 'Z':
			return r + ('a' - 'A')
		default:
			return '_'
		}
	}, testName)
	suffix := fmt.Sprintf("_%d_%d", os.Getpid(), counter.Add(1))
	const prefix = "rolltop_test_"
	room := maxDatabaseNameLen - len(prefix) - len(suffix)
	if len(sanitized) > room {
		sanitized = sanitized[:room]
	}
	return prefix + sanitized + suffix
}

// withDatabase rewrites the DSN to name a different database.
//
// pgx.ParseConfig round-trips are not usable here: ConnString reports the string
// it parsed, so mutating Config.Database and re-serializing silently returns the
// original database name and every test would share one database.
func withDatabase(dsn, name string) (string, error) {
	parsed, err := url.Parse(dsn)
	if err != nil || (parsed.Scheme != "postgres" && parsed.Scheme != "postgresql") {
		// The rejected value is echoed so the operator can see the typo, which
		// makes this the one place in the package that can print a DSN — and a
		// keyword-form DSN carries its password in the clear.
		return "", fmt.Errorf("%s must be a postgres:// URL so the database name can be replaced, got %q", EnvVar, pgdsn.Redact(dsn))
	}
	parsed.Path = "/" + name
	return parsed.String(), nil
}

// quoteIdentifier is applied to names this package generates itself, so it
// guards against a test name producing a reserved word rather than against
// injection.
func quoteIdentifier(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}
