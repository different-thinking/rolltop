package store

import "testing"

// The duplicate-copy columns have to be in place before the calendar migration
// runs. Both extend the same per-user schema, and only their order decides
// which checksum an installed database recorded.
func TestUser033RunsAfterUser032(t *testing.T) {
	sets := currentUserMigrationSetsForUpgradeTest()
	for i, set := range sets {
		if set.Version != UserSchemaVersion033 {
			continue
		}
		if i == 0 || sets[i-1].Version != UserSchemaVersion032 {
			t.Fatalf("user-033 runs at index %d, want it directly after %q", i, UserSchemaVersion032)
		}
		return
	}
	t.Fatalf("user-033 is not registered")
}
