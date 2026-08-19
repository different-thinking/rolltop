// File overview: Contact addresses keep the order the user put them in. They
// used to be deleted and re-inserted on every save, so the row order followed
// the submitted order by accident; keeping the rows now (an outgoing identity
// points at one and cascades on its deletion) means the order has to be
// recorded instead of inferred from the ids.

package store

func userContactEmailPositionMigrationSet() migrationSet {
	return migrationSet{
		Scope:   "user",
		Version: UserSchemaVersion038,
		Label:   "user schema 038 contact email order",
		Statements: []string{
			// Existing rows all start at zero, which leaves the id as the
			// tiebreaker -- the order they already read back in.
			`ALTER TABLE contact_emails ADD COLUMN sort_order INTEGER NOT NULL DEFAULT 0`,
		},
	}
}
