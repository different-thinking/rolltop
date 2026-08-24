// File overview: Recovery must honor the account's sync start date via the
// snapshot's fetchable subset.

package syncer

import "testing"

func TestRestrictSnapshotToFetchableSkipsPreCutoffUIDs(t *testing.T) {
	snapshot := MailboxUIDSnapshot{
		UIDs: []uint32{1, 2, 3, 4, 5},
		// 9 is not in the validated full list and must not sneak in; 1 and 2
		// predate the cutoff and must not be re-downloaded.
		FetchableUIDs: []uint32{3, 4, 5, 9},
	}
	skipped := restrictSnapshotToFetchable(&snapshot)
	if skipped != 2 {
		t.Fatalf("skipped=%d, want the two pre-cutoff UIDs", skipped)
	}
	if len(snapshot.UIDs) != 3 || snapshot.UIDs[0] != 3 || snapshot.UIDs[2] != 5 {
		t.Fatalf("fetchable UIDs=%v, want [3 4 5]", snapshot.UIDs)
	}
}

func TestRestrictSnapshotToFetchableLeavesUncutSnapshotsAlone(t *testing.T) {
	snapshot := MailboxUIDSnapshot{UIDs: []uint32{1, 2, 3}}
	if skipped := restrictSnapshotToFetchable(&snapshot); skipped != 0 {
		t.Fatalf("skipped=%d, want 0 without a cutoff list", skipped)
	}
	if len(snapshot.UIDs) != 3 {
		t.Fatalf("UIDs=%v, want unchanged", snapshot.UIDs)
	}
}
