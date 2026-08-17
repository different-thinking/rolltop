// File overview: OAuth authentication and a sync start date for mail accounts.
// Gmail accounts authenticate with a short-lived token from a google_connections
// row instead of a stored password, and a start date keeps the first sync of a
// decade-old mailbox from running for hours.

package store

func userGoogleMailAccountMigrationSet() migrationSet {
	return migrationSet{
		Scope:   "user",
		Version: UserSchemaVersion030,
		Label:   "user schema 030 google mail authentication",
		Statements: []string{
			// SQLite cannot add a column with a REFERENCES clause unless its
			// default is NULL, so the link is a plain integer and 0 means "no
			// connection". Deleting a connection therefore cannot cascade here;
			// the store resolves a missing connection into a reauthorization
			// prompt, which is the same state a revoked grant produces anyway.
			`ALTER TABLE mail_accounts ADD COLUMN auth_type TEXT NOT NULL DEFAULT 'password'`,
			`ALTER TABLE mail_accounts ADD COLUMN google_connection_id INTEGER NOT NULL DEFAULT 0`,
			// Unix seconds; 0 keeps the existing behaviour of mirroring whatever
			// the server offers. Existing accounts have already paid for their
			// initial sync, so they must not be given a cutoff retroactively.
			`ALTER TABLE mail_accounts ADD COLUMN sync_start_at INTEGER NOT NULL DEFAULT 0`,
			`ALTER TABLE smtp_accounts ADD COLUMN auth_type TEXT NOT NULL DEFAULT 'password'`,
			`ALTER TABLE smtp_accounts ADD COLUMN google_connection_id INTEGER NOT NULL DEFAULT 0`,
		},
	}
}
