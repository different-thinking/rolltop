package store

import (
	"context"
	"database/sql"
	"testing"

	_ "github.com/mattn/go-sqlite3"
)

// The newest migration owns the whole-list invariants: it is the one that would
// break them, and asserting them from every migration's test would mean fixing
// the same thing in a dozen places the next time one is added.
func TestUser036IsLatestRegisteredUserMigration(t *testing.T) {
	sets := currentUserMigrationSetsForUpgradeTest()
	if len(sets) < 2 {
		t.Fatalf("registered user migrations=%d, want at least 2", len(sets))
	}
	if latest := sets[len(sets)-1]; latest.Version != UserSchemaVersion036 {
		t.Fatalf("latest user migration=%q, want %q", latest.Version, UserSchemaVersion036)
	}
	if predecessor := sets[len(sets)-2]; predecessor.Version != UserSchemaVersion035 {
		t.Fatalf("user-036 predecessor=%q, want %q", predecessor.Version, UserSchemaVersion035)
	}
	// Application order is not numeric — user-011 has always run before
	// user-004 — so only duplicates are worth asserting here. A version applied
	// twice would record one checksum over two different statement lists.
	seen := map[string]bool{}
	for _, set := range sets {
		if seen[set.Version] {
			t.Fatalf("user migration %q is registered twice", set.Version)
		}
		seen[set.Version] = true
	}
	// The legacy fixture applies a frozen prefix of this list to build a v21
	// database. If it ever stops being a prefix, the upgrade tests would be
	// migrating from a schema the app never actually shipped.
	legacy := legacyUserMigrationSetsThroughV21()
	if len(legacy) > len(sets) {
		t.Fatalf("legacy prefix has %d sets, more than the %d registered", len(legacy), len(sets))
	}
	for i, set := range legacy {
		if sets[i].Version != set.Version {
			t.Fatalf("registered migration %d = %q, want the legacy prefix entry %q", i, sets[i].Version, set.Version)
		}
	}
}

// The point of user-036: an address book may hold the same address twice. The
// index has to stay, because every lookup by address uses it, and it has to
// stop being unique, because the second holder is a person Google owns and
// Rolltop has to mirror.
func TestUser036LeavesTheContactAddressLookupWithoutAUniqueConstraint(t *testing.T) {
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
