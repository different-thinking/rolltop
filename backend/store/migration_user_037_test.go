package store

import (
	"context"
	"database/sql"
	"testing"

	_ "github.com/mattn/go-sqlite3"
)

// The point of user-037: an address book may hold the same address twice. The
// index has to stay, because every lookup by address uses it, and it has to
// stop being unique, because the second holder is a person Google owns and
// Rolltop has to mirror.
func TestUser037LeavesTheContactAddressLookupWithoutAUniqueConstraint(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	for _, statement := range []string{
		`CREATE TABLE contact_emails (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			user_id INTEGER NOT NULL,
			contact_id INTEGER NOT NULL,
			normalized_email TEXT NOT NULL DEFAULT ''
		)`,
		`CREATE UNIQUE INDEX idx_contact_emails_user_normalized ON contact_emails(user_id, normalized_email) WHERE normalized_email <> ''`,
		`INSERT INTO contact_emails (user_id, contact_id, normalized_email) VALUES (1, 1, 'haushalt@example.test')`,
	} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatalf("fixture %q: %v", statement, err)
		}
	}
	// The pre-migration state is the bug: the second holder cannot be stored.
	if _, err := db.ExecContext(ctx, `INSERT INTO contact_emails (user_id, contact_id, normalized_email) VALUES (1, 2, 'haushalt@example.test')`); err == nil {
		t.Fatal("fixture accepted a shared address before the migration, so it does not reproduce the state being fixed")
	}
	for _, statement := range userSharedContactEmailMigrationSet().Statements {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatalf("apply %q: %v", statement, err)
		}
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO contact_emails (user_id, contact_id, normalized_email) VALUES (1, 2, 'haushalt@example.test')`); err != nil {
		t.Fatalf("shared address after the migration: %v", err)
	}
	var indexes int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM sqlite_master WHERE type = 'index' AND tbl_name = 'contact_emails' AND name = 'idx_contact_emails_user_address'`).
		Scan(&indexes); err != nil {
		t.Fatal(err)
	}
	if indexes != 1 {
		t.Fatal("the address lookup index is gone, so every lookup by address now scans the table")
	}
}
