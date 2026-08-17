// File overview: Selection of the move run reported to the user after a
// background move left messages behind.

package web

import (
	"testing"
	"time"

	"rolltop/backend/store"
	"rolltop/backend/syncer"
)

func moveRun(id int64, status, errText string, age time.Duration) store.SyncRun {
	return store.SyncRun{
		ID: id, Status: status, Error: errText,
		LatestNewFrom: syncer.MoveSyncRunMarker,
		UpdatedAt:     time.Now().Add(-age),
	}
}

func mirrorRun(id int64, status string, age time.Duration) store.SyncRun {
	return store.SyncRun{ID: id, Status: status, CurrentMailbox: "INBOX", UpdatedAt: time.Now().Add(-age)}
}

func TestNewestUnfinishedMoveRun(t *testing.T) {
	failed := moveRun(7, "failed", "Moved 0 of 3700 messages; 1 could not be moved.", time.Minute)

	for name, testCase := range map[string]struct {
		// ListSyncRunsForUser orders running runs first and then by updated_at
		// descending, so the input order deliberately does not match age.
		runs []store.SyncRun
		want int64
	}{
		// A finished move queues the mailbox refresh that supersedes it, so the
		// newest run is never the failed move by the time anyone looks.
		"refresh run supersedes the failed move": {
			runs: []store.SyncRun{mirrorRun(8, "ok", time.Second), failed},
			want: 7,
		},
		"running refresh sorts ahead of the failed move": {
			runs: []store.SyncRun{mirrorRun(9, "running", 5*time.Minute), failed},
			want: 7,
		},
		"failed move alone": {
			runs: []store.SyncRun{failed},
			want: 7,
		},
		"a later successful move clears it": {
			runs: []store.SyncRun{moveRun(8, "ok", "", time.Second), failed},
			want: 0,
		},
		"a move still running reports itself through progress": {
			runs: []store.SyncRun{moveRun(8, "running", "", 0), failed},
			want: 0,
		},
		"a stale failure is no longer worth reporting": {
			runs: []store.SyncRun{moveRun(8, "failed", "left behind", 48*time.Hour)},
			want: 0,
		},
		"an interrupted move without detail says nothing useful": {
			runs: []store.SyncRun{moveRun(8, "interrupted", "  ", time.Minute)},
			want: 0,
		},
		"no moves at all": {
			runs: []store.SyncRun{mirrorRun(8, "ok", time.Second), mirrorRun(9, "failed", time.Minute)},
			want: 0,
		},
		"no runs at all": {
			runs: nil,
			want: 0,
		},
	} {
		t.Run(name, func(t *testing.T) {
			got := newestUnfinishedMoveRun(testCase.runs)
			if testCase.want == 0 {
				if got != nil {
					t.Fatalf("reported run %d, want none", got.ID)
				}
				return
			}
			if got == nil || got.ID != testCase.want {
				t.Fatalf("reported run = %v, want %d", got, testCase.want)
			}
		})
	}
}
