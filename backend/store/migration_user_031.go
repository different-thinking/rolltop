// File overview: Per-identity Archive folder. Sent and Drafts folders already
// live on the identity, so the folder the Archive action files mail into is
// recorded next to them; the per-account swipe mapping stays as the fallback
// for identities that leave it unset.

package store

func userIdentityArchiveMailboxMigrationSet() migrationSet {
	return migrationSet{
		Scope:   "user",
		Version: UserSchemaVersion031,
		Label:   "user schema 031 identity archive mailbox",
		Statements: []string{
			`ALTER TABLE mail_identities ADD COLUMN archive_mailbox_id INTEGER NOT NULL DEFAULT 0`,
		},
	}
}
