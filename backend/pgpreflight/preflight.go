// File overview: PostgreSQL migration preflight, the Go twin of
// scripts/pg-preflight.sql. It connects to a candidate target database and
// verifies every capability docs/postgres-migration-plan.md assumes: server
// version and encoding, deterministic default collation, column- and
// index-level COLLATE "C" byte ordering, the trusted extensions, UTF-8
// strictness, and the SQL features the ported queries rely on. Run from the
// app container it also measures the round-trip latency of the real
// app-to-database path, which a bastion session cannot. The DSN is used for
// this one connection and never stored or logged; results carry no
// credentials.

package pgpreflight

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// scratchSchema must match scripts/pg-preflight.sql so a half-finished run of
// either tool is cleaned up by the next run of the other.
const scratchSchema = "preflight_scratch"

// Check is one verified capability. Status is "pass", "fail", or "info";
// info rows report facts that need a human judgement rather than having a
// hard pass condition.
type Check struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	Status string `json:"status"`
	Detail string `json:"detail"`
}

// Report is the outcome of one preflight run. OK is true only when no check
// failed. Duration covers the whole run including connect.
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
	r.checks = append(r.checks, Check{ID: id, Title: title, Status: "pass", Detail: detail})
}

func (r *runner) fail(id, title string, err error) {
	r.failed = true
	detail := ""
	if err != nil {
		detail = err.Error()
	}
	r.checks = append(r.checks, Check{ID: id, Title: title, Status: "fail", Detail: detail})
}

func (r *runner) info(id, title, detail string) {
	r.checks = append(r.checks, Check{ID: id, Title: title, Status: "info", Detail: detail})
}

// Run executes the preflight against dsn. It never panics and always returns
// a report; a connection failure is itself a reported check. The only
// persistent effects on the target are the three CREATE EXTENSION calls,
// which the migration wants anyway — the scratch schema is dropped at the
// end.
func Run(ctx context.Context, dsn string) Report {
	started := time.Now()
	r := &runner{}
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		// The driver echoes the failing host but not the password; the DSN
		// itself must not appear here in case it embeds one.
		r.fail("connect", "Connect to the database", errors.New(sanitizeConnectError(err)))
		return r.report(started)
	}
	r.conn = conn
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = conn.Exec(cleanupCtx, `DROP SCHEMA IF EXISTS `+scratchSchema+` CASCADE`)
		_ = conn.Close(cleanupCtx)
	}()
	r.pass("connect", "Connect to the database", "")

	r.checkLatency(ctx)
	r.checkServer(ctx)
	r.checkDeterministicCollation(ctx)
	if r.checkScratchSchema(ctx) {
		r.checkCollateC(ctx)
		r.checkExtensions(ctx)
		r.checkUTF8Strictness(ctx)
		r.checkSQLFeatures(ctx)
	}
	r.checkConnectionBudget(ctx)
	return r.report(started)
}

func (r *runner) report(started time.Time) Report {
	return Report{OK: !r.failed, Checks: r.checks, DurationMS: time.Since(started).Milliseconds()}
}

// sanitizeConnectError keeps driver messages but strips anything that looks
// like a keyword/value DSN echoed back, so a password never reaches the
// browser or a log line.
func sanitizeConnectError(err error) string {
	message := err.Error()
	if idx := strings.Index(message, "password="); idx >= 0 {
		message = message[:idx] + "password=…"
	}
	return message
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
			r.fail("latency", "Round-trip latency", err)
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
	var versionNum int
	var version, encoding string
	if err := r.conn.QueryRow(ctx,
		`SELECT current_setting('server_version_num')::int, current_setting('server_version'), current_setting('server_encoding')`).
		Scan(&versionNum, &version, &encoding); err != nil {
		r.fail("server", "Server version and encoding", err)
		return
	}
	if versionNum < 160000 {
		r.fail("version", "Server version >= 16", fmt.Errorf("server version %s is below 16", version))
	} else {
		r.pass("version", "Server version >= 16", version)
	}
	if encoding != "UTF8" {
		r.fail("encoding", "Server encoding is UTF8", fmt.Errorf("server encoding is %s", encoding))
	} else {
		r.pass("encoding", "Server encoding is UTF8", "")
	}
	var datname, collate, ctype string
	if err := r.conn.QueryRow(ctx,
		`SELECT datname, datcollate, datctype FROM pg_database WHERE datname = current_database()`).
		Scan(&datname, &collate, &ctype); err == nil {
		r.info("locale", "Database locale", fmt.Sprintf("%s: LC_COLLATE=%s LC_CTYPE=%s", datname, collate, ctype))
	}
}

