// File overview: Cross-account duplicate copy suppression. A mailbox that
// aggregates other accounts (Gmail fetching POP3 mail, or a provider-side
// forward) hands the same message to two accounts, so the mirror holds two rows
// for one delivery. The pointer recorded here names the copy that stays
// visible, and a delete trigger keeps the pointer from outliving it.

package store

func userDuplicateCopyMigrationSet() migrationSet {
	return migrationSet{
		Scope:   "user",
		Version: UserSchemaVersion031,
		Label:   "user schema 031 cross-account duplicate copies",
		Statements: []string{
			// 0 means "visible"; any other value is the local id of the copy this
			// row duplicates. Existing rows start visible, so an upgrade never
			// hides mail before the first scan has looked at it.
			`ALTER TABLE messages ADD COLUMN duplicate_of_message_id INTEGER NOT NULL DEFAULT 0`,
			// Duplicate detection groups by Message-ID within one tenant. The
			// partial index keeps rows without the header out of the index, since
			// they can never join a group.
			`CREATE INDEX IF NOT EXISTS idx_messages_user_message_id_header
				ON messages(user_id, message_id_header, account_id)
				WHERE message_id_header <> ''`,
			`CREATE INDEX IF NOT EXISTS idx_messages_user_duplicate_of
				ON messages(user_id, duplicate_of_message_id)
				WHERE duplicate_of_message_id <> 0`,
			// Hiding a message behind a pointer is only safe while the pointer
			// resolves. Reconciliation, folder purges, account deletion, and
			// UIDVALIDITY rebuilds all delete message rows through different code
			// paths, and a stale pointer in any of them would hide mail that no
			// longer has a visible twin. The trigger states that invariant once,
			// in the one place every delete has to pass through.
			`CREATE TRIGGER IF NOT EXISTS messages_clear_duplicate_pointer
				AFTER DELETE ON messages
				FOR EACH ROW WHEN OLD.id IN (
					SELECT duplicate_of_message_id FROM messages
					WHERE user_id = OLD.user_id AND duplicate_of_message_id = OLD.id
				)
				BEGIN
					UPDATE messages SET duplicate_of_message_id = 0
					WHERE user_id = OLD.user_id AND duplicate_of_message_id = OLD.id;
				END`,
		},
	}
}
