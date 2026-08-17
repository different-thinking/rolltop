// File overview: Per-user Google OAuth connections that later carry Gmail,
// Contacts, and Calendar access. Tokens are stored encrypted, never in plain
// text, so this table holds ciphertext columns rather than credentials.

package store

func userGoogleConnectionMigrationSet() migrationSet {
	return migrationSet{
		Scope:   "user",
		Version: UserSchemaVersion029,
		Label:   "user schema 029 google oauth connections",
		Statements: []string{
			`CREATE TABLE IF NOT EXISTS google_connections (
				id INTEGER PRIMARY KEY AUTOINCREMENT,
				user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
				-- The address is display data and can change; Google's subject
				-- claim is the account's stable identifier and therefore the
				-- key a reconnect matches on.
				google_email TEXT NOT NULL,
				google_subject TEXT NOT NULL,
				encrypted_refresh_token TEXT NOT NULL,
				encrypted_access_token TEXT NOT NULL DEFAULT '',
				access_token_expires_at INTEGER NOT NULL DEFAULT 0,
				granted_scopes TEXT NOT NULL DEFAULT '',
				status TEXT NOT NULL DEFAULT 'ok',
				status_detail TEXT NOT NULL DEFAULT '',
				created_at INTEGER NOT NULL,
				updated_at INTEGER NOT NULL,
				-- Also the lookup index for every read: connections are always
				-- fetched by tenant, and by subject when a reconnect reuses a
				-- row, so no separate index is needed on those columns.
				UNIQUE(user_id, google_subject)
			)`,
		},
	}
}