// checkDeterministicCollation verifies the property the migration plan's
// §3.4 rests on: under a deterministic default collation, text equality is
// byte-exact regardless of locale, which keeps every COLLATE BINARY
// comparison site correct without per-query changes.
func (r *runner) checkDeterministicCollation(ctx context.Context) {
	const title = "Default collation is deterministic (equality is byte-exact)"
	var deterministic bool
	if err := r.conn.QueryRow(ctx,
		`SELECT c.collisdeterministic FROM pg_collation c WHERE c.collname = 'default'`).
		Scan(&deterministic); err != nil {
		r.fail("collation-deterministic", title, err)
		return
	}
	if !deterministic {
		r.fail("collation-deterministic", title, errors.New("database default collation is not deterministic"))
		return
	}
	var caseFold, ligature, padding bool
	if err := r.conn.QueryRow(ctx, `SELECT 'a' = 'A', 'ss' = 'ß', 'abc' = 'abc '`).
		Scan(&caseFold, &ligature, &padding); err != nil {
		r.fail("collation-deterministic", title, err)
		return
	}
	if caseFold || ligature || padding {
		r.fail("collation-deterministic", title, errors.New("text equality is not byte-exact under the default collation"))
		return
	}
	r.pass("collation-deterministic", title, "")
}

func (r *runner) checkScratchSchema(ctx context.Context) bool {
	const title = "Privileges: create schema, tables, indexes"
	if _, err := r.conn.Exec(ctx, `DROP SCHEMA IF EXISTS `+scratchSchema+` CASCADE`); err != nil {
		r.fail("privileges", title, err)
		return false
	}
	if _, err := r.conn.Exec(ctx, `CREATE SCHEMA `+scratchSchema); err != nil {
		r.fail("privileges", title, err)
		return false
	}
	r.pass("privileges", title, "")
	return true
}

func (r *runner) checkCollateC(ctx context.Context) {
	const title = `Column- and index-level COLLATE "C"`
	statements := []string{
		`CREATE TABLE ` + scratchSchema + `.collate_c (key text COLLATE "C" NOT NULL)`,
		`CREATE INDEX ON ` + scratchSchema + `.collate_c (key)`,
		`CREATE TABLE ` + scratchSchema + `.collate_default (key text NOT NULL)`,
		`CREATE INDEX ON ` + scratchSchema + `.collate_default (key COLLATE "C")`,
		`INSERT INTO ` + scratchSchema + `.collate_c VALUES ('a'), ('B'), ('Z'), ('ä')`,
		`INSERT INTO ` + scratchSchema + `.collate_default SELECT key FROM ` + scratchSchema + `.collate_c`,
	}
	for _, statement := range statements {
		if _, err := r.conn.Exec(ctx, statement); err != nil {
			r.fail("collate-c", title, err)
			return
		}
	}
	// Byte order of the UTF-8 encodings: B (0x42) < Z (0x5A) < a (0x61) < ä (0xC3A4).
	const wantByteOrder = "B,Z,a,ä"
	var columnOrder, queryOrder, defaultOrder string
	if err := r.conn.QueryRow(ctx,
		`SELECT string_agg(key, ',' ORDER BY key) FROM `+scratchSchema+`.collate_c`).
		Scan(&columnOrder); err != nil {
		r.fail("collate-c", title, err)
		return
	}
	if columnOrder != wantByteOrder {
		r.fail("collate-c", title, fmt.Errorf(`column declared COLLATE "C" sorts as %s, want %s`, columnOrder, wantByteOrder))
		return
	}
	if err := r.conn.QueryRow(ctx,
		`SELECT string_agg(key, ',' ORDER BY key COLLATE "C") FROM `+scratchSchema+`.collate_default`).
		Scan(&queryOrder); err != nil {
		r.fail("collate-c", title, err)
		return
	}
	if queryOrder != wantByteOrder {
		r.fail("collate-c", title, fmt.Errorf(`per-query COLLATE "C" sorts as %s, want %s`, queryOrder, wantByteOrder))
		return
	}
	r.pass("collate-c", title, "")
	if err := r.conn.QueryRow(ctx,
		`SELECT string_agg(key, ',' ORDER BY key) FROM `+scratchSchema+`.collate_default`).
		Scan(&defaultOrder); err == nil {
		r.info("collate-default", "Sort order under the database default collation",
			fmt.Sprintf("%s (byte order would be %s)", defaultOrder, wantByteOrder))
	}
}

func (r *runner) checkExtensions(ctx context.Context) {
	const title = "Trusted extensions: pg_trgm, citext, unaccent"
	for _, extension := range []string{"pg_trgm", "citext", "unaccent"} {
		if _, err := r.conn.Exec(ctx, `CREATE EXTENSION IF NOT EXISTS `+extension+` WITH SCHEMA public`); err != nil {
			r.fail("extensions", title, fmt.Errorf("%s: %w", extension, err))
			return
		}
	}
	var similarity float64
	var unaccented string
	if err := r.conn.QueryRow(ctx,
		`SELECT public.similarity('rolltop', 'roltop'), public.unaccent('Müller')`).
		Scan(&similarity, &unaccented); err != nil {
		r.fail("extensions", title, err)
		return
	}
	if similarity <= 0 || unaccented != "Muller" {
		r.fail("extensions", title, fmt.Errorf("similarity=%v unaccent=%q", similarity, unaccented))
		return
	}
	r.pass("extensions", title, "")
}

