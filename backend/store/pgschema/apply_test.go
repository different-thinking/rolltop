package pgschema_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// TestBaselineAppliesToPostgres is the check the golden-file test cannot make:
// that PostgreSQL actually accepts the generated baseline, and that what it
// creates has the properties the migration depends on. It needs a live server
// with CREATEDB rights (a CI-local container, never the hosted database — the
// application role there has CREATEDB=false).
func TestBaselineAppliesToPostgres(t *testing.T) {
	adminDSN := os.Getenv("TEST_DATABASE_URL")
	if adminDSN == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	baseline, err := os.ReadFile("baseline.sql")
	if err != nil {
		t.Fatal(err)
	}
	conn := connect(t, ctx, adminDSN)
	const name = "rolltop_baseline_probe"
	mustExec(t, ctx, conn, `DROP DATABASE IF EXISTS `+name)
	if _, err := conn.Exec(ctx, `CREATE DATABASE `+name); err != nil {
		t.Skipf("cannot create a probe database (needs CREATEDB): %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		_, _ = conn.Exec(cleanupCtx, `DROP DATABASE IF EXISTS `+name)
		_ = conn.Close(cleanupCtx)
	})

	probe := connectToDatabase(t, ctx, adminDSN, name)
	defer func() { _ = probe.Close(context.Background()) }()
	if _, err := probe.Exec(ctx, string(baseline)); err != nil {
		t.Fatalf("baseline does not apply: %v", err)
	}

	// Applying twice must fail rather than silently diverge: the baseline is
	// a create-from-empty script, not an idempotent migration.
	if _, err := probe.Exec(ctx, string(baseline)); err == nil {
		t.Error("re-applying the baseline succeeded; it is meant to require an empty database")
	}

	assertSchemaProperties(t, ctx, probe, string(baseline))
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

	if want := strings.Count(baseline, "\nCREATE TABLE "); tables != want {
		t.Errorf("created %d tables, baseline declares %d", tables, want)
	}
	if want := strings.Count(baseline, "ALTER TABLE "); foreignKeys != want {
		t.Errorf("created %d foreign keys, baseline declares %d", foreignKeys, want)
	}
	if triggers != 1 {
		t.Errorf("created %d triggers, want the one duplicate-pointer trigger", triggers)
	}
	if indexes == 0 {
		t.Error("no indexes were created")
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

	// The identity columns have to accept setval, because the migration tool
	// copies ids verbatim and then advances the sequences past them.
	var nextID int64
	mustScan(t, ctx, conn, &nextID, `SELECT setval(pg_get_serial_sequence('users', 'id'), 4242)`)
	if nextID != 4242 {
		t.Errorf("setval on users.id returned %d", nextID)
	}

	// The translated trigger has to behave like the SQLite original: deleting
	// a message that others point at clears their pointers.
	assertDuplicatePointerTrigger(t, ctx, conn)
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
}

func connect(t *testing.T, ctx context.Context, dsn string) *pgx.Conn {
	t.Helper()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Skipf("cannot reach TEST_DATABASE_URL: %v", err)
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

// connectToDatabase dials the same server as dsn but a different database.
//
// The config is passed to ConnectConfig rather than serialized back with
// ConnString(): that method returns the string the config was parsed from and
// ignores later field changes, so round-tripping silently connects to the
// original database. An earlier revision did exactly that and applied the
// baseline into the server's default database.
func connectToDatabase(t *testing.T, ctx context.Context, dsn, database string) *pgx.Conn {
	t.Helper()
	config, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	config.Database = database
	conn, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		t.Fatalf("connect to %s: %v", database, err)
	}
	var connected string
	if err := conn.QueryRow(ctx, `SELECT current_database()`).Scan(&connected); err != nil {
		t.Fatal(err)
	}
	if connected != database {
		t.Fatalf("connected to database %q, want %q", connected, database)
	}
	return conn
}
