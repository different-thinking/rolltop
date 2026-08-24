package store

import (
	"context"
	"testing"
)

// The Gmail endpoint is pinned rather than typed, so an outgoing server saved
// before submission moved to 587 keeps a port its owner cannot correct in the
// settings. Migration 0003 moves those rows, and only those: a host somebody
// entered themselves is theirs, whatever port it names.
func TestGmailSubmissionPortMigrationMovesOnlyThePinnedRows(t *testing.T) {
	db := mustOpenTestStore(t)
	ctx := context.Background()
	user, err := db.CreateUser(ctx, "owner@example.test", "Owner", "hash", true)
	if err != nil {
		t.Fatal(err)
	}

	// The rows are written with SQL rather than through CreateSMTPAccount:
	// what is under test is a statement against the schema, and the rows it
	// exists for were written by an older binary with rules of its own.
	insert := func(label, host string, port int, authType string, connectionID int64) int64 {
		t.Helper()
		var id int64
		err := db.DB().QueryRowContext(ctx, `INSERT INTO smtp_accounts
			(user_id, label, host, port, username, encrypted_password, use_tls, created_at, updated_at, auth_type, google_connection_id)
			VALUES ($1, $2, $3, $4, '', '', 1, 0, 0, $5, $6) RETURNING id`,
			user.ID, label, host, port, authType, connectionID).Scan(&id)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	pinned := insert("Google", "smtp.gmail.com", 465, AuthTypeGoogleOAuth, 1)
	typedHost := insert("Own server", "mail.example.test", 465, "password", 0)
	ownGmail := insert("Gmail with a password", "smtp.gmail.com", 465, "password", 0)
	alreadyMoved := insert("Google on 587", "smtp.gmail.com", 587, AuthTypeGoogleOAuth, 1)

	migration := migrationByVersion(t, "0003-gmail-submission-port")
	for _, statement := range migration.Statements {
		if _, err := db.DB().ExecContext(ctx, statement); err != nil {
			t.Fatalf("apply %s: %v", migration.Version, err)
		}
	}

	for _, want := range []struct {
		id   int64
		port int
		why  string
	}{
		{pinned, 587, "the pinned Google endpoint moves"},
		{typedHost, 465, "a host the user typed is left alone"},
		{ownGmail, 465, "Gmail reached with a password is the user's own setting"},
		{alreadyMoved, 587, "a row already on the new port is untouched"},
	} {
		var port int
		if err := db.DB().QueryRowContext(ctx, `SELECT port FROM smtp_accounts WHERE id = $1`, want.id).Scan(&port); err != nil {
			t.Fatal(err)
		}
		if port != want.port {
			t.Errorf("account %d is on port %d, want %d: %s", want.id, port, want.port, want.why)
		}
	}
}

func migrationByVersion(t *testing.T, version string) postgresMigration {
	t.Helper()
	for _, migration := range postgresMigrations {
		if migration.Version == version {
			return migration
		}
	}
	t.Fatalf("migration %q is not in the list", version)
	return postgresMigration{}
}
