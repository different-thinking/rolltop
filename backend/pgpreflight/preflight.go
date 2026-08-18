// File overview: PostgreSQL migration preflight, the Go twin of
// scripts/pg-preflight.sql. It connects to a candidate target database and
// verifies every capability docs/postgres-migration-plan.md assumes: server
// version and encoding, byte-exact text equality, column- and index-level
// COLLATE "C" byte ordering, the trusted extensions, UTF-8 strictness, and
// the SQL features the ported queries rely on. Run from the app container it
// also measures the round-trip latency of the real app-to-database path,
// which a bastion session cannot.
//
// Two properties this file is responsible for, both of which failed review
// once already:
//
//   - The DSN never leaves this process. Driver errors echo the connection
//     string, and pgx's own redaction misses the libpq keyword form with
//     spaces (`password = secret`), so redactSecrets below scrubs every
//     spelling before an error is handed on.
//   - A run leaves nothing behind but the extensions. The scratch schema is
//     dropped even when the run's context was cancelled, which force-closes
//     the pgx connection; the cleanup then reconnects to finish the job.

package pgpreflight

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Check statuses. The frontend union in types.ts mirrors these three values.
const (
	StatusPass = "pass"
	StatusFail = "fail"
	StatusInfo = "info"
)

// scratchSchema must match scripts/pg-preflight.sql so a half-finished run of
// either tool is cleaned up by the next run of the other. TestTwinsAgree
// asserts the two stay in sync.
const scratchSchema = "preflight_scratch"

// minServerVersion is the oldest Postgres the migration plan targets.
const minServerVersion = 160000

// requiredExtensions are the trusted extensions phase 7 (search) needs.
var requiredExtensions = []string{"pg_trgm", "citext", "unaccent"}

// wantByteOrder is how the probe values sort under byte order: the UTF-8
// encodings are B (0x42) < Z (0x5A) < a (0x61) < ä (0xC3A4).
const wantByteOrder = "B,Z,a,ä"

// defaultConnectTimeout bounds the dial so a blackholed host reports
// "unreachable" in seconds instead of consuming the caller's whole budget. A
// DSN that sets connect_timeout itself keeps its own value.
const defaultConnectTimeout = 10 * time.Second

// cleanupTimeout bounds the post-run scratch-schema drop, including the
// reconnect the cancelled-context path needs.
const cleanupTimeout = 15 * time.Second

// ErrBusy reports that another preflight is already running. Runs share one
// fixed scratch schema and each starts by dropping it, so two concurrent runs
// against the same target would delete each other's tables mid-battery.
var ErrBusy = errors.New("a preflight run is already in progress")

// runLock serializes runs process-wide. It does not coordinate with a
// bastion psql session against the same database; that remains an operator
// concern, documented in scripts/pg-preflight.sql.
var runLock sync.Mutex

// Check is one verified capability. Status is StatusPass, StatusFail, or
// StatusInfo; info rows report facts that need a human judgement rather than
// having a hard pass condition.
type Check struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	Status string `json:"status"`
	Detail string `json:"detail"`
}

// Report is the outcome of one preflight run. OK is true only when no check
// failed. DurationMS covers the whole run including connect.
type Report struct {
	OK         bool    `json:"ok"`
	Checks     []Check `json:"checks"`
	DurationMS int64   `json:"duration_ms"`
}

type runner struct {
	conn   *pgx.Conn
	checks []Check
	failed bool
}

func (r *runner) pass(id, title, detail string) {
	r.checks = append(r.checks, Check{ID: id, Title: title, Status: StatusPass, Detail: detail})
}

// fail records a failure from a plain message. Callers with an error value
// use failErr so redaction happens in exactly one place.
func (r *runner) fail(id, title, detail string) {
	r.failed = true
	r.checks = append(r.checks, Check{ID: id, Title: title, Status: StatusFail, Detail: detail})
}

