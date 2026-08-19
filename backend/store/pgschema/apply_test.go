package pgschema_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"rolltop/backend/pgdsn"
	"rolltop/backend/store/pgschema"
	"rolltop/backend/store/pgtestdb"
)

// TestBaselineAppliesToPostgres is the check the golden-file test cannot make:
// that PostgreSQL actually accepts the generated baseline, and that what it
// creates has the properties the migration depends on. It needs a live server
// with CREATEDB rights (a CI-local container, never the hosted database — the
// application role there has CREATEDB=false), which pgtestdb provides: it skips
// when TEST_DATABASE_URL is unset and fails when the server cannot serve, so a
// misconfigured CI service cannot leave this merge gate green with its
// protection silently gone.
func TestBaselineAppliesToPostgres(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	probe := connect(t, ctx, pgtestdb.New(t))
	defer func() { _ = probe.Close(context.Background()) }()
	if _, err := probe.Exec(ctx, pgschema.Baseline); err != nil {
		t.Fatalf("baseline does not apply: %v", err)
	}

	// Applying twice must fail rather than silently diverge: the baseline is
	// a create-from-empty script, not an idempotent migration.
	if _, err := probe.Exec(ctx, pgschema.Baseline); err == nil {
		t.Error("re-applying the baseline succeeded; it is meant to require an empty database")
	}

	assertSchemaProperties(t, ctx, probe, pgschema.Baseline)
}

