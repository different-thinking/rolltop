// File overview: Take Sent out of the whole-account lists. A folder's All Mail
// flag decides whether All Mail, Inbox and every category view draw from it, and
// Sent shipped with it on, so the user's own writing came back at them in all
// three at once.

package store

func userSentMailboxAllMailMigrationSet() migrationSet {
	return migrationSet{
		Scope:   "user",
		Version: UserSchemaVersion036,
		Label:   "user schema 036 sent folder out of all mail",
		Statements: []string{
			// A one-time backfill rather than a startup seed step: those run on
			// every boot, and re-forcing the flag there would mean a reader who
			// deliberately switches Sent back into All Mail loses that choice at
			// the next restart. The migration's own applied-version record is
			// what keeps this to a single pass.
			`UPDATE mailboxes
				SET show_in_all_mail = 0,
					updated_at = CAST(strftime('%s', 'now') AS INTEGER)
				WHERE role = 'sent' AND show_in_all_mail = 1`,
		},
	}
}
