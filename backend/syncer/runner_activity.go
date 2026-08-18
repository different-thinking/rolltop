// File overview: What the runner is doing right now, for the Activity view. The
// runner already tracks every reservation it hands out so it can decide who
// yields to whom; this exposes that same record as a read-only snapshot, so the
// answer the user reads is the state the scheduler is actually working from and
// not a second account of it kept in parallel.

package syncer

import (
	"context"
	"fmt"
	"sort"
	"strings"
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
		cancellable := r.activityCancelLocked(userID, key, activity.kind) != nil
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
	// a stalled backlog look like a broken counter. The pending flags stay set
	// while a turn of the same work is still running, so a queued row is only
	// added when no live activity already owns its key: two rows under one key
	// would make the list claim the same worker twice.
	attachmentKey := runnerMailboxWorkActivityKey(mailboxKey(userID, "__attachments__"))
	if _, live := r.workActivities[attachmentKey]; !live && r.attachmentPending[userID] {
		out = append(out, WorkerActivity{
			Key: attachmentKey, Kind: runnerWorkAttachmentIndex, Phase: "waiting", Waiting: true,
		})
	}
	senderStatsKey := runnerUserWorkActivityKey(runnerWorkSenderStats, userID)
	if _, live := r.workActivities[senderStatsKey]; !live && r.senderStatsPending[userID] {
		out = append(out, WorkerActivity{
			Key: senderStatsKey, Kind: runnerWorkSenderStats, Phase: "waiting", Waiting: true,
		})
	}
	// Folder syncs parked behind a foreground operation wait in their own maps.
	// They are the "stalled backlog" case in person, so they have to show.
	appendParkedMailbox := func(reservationKey, mailbox string) {
		activityKey := runnerMailboxWorkActivityKey(reservationKey)
		if _, live := r.workActivities[activityKey]; live {
			return
		}
		out = append(out, WorkerActivity{
			Key: activityKey, Kind: runnerWorkMailboxSync, Phase: "waiting", Mailbox: mailbox, Waiting: true,
		})
	}
	userPrefix := fmt.Sprintf("%d:", userID)
	for key := range r.mailboxPending {
		if mailbox, ok := strings.CutPrefix(key, userPrefix); ok {
			appendParkedMailbox(key, mailbox)
		}
	}
	for key := range r.accountMailboxPending {
		if keyUserID, _, mailbox, ok := parseAccountMailboxKey(key); ok && keyUserID == userID {
			appendParkedMailbox(key, mailbox)
		}
	}
	r.mu.Unlock()
	// Running work first, in the order it started; queued work after it. A
	// waiting row carries no start time, and sorting its zero time first would
	// show the queue above the very work it is queued behind.
	sort.Slice(out, func(a, b int) bool {
		if out[a].Waiting != out[b].Waiting {
			return !out[a].Waiting
		}
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
//
// The cancellation is the turn's, not the row's: a reservation batch shares one
// context, exactly as CancelSyncRun cancels a whole run's keys, so stopping one
// folder of a multi-folder turn ends the turn. The view says so on the button.
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
	cancel := r.activityCancelLocked(userID, key, activity.kind)
	r.mu.Unlock()
	if cancel == nil {
		return false
	}
	cancel()
	return true
}

// activityCancelLocked finds the cancel func behind one activity, or nil when
// there is nothing to stop. It is the one lookup both Cancellable and the
// cancel itself use, so the flag the view shows and the action the button takes
// can never disagree. The attachment worker keeps its cancel in its own map
// rather than under a mailbox reservation, which is why the kind decides where
// to look.
func (r *Runner) activityCancelLocked(userID int64, key, kind string) context.CancelFunc {
	if kind == runnerWorkAttachmentIndex {
		return r.attachmentCancels[userID]
	}
	work, ok := r.mailboxCancels[mailboxReservationKeyFromActivityKey(key)]
	if !ok || work.userID != userID {
		return nil
	}
	return work.cancel
}

// mailboxReservationKeyFromActivityKey undoes the prefix the activity key
// carries. A user-scoped activity has no reservation behind it and yields a key
// that matches nothing, which is the right answer: there is nothing to cancel.
func mailboxReservationKeyFromActivityKey(key string) string {
	if reservation, ok := strings.CutPrefix(key, runnerMailboxWorkActivityPrefix); ok && reservation != "" {
		return reservation
	}
	return ""
}
