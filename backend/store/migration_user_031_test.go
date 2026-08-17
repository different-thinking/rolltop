package store

import (
	"context"
	"database/sql"
	"testing"

	_ "github.com/mattn/go-sqlite3"
)

func applyUserGoogleContactMigration(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	for _, statement := range userGoogleContactMigrationSet().Statements {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatalf("apply %q: %v", statement, err)
		}
	}
}

func openContactMigrationFixture(t *testing.T) (*sql.DB, context.Context) {
	t.Helper()
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	db.SetMaxOpenConns(1)
	ctx := context.Background()
	for _, statement := range []string{
		`CREATE TABLE contacts (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			user_id INTEGER NOT NULL,
			display_name TEXT NOT NULL DEFAULT '',
			created_at INTEGER NOT NULL DEFAULT 0,
			updated_at INTEGER NOT NULL DEFAULT 0
		)`,
		`INSERT INTO contacts (user_id, display_name) VALUES (1, 'Existing Person')`,
	} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	return db, ctx
}

// Contacts that predate the migration were typed in by hand or imported, and a
// sync must never take them for Google's copy. That is decided entirely by the
// column defaults, which cannot be corrected without a second migration.
func TestGoogleContactMigrationLeavesExistingContactsLocal(t *testing.T) {
	db, ctx := openContactMigrationFixture(t)
	applyUserGoogleContactMigration(t, ctx, db)

	var source, externalID, etag string
	var connectionID, remoteUpdated int64
	if err := db.QueryRowContext(ctx,
		`SELECT source, google_connection_id, external_id, etag, remote_updated_at FROM contacts WHERE user_id = 1`).
		Scan(&source, &connectionID, &externalID, &etag, &remoteUpdated); err != nil {
		t.Fatal(err)
	}
	if source != "local" || connectionID != 0 || externalID != "" || etag != "" || remoteUpdated != 0 {
		t.Fatalf("existing contact migrated to source=%q connection=%d external=%q etag=%q updated=%d, want an unlinked local contact",
			source, connectionID, externalID, etag, remoteUpdated)
	}
}

// The unique index has to leave room for any number of unlinked contacts while
// still refusing a second copy of one Google person, which is what makes the
// delta sync's "find the row for this resource name" lookup unambiguous.
func TestGoogleContactMigrationIndexOnlyConstrainsLinkedContacts(t *testing.T) {
	db, ctx := openContactMigrationFixture(t)
	applyUserGoogleContactMigration(t, ctx, db)

	for i := 0; i < 3; i++ {
		if _, err := db.ExecContext(ctx,
			`INSERT INTO contacts (user_id, display_name) VALUES (1, 'Another Local')`); err != nil {
			t.Fatalf("unlinked contacts must not collide: %v", err)
		}
	}
	insertLinked := func(userID int64, connectionID int64, externalID string) error {
		_, err := db.ExecContext(ctx,
			`INSERT INTO contacts (user_id, display_name, source, google_connection_id, external_id)
				VALUES (?, 'Google Person', 'google', ?, ?)`, userID, connectionID, externalID)
		return err
	}
	if err := insertLinked(1, 7, "people/c1"); err != nil {
		t.Fatal(err)
	}
	if err := insertLinked(1, 7, "people/c1"); err == nil {
		t.Fatal("a second row for the same Google person was accepted")
	}
	// The same person may legitimately exist under a second connected account,
	// and must exist independently for a different tenant.
	if err := insertLinked(1, 8, "people/c1"); err != nil {
		t.Fatalf("same resource name under another connection: %v", err)
	}
	if err := insertLinked(2, 7, "people/c1"); err != nil {
		t.Fatalf("same resource name for another user: %v", err)
	}
}

// Registering a migration in migrate() but forgetting the upgrade test's list
// leaves the upgrade path untested precisely for the newest schema, which is the
// one most likely to be wrong.
func TestUser031IsLatestRegisteredUserMigration(t *testing.T) {
	sets := currentUserMigrationSetsForUpgradeTest()
	if len(sets) < 2 {
		t.Fatalf("registered user migrations=%d, want at least 2", len(sets))
	}
	if latest := sets[len(sets)-1]; latest.Version != UserSchemaVersion031 {
		t.Fatalf("latest user migration=%q, want %q", latest.Version, UserSchemaVersion031)
	}
	if predecessor := sets[len(sets)-2]; predecessor.Version != UserSchemaVersion030 {
		t.Fatalf("user-031 predecessor=%q, want %q", predecessor.Version, UserSchemaVersion030)
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
}
