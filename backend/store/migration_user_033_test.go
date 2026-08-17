package store

import "testing"

// The newest migration owns the whole-list invariants: it is the one that would
// break them, and asserting them from every migration's test would mean fixing
// the same thing in a dozen places the next time one is added.
func TestUser033IsLatestRegisteredUserMigration(t *testing.T) {
	sets := currentUserMigrationSetsForUpgradeTest()
	if len(sets) < 2 {
		t.Fatalf("registered user migrations=%d, want at least 2", len(sets))
	}
	if latest := sets[len(sets)-1]; latest.Version != UserSchemaVersion033 {
		t.Fatalf("latest user migration=%q, want %q", latest.Version, UserSchemaVersion033)
	}
	if predecessor := sets[len(sets)-2]; predecessor.Version != UserSchemaVersion032 {
		t.Fatalf("user-033 predecessor=%q, want %q", predecessor.Version, UserSchemaVersion032)
	}
	// Application order is not numeric — user-011 has always run before
	// user-004 — so only duplicates are worth asserting here. A version applied
	// twice would record one checksum over two different statement lists.
	seen := map[string]bool{}
	for _, set := range sets {
		if seen[set.Version] {
			t.Fatalf("user migration %q is registered twice", set.Version)
		}
		seen[set.Version] = true
	}
	// The legacy fixture applies a frozen prefix of this list to build a v21
	// database. If it ever stops being a prefix, the upgrade tests would be
	// migrating from a schema the app never actually shipped.
	legacy := legacyUserMigrationSetsThroughV21()
	if len(legacy) > len(sets) {
		t.Fatalf("legacy prefix has %d sets, more than the %d registered", len(legacy), len(sets))
	}
	for i, set := range legacy {
		if sets[i].Version != set.Version {
			t.Fatalf("registered migration %d = %q, want the legacy prefix entry %q", i, sets[i].Version, set.Version)
		}
	}
}