func (r *runner) failErr(id, title string, err error) {
	detail := ""
	if err != nil {
		detail = redactSecrets(err.Error())
	}
	r.fail(id, title, detail)
}

func (r *runner) info(id, title, detail string) {
	r.checks = append(r.checks, Check{ID: id, Title: title, Status: StatusInfo, Detail: detail})
}

// execAll runs statements in order and reports the first failure under id.
// It returns false once anything failed so callers can stop.
func (r *runner) execAll(ctx context.Context, id, title string, statements ...string) bool {
	for _, statement := range statements {
		if _, err := r.conn.Exec(ctx, statement); err != nil {
			r.failErr(id, title, err)
			return false
		}
	}
	return true
}

// Secret spellings pgx may echo back: the libpq keyword form with any
// spacing, quoted or bare, and the userinfo section of a URL DSN. pgconn's
// own redactPW only handles the space-free keyword form, so a DSN written as
// `password = secret` reaches the caller in cleartext without this.
var secretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)password\s*=\s*'[^']*'`),
	regexp.MustCompile(`(?i)password\s*=\s*"[^"]*"`),
	regexp.MustCompile(`(?i)password\s*=\s*[^\s'"]+`),
	regexp.MustCompile(`(?i)\b(pgpassword|passfile)\s*=\s*\S+`),
}

// urlUserinfo matches the credentials section of a URL-style DSN, keeping the
// scheme and user so the message stays useful.
var urlUserinfo = regexp.MustCompile(`(?i)(postgres(?:ql)?://[^:/@\s]*):[^@/\s]*@`)

// redactSecrets removes credential material from text that may embed a DSN.
// It is applied to every error this package reports, not only connect
// failures, because pgx echoes the connection string from several paths.
func redactSecrets(message string) string {
	message = urlUserinfo.ReplaceAllString(message, "$1:…@")
	for _, pattern := range secretPatterns {
		message = pattern.ReplaceAllString(message, "password=…")
	}
	return message
}

// Run executes the preflight against dsn. It never panics and always returns
// a report; a connection failure is itself a reported check. It returns
// ErrBusy when another run holds the lock. The only persistent effects on the
// target are the CREATE EXTENSION calls, which the migration wants anyway —
// the scratch schema is dropped at the end even if the run is cancelled.
func Run(ctx context.Context, dsn string) (Report, error) {
	if !runLock.TryLock() {
		return Report{}, ErrBusy
	}
	defer runLock.Unlock()

	started := time.Now()
	r := &runner{}
	conn, err := connect(ctx, dsn)
	if err != nil {
		r.failErr("connect", "Connect to the database", err)
		return r.report(started), nil
	}
	r.conn = conn
	defer dropScratchSchema(conn, dsn)
	r.pass("connect", "Connect to the database", "")

	r.checkLatency(ctx)
	r.checkServer(ctx)
	r.checkByteExactEquality(ctx)
	if r.checkScratchSchema(ctx) {
		r.checkCollateC(ctx)
		r.checkExtensions(ctx)
		r.checkUTF8Strictness(ctx)
		r.checkSQLFeatures(ctx)
	}
	r.checkConnectionBudget(ctx)
	return r.report(started), nil
}

// parseWithDefaults applies a default dial timeout unless the DSN sets one,
// so an unreachable host fails fast instead of holding the caller's whole
// budget.
func parseWithDefaults(dsn string) (*pgx.ConnConfig, error) {
	config, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
	if config.ConnectTimeout == 0 {
		config.ConnectTimeout = defaultConnectTimeout
	}
	return config, nil
}

func connect(ctx context.Context, dsn string) (*pgx.Conn, error) {
	config, err := parseWithDefaults(dsn)
	if err != nil {
		return nil, err
	}
	return pgx.ConnectConfig(ctx, config)
}

