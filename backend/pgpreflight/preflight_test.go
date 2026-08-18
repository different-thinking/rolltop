package pgpreflight

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const testPassword = "supersecret-do-not-echo"

// unreachableDSNs spell the same credentials the way a real admin might, in
// every form pgx accepts. The keyword form with spaces around "=" is the one
// pgx's own redaction misses, and the one an earlier revision leaked.
func unreachableDSNs() []string {
	return []string{
		"postgres://rolltop:" + testPassword + "@127.0.0.1:1/rolltop?connect_timeout=1",
		"postgresql://rolltop:" + testPassword + "@127.0.0.1:1/rolltop?connect_timeout=1",
		"host=127.0.0.1 port=1 password=" + testPassword + " connect_timeout=1",
		"host=127.0.0.1 port=1 password = " + testPassword + " connect_timeout=1",
		"host=127.0.0.1 port=1 password='" + testPassword + "' connect_timeout=1",
		"host=127.0.0.1 port=1 password = '" + testPassword + "' connect_timeout=1",
		// Parse failures take a different path through pgx and echo the whole
		// connection string back, which is how the leak was found.
		"host=127.0.0.1 port=notaport password = " + testPassword,
		"host=127.0.0.1 port=notaport password=" + testPassword,
	}
}

// TestRunNeverEchoesCredentials is the regression test for the reported leak:
// no DSN spelling may put the password into a reported check.
func TestRunNeverEchoesCredentials(t *testing.T) {
	for _, dsn := range unreachableDSNs() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		report, err := Run(ctx, dsn)
		cancel()
		if err != nil {
			t.Fatalf("Run(%q) returned %v", dsn, err)
		}
		if report.OK {
			t.Errorf("Run(%q) reported OK against an unreachable target", dsn)
		}
		for _, check := range report.Checks {
			if strings.Contains(check.Detail, testPassword) {
				t.Errorf("DSN %q leaked the password in check %q: %s", dsn, check.ID, check.Detail)
			}
		}
	}
}

func TestRedactSecrets(t *testing.T) {
	cases := []string{
		"failed: host=db password=hunter2 port=5432",
		"failed: host=db password = hunter2 port=5432",
		"failed: host=db PASSWORD  =  hunter2",
		"failed: host=db password='hunter2'",
		"failed: host=db password = 'hunter2'",
		`failed: host=db password="hunter2"`,
		"cannot parse `postgres://rolltop:hunter2@db:5432/x`: bad port",
		"cannot parse `postgresql://rolltop:hunter2@db:5432/x`: bad port",
		"failed: PGPASSWORD=hunter2",
	}
	for _, message := range cases {
		if got := redactSecrets(message); strings.Contains(got, "hunter2") {
			t.Errorf("redactSecrets(%q) = %q, password survived", message, got)
		}
	}
	// Redaction must not swallow the useful part of the message.
	got := redactSecrets("cannot parse `postgres://rolltop:hunter2@db:5432/x`: invalid port")
	if !strings.Contains(got, "invalid port") || !strings.Contains(got, "rolltop") {
		t.Errorf("redactSecrets removed diagnostic context: %s", got)
	}
}

// TestRunRejectsConcurrentRuns covers the shared scratch schema: a second run
// must be refused rather than dropping the first run's tables.
func TestRunRejectsConcurrentRuns(t *testing.T) {
	runLock.Lock()
	defer runLock.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := Run(ctx, "host=127.0.0.1 port=1 connect_timeout=1"); !errors.Is(err, ErrBusy) {
		t.Fatalf("second concurrent run returned %v, want ErrBusy", err)
	}
}

// TestRunReleasesLock guards against the lock leaking on the error path, which
// would wedge the feature until restart.
func TestRunReleasesLock(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for i := 0; i < 2; i++ {
		if _, err := Run(ctx, "host=127.0.0.1 port=1 connect_timeout=1"); err != nil {
			t.Fatalf("run %d returned %v", i, err)
		}
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if !runLock.TryLock() {
			t.Error("lock still held after Run returned")
			return
		}
		runLock.Unlock()
	}()
	wg.Wait()
}

func TestRunConnectFailureShape(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	report, err := Run(ctx, "postgres://user@127.0.0.1:1/nope?connect_timeout=1")
	if err != nil {
		t.Fatalf("Run returned %v", err)
	}
	if report.OK {
		t.Fatal("connecting to a closed port reported OK")
	}
	if len(report.Checks) != 1 || report.Checks[0].ID != "connect" || report.Checks[0].Status != StatusFail {
		t.Fatalf("unexpected checks: %+v", report.Checks)
	}
	if report.Checks[0].Detail == "" {
		t.Error("connect failure carries no detail")
	}
}

