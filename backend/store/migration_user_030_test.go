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

// A migration that ships after its successor would apply its ALTERs to a table
// that already has the columns, so the recorded order matters as much as the
// statements do.
func TestUser030RunsAfterUser029(t *testing.T) {
	sets := currentUserMigrationSetsForUpgradeTest()
	for i, set := range sets {
		if set.Version != UserSchemaVersion030 {
			continue
		}
		if i == 0 || sets[i-1].Version != UserSchemaVersion029 {
			t.Fatalf("user-030 predecessor=%v, want %q", sets[max(0, i-1):i], UserSchemaVersion029)
		}
		return
	}
	t.Fatalf("%s missing from current user migrations", UserSchemaVersion030)
}