// dropScratchSchema removes the scratch schema and closes the connection.
// When the run ended through a cancelled context, pgx has already destroyed
// the connection, so the drop is retried over a fresh one: otherwise an
// aborted run leaves its tables in the candidate database, contradicting what
// the UI promises.
func dropScratchSchema(conn *pgx.Conn, dsn string) {
	ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
	defer cancel()
	_, err := conn.Exec(ctx, `DROP SCHEMA IF EXISTS `+scratchSchema+` CASCADE`)
	_ = conn.Close(ctx)
	if err == nil {
		return
	}
	fresh, freshErr := connect(ctx, dsn)
	if freshErr != nil {
		return
	}
	defer func() { _ = fresh.Close(ctx) }()
	_, _ = fresh.Exec(ctx, `DROP SCHEMA IF EXISTS `+scratchSchema+` CASCADE`)
}

func (r *runner) report(started time.Time) Report {
	return Report{OK: !r.failed, Checks: r.checks, DurationMS: time.Since(started).Milliseconds()}
}

// checkLatency measures round trips on the live connection. Run inside the
// platform this is the latency the ported store will actually pay per
// statement, which is the number §8.2 of the migration plan wants.
func (r *runner) checkLatency(ctx context.Context) {
	const rounds = 10
	samples := make([]time.Duration, 0, rounds)
	for i := 0; i < rounds; i++ {
		start := time.Now()
		var one int
		if err := r.conn.QueryRow(ctx, `SELECT 1`).Scan(&one); err != nil {
			r.failErr("latency", "Round-trip latency", err)
			return
		}
		samples = append(samples, time.Since(start))
	}
	sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
	median := samples[len(samples)/2]
	r.info("latency", "Round-trip latency",
		fmt.Sprintf("median %.2f ms, fastest %.2f ms over %d round trips",
			float64(median.Microseconds())/1000, float64(samples[0].Microseconds())/1000, rounds))
}

func (r *runner) checkServer(ctx context.Context) {
	const versionTitle = "Server version >= 16"
	const encodingTitle = "Server encoding is UTF8"
	var versionNum int
	var version, encoding, collate, ctype, provider string
	// One round trip for everything the gate and the locale info row need.
	// A restrictive host that refuses the catalog read must fail the gate
	// rather than silently omit it, so both checks report the same error.
	err := r.conn.QueryRow(ctx, `
		SELECT current_setting('server_version_num')::int,
		       current_setting('server_version'),
		       current_setting('server_encoding'),
		       d.datcollate, d.datctype, d.datlocprovider::text
		FROM pg_database d WHERE d.datname = current_database()`).
		Scan(&versionNum, &version, &encoding, &collate, &ctype, &provider)
	if err != nil {
		r.failErr("version", versionTitle, err)
		r.failErr("encoding", encodingTitle, err)
		return
	}
	if versionNum < minServerVersion {
		r.fail("version", versionTitle, fmt.Sprintf("server version %s is below 16", version))
	} else {
		r.pass("version", versionTitle, version)
	}
	if encoding != "UTF8" {
		r.fail("encoding", encodingTitle, fmt.Sprintf("server encoding is %s", encoding))
	} else {
		r.pass("encoding", encodingTitle, "")
	}
	r.info("locale", "Database locale",
		fmt.Sprintf("LC_COLLATE=%s LC_CTYPE=%s provider=%s", collate, ctype, localeProviderName(provider)))
}

func localeProviderName(code string) string {
	switch code {
	case "c":
		return "libc"
	case "i":
		return "icu"
	case "b":
		return "builtin"
	}
	return code
}

