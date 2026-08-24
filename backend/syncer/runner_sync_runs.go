package syncer

import (
	"context"
	"time"
)

type runnerSyncRunControl struct {
	userID      int64
	keys        []string
	cancel      context.CancelFunc
	diagnostics *syncRunDiagnostics
}

// SyncRunLiveDetails is metadata for a currently executing runner job.
type SyncRunLiveDetails struct {
	Active         bool
	Cancellable    bool
	Phase          string
	Detail         string
	PhaseStartedAt time.Time
}

func (r *Runner) registerSyncRunControl(userID, runID int64, keys []string, diagnostics *syncRunDiagnostics) {
	if r == nil || userID <= 0 || runID <= 0 {
		return
	}
	r.mu.Lock()
	r.runControls[runID] = runnerSyncRunControl{userID: userID, keys: append([]string(nil), keys...), diagnostics: diagnostics}
	r.mu.Unlock()
}

// registerSyncRunCancel tracks a directly cancellable run — maintenance jobs
// and Service-owned background runs (moves, copies, Trash purges) — so the
// sync-run cancel API stops the worker instead of only stamping the row.
func (r *Runner) registerSyncRunCancel(userID, runID int64, cancel context.CancelFunc) {
	if r == nil || userID <= 0 || runID <= 0 || cancel == nil {
		return
	}
	r.mu.Lock()
	r.runControls[runID] = runnerSyncRunControl{userID: userID, cancel: cancel}
	r.mu.Unlock()
}

func (r *Runner) unregisterSyncRunControl(runID int64) {
	if r == nil || runID <= 0 {
		return
	}
	r.mu.Lock()
	delete(r.runControls, runID)
	r.mu.Unlock()
}

func (r *Runner) SyncRunLiveDetails(userID, runID int64) SyncRunLiveDetails {
	if r == nil || userID <= 0 || runID <= 0 {
		return SyncRunLiveDetails{}
	}
	r.mu.Lock()
	control, ok := r.runControls[runID]
	r.mu.Unlock()
	if !ok || control.userID != userID {
		return SyncRunLiveDetails{}
	}
	phase, detail, started := control.diagnostics.snapshot()
	return SyncRunLiveDetails{Active: true, Cancellable: true, Phase: phase, Detail: detail, PhaseStartedAt: started}
}

// CancelSyncRun requests cancellation for one live runner job. It only operates
// on a run owned by this user and never affects another account or mailbox.
func (r *Runner) CancelSyncRun(userID, runID int64) bool {
	if r == nil || userID <= 0 || runID <= 0 {
		return false
	}
	r.mu.Lock()
	control, ok := r.runControls[runID]
	if !ok || control.userID != userID {
		r.mu.Unlock()
		return false
	}
	cancels := make([]func(), 0, len(control.keys)+1)
	if control.cancel != nil {
		cancels = append(cancels, control.cancel)
	}
	for _, key := range control.keys {
		if work := r.mailboxCancels[key]; work.userID == userID && work.cancel != nil {
			cancels = append(cancels, work.cancel)
		}
	}
	r.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
	return len(cancels) > 0
}
