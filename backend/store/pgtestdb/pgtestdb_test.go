package pgtestdb

import (
	"context"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

// TestNewDropsTheDatabaseAfterTheTest is the regression guard for the first
// version of this helper, which registered a cleanup against a connection that
// its own defer had already closed. Every test leaked a database, and nothing
// failed: the cleanup only logged.
func TestNewDropsTheDatabaseAfterTheTest(t *testing.T) {
	var dsn string
	t.Run("inner", func(t *testing.T) {
		dsn = New(t)
	})
	if dsn == "" {
		t.Skipf("%s not set", EnvVar)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	admin, err := connect(ctx, os.Getenv(EnvVar))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = admin.Close(context.Background()) }()

	name := databaseNameFromDSN(t, dsn)
	var remaining int
	if err := admin.QueryRow(ctx, `SELECT count(*) FROM pg_database WHERE datname = $1`, name).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 0 {
		t.Errorf("database %s still exists after its test finished", name)
	}
}

// TestNewIsolatesDatabases proves two calls do not hand out the same database,
// which is what lets tests run in parallel.
func TestNewIsolatesDatabases(t *testing.T) {
	first := New(t)
	second := New(t)
	if first == second {
		t.Fatalf("both calls returned %s", first)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, dsn := range []string{first, second} {
		conn, err := connect(ctx, dsn)
		if err != nil {
			t.Fatalf("connect to %s: %v", databaseNameFromDSN(t, dsn), err)
		}
		var current string
		if err := conn.QueryRow(ctx, `SELECT current_database()`).Scan(&current); err != nil {
			t.Fatal(err)
		}
		if want := databaseNameFromDSN(t, dsn); current != want {
			t.Errorf("connected to %s, want %s", current, want)
		}
		_ = conn.Close(ctx)
	}
}

func TestWithDatabaseRejectsNonURLDSN(t *testing.T) {
	_, err := withDatabase("host=localhost user=rolltop password=hunter2", "x")
	if err == nil {
		t.Fatal("a keyword DSN was accepted")
	}
	if strings.Contains(err.Error(), "hunter2") {
		t.Errorf("error echoes the password: %q", err)
	}
}

func TestDatabaseNameFitsPostgresIdentifier(t *testing.T) {
	long := strings.Repeat("TestSomethingWithAVeryLongName/", 10)
	name := databaseName(long)
	if len(name) > maxDatabaseNameLen {
		t.Errorf("name is %d bytes: %s", len(name), name)
	}
	if strings.ContainsAny(name, "/ ") {
		t.Errorf("name carries characters from the test name: %s", name)
	}
}

func databaseNameFromDSN(t *testing.T, dsn string) string {
	t.Helper()
	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimPrefix(parsed.Path, "/")
}