// checkByteExactEquality verifies the property §3.4 of the migration plan
// rests on: text equality compares byte-wise, so the 58 COLLATE BINARY
// comparison sites keep SQLite semantics under whatever locale the hoster's
// cluster uses.
//
// This is deliberately a behavioral probe rather than a catalog lookup. The
// pg_collation row named "default" is a pinned placeholder whose
// collisdeterministic is hard-wired true and says nothing about the database,
// so querying it only looks like a check. Postgres also does not currently
// allow a nondeterministic collation as a database default — verified against
// 16, where even an ICU und-u-ks-level1 database still compares 'a' <> 'A' —
// which makes these probes a guard against a future relaxation rather than a
// live hazard.
func (r *runner) checkByteExactEquality(ctx context.Context) {
	const title = "Text equality is byte-exact"
	// The accent and normalization operands use unicode escapes so no editor
	// normalization can collapse the two sides into the same bytes: U+00E4 is
	// precomposed 'a-umlaut', U+0061 U+0308 is 'a' plus a combining diaeresis.
	// An earlier revision wrote them as literals and compared a value with
	// itself, which the SQL twin caught only when run against a live server.
	var caseFold, ligature, padding, accent, normalization bool
	if err := r.conn.QueryRow(ctx,
		`SELECT 'a' = 'A', 'ss' = U&'\00DF', 'abc' = 'abc ',
		        'a' = U&'\00E1', U&'\00E4' = U&'a\0308'`).
		Scan(&caseFold, &ligature, &padding, &accent, &normalization); err != nil {
		r.failErr("byte-exact-equality", title, err)
		return
	}
	insensitivities := []struct {
		got  bool
		name string
	}{
		{caseFold, "case ('a' = 'A')"},
		{ligature, "ligature ('ss' = 'ß')"},
		{padding, "padding ('abc' = 'abc ')"},
		{accent, "accent ('a' = 'á')"},
		{normalization, "normalization (precomposed = decomposed)"},
	}
	for _, insensitivity := range insensitivities {
		if insensitivity.got {
			r.fail("byte-exact-equality", title,
				"the default collation is "+insensitivity.name+"-insensitive, so = is not byte-exact")
			return
		}
	}
	r.pass("byte-exact-equality", title, "")
}

func (r *runner) checkScratchSchema(ctx context.Context) bool {
	const title = "Privileges: create schema, tables, indexes"
	if !r.execAll(ctx, "privileges", title,
		`DROP SCHEMA IF EXISTS `+scratchSchema+` CASCADE`,
		`CREATE SCHEMA `+scratchSchema) {
		return false
	}
	r.pass("privileges", title, "")
	return true
}

func (r *runner) checkCollateC(ctx context.Context) {
	const title = `Column- and index-level COLLATE "C"`
	if !r.execAll(ctx, "collate-c", title,
		`CREATE TABLE `+scratchSchema+`.collate_c (key text COLLATE "C" NOT NULL)`,
		`CREATE INDEX ON `+scratchSchema+`.collate_c (key)`,
		`CREATE TABLE `+scratchSchema+`.collate_default (key text NOT NULL)`,
		`CREATE INDEX ON `+scratchSchema+`.collate_default (key COLLATE "C")`,
		`INSERT INTO `+scratchSchema+`.collate_c VALUES ('a'), ('B'), ('Z'), ('ä')`,
		`INSERT INTO `+scratchSchema+`.collate_default SELECT key FROM `+scratchSchema+`.collate_c`) {
		return
	}
	probes := []struct {
		query string
		what  string
	}{
		{`SELECT string_agg(key, ',' ORDER BY key) FROM ` + scratchSchema + `.collate_c`,
			`column declared COLLATE "C"`},
		{`SELECT string_agg(key, ',' ORDER BY key COLLATE "C") FROM ` + scratchSchema + `.collate_default`,
			`per-query COLLATE "C"`},
	}
	for _, probe := range probes {
		var order string
		if err := r.conn.QueryRow(ctx, probe.query).Scan(&order); err != nil {
			r.failErr("collate-c", title, err)
			return
		}
		if order != wantByteOrder {
			r.fail("collate-c", title, fmt.Sprintf("%s sorts as %s, want %s", probe.what, order, wantByteOrder))
			return
		}
	}
	r.pass("collate-c", title, "")
	var defaultOrder string
	if err := r.conn.QueryRow(ctx,
		`SELECT string_agg(key, ',' ORDER BY key) FROM `+scratchSchema+`.collate_default`).
		Scan(&defaultOrder); err == nil {
		r.info("collate-default", "Sort order under the database default collation",
			fmt.Sprintf("%s (byte order would be %s)", defaultOrder, wantByteOrder))
	}
}