// TestConnectAppliesDefaultTimeout pins the fail-fast behavior for a DSN that
// does not set connect_timeout itself, and the deference to one that does.
func TestConnectAppliesDefaultTimeout(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := connect(ctx, "host=127.0.0.1 port=1"); err == nil {
		t.Fatal("connect to a closed port succeeded")
	}
	config, err := parseWithDefaults("host=127.0.0.1 port=1")
	if err != nil {
		t.Fatal(err)
	}
	if config.ConnectTimeout != defaultConnectTimeout {
		t.Errorf("ConnectTimeout = %v, want the %v default", config.ConnectTimeout, defaultConnectTimeout)
	}
	config, err = parseWithDefaults("host=127.0.0.1 port=1 connect_timeout=3")
	if err != nil {
		t.Fatal(err)
	}
	if config.ConnectTimeout != 3*time.Second {
		t.Errorf("ConnectTimeout = %v, want the DSN's own 3s", config.ConnectTimeout)
	}
}

// TestTwinsAgree keeps backend/pgpreflight and scripts/pg-preflight.sql in
// sync: the literals they share must appear in both, so editing one twin
// alone fails the build rather than silently producing two tools that
// disagree about migration readiness.
func TestTwinsAgree(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "scripts", "pg-preflight.sql"))
	if err != nil {
		t.Fatal(err)
	}
	script := string(raw)
	shared := []string{
		scratchSchema,
		wantByteOrder,
		"160000",
		`chr(0)`,
		`convert_from('\xff'::bytea, 'UTF8')`,
		`COLLATE "C"`,
		"setval",
		"to_tsvector",
	}
	shared = append(shared, requiredExtensions...)
	for _, literal := range shared {
		if !strings.Contains(script, literal) {
			t.Errorf("scripts/pg-preflight.sql is missing %q, which the Go preflight checks", literal)
		}
	}
	// The SQLSTATEs the Go twin asserts must be the ones the SQL twin names
	// by condition, or the two disagree about what counts as a pass.
	for condition, sqlstate := range map[string]string{
		"program_limit_exceeded":      sqlstateNullNotPermitted,
		"character_not_in_repertoire": sqlstateInvalidEncoding,
	} {
		if !strings.Contains(script, condition) {
			t.Errorf("scripts/pg-preflight.sql does not handle %s (SQLSTATE %s)", condition, sqlstate)
		}
	}
}

// TestRunAgainstRealPostgres exercises the full preflight against a live
// server. It is skipped unless TEST_DATABASE_URL points at one, so the suite
// stays green in environments without Postgres.
func TestRunAgainstRealPostgres(t *testing.T) {
	dsn := requireTestDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	report, err := Run(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	for _, check := range report.Checks {
		t.Logf("%-22s %-4s %s %s", check.ID, check.Status, check.Title, check.Detail)
	}
	if !report.OK {
		t.Fatal("preflight failed against TEST_DATABASE_URL")
	}
	ids := map[string]bool{}
	for _, check := range report.Checks {
		ids[check.ID] = true
	}
	for _, want := range []string{
		"connect", "latency", "version", "encoding", "locale", "byte-exact-equality",
		"privileges", "collate-c", "collate-default", "extensions",
		"utf8-nul", "utf8-invalid", "sql-features", "connections",
	} {
		if !ids[want] {
			t.Errorf("report is missing check %q", want)
		}
	}
}

// TestRunCleansUpAfterCancellation is the regression test for the leftover
// scratch schema: a cancelled run force-closes the pgx connection, so the
// cleanup has to reconnect to finish the drop.
func TestRunCleansUpAfterCancellation(t *testing.T) {
	dsn := requireTestDatabase(t)
	// Long enough to create the scratch schema, too short to finish.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := Run(ctx, dsn); err != nil {
		t.Fatal(err)
	}
	verifyCtx, verifyCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer verifyCancel()
	conn, err := connect(verifyCtx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(verifyCtx) }()
	var present bool
	if err := conn.QueryRow(verifyCtx,
		`SELECT EXISTS (SELECT 1 FROM pg_namespace WHERE nspname = $1)`, scratchSchema).
		Scan(&present); err != nil {
		t.Fatal(err)
	}
	if present {
		t.Fatalf("schema %s survived a cancelled run", scratchSchema)
	}
}

// TestUTF8ChecksFailOnTransportError proves the strictness probes no longer
// report a pass for a check that never reached the server: running them on a
// closed connection must fail, not silently succeed.
func TestUTF8ChecksFailOnTransportError(t *testing.T) {
	dsn := requireTestDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, err := connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.Close(ctx); err != nil {
		t.Fatal(err)
	}
	r := &runner{conn: conn}
	r.checkUTF8Strictness(ctx)
	if !r.failed {
		t.Fatal("UTF-8 checks passed against a closed connection")
	}
	for _, check := range r.checks {
		if check.Status == StatusPass {
			t.Errorf("check %q reported pass without reaching the server", check.ID)
		}
	}
}

func requireTestDatabase(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	return dsn
}
