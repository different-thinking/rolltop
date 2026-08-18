package pgpreflight

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

// TestRunAgainstRealPostgres exercises the full preflight against a live
// server. It is skipped unless TEST_DATABASE_URL points at one, so the suite
// stays green in environments without Postgres.
func TestRunAgainstRealPostgres(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	report := Run(ctx, dsn)
	for _, check := range report.Checks {
		t.Logf("%-24s %-4s %s %s", check.ID, check.Status, check.Title, check.Detail)
	}
	if !report.OK {
		t.Fatalf("preflight failed against %q", "TEST_DATABASE_URL")
	}
	ids := map[string]bool{}
	for _, check := range report.Checks {
		ids[check.ID] = true
	}
	for _, want := range []string{"connect", "latency", "version", "encoding", "collation-deterministic", "privileges", "collate-c", "extensions", "utf8-nul", "utf8-invalid", "sql-features", "connections"} {
		if !ids[want] {
			t.Errorf("report is missing check %q", want)
		}
	}
}

// TestRunConnectFailure must produce a failed report, not an error or panic,
// and must not echo credentials from the DSN back in the check detail.
func TestRunConnectFailure(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	report := Run(ctx, "postgres://user:supersecret@127.0.0.1:1/nope?connect_timeout=1")
	if report.OK {
		t.Fatal("connecting to a closed port reported OK")
	}
	if len(report.Checks) != 1 || report.Checks[0].ID != "connect" || report.Checks[0].Status != "fail" {
		t.Fatalf("unexpected checks: %+v", report.Checks)
	}
	if detail := report.Checks[0].Detail; detail == "" {
		t.Error("connect failure carries no detail")
	} else if strings.Contains(detail, "supersecret") {
		t.Errorf("connect failure echoes the password: %s", detail)
	}
}

// TestSanitizeConnectError strips anything after a password keyword.
func TestSanitizeConnectError(t *testing.T) {
	got := sanitizeConnectError(errors.New("failed: host=db password=hunter2 port=5432"))
	if strings.Contains(got, "hunter2") {
		t.Fatalf("password survived sanitization: %s", got)
	}
}