func (r *runner) checkExtensions(ctx context.Context) {
	title := "Trusted extensions: " + joinComma(requiredExtensions)
	for _, extension := range requiredExtensions {
		if !r.execAll(ctx, "extensions", title, `CREATE EXTENSION IF NOT EXISTS `+extension+` WITH SCHEMA public`) {
			return
		}
	}
	var similarity float64
	var unaccented string
	if err := r.conn.QueryRow(ctx,
		`SELECT public.similarity('rolltop', 'roltop'), public.unaccent('Müller')`).
		Scan(&similarity, &unaccented); err != nil {
		r.failErr("extensions", title, err)
		return
	}
	if similarity <= 0 || unaccented != "Muller" {
		r.fail("extensions", title, fmt.Sprintf("similarity=%v unaccent=%q", similarity, unaccented))
		return
	}
	r.pass("extensions", title, "")
}

func joinComma(values []string) string {
	out := ""
	for i, value := range values {
		if i > 0 {
			out += ", "
		}
		out += value
	}
	return out
}

// SQLSTATEs the UTF-8 probes must provoke. Anything else — a transport
// error, a cancelled context — means the property was never tested, so it
// cannot be reported as a pass.
const (
	sqlstateNullNotPermitted = "54000"
	sqlstateInvalidEncoding  = "22021"
)

// checkUTF8Strictness documents that the target rejects what SQLite accepted,
// which is why the migration plan requires write-path sanitization (§8.1).
// Both statements are expected to fail with a specific SQLSTATE.
func (r *runner) checkUTF8Strictness(ctx context.Context) {
	probes := []struct {
		id       string
		title    string
		query    string
		sqlstate string
	}{
		{"utf8-nul", "NUL character is rejected", `SELECT chr(0)`, sqlstateNullNotPermitted},
		{"utf8-invalid", "Invalid UTF-8 is rejected", `SELECT convert_from('\xff'::bytea, 'UTF8')`, sqlstateInvalidEncoding},
	}
	for _, probe := range probes {
		var out string
		err := r.conn.QueryRow(ctx, probe.query).Scan(&out)
		if err == nil {
			r.fail(probe.id, probe.title, "the server accepted the value instead of rejecting it")
			continue
		}
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) {
			// A transport or context error proves nothing about the server's
			// strictness; reporting a pass here would be a vacuous green row.
			r.failErr(probe.id, probe.title, fmt.Errorf("check did not complete: %w", err))
			continue
		}
		if pgErr.Code != probe.sqlstate {
			r.fail(probe.id, probe.title,
				fmt.Sprintf("rejected with SQLSTATE %s, expected %s: %s", pgErr.Code, probe.sqlstate, pgErr.Message))
			continue
		}
		r.pass(probe.id, probe.title, pgErr.Message)
	}
}

