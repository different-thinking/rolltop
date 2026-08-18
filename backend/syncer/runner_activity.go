// File overview: What the runner is doing right now, for the Activity view. The
// runner already tracks every reservation it hands out so it can decide who
// yields to whom; this exposes that same record as a read-only snapshot, so the
// answer the user reads is the state the scheduler is actually working from and
// not a second account of it kept in parallel.

package syncer

import (
	"sort"
	"time"
)

// WorkerActivity is one piece of background work in progress for one user.
type WorkerActivity struct {
	// Key identifies the reservation, and is what a cancel names.
	Key string
	// Kind is the runner's own name for the work: mailbox_sync,
	// attachment_index, sender_stats, and the rest.
	Kind string
	// Phase is how far the work has got, where the work reports one.
	Phase     string
	AccountID int64
	Mailbox   string
	StartedAt time.Time
	// Cancellable reports whether stopping this one is possible. Work holding a
	// mailbox reservation can be cancelled; a bare foreground reservation is a
	// promise made to an HTTP request, and cancelling it would break that
	// request rather than free anything.
	Cancellable bool
	// Waiting marks work that is queued rather than running: it asked for a
	// turn and something else holds the slot. Without it a paused backlog and a
	// busy worker are indistinguishable, which is the whole reason a pending
	// counter can appear stuck.
	Waiting bool
}

// WorkerActivities reports the background work in progress for one user, newest
// last so a list reads in the order the work started.
func (r *Runner) WorkerActivities(userID int64) []WorkerActivity {
	if r == nil || userID <= 0 {
		return nil
	}
	r.mu.Lock()
	out := make([]WorkerActivity, 0, len(r.workActivities))
	for key, activity := range r.workActivities {
		if activity.userID != userID {
			continue
		}
		_, cancellable := r.mailboxCancels[mailboxReservationKeyFromActivityKey(key)]
		out = append(out, WorkerActivity{
			Key:         key,
			Kind:        activity.kind,
			Phase:       activity.phase,
			AccountID:   activity.accountID,
			Mailbox:     activity.mailbox,
			StartedAt:   activity.startedAt,
			Cancellable: cancellable,
		})
	}
	// Work that is queued rather than running has no reservation to report, so
	// it would be invisible here -- and invisible waiting is exactly what makes
	// a stalled backlog look like a broken counter.
	if r.attachmentPending[userID] {
		out = append(out, WorkerActivity{
			Key:  runnerMailboxWorkActivityKey(mailboxKey(userID, "__attachments__")),
			Kind: runnerWorkAttachmentIndex, Phase: "waiting", Waiting: true,
		})
	}
	if r.senderStatsPending[userID] {
		out = append(out, WorkerActivity{
			Key:  runnerUserWorkActivityKey(runnerWorkSenderStats, userID),
			Kind: runnerWorkSenderStats, Phase: "waiting", Waiting: true,
		})
	}
	r.mu.Unlock()
	sort.Slice(out, func(a, b int) bool {
		if out[a].StartedAt.Equal(out[b].StartedAt) {
			return out[a].Key < out[b].Key
		}
		return out[a].StartedAt.Before(out[b].StartedAt)
	})
	return out
}

// CancelWorkerActivity stops one piece of background work by the key the
// snapshot reported. It refuses anything belonging to another user, and answers
// whether it actually cancelled something rather than whether the key looked
// plausible.
func (r *Runner) CancelWorkerActivity(userID int64, key string) bool {
	if r == nil || userID <= 0 || key == "" {
		return false
	}
	r.mu.Lock()
	activity, tracked := r.workActivities[key]
	if !tracked || activity.userID != userID {
		r.mu.Unlock()
		return false
	}
	reservation := mailboxReservationKeyFromActivityKey(key)
	work, ok := r.mailboxCancels[reservation]
	if !ok || work.userID != userID || work.cancel == nil {
		r.mu.Unlock()
		return false
	}
	cancel := work.cancel
	r.mu.Unlock()
	cancel()
	return true
}

// mailboxReservationKeyFromActivityKey undoes the prefix the activity key
// carries. A user-scoped activity has no reservation behind it and yields a key
// that matches nothing, which is the right answer: there is nothing to cancel.
func mailboxReservationKeyFromActivityKey(key string) string {
	const prefix = "mailbox:"
	if len(key) > len(prefix) && key[:len(prefix)] == prefix {
		return key[len(prefix):]
	}
	return ""
}
