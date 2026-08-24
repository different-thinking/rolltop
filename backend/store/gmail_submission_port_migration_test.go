package store

import (
	"context"
	"testing"
)

// The Gmail endpoint is written by the server rather than typed, so an account
// saved before submission moved to 587 carries a port its owner never chose.
// Migration 0003 moves those rows in both tables that hold a submission
// endpoint, and only those: a host somebody entered themselves is theirs,
// whatever port it names.
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

	// mail_accounts.smtp_* is the second place the endpoint lands: it is
	// copied into a new outgoing server when a user who has none saves a
	// mailbox, so a stale port there reaches sending later on.
	insertMailAccount := func(email, smtpHost string, smtpPort int, authType string) int64 {
		t.Helper()
		var id int64
		err := db.DB().QueryRowContext(ctx, `INSERT INTO mail_accounts
			(user_id, email, host, port, username, encrypted_password, use_tls, smtp_host, smtp_port, smtp_use_tls, created_at, updated_at, auth_type)
			VALUES ($1, $2, 'imap.gmail.com', 993, $2, '', 1, $3, $4, 1, 0, 0, $5) RETURNING id`,
			user.ID, email, smtpHost, smtpPort, authType).Scan(&id)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	pinnedIncoming := insertMailAccount("pinned@gmail.example.test", "smtp.gmail.com", 465, AuthTypeGoogleOAuth)
	passwordIncoming := insertMailAccount("own@gmail.example.test", "smtp.gmail.com", 465, "password")

	migration := migrationByVersion(t, "0003-gmail-submission-port")
	for _, statement := range migration.Statements {
		if _, err := db.DB().ExecContext(ctx, statement); err != nil {
			t.Fatalf("apply %s: %v", migration.Version, err)
		}
	}

	for _, want := range []struct {
		table string
		id    int64
		port  int
		why   string
	}{
		{"smtp_accounts", pinned, 587, "the pinned Google endpoint moves"},
		{"smtp_accounts", typedHost, 465, "a host the user typed is left alone"},
		{"smtp_accounts", ownGmail, 465, "Gmail reached with a password is the user's own setting"},
		{"smtp_accounts", alreadyMoved, 587, "a row already on the new port is untouched"},
		{"mail_accounts", pinnedIncoming, 587, "the endpoint a new outgoing server would be built from moves too"},
		{"mail_accounts", passwordIncoming, 465, "an incoming account with a password keeps what it was given"},
	} {
		column := "port"
		if want.table == "mail_accounts" {
			column = "smtp_port"
		}
		var port int
		query := "SELECT " + column + " FROM " + want.table + " WHERE id = $1"
		if err := db.DB().QueryRowContext(ctx, query, want.id).Scan(&port); err != nil {
			t.Fatal(err)
		}
		if port != want.port {
			t.Errorf("%s row %d is on port %d, want %d: %s", want.table, want.id, port, want.port, want.why)
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
