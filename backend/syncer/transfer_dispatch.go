// File overview: Process ownership helpers for at-most-once remote transfer dispatch.

package syncer

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"time"

	"rolltop/backend/store"
)

var processMessageTransferOwner = newMessageTransferOwner()

func newMessageTransferOwner() string {
	var token [16]byte
	if _, err := rand.Read(token[:]); err == nil {
		return "process-" + hex.EncodeToString(token[:])
	}
	return fmt.Sprintf("process-%d-%d", os.Getpid(), time.Now().UnixNano())
}

// messageTransferStaleClaimAge is how long a same-process dispatch claim may
// stay unsettled before reconciliation may take it over. A legitimate dispatch
// never holds a claim this long — every command it can issue is socket-bounded
// far below it — so a claim this old belongs to a goroutine that died without
// settling. Without the escape, such a claim refused every retry of that
// message until the process restarted.
const messageTransferStaleClaimAge = 10 * time.Minute

func messageTransferCanReconcile(transfer store.MessageTransfer) bool {
	if transfer.DispatchedAt.IsZero() {
		return false
	}
	if !transfer.DispatchFinishedAt.IsZero() || transfer.DispatchOwner != processMessageTransferOwner {
		return true
	}
	return time.Since(transfer.DispatchedAt) >= messageTransferStaleClaimAge
}

// messageTransferStaleClaimCutoff is the dispatch age boundary handed to the
// store's reopen guard, matching messageTransferCanReconcile's escape.
func messageTransferStaleClaimCutoff() time.Time {
	return time.Now().Add(-messageTransferStaleClaimAge)
}

func messageTransferClaim(transfer store.MessageTransfer) store.MessageTransferDispatchClaim {
	return store.MessageTransferDispatchClaim{Owner: transfer.DispatchOwner, Attempt: transfer.DispatchAttempt}
}
