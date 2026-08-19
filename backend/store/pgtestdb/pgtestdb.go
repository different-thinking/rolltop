// File overview: Throwaway PostgreSQL databases for tests. TEST_DATABASE_URL
// names a server the test run may create databases on; New hands out one empty
// database per test and drops it afterwards, so tests that write schema or rows
// cannot see each other's state.
//
// The server must be a test-local container with CREATEDB rights. Pointing this
// at the hosted database does not work and is not meant to: that role has
// CREATEDB=false, and New says so rather than failing with a bare permission
// error.
//
// Nothing here prints a DSN unredacted. The admin connection string is a
// credential like any other, and it reaches CI logs through a failed test as
// readily as through a log line.

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
	"rolltop/backend/sqlident"
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
	return newDatabase(t, adminDSNOrSkip(t), "")
}

// adminDSNOrSkip resolves the server tests may create databases on, skipping
// the test when none is configured.
//
// Skipping an unset variable and failing an unusable one is the whole contract:
// a developer without a database gets a suite that says so, while a CI service
// that failed to come up gets a red run rather than a green one that verified
// nothing.
func adminDSNOrSkip(t *testing.T) string {
	t.Helper()
	adminDSN := strings.TrimSpace(os.Getenv(EnvVar))
	if adminDSN == "" {
		t.Skipf("%s not set", EnvVar)
	}
	return adminDSN
}

// newDatabase creates one empty database and registers its cleanup. A non-empty
// template clones that database's schema instead of starting from nothing.
func newDatabase(t *testing.T, adminDSN, template string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	admin, err := connect(ctx, adminDSN)
	if err != nil {
		t.Fatalf("%s is set but unusable: %v", EnvVar, err)
	}
	defer func() { _ = admin.Close(context.Background()) }()

	// The name carries this process's id and a counter, so it cannot collide
	// with a concurrent test binary or a sibling subtest. A database left by a
	// killed run therefore also cannot be reclaimed by name here — cleaning
	// those up is a job for the test server, not for an unrelated test.
	name := databaseName(t.Name())
	create := `CREATE DATABASE ` + sqlident.Quote(name)
	if template != "" {
		create += ` TEMPLATE ` + sqlident.Quote(template)
	}
	if _, err := admin.Exec(ctx, create); err != nil {
		t.Fatalf("cannot create a test database on %s (the server must be a test-local instance whose role has CREATEDB): %v", EnvVar, pgdsn.Redact(err.Error()))
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		// A fresh connection, because the one above is closed by the time
		// cleanups run. Reusing it drops nothing and leaves a database per test
		// behind on the server.
		cleaner, err := connect(cleanupCtx, adminDSN)
		if err != nil {
			t.Errorf("connect to drop test database %s: %v", name, pgdsn.Redact(err.Error()))
			return
		}
		defer func() { _ = cleaner.Close(context.Background()) }()
		// Connections the test left open block DROP DATABASE; FORCE closes them
		// rather than failing the run over a handle the test forgot.
		if _, err := cleaner.Exec(cleanupCtx, `DROP DATABASE IF EXISTS `+sqlident.Quote(name)+` WITH (FORCE)`); err != nil {
			t.Errorf("drop test database %s: %v", name, pgdsn.Redact(err.Error()))
		}
	})
	dsn, err := withDatabase(adminDSN, name)
	if err != nil {
		t.Fatal(err)
	}
	return dsn
}

// connect is the one place this package dials, so no call site can forget that
// a pgx parse error quotes the whole connection string back.
func connect(ctx context.Context, dsn string) (*pgx.Conn, error) {
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("%s", pgdsn.Redact(err.Error()))
	}
	return conn, nil
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
	if room < 0 {
		room = 0
	}
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
//
// The opaque check is the same hazard by a different route. `postgres:host/db`
// — a URL missing its slashes — parses with the right scheme but puts everything
// in URL.Opaque, and String() then ignores Path entirely, so the rewrite would
// be dropped and the returned DSN would still address the admin database.
func withDatabase(dsn, name string) (string, error) {
	parsed, err := url.Parse(dsn)
	if err != nil || (parsed.Scheme != "postgres" && parsed.Scheme != "postgresql") || parsed.Opaque != "" || parsed.Host == "" {
		// The rejected value is echoed so the operator can see the typo, which
		// makes this one of the places that can print a DSN — and a keyword-form
		// DSN carries its password in the clear.
		return "", fmt.Errorf("%s must be a postgres://host/database URL so the database name can be replaced, got %q", EnvVar, pgdsn.Redact(dsn))
	}
	parsed.Path = "/" + name
	return parsed.String(), nil
}
