// File overview: Selection of the move run reported to the user after a
// background move left messages behind.

package web

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"rolltop/backend/store"
	"rolltop/backend/syncer"
)

func moveRun(status, errText string, age time.Duration) store.SyncRun {
	return store.SyncRun{
		ID: 7, Status: status, Error: errText,
		LatestNewFrom: syncer.MoveSyncRunMarker,
		UpdatedAt:     time.Now().Add(-age),
	}
}

func TestUnfinishedMoveRun(t *testing.T) {
	for name, testCase := range map[string]struct {
		run    store.SyncRun
		report bool
	}{
		"a move that left messages behind": {
			run:    moveRun("failed", "Moved 0 of 3700 messages; 1 could not be moved.", time.Minute),
			report: true,
		},
		"an interrupted move that says what it left": {
			run:    moveRun("interrupted", "Server stopped before this move finished.", time.Minute),
			report: true,
		},
		"a move that succeeded": {
			run:    moveRun("ok", "", time.Second),
			report: false,
		},
		"a move still running reports itself through progress": {
			run:    moveRun("running", "", 0),
			report: false,
		},
		"a stale failure is no longer worth reporting": {
			run:    moveRun("failed", "left behind", 48*time.Hour),
			report: false,
		},
		"a failure without detail says nothing useful": {
			run:    moveRun("failed", "  ", time.Minute),
			report: false,
		},
	} {
		t.Run(name, func(t *testing.T) {
			got := unfinishedMoveRun(testCase.run)
			if testCase.report != (got != nil) {
				t.Fatalf("reported = %v, want reported = %t", got, testCase.report)
			}
		})
	}
}

// The recent-activity feed is bounded and collapses runs, so the move cannot be
// picked out of it: a busy account outruns that window well inside the day this
// notice covers, and the delete the user is waiting on would quietly stop being
// reported.
func TestUnfinishedMoveRunSurvivesLaterSyncActivity(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(filepath.Join(t.TempDir(), "rolltop.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	user, err := db.CreateUser(ctx, "unfinished-move@example.test", "Unfinished Move", "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	account, err := db.CreateMailAccount(ctx, store.MailAccount{
		UserID: user.ID, Email: "unfinished-move@example.test", Host: "imap.example.test", Port: 993,
		Username: "unfinished-move", EncryptedPassword: "encrypted-test-value", UseTLS: true,
		Mailbox: store.DefaultMailboxPattern,
	})
	if err != nil {
		t.Fatal(err)
	}

	failed, err := db.CreateSyncRun(ctx, user.ID, account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.FinishSyncRun(ctx, user.ID, failed.ID, "failed", store.SyncProgress{
		MessagesTotal: 3700, MessagesSeen: 3700, MessagesStored: 3699, MessagesSkipped: 1,
		LatestNewFrom: syncer.MoveSyncRunMarker, LatestNewSubject: "Moving messages",
	}, "Moved 3699 of 3700 messages; 1 could not be moved."); err != nil {
		t.Fatal(err)
	}

	// Far more ordinary sync activity than the recent feed keeps.
	for i := 0; i < 60; i++ {
		run, err := db.CreateSyncRun(ctx, user.ID, account.ID)
		if err != nil {
			t.Fatal(err)
		}
		if err := db.FinishSyncRun(ctx, user.ID, run.ID, "ok", store.SyncProgress{
			MessagesStored: 1, NewMessages: 1, CurrentMailbox: fmt.Sprintf("Folder %d", i),
		}, ""); err != nil {
			t.Fatal(err)
		}
	}

	newest, err := db.LatestSyncRunWithMarkerForUser(ctx, user.ID, syncer.MoveSyncRunMarker)
	if err != nil {
		t.Fatalf("newest move run: %v", err)
	}
	if newest.ID != failed.ID {
		t.Fatalf("newest move run = %d, want the failed move %d", newest.ID, failed.ID)
	}
	if unfinishedMoveRun(newest) == nil {
		t.Fatal("the failed move stopped being reported once other sync activity buried it")
	}

	// A later move that succeeded is what clears it, not unrelated activity.
	done, err := db.CreateSyncRun(ctx, user.ID, account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.FinishSyncRun(ctx, user.ID, done.ID, "ok", store.SyncProgress{
		MessagesTotal: 4, MessagesSeen: 4, MessagesStored: 4, LatestNewFrom: syncer.MoveSyncRunMarker,
	}, ""); err != nil {
		t.Fatal(err)
	}
	newest, err = db.LatestSyncRunWithMarkerForUser(ctx, user.ID, syncer.MoveSyncRunMarker)
	if err != nil {
		t.Fatal(err)
	}
	if unfinishedMoveRun(newest) != nil {
		t.Fatal("a later successful move did not clear the notice")
	}
}

// A user who has never moved anything has nothing to report rather than an error.
func TestUnfinishedMoveRunWithoutAnyMoves(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(filepath.Join(t.TempDir(), "rolltop.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	user, err := db.CreateUser(ctx, "no-moves@example.test", "No Moves", "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.LatestSyncRunWithMarkerForUser(ctx, user.ID, syncer.MoveSyncRunMarker); !store.IsNotFound(err) {
		t.Fatalf("newest move run error = %v, want not found", err)
	}
}
