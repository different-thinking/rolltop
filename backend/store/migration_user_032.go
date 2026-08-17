// File overview: Message categories. Every message carries one category decided
// from its own headers, and a per-sender override records the corrections the
// user makes so the same sender keeps landing where they put it.

package store

func userMessageCategoryMigrationSet() migrationSet {
	return migrationSet{
		Scope:   "user",
		Version: UserSchemaVersion032,
		Label:   "user schema 032 message categories",
		Statements: []string{
			// The empty default means "not classified yet" rather than a
			// category of its own: it is what the backfill selects on, and what
			// keeps every existing row out of the category lists until it has
			// actually been read.
			`ALTER TABLE messages ADD COLUMN category TEXT NOT NULL DEFAULT ''`,
			// The bare sender address, kept beside the display form the list
			// renders. A correction is remembered per sender, and matching it
			// against the display form would mean normalizing addresses in SQL
			// on every row instead of once when the message is stored.
			`ALTER TABLE messages ADD COLUMN sender_address TEXT NOT NULL DEFAULT ''`,
			`CREATE INDEX IF NOT EXISTS idx_messages_user_sender_address ON messages(user_id, sender_address)`,
			// The category lists page by date within one tenant's category, so
			// the index carries the sort column and the lists never fall back to
			// scanning every message the user owns.
			`CREATE INDEX IF NOT EXISTS idx_messages_user_category_date ON messages(user_id, category, date_unix)`,
			// The backfill's own lookup. A partial index stays small once the
			// backfill has drained, which is the state the app spends its life
			// in, and it costs nothing to maintain for already-classified rows.
			`CREATE INDEX IF NOT EXISTS idx_messages_category_pending ON messages(user_id, id) WHERE category = ''`,
			`CREATE TABLE IF NOT EXISTS category_sender_overrides (
				id INTEGER PRIMARY KEY AUTOINCREMENT,
				user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
				-- The bare, lowercased address. Display names change and differ
				-- between messages from the same sender, so they cannot be part
				-- of the key a correction is remembered under.
				sender TEXT NOT NULL,
				category TEXT NOT NULL,
				created_at INTEGER NOT NULL,
				updated_at INTEGER NOT NULL,
				UNIQUE(user_id, sender)
			)`,
		},
	}
}