func assertSchemaProperties(t *testing.T, ctx context.Context, conn *pgx.Conn, baseline string) {
	t.Helper()

	var tables, indexes, foreignKeys, triggers int
	mustScan(t, ctx, conn, &tables,
		`SELECT count(*) FROM information_schema.tables WHERE table_schema = 'public' AND table_type = 'BASE TABLE'`)
	mustScan(t, ctx, conn, &indexes,
		`SELECT count(*) FROM pg_indexes WHERE schemaname = 'public'`)
	mustScan(t, ctx, conn, &foreignKeys,
		`SELECT count(*) FROM pg_constraint WHERE contype = 'f'`)
	mustScan(t, ctx, conn, &triggers,
		`SELECT count(*) FROM pg_trigger WHERE NOT tgisinternal`)

	// Expected counts come from the object headers Render emits, not from
	// counting substrings in the SQL: a comment or trigger body containing
	// "ALTER TABLE " would otherwise read as a schema bug, and a second
	// deliberate trigger would need an edit in this file too.
	declared := countDeclaredObjects(baseline)
	if want := declared[pgschema.TableKind]; tables != want {
		t.Errorf("created %d tables, baseline declares %d", tables, want)
	}
	if want := declared[pgschema.TriggerKind]; triggers != want {
		t.Errorf("created %d triggers, baseline declares %d", triggers, want)
	}
	if declared[pgschema.ForeignKeysKind] == 0 || foreignKeys == 0 {
		t.Errorf("no foreign keys created (%d) or declared (%d)", foreignKeys, declared[pgschema.ForeignKeysKind])
	}
	if want := declared[pgschema.IndexKind]; indexes < want {
		t.Errorf("created %d indexes, baseline declares at least %d", indexes, want)
	}
	// The plugin tables are the ones an earlier revision omitted entirely,
	// because the derivation used a store without the file-backed plugin
	// migrations. Assert they are really here, in the database.
	var pluginTables int
	mustScan(t, ctx, conn, &pluginTables,
		`SELECT count(*) FROM information_schema.tables
		 WHERE table_schema = 'public' AND table_name LIKE 'plugin\_%'`)
	if pluginTables == 0 {
		t.Error("no plugin tables in the applied schema")
	}

	// Every text column must be C-collated, which is what keeps ORDER BY
	// byte-wise under the hosted cluster's en_US locale (plan §13).
	var uncollated []string
	rows, err := conn.Query(ctx, `
		SELECT table_name || '.' || column_name
		FROM information_schema.columns
		WHERE table_schema = 'public' AND data_type = 'text'
		  AND (collation_name IS DISTINCT FROM 'C')
		ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var column string
		if err := rows.Scan(&column); err != nil {
			t.Fatal(err)
		}
		uncollated = append(uncollated, column)
	}
	if len(uncollated) > 0 {
		t.Errorf("text columns without COLLATE \"C\": %s", strings.Join(uncollated, ", "))
	}

	// The identity columns keep SQLite's permissive semantics: an explicit id
	// on insert and setval on the sequence both have to work. The explicit-id
	// insert is the half GENERATED ALWAYS would reject; asserting only setval
	// let that through review once already.
	assertVerbatimIDInsert(t, ctx, conn)
	var nextID int64
	mustScan(t, ctx, conn, &nextID, `SELECT setval(pg_get_serial_sequence('users', 'id'), 4242)`)
	if nextID != 4242 {
		t.Errorf("setval on users.id returned %d", nextID)
	}

	// The translated trigger has to behave like the SQLite original: deleting
	// a message that others point at clears their pointers.
	assertDuplicatePointerTrigger(t, ctx, conn)
}

// countDeclaredObjects counts the "-- <kind> <name>" headers Render writes,
// which is the structural record of what the baseline contains.
func countDeclaredObjects(baseline string) map[string]int {
	counts := map[string]int{}
	for _, line := range strings.Split(baseline, "\n") {
		if !strings.HasPrefix(line, "-- ") {
			continue
		}
		for _, kind := range []string{pgschema.ForeignKeysKind, pgschema.TableKind, pgschema.IndexKind, pgschema.TriggerKind} {
			if strings.HasPrefix(line, "-- "+kind+" ") {
				counts[kind]++
				break
			}
		}
	}
	return counts
}

// assertVerbatimIDInsert pins the identity form the baseline declares:
// GENERATED BY DEFAULT accepts an explicit id, GENERATED ALWAYS refuses it
// with SQLSTATE 428C9.
func assertVerbatimIDInsert(t *testing.T, ctx context.Context, conn *pgx.Conn) {
	t.Helper()
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var id int64
	if err := tx.QueryRow(ctx, `INSERT INTO users (id, email, name, password_hash, created_at, updated_at)
		VALUES (987654, 'verbatim@example.test', 'Verbatim', 'hash', 0, 0) RETURNING id`).Scan(&id); err != nil {
		t.Fatalf("identity column rejects an explicit id: %v", err)
	}
	if id != 987654 {
		t.Errorf("inserted id %d, want the value supplied verbatim", id)
	}
}

// assertDuplicatePointerTrigger exercises the one hand-translated object in
// the baseline. A mistranslated data-repair trigger would leave dangling
// duplicate pointers that nothing else notices.
func assertDuplicatePointerTrigger(t *testing.T, ctx context.Context, conn *pgx.Conn) {
	t.Helper()
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var userID, accountID, mailboxID, blobID int64
	mustScanTx(t, ctx, tx, &userID, `INSERT INTO users (email, name, password_hash, created_at, updated_at)
		VALUES ('trigger@example.test', 'Trigger', 'hash', 0, 0) RETURNING id`)
	mustScanTx(t, ctx, tx, &accountID, `INSERT INTO mail_accounts (user_id, email, host, port, username, encrypted_password, created_at, updated_at)
		VALUES ($1, 'a@example.test', 'imap.example.test', 993, 'u', 'p', 0, 0) RETURNING id`, userID)
	mustScanTx(t, ctx, tx, &mailboxID, `INSERT INTO mailboxes (user_id, account_id, name, created_at, updated_at)
		VALUES ($1, $2, 'INBOX', 0, 0) RETURNING id`, userID, accountID)
	mustScanTx(t, ctx, tx, &blobID, `INSERT INTO blobs (user_id, kind, path, sha256, size, created_at)
		VALUES ($1, 'raw', 'blob', 'sha', 1, 0) RETURNING id`, userID)

	insertMessage := `INSERT INTO messages (user_id, account_id, mailbox_id, blob_id, uid, blob_path, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, '', 0, 0) RETURNING id`
	var originalID, duplicateID int64
	mustScanTx(t, ctx, tx, &originalID, insertMessage, userID, accountID, mailboxID, blobID, 1)
	mustScanTx(t, ctx, tx, &duplicateID, insertMessage, userID, accountID, mailboxID, blobID, 2)
	if _, err := tx.Exec(ctx, `UPDATE messages SET duplicate_of_message_id = $1 WHERE id = $2`, originalID, duplicateID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM messages WHERE id = $1`, originalID); err != nil {
		t.Fatal(err)
	}
	var pointer int64
	if err := tx.QueryRow(ctx, `SELECT duplicate_of_message_id FROM messages WHERE id = $1`, duplicateID).Scan(&pointer); err != nil {
		t.Fatal(err)
	}
	if pointer != 0 {
		t.Errorf("duplicate pointer = %d after the original was deleted, want 0", pointer)
	}

	// The translation is statement-level with a transition table, so a
	// multi-row DELETE has to clear every affected pointer in one pass — the
	// case a row-level port would handle by running one UPDATE per row.
	var firstID, secondID, pointingAtFirst, pointingAtSecond int64
	mustScanTx(t, ctx, tx, &firstID, insertMessage, userID, accountID, mailboxID, blobID, 10)
	mustScanTx(t, ctx, tx, &secondID, insertMessage, userID, accountID, mailboxID, blobID, 11)
	mustScanTx(t, ctx, tx, &pointingAtFirst, insertMessage, userID, accountID, mailboxID, blobID, 12)
	mustScanTx(t, ctx, tx, &pointingAtSecond, insertMessage, userID, accountID, mailboxID, blobID, 13)
	if _, err := tx.Exec(ctx, `UPDATE messages SET duplicate_of_message_id = $1 WHERE id = $2`, firstID, pointingAtFirst); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE messages SET duplicate_of_message_id = $1 WHERE id = $2`, secondID, pointingAtSecond); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM messages WHERE id = ANY($1)`, []int64{firstID, secondID}); err != nil {
		t.Fatal(err)
	}
	var remaining int64
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM messages WHERE duplicate_of_message_id <> 0`).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 0 {
		t.Errorf("%d duplicate pointers survived a multi-row delete", remaining)
	}
}

// connect fails rather than skips. pgtestdb has already established that the
// server is reachable, so a failure here is the probe database being unusable,
// which is a broken environment rather than an absent one.
func connect(t *testing.T, ctx context.Context, dsn string) *pgx.Conn {
	t.Helper()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect to the probe database: %v", pgdsn.Redact(err.Error()))
	}
	return conn
}

func mustExec(t *testing.T, ctx context.Context, conn *pgx.Conn, sql string) {
	t.Helper()
	if _, err := conn.Exec(ctx, sql); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

func mustScan(t *testing.T, ctx context.Context, conn *pgx.Conn, dest any, sql string) {
	t.Helper()
	if err := conn.QueryRow(ctx, sql).Scan(dest); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

func mustScanTx(t *testing.T, ctx context.Context, tx pgx.Tx, dest any, sql string, args ...any) {
	t.Helper()
	if err := tx.QueryRow(ctx, sql, args...).Scan(dest); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}
