// File overview: Whether a folder gets reconciled at all. Reconciliation is the
// only thing that removes mail the server no longer has, and it used to be
// switched off together with the flag sync on any folder above the inline
// metadata limit — so a large folder kept every message deleted elsewhere.

package syncer

import (
	"testing"
	"time"

	"rolltop/backend/store"
)

func largeFolderPlan(messages uint32) MailboxPlan {
	return MailboxPlan{Status: MailboxStatus{Messages: messages}}
}

// A folder too large for the flag sync is still reconciled. Skipping the flag
// sync leaves a read mark stale; skipping this leaves mail on the reader's
// screen that the server does not have, and nothing else ever removes it.
func TestLargeMailboxIsStillReconciled(t *testing.T) {
	fixture := newMoveTestFixture(t)
	plan := largeFolderPlan(inlineMetadataSyncLimit + 1)

	if fixture.service.shouldSyncInlineMetadata(plan) {
		t.Fatal("the plan is not above the inline metadata limit, so this proves nothing")
	}
	if _, due := fixture.service.mailboxReconcileDue(fixture.userID, fixture.source, plan); !due {
		t.Fatal("a large folder was never reconciled, so mail deleted on the server stays in the mirror")
	}
}

// It pays for that listing on an interval rather than on every poll: the
// listing returns every UID the folder holds and is compared against every row
// the mirror holds.
func TestLargeMailboxReconciliationIsPaced(t *testing.T) {
	fixture := newMoveTestFixture(t)
	plan := largeFolderPlan(inlineMetadataSyncLimit + 1)

	fixture.service.recordMailboxReconciled(fixture.userID, fixture.source)

	since, due := fixture.service.mailboxReconcileDue(fixture.userID, fixture.source, plan)
	if due {
		t.Fatal("a large folder reconciled a moment ago was asked to list every UID again")
	}
	if since > time.Minute {
		t.Fatalf("reported %s since the last reconciliation, want the moment just recorded", since)
	}

	// Once the interval has passed it runs again — the pace must not become a
	// gate, or the bug comes back with a longer fuse.
	fixture.service.reconcileMu.Lock()
	fixture.service.lastReconciled[mailboxReconcileKey{userID: fixture.userID, mailboxID: fixture.source.ID}] =
		time.Now().Add(-largeMailboxReconcileInterval - time.Second)
	fixture.service.reconcileMu.Unlock()

	if _, due := fixture.service.mailboxReconcileDue(fixture.userID, fixture.source, plan); !due {
		t.Fatal("a large folder past its reconciliation interval was still skipped")
	}
}

// A folder small enough for inline metadata is reconciled every turn, as it
// always has been — the pace applies only to the folders that could not afford
// it.
func TestSmallMailboxReconcilesEveryTurn(t *testing.T) {
	fixture := newMoveTestFixture(t)
	plan := largeFolderPlan(12)

	fixture.service.recordMailboxReconciled(fixture.userID, fixture.source)

	if _, due := fixture.service.mailboxReconcileDue(fixture.userID, fixture.source, plan); !due {
		t.Fatal("a small folder was paced, which delays every deletion it would have mirrored")
	}
}

// The pace is per folder, so one large folder's turn does not stand in for
// another's.
func TestReconciliationPaceIsPerMailbox(t *testing.T) {
	fixture := newMoveTestFixture(t)
	plan := largeFolderPlan(inlineMetadataSyncLimit + 1)
	other := store.Mailbox{ID: fixture.source.ID + 1000, Name: "Archive"}

	fixture.service.recordMailboxReconciled(fixture.userID, fixture.source)

	if _, due := fixture.service.mailboxReconcileDue(fixture.userID, other, plan); !due {
		t.Fatal("reconciling one folder marked another as done")
	}
}
