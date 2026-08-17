// File overview: Cooperative turn budget that lets a bounded mailbox sync stop
// at a safe point instead of being cancelled inside an IMAP fetch.

package syncer

import (
	"context"
	"errors"
	"time"

	"rolltop/backend/store"
)

// ErrSyncTurnPaused reports that a bounded mailbox turn stopped on its own time
// budget with its UID checkpoint durable. The folder is not finished, but
// nothing failed: the caller should schedule the next turn instead of surfacing
// an error. A large initial backfill takes many turns and must not look like a
// broken sync after each one.
var ErrSyncTurnPaused = errors.New("sync turn paused on its time budget")

// errSyncTurnBudgetSpent stops a streaming fetch from inside its message
// handler. Fetchers wrap handler errors, so callers match it with errors.Is.
var errSyncTurnBudgetSpent = errors.New("sync turn budget spent")

// syncTurnBudgetReserve keeps the tail of a bounded turn for the work that makes
// the turn durable: flushing the search, classification, and import batches,
// writing the UID checkpoint, and updating sync run progress. Without the
// reserve, the turn deadline lands inside an IMAP command instead, which
// discards the in-flight batch and tears down the connection.
const syncTurnBudgetReserve = 5 * time.Second

type syncTurnBudgetKey struct{}

type syncTurnBudget struct {
	pauseAt time.Time
}

// withSyncTurnBudget marks ctx as a bounded sync turn that may pause itself
// before its deadline. Contexts without a deadline - explicit maintenance,
// run-to-completion jobs, tests - are returned unchanged and keep running.
func withSyncTurnBudget(ctx context.Context) context.Context {
	deadline, ok := ctx.Deadline()
	if !ok {
		return ctx
	}
	reserve := syncTurnBudgetReserve
	if budget := time.Until(deadline); reserve > budget/3 {
		// A very short budget still needs most of its time for fetching.
		reserve = budget / 3
	}
	return context.WithValue(ctx, syncTurnBudgetKey{}, syncTurnBudget{pauseAt: deadline.Add(-reserve)})
}

func syncTurnBudgeted(ctx context.Context) bool {
	_, ok := ctx.Value(syncTurnBudgetKey{}).(syncTurnBudget)
	return ok
}

// syncTurnBudgetSpent reports that a budgeted turn should stop before starting
// more remote work.
func syncTurnBudgetSpent(ctx context.Context) bool {
	budget, ok := ctx.Value(syncTurnBudgetKey{}).(syncTurnBudget)
	if !ok {
		return false
	}
	return !time.Now().Before(budget.pauseAt)
}

// syncTurnMadeProgress guards the pause against a rescheduling loop that never
// advances: a turn may only end early after it stored, skipped, or completed
// something durable.
func syncTurnMadeProgress(progress store.SyncProgress) bool {
	return progress.MessagesStored > 0 || progress.MessagesSkipped > 0 || progress.MailboxesDone > 0
}

// pauseSyncTurnIfBudgetSpent is the per-message check a fetch handler returns to
// its fetcher. A nil result keeps the stream running.
func pauseSyncTurnIfBudgetSpent(ctx context.Context, progress store.SyncProgress) error {
	if syncTurnBudgetSpent(ctx) && syncTurnMadeProgress(progress) {
		return errSyncTurnBudgetSpent
	}
	return nil
}

// syncTurnPaused reports whether err ended a budgeted turn rather than failing
// it. The cooperative stop above is the normal path, but a single slow message
// can still let the hard turn deadline fire inside an IMAP command; once that
// deadline has passed, every error it produces describes the expired turn and
// not a broken account. A per-command timeout that fires while the turn itself
// is still alive stays a failure, as does process shutdown, which cancels
// rather than expires.
func syncTurnPaused(ctx context.Context, err error) bool {
	if err == nil || !syncTurnBudgeted(ctx) {
		return false
	}
	if errors.Is(err, errSyncTurnBudgetSpent) {
		return true
	}
	return errors.Is(ctx.Err(), context.DeadlineExceeded)
}

// syncTurnPausedWithProgress is the rescheduling decision. A turn that spent its
// whole budget without mirroring anything - a stalled server, a folder that
// cannot be selected - has nothing to resume from, so it keeps reporting its
// error instead of asking for an immediate retry that would repeat at full
// speed.
func syncTurnPausedWithProgress(ctx context.Context, progress store.SyncProgress, err error) bool {
	return syncTurnPaused(ctx, err) && syncTurnMadeProgress(progress)
}