func (r *runner) checkSQLFeatures(ctx context.Context) {
	const title = "SQL features: RETURNING, ON CONFLICT, excluded.*, = ANY, identity setval, tsvector/GIN"
	features := scratchSchema + ".features"
	if !r.execAll(ctx, "sql-features", title,
		`CREATE TABLE `+features+` (
			id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
			slot text COLLATE "C" NOT NULL UNIQUE,
			hits bigint NOT NULL DEFAULT 0,
			body tsvector
		)`,
		`CREATE INDEX ON `+features+` USING gin (body)`) {
		return
	}
	var newID int64
	if err := r.conn.QueryRow(ctx, `INSERT INTO `+features+` (slot) VALUES ('a') RETURNING id`).Scan(&newID); err != nil {
		r.failErr("sql-features", title, fmt.Errorf("INSERT RETURNING: %w", err))
		return
	}
	tag, err := r.conn.Exec(ctx, `INSERT INTO `+features+` (slot) VALUES ('a') ON CONFLICT DO NOTHING`)
	if err != nil {
		r.failErr("sql-features", title, fmt.Errorf("ON CONFLICT DO NOTHING: %w", err))
		return
	}
	if tag.RowsAffected() != 0 {
		r.fail("sql-features", title, fmt.Sprintf("ON CONFLICT DO NOTHING affected %d rows", tag.RowsAffected()))
		return
	}
	var hits int64
	if err := r.conn.QueryRow(ctx, `INSERT INTO `+features+` (slot, hits) VALUES ('a', 5)
			ON CONFLICT (slot) DO UPDATE SET hits = `+features+`.hits + excluded.hits
			RETURNING hits`).Scan(&hits); err != nil {
		r.failErr("sql-features", title, fmt.Errorf("upsert with excluded.*: %w", err))
		return
	}
	if hits != 5 {
		r.fail("sql-features", title, fmt.Sprintf("upsert with excluded.* produced hits=%d", hits))
		return
	}
	var count int64
	if err := r.conn.QueryRow(ctx, `SELECT count(*) FROM `+features+` WHERE id = ANY($1)`, []int64{newID}).Scan(&count); err != nil {
		r.failErr("sql-features", title, fmt.Errorf("= ANY(array): %w", err))
		return
	}
	if count != 1 {
		r.fail("sql-features", title, fmt.Sprintf("= ANY(array) matched %d rows, want 1", count))
		return
	}
	if !r.execAll(ctx, "sql-features", title,
		`SELECT setval(pg_get_serial_sequence('`+features+`', 'id'), 1000000)`) {
		return
	}
	if err := r.conn.QueryRow(ctx, `INSERT INTO `+features+` (slot) VALUES ('b') RETURNING id`).Scan(&newID); err != nil {
		r.failErr("sql-features", title, fmt.Errorf("identity after setval: %w", err))
		return
	}
	if newID != 1000001 {
		r.fail("sql-features", title, fmt.Sprintf("identity after setval produced id=%d, want 1000001", newID))
		return
	}
	if !r.execAll(ctx, "sql-features", title,
		`UPDATE `+features+` SET body = to_tsvector('simple', 'quarterly report attached')`) {
		return
	}
	if err := r.conn.QueryRow(ctx, `SELECT count(*) FROM `+features+` WHERE body @@ to_tsquery('simple', 'report')`).Scan(&count); err != nil {
		r.failErr("sql-features", title, fmt.Errorf("tsvector @@ tsquery: %w", err))
		return
	}
	if count < 1 {
		r.fail("sql-features", title, "tsvector @@ tsquery matched no rows")
		return
	}
	r.pass("sql-features", title, "")
}

func (r *runner) checkConnectionBudget(ctx context.Context) {
	var maxConnections string
	var roleLimit int
	var createDB bool
	if err := r.conn.QueryRow(ctx,
		`SELECT current_setting('max_connections'), rolconnlimit, rolcreatedb FROM pg_roles WHERE rolname = current_user`).
		Scan(&maxConnections, &roleLimit, &createDB); err != nil {
		r.info("connections", "Connection budget", "unavailable: "+redactSecrets(err.Error()))
		return
	}
	limit := "no per-role limit"
	if roleLimit >= 0 {
		limit = fmt.Sprintf("per-role limit %d", roleLimit)
	}
	r.info("connections", "Connection budget",
		fmt.Sprintf("max_connections=%s, %s, CREATEDB=%t", maxConnections, limit, createDB))
}

// LockForTest holds the run lock so tests in other packages can exercise the
// busy path. The returned function releases it.
func LockForTest() func() {
	runLock.Lock()
	return runLock.Unlock
}
