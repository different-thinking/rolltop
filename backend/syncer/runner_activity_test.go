// File overview: The worker-activity snapshot and its cancel action. The two
// properties nothing may break: one tenant can never see or stop another
// tenant's work, and the snapshot never claims the same worker twice.

package syncer

import (
	"testing"
	"time"
)

func TestWorkerActivitiesAndCancelStayInsideTheTenant(t *testing.T) {
	runner := NewRunner(nil)
	ownerID, otherID := int64(7), int64(9)
	ownKey := accountMailboxKey(ownerID, 11, "INBOX")
	otherKey := accountMailboxKey(otherID, 22, "Private Folder")
	otherCancelled := false

	runner.mu.Lock()
	runner.mailboxRunning[ownKey] = true
	runner.startWorkActivityLocked(runnerMailboxWorkActivityKey(ownKey), runnerWorkActivity{
		kind: runnerWorkMailboxSync, userID: ownerID, accountID: 11, mailbox: "INBOX", startedAt: time.Unix(100, 0),
	})
	runner.mailboxRunning[otherKey] = true
	runner.startWorkActivityLocked(runnerMailboxWorkActivityKey(otherKey), runnerWorkActivity{
		kind: runnerWorkMailboxSync, userID: otherID, accountID: 22, mailbox: "Private Folder", startedAt: time.Unix(100, 0),
	})
	runner.mailboxCancels[otherKey] = runnerMailboxCancellation{
		userID: otherID,
		cancel: func() { otherCancelled = true },
	}
	runner.mu.Unlock()

	activities := runner.WorkerActivities(ownerID)
	if len(activities) != 1 || activities[0].Mailbox != "INBOX" {
		t.Fatalf("activities for owner = %+v, want only their own INBOX sync", activities)
	}

	// The other tenant's key is real and cancellable -- for its owner. Asking
	// with the wrong user must refuse without touching the cancel func.
	if runner.CancelWorkerActivity(ownerID, runnerMailboxWorkActivityKey(otherKey)) {
		t.Fatal("one tenant cancelled another tenant's worker")
	}
	if otherCancelled {
		t.Fatal("the other tenant's work was cancelled by a cross-tenant request")
	}
	if !runner.CancelWorkerActivity(otherID, runnerMailboxWorkActivityKey(otherKey)) {
		t.Fatal("the owner could not cancel their own worker")
	}
	if !otherCancelled {
		t.Fatal("the owner's cancel did not reach the work")
	}
}

// The pending flags stay set while a turn of the same work is still running, so
// a naive snapshot would list the worker twice under one key -- and the view
// keys its rows on exactly that string.
func TestWorkerActivitiesDoNotDuplicateRunningWorkAsWaiting(t *testing.T) {
	runner := NewRunner(nil)
	userID := int64(7)
	key := mailboxKey(userID, "__attachments__")

	runner.mu.Lock()
	runner.mailboxRunning[key] = true
	runner.startWorkActivityLocked(runnerMailboxWorkActivityKey(key), runnerWorkActivity{
		kind: runnerWorkAttachmentIndex, userID: userID, startedAt: time.Unix(100, 0),
	})
	runner.attachmentPending[userID] = true
	runner.senderStatsPending[userID] = true
	runner.mu.Unlock()

	activities := runner.WorkerActivities(userID)
	seen := map[string]int{}
	for _, activity := range activities {
		seen[activity.Key]++
	}
	for key, count := range seen {
		if count > 1 {
			t.Fatalf("key %q reported %d times, want each worker exactly once: %+v", key, count, activities)
		}
	}
	// The queued sender-stats work has no running twin and must still show,
	// sorted after the running work.
	last := activities[len(activities)-1]
	if !last.Waiting || last.Kind != runnerWorkSenderStats {
		t.Fatalf("activities = %+v, want the queued sender-stats row last", activities)
	}
}

// Folder syncs parked behind a foreground operation live in their own pending
// maps. Invisible waiting is what makes a stalled backlog look like a broken
// counter, so they must appear as queued rows.
func TestWorkerActivitiesShowParkedFolderSyncs(t *testing.T) {
	runner := NewRunner(nil)
	userID := int64(7)

	runner.mu.Lock()
	runner.mailboxPending[mailboxKey(userID, "INBOX")] = true
	runner.accountMailboxPending[accountMailboxKey(userID, 11, "Archive")] = true
	runner.accountMailboxPending[accountMailboxKey(9, 22, "Other Tenant")] = true
	runner.mu.Unlock()

	activities := runner.WorkerActivities(userID)
	mailboxes := map[string]bool{}
	for _, activity := range activities {
		if !activity.Waiting || activity.Kind != runnerWorkMailboxSync {
			t.Fatalf("activity = %+v, want only queued folder syncs", activity)
		}
		mailboxes[activity.Mailbox] = true
	}
	if !mailboxes["inbox"] || !mailboxes["archive"] || len(mailboxes) != 2 {
		t.Fatalf("parked folders = %v, want the tenant's inbox and archive only", mailboxes)
	}
}

// The attachment worker keeps its cancel in its own map rather than under a
// mailbox reservation. The view must still offer Stop for it, and the button
// must actually reach the work.
func TestAttachmentIndexWorkIsCancellable(t *testing.T) {
	runner := NewRunner(nil)
	userID := int64(7)
	key := mailboxKey(userID, "__attachments__")
	cancelled := false

	runner.mu.Lock()
	runner.mailboxRunning[key] = true
	runner.startWorkActivityLocked(runnerMailboxWorkActivityKey(key), runnerWorkActivity{
		kind: runnerWorkAttachmentIndex, userID: userID, startedAt: time.Unix(100, 0),
	})
	runner.attachmentCancels[userID] = func() { cancelled = true }
	runner.mu.Unlock()

	activities := runner.WorkerActivities(userID)
	if len(activities) != 1 || !activities[0].Cancellable {
		t.Fatalf("activities = %+v, want one cancellable attachment-index row", activities)
	}
	if !runner.CancelWorkerActivity(userID, activities[0].Key) {
		t.Fatal("cancelling the attachment index was refused")
	}
	if !cancelled {
		t.Fatal("the attachment worker's cancel func was not called")
	}
	if runner.CancelWorkerActivity(9, activities[0].Key) {
		t.Fatal("another tenant cancelled the attachment index")
	}
}
