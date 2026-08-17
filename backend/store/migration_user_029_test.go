package store

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
)

func TestGoogleConnectionMigrationScopesConnectionsPerTenant(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, `CREATE TABLE users (id INTEGER PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	for _, statement := range userGoogleConnectionMigrationSet().Statements {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO users(id) VALUES (1), (2)`); err != nil {
		t.Fatal(err)
	}
	insert := `INSERT INTO google_connections
		(user_id, google_email, google_subject, encrypted_refresh_token, created_at, updated_at)
		VALUES (?, ?, ?, ?, 1, 1)`
	if _, err := db.ExecContext(ctx, insert, 1, "shared@gmail.example.test", "subject-1", "v1:a:b"); err != nil {
		t.Fatal(err)
	}
	// The same Google account may be connected by two different Rolltop users;
	// uniqueness is per tenant, not global.
	if _, err := db.ExecContext(ctx, insert, 2, "shared@gmail.example.test", "subject-1", "v1:c:d"); err != nil {
		t.Fatalf("second tenant connecting the same Google account: %v", err)
	}
	if _, err := db.ExecContext(ctx, insert, 1, "other@gmail.example.test", "subject-1", "v1:e:f"); err == nil {
		t.Fatal("duplicate subject for one tenant was accepted, want unique constraint failure")
	}
	// The address is not the identity: one tenant may hold two connections that
	// currently report the same address but are different Google accounts.
	if _, err := db.ExecContext(ctx, insert, 1, "shared@gmail.example.test", "subject-2", "v1:g:h"); err != nil {
		t.Fatalf("same address under a different subject was rejected: %v", err)
	}

	var status, detail, scopes, accessToken string
	var expiresAt int64
	if err := db.QueryRowContext(ctx, `SELECT status, status_detail, granted_scopes,
		encrypted_access_token, access_token_expires_at
		FROM google_connections WHERE user_id = 1 AND google_subject = 'subject-1'`).
		Scan(&status, &detail, &scopes, &accessToken, &expiresAt); err != nil {
		t.Fatal(err)
	}
	if status != "ok" {
		t.Fatalf("default status=%q, want %q", status, "ok")
	}
	if detail != "" || scopes != "" || accessToken != "" || expiresAt != 0 {
		t.Fatalf("defaults not empty: detail=%q scopes=%q accessToken=%q expiresAt=%d",
			detail, scopes, accessToken, expiresAt)
	}

	var definition string
	if err := db.QueryRowContext(ctx, `SELECT sql FROM sqlite_master
		WHERE type = 'table' AND name = 'google_connections'`).Scan(&definition); err != nil {
		t.Fatal(err)
	}
	normalized := strings.ToLower(strings.Join(strings.Fields(definition), " "))
	// Tokens must only ever reach SQLite as ciphertext, so no column may invite
	// a plain-text credential.
	for _, forbidden := range []string{" refresh_token text", " access_token text", "password"} {
		if strings.Contains(normalized, forbidden) {
			t.Fatalf("google_connections exposes a plain-text credential column %q: %s", forbidden, definition)
		}
	}
	if !strings.Contains(normalized, "on delete cascade") {
		t.Fatalf("google_connections is not cascade-deleted with its user: %s", definition)
	}
	// The DDL text alone proves nothing without enforcement, so delete a user
	// and check the tokens really go with them.
	if _, err := db.ExecContext(ctx, `PRAGMA foreign_keys = ON`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM users WHERE id = 2`); err != nil {
		t.Fatal(err)
	}
	var remaining int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM google_connections WHERE user_id = 2`).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 0 {
		t.Fatalf("connections left after deleting their user = %d, want 0", remaining)
	}
}

func TestUser031IsLatestRegisteredUserMigration(t *testing.T) {
	sets := currentUserMigrationSetsForUpgradeTest()
	if len(sets) < 2 {
		t.Fatalf("registered user migrations=%d, want at least 2", len(sets))
	}
	latest := sets[len(sets)-1]
	predecessor := sets[len(sets)-2]
	if latest.Version != UserSchemaVersion031 {
		t.Fatalf("latest user migration=%q, want %q", latest.Version, UserSchemaVersion031)
	}
	// user-030 was withdrawn before release, so 029 is 031's real predecessor.
	if predecessor.Version != UserSchemaVersion029 {
		t.Fatalf("user-031 predecessor=%q, want %q", predecessor.Version, UserSchemaVersion029)
	}
}
