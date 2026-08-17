package store

import (
	"context"
	"database/sql"
	"testing"

	_ "github.com/mattn/go-sqlite3"
)

// The defaults decide what happens to every account that already exists, which
// is the part of an ALTER that cannot be corrected later without a second
// migration, so they are asserted against real rows rather than read off the DDL.
func TestGoogleMailAccountMigrationLeavesExistingAccountsUntouched(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	ctx := context.Background()
	for _, statement := range []string{
		`CREATE TABLE mail_accounts (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			user_id INTEGER NOT NULL,
			email TEXT NOT NULL,
			encrypted_password TEXT NOT NULL DEFAULT ''
		)`,
		`CREATE TABLE smtp_accounts (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			user_id INTEGER NOT NULL,
			encrypted_password TEXT NOT NULL DEFAULT ''
		)`,
		`INSERT INTO mail_accounts (user_id, email, encrypted_password) VALUES (1, 'old@example.test', 'v1:a:b')`,
		`INSERT INTO smtp_accounts (user_id, encrypted_password) VALUES (1, 'v1:c:d')`,
	} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	for _, statement := range userGoogleMailAccountMigrationSet().Statements {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}

	var authType string
	var connectionID, syncStart int64
	if err := db.QueryRowContext(ctx,
		`SELECT auth_type, google_connection_id, sync_start_at FROM mail_accounts WHERE user_id = 1`).
		Scan(&authType, &connectionID, &syncStart); err != nil {
		t.Fatal(err)
	}
	if authType != "password" || connectionID != 0 {
		t.Fatalf("existing mail account migrated to auth_type=%q connection=%d, want password/0", authType, connectionID)
	}
	// A cutoff applied retroactively would orphan mail that has already been
	// mirrored and paid for, so existing accounts must keep fetching everything.
	if syncStart != 0 {
		t.Fatalf("existing mail account received sync_start_at=%d, want 0", syncStart)
	}
	if err := db.QueryRowContext(ctx,
		`SELECT auth_type, google_connection_id FROM smtp_accounts WHERE user_id = 1`).
		Scan(&authType, &connectionID); err != nil {
		t.Fatal(err)
	}
	if authType != "password" || connectionID != 0 {
		t.Fatalf("existing SMTP account migrated to auth_type=%q connection=%d, want password/0", authType, connectionID)
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
