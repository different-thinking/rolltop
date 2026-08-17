// File overview: Per-account Sent folder choices backing the global Sent view.
// Sync already gives one folder per account the sent role, so this table only
// records a deliberate override; an account without a row keeps using its
// detected Sent folder.

package store

func userSentMailboxMigrationSet() migrationSet {
	return migrationSet{
		Scope:   "user",
		Version: UserSchemaVersion030,
		Label:   "user schema 030 sent mailboxes",
		Statements: []string{
			`CREATE TABLE IF NOT EXISTS sent_mailboxes (
				user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
				account_id INTEGER NOT NULL,
				mailbox_id INTEGER NOT NULL,
				created_at INTEGER NOT NULL,
				updated_at INTEGER NOT NULL,
				-- One Sent folder per account, and a folder cannot stand in for
				-- two accounts, mirroring how archive destinations are keyed.
				PRIMARY KEY(user_id, account_id),
				UNIQUE(user_id, mailbox_id),
				FOREIGN KEY(user_id, account_id) REFERENCES mail_accounts(user_id, id) ON DELETE CASCADE,
				FOREIGN KEY(user_id, account_id, mailbox_id) REFERENCES mailboxes(user_id, account_id, id) ON DELETE CASCADE
			)`,
			`CREATE INDEX IF NOT EXISTS idx_sent_mailboxes_user_mailbox
				ON sent_mailboxes(user_id, mailbox_id)`,
		},
	}
}
