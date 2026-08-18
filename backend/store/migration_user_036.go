// File overview: Contact email addresses stop being unique inside one address
// book. Google lets two people share an address -- a couple, a shared office
// mailbox, a role account someone kept on two cards -- and Rolltop mirrors what
// Google has. A unique index over the normalized address made the second of
// those people impossible to store, which failed the contact sync for the whole
// account rather than for the one row.

package store

func userSharedContactEmailMigrationSet() migrationSet {
	return migrationSet{
		Scope:   "user",
		Version: UserSchemaVersion036,
		Label:   "user schema 036 shared contact emails",
		Statements: []string{
			// The lookup this index served is still needed; only its
			// uniqueness is dropped. A new name rather than the old one so a
			// database carrying the unique index cannot keep it by accident:
			// CREATE INDEX IF NOT EXISTS would have found the old name taken
			// and left the constraint in place.
			`DROP INDEX IF EXISTS idx_contact_emails_user_normalized`,
			`CREATE INDEX IF NOT EXISTS idx_contact_emails_user_address ON contact_emails(user_id, normalized_email)`,
		},
	}
}
