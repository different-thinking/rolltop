// File overview: The sync start date has to survive the repair path, which is
// the one place that asks the server for a UID list and then downloads whatever
// is missing locally.

package syncer

import (
	"context"
	"reflect"
	"testing"

	"rolltop/backend/store"
)

type cutoffRepairFetcher struct {
	*statusSelectRaceFetcher
	snapshot    MailboxUIDSnapshot
	requested   []uint32
	sparseCalls int
}

func (f *cutoffRepairFetcher) SnapshotMailboxUIDs(context.Context, store.MailAccount, string) (MailboxUIDSnapshot, error) {
	return f.snapshot, nil
}

func (f *cutoffRepairFetcher) FetchUIDsWithUIDValidity(_ context.Context, _ store.MailAccount, _ string, uids []uint32, _ uint32, _ func(FetchedMessage) error) error {
	f.sparseCalls++
	f.requested = append(f.requested, uids...)
	return nil
}

// Repair compares the server's UID list against local rows and downloads the
// difference. Handed the unfiltered list, it treats every pre-cutoff message as
// missing and pulls the entire history the setting exists to skip — on every
// requested sync, because local can never catch up with the full remote count.
func TestRepairOnlyFetchesUIDsTheSyncStartDateAllows(t *testing.T) {
	fixture := newMoveTestFixture(t)
	fetcher := &cutoffRepairFetcher{
		statusSelectRaceFetcher: &statusSelectRaceFetcher{
			moveTestFetcher:  fixture.fetcher,
			statusValidity:   moveTestSourceUIDValidity,
			selectedValidity: moveTestSourceUIDValidity,
		},
		snapshot: MailboxUIDSnapshot{
			// 42 is the fixture's already-mirrored message, so 41 is the only
			// UID repair should ask for: 40 is behind the cutoff.
			UIDs:          []uint32{40, 41, 42},
			FetchableUIDs: []uint32{41, 42},
			UIDValidity:   moveTestSourceUIDValidity,
			UIDNext:       43,
		},
	}
	fixture.service.Fetcher = fetcher
	plan := MailboxPlan{
		Name: fixture.source.Name, LastUID: 0,
		Status: MailboxStatus{Messages: 3, UIDNext: 43, UIDValidity: moveTestSourceUIDValidity},
	}
	_, repaired, err := fixture.service.repairRequestedIncompleteMailbox(context.Background(), fixture.userID,
		fixture.account, fixture.source, plan, true, 0, nil)
	if err != nil {
		t.Fatalf("repair: %v", err)
	}
	if !repaired {
		t.Fatal("repair did not run")
	}
	if fetcher.sparseCalls != 1 {
		t.Fatalf("sparse fetches = %d, want 1", fetcher.sparseCalls)
	}
	if want := []uint32{41}; !reflect.DeepEqual(fetcher.requested, want) {
		t.Fatalf("requested UIDs = %v, want %v — the pre-cutoff UID must not be downloaded", fetcher.requested, want)
	}
}

// Accounts without a cutoff carry a nil fetchable list, and repair must keep
// seeing every remote UID for them.
func TestRepairFetchesEverythingWithoutACutoff(t *testing.T) {
	fixture := newMoveTestFixture(t)
	fetcher := &cutoffRepairFetcher{
		statusSelectRaceFetcher: &statusSelectRaceFetcher{
			moveTestFetcher:  fixture.fetcher,
			statusValidity:   moveTestSourceUIDValidity,
			selectedValidity: moveTestSourceUIDValidity,
		},
		snapshot: MailboxUIDSnapshot{
			UIDs:        []uint32{40, 41, 42},
			UIDValidity: moveTestSourceUIDValidity,
			UIDNext:     43,
		},
	}
	fixture.service.Fetcher = fetcher
	plan := MailboxPlan{
		Name: fixture.source.Name, LastUID: 0,
		Status: MailboxStatus{Messages: 3, UIDNext: 43, UIDValidity: moveTestSourceUIDValidity},
	}
	if _, _, err := fixture.service.repairRequestedIncompleteMailbox(context.Background(), fixture.userID,
		fixture.account, fixture.source, plan, true, 0, nil); err != nil {
		t.Fatalf("repair: %v", err)
	}
	// 42 is already mirrored by the fixture; without a cutoff both older UIDs
	// are still fair game.
	if want := []uint32{40, 41}; !reflect.DeepEqual(fetcher.requested, want) {
		t.Fatalf("requested UIDs = %v, want %v", fetcher.requested, want)
	}
}
