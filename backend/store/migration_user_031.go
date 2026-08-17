// File overview: Contact provenance and Google People sync state. A contact
// gains a source, an optional link to the Google connection that owns it, and
// the remote identifiers a two-way sync needs; a companion table holds the sync
// token per connection.

package store

func userGoogleContactMigrationSet() migrationSet {
	return migrationSet{
		Scope:   "user",
		Version: UserSchemaVersion031,
		Label:   "user schema 031 google contacts",
		Statements: []string{
			// 'local' is the pre-existing state: every contact in an installed
			// database was typed in, imported from a vCard, or captured from a
			// sender. Only the sync promotes rows to 'google'.
			`ALTER TABLE contacts ADD COLUMN source TEXT NOT NULL DEFAULT 'local'`,
			// Plain integer, as in user-030: SQLite cannot add a column with a
			// REFERENCES clause unless the default is NULL. 0 means "not linked".
			`ALTER TABLE contacts ADD COLUMN google_connection_id INTEGER NOT NULL DEFAULT 0`,
			// The People API resource name, e.g. people/c1234567890123456789.
			`ALTER TABLE contacts ADD COLUMN external_id TEXT NOT NULL DEFAULT ''`,
			// Google's optimistic-concurrency token. Writing back without the
			// current etag either fails or silently clobbers a newer remote
			// edit, so it is stored alongside every synced contact.
			`ALTER TABLE contacts ADD COLUMN etag TEXT NOT NULL DEFAULT ''`,
			`ALTER TABLE contacts ADD COLUMN remote_updated_at INTEGER NOT NULL DEFAULT 0`,
			// Partial index: unlinked contacts all share the empty external id,
			// and there can be any number of those. It doubles as the lookup a
			// delta sync performs for every changed person.
			`CREATE UNIQUE INDEX IF NOT EXISTS idx_contacts_google_external
				ON contacts(user_id, google_connection_id, external_id)
				WHERE external_id <> ''`,
			// One row per connection. The sync token is Google's cursor, not a
			// credential, so unlike the tokens in google_connections it is
			// stored as it arrives.
			`CREATE TABLE IF NOT EXISTS google_people_sync (
				user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
				connection_id INTEGER NOT NULL,
				sync_token TEXT NOT NULL DEFAULT '',
				last_sync_at INTEGER NOT NULL DEFAULT 0,
				last_success_at INTEGER NOT NULL DEFAULT 0,
				status TEXT NOT NULL DEFAULT '',
				status_detail TEXT NOT NULL DEFAULT '',
				created_at INTEGER NOT NULL,
				updated_at INTEGER NOT NULL,
				PRIMARY KEY (user_id, connection_id)
			)`,
		},
	}
}
