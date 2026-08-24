// File overview: Per-account failure tracking and exponential backoff for
// remote IMAP work.

package syncer

import (
	"context"
	"errors"
	"log"
	"math/rand"
	"time"
)

const (
	// accountBackoffBase is the first pause after an account starts failing.
	accountBackoffBase = time.Minute
	// accountBackoffCap bounds the exponential growth for transient failures.
	accountBackoffCap = 30 * time.Minute
	// accountAuthBackoff is applied immediately to a refused credential:
	// retrying a wrong password at poll frequency is how providers lock
	// accounts, and no retry fixes it — only the user can.
	accountAuthBackoff = 30 * time.Minute
)

type accountHealthKey struct {
	userID    int64
	accountID int64
}

type accountHealth struct {
	consecutiveFailures int
	kind                RemoteErrorKind
	lastError           string
	lastFailureAt       time.Time
	backoffUntil        time.Time
}

// AccountSyncHealth is the read-side view of one account's failure state.
type AccountSyncHealth struct {
	ConsecutiveFailures int
	Kind                RemoteErrorKind
	LastError           string
	LastFailureAt       time.Time
	BackoffUntil        time.Time
}

// RecordAccountSyncOutcome is the Service's per-account outcome hook. A clean
// turn (or one that merely paused on its budget) clears the account's failure
// state; a failed one grows its backoff exponentially with jitter, and a
// refused credential jumps straight to the auth pause.
func (r *Runner) RecordAccountSyncOutcome(userID, accountID int64, err error) {
	if r == nil || userID <= 0 || accountID <= 0 {
		return
	}
	key := accountHealthKey{userID: userID, accountID: accountID}
	if err == nil || errors.Is(err, ErrSyncTurnPaused) {
		r.healthMu.Lock()
		if r.accountHealth != nil {
			delete(r.accountHealth, key)
		}
		r.healthMu.Unlock()
		return
	}
	if r.context().Err() != nil || errors.Is(err, context.Canceled) {
		// Shutdown, a user's cancel, a recovery signal preempting the turn — a
		// local cancellation says nothing about the server. It neither counts
		// as a failure nor clears an earlier one.
		return
	}
	kind := ClassifyRemoteError(err)
	now := time.Now()
	r.healthMu.Lock()
	if r.accountHealth == nil {
		r.accountHealth = map[accountHealthKey]*accountHealth{}
	}
	health := r.accountHealth[key]
	if health == nil {
		health = &accountHealth{}
		r.accountHealth[key] = health
	}
	health.consecutiveFailures++
	health.kind = kind
	health.lastError = err.Error()
	health.lastFailureAt = now
	base := accountBackoffBase
	authPause := accountAuthBackoff
	backoffCap := accountBackoffCap
	if r.accountBackoffBaseOverride > 0 {
		// Focused tests shrink the policy; production keeps the constants.
		base = r.accountBackoffBaseOverride
		authPause = r.accountBackoffBaseOverride
		backoffCap = r.accountBackoffBaseOverride * 30
	}
	var pause time.Duration
	if kind == RemoteErrorAuth {
		pause = authPause
	} else {
		// The shift count is capped: a long failure streak would otherwise
		// wrap the int64 and could land on a small positive pause. Sixteen
		// doublings are far past every cap in use.
		shift := health.consecutiveFailures - 1
		if shift > 16 {
			shift = 16
		}
		pause = base << shift
		if pause > backoffCap || pause <= 0 {
			pause = backoffCap
		}
	}
	// Jitter spreads the retries of many accounts that failed together — a
	// server restart, a network blip — so they do not all reconnect at once.
	pause += time.Duration(rand.Int63n(int64(pause)/5 + 1))
	health.backoffUntil = now.Add(pause)
	failures := health.consecutiveFailures
	r.healthMu.Unlock()
	log.Printf("imap account backoff user_id=%d account_id=%d failures=%d kind=%s pause=%s: %v",
		userID, accountID, failures, kind, pause.Round(time.Second), err)
}

// AccountSyncBackoff reports how long scheduled work should keep its hands off
// this account, with the failure that earned the pause. Zero means go ahead.
// Explicit user actions are deliberately not gated on this: they may always
// try, and their outcome updates the same record.
func (r *Runner) AccountSyncBackoff(userID, accountID int64) (time.Duration, string) {
	if r == nil {
		return 0, ""
	}
	r.healthMu.Lock()
	defer r.healthMu.Unlock()
	health := r.accountHealth[accountHealthKey{userID: userID, accountID: accountID}]
	if health == nil {
		return 0, ""
	}
	remaining := time.Until(health.backoffUntil)
	if remaining <= 0 {
		return 0, ""
	}
	return remaining, health.kind.String() + ": " + health.lastError
}

// AccountHealthSnapshot exposes one account's failure state for status
// surfaces and logs.
func (r *Runner) AccountHealthSnapshot(userID, accountID int64) (AccountSyncHealth, bool) {
	if r == nil {
		return AccountSyncHealth{}, false
	}
	r.healthMu.Lock()
	defer r.healthMu.Unlock()
	health := r.accountHealth[accountHealthKey{userID: userID, accountID: accountID}]
	if health == nil {
		return AccountSyncHealth{}, false
	}
	return AccountSyncHealth{
		ConsecutiveFailures: health.consecutiveFailures,
		Kind:                health.kind,
		LastError:           health.lastError,
		LastFailureAt:       health.lastFailureAt,
		BackoffUntil:        health.backoffUntil,
	}, true
}