// checkUTF8Strictness documents that the target rejects what SQLite accepted,
// which is why the migration plan requires write-path sanitization (§8.1).
// Both statements are expected to error.
func (r *runner) checkUTF8Strictness(ctx context.Context) {
	var out string
	if err := r.conn.QueryRow(ctx, `SELECT chr(0)`).Scan(&out); err == nil {
		r.fail("utf8-nul", "NUL character is rejected", errors.New("NUL character was accepted in text"))
	} else {
		r.pass("utf8-nul", "NUL character is rejected", "")
	}
	if err := r.conn.QueryRow(ctx, `SELECT convert_from('\xff'::bytea, 'UTF8')`).Scan(&out); err == nil {
		r.fail("utf8-invalid", "Invalid UTF-8 is rejected", errors.New("invalid UTF-8 byte sequence was accepted"))
	} else {
		r.pass("utf8-invalid", "Invalid UTF-8 is rejected", "")
	}
}

func (r *runner) checkSQLFeatures(ctx context.Context) {
	const title = "SQL features: RETURNING, ON CONFLICT, excluded.*, = ANY, identity setval, tsvector/GIN"
	features := scratchSchema + ".features"
	setup := []string{
		`CREATE TABLE ` + features + ` (
			id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
			slot text COLLATE "C" NOT NULL UNIQUE,
			hits bigint NOT NULL DEFAULT 0,
			body tsvector
		)`,
		`CREATE INDEX ON ` + features + ` USING gin (body)`,
	}
	for _, statement := range setup {
		if _, err := r.conn.Exec(ctx, statement); err != nil {
			r.fail("sql-features", title, err)
			return
		}
	}
	var newID int64
	if err := r.conn.QueryRow(ctx, `INSERT INTO `+features+` (slot) VALUES ('a') RETURNING id`).Scan(&newID); err != nil {
		r.fail("sql-features", title, fmt.Errorf("INSERT RETURNING: %w", err))
		return
	}
	tag, err := r.conn.Exec(ctx, `INSERT INTO `+features+` (slot) VALUES ('a') ON CONFLICT DO NOTHING`)
	if err != nil {
		r.fail("sql-features", title, fmt.Errorf("ON CONFLICT DO NOTHING: %w", err))
		return
	}
	if tag.RowsAffected() != 0 {
		r.fail("sql-features", title, fmt.Errorf("ON CONFLICT DO NOTHING affected %d rows", tag.RowsAffected()))
		return
	}
	var hits int64
	if err := r.conn.QueryRow(ctx, `INSERT INTO `+features+` (slot, hits) VALUES ('a', 5)
			ON CONFLICT (slot) DO UPDATE SET hits = `+features+`.hits + excluded.hits
			RETURNING hits`).Scan(&hits); err != nil {
		r.fail("sql-features", title, fmt.Errorf("upsert with excluded.*: %w", err))
		return
	}
	if hits != 5 {
		r.fail("sql-features", title, fmt.Errorf("upsert with excluded.* produced hits=%d", hits))
		return
	}
	var count int64
	if err := r.conn.QueryRow(ctx, `SELECT count(*) FROM `+features+` WHERE id = ANY($1)`, []int64{newID}).Scan(&count); err != nil || count != 1 {
		r.fail("sql-features", title, fmt.Errorf("= ANY(array): count=%d err=%v", count, err))
		return
	}
	if _, err := r.conn.Exec(ctx, `SELECT setval(pg_get_serial_sequence('`+features+`', 'id'), 1000000)`); err != nil {
		r.fail("sql-features", title, fmt.Errorf("setval on identity: %w", err))
		return
	}
	if err := r.conn.QueryRow(ctx, `INSERT INTO `+features+` (slot) VALUES ('b') RETURNING id`).Scan(&newID); err != nil || newID != 1000001 {
		r.fail("sql-features", title, fmt.Errorf("identity after setval: id=%d err=%v", newID, err))
		return
	}
	if _, err := r.conn.Exec(ctx, `UPDATE `+features+` SET body = to_tsvector('simple', 'quarterly report attached')`); err != nil {
		r.fail("sql-features", title, fmt.Errorf("to_tsvector: %w", err))
		return
	}
	if err := r.conn.QueryRow(ctx, `SELECT count(*) FROM `+features+` WHERE body @@ to_tsquery('simple', 'report')`).Scan(&count); err != nil || count < 1 {
		r.fail("sql-features", title, fmt.Errorf("tsvector @@ tsquery: count=%d err=%v", count, err))
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
		r.info("connections", "Connection budget", "unavailable: "+err.Error())
		return
	}
	limit := "no per-role limit"
	if roleLimit >= 0 {
		limit = fmt.Sprintf("per-role limit %d", roleLimit)
	}
	r.info("connections", "Connection budget",
		fmt.Sprintf("max_connections=%s, %s, CREATEDB=%t", maxConnections, limit, createDB))
}
