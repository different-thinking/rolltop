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
func TestUser038IsLatestRegisteredUserMigration(t *testing.T) {
	sets := currentUserMigrationSetsForUpgradeTest()
	if len(sets) < 2 {
		t.Fatalf("registered user migrations=%d, want at least 2", len(sets))
	}
	if latest := sets[len(sets)-1]; latest.Version != UserSchemaVersion038 {
		t.Fatalf("latest user migration=%q, want %q", latest.Version, UserSchemaVersion038)
	}
	if predecessor := sets[len(sets)-2]; predecessor.Version != UserSchemaVersion037 {
		t.Fatalf("user-038 predecessor=%q, want %q", predecessor.Version, UserSchemaVersion037)
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

// The point of user-038: address rows now survive a contact save, so the order
// the user put them in has to be stored rather than read off the ids. Existing
// rows start at zero, which is exactly the order they already read back in.
func TestUser038AddsContactEmailOrderDefaultingToTheExistingOrder(t *testing.T) {
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
			email TEXT NOT NULL DEFAULT ''
		)`,
		`INSERT INTO contact_emails (user_id, contact_id, email) VALUES (1, 1, 'one@example.test')`,
		`INSERT INTO contact_emails (user_id, contact_id, email) VALUES (1, 1, 'two@example.test')`,
	} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatalf("fixture %q: %v", statement, err)
		}
	}
	for _, statement := range userContactEmailPositionMigrationSet().Statements {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatalf("apply %q: %v", statement, err)
		}
	}
	rows, err := db.QueryContext(ctx, `SELECT email FROM contact_emails WHERE user_id = 1 AND contact_id = 1 ORDER BY sort_order, id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var order []string
	for rows.Next() {
		var email string
		if err := rows.Scan(&email); err != nil {
			t.Fatal(err)
		}
		order = append(order, email)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(order) != 2 || order[0] != "one@example.test" || order[1] != "two@example.test" {
		t.Fatalf("order after the migration = %v, want the rows in their existing order", order)
	}
}
