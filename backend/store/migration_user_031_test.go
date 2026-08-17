package store

import "testing"

// A migration that ships after its successor would apply its ALTERs to a table
// that already has the columns, so the recorded order matters as much as the
// statements do.
func TestUser031RunsAfterUser030(t *testing.T) {
	sets := currentUserMigrationSetsForUpgradeTest()
	for i, set := range sets {
		if set.Version != UserSchemaVersion031 {
			continue
		}
		if i == 0 || sets[i-1].Version != UserSchemaVersion030 {
			t.Fatalf("user-031 predecessor=%v, want %q", sets[max(0, i-1):i], UserSchemaVersion030)
		}
		return
	}
	t.Fatalf("%s missing from current user migrations", UserSchemaVersion031)
}
