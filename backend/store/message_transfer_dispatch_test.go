package store

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestMessageTransferReopenProofIsAttemptScopedAndAtomic(t *testing.T) {
	ctx := context.Background()
	db, err := openTestStore(t)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	user := createPendingMoveTestUser(t, ctx, db, "dispatch-reopen@example.test")
	account := createPendingMoveTestAccount(t, ctx, db, user, "primary")
	source := arrivalTestMailbox(t, ctx, db, user, account, "Source", 901)
	destination := arrivalTestMailbox(t, ctx, db, user, account, "Destination", 902)
	raw := []byte("Message-ID: <dispatch-reopen@example.test>\r\n\r\nbody\r\n")
	message, _ := arrivalTestMessage(t, ctx, db, user, account, source, 1, raw,
		"<dispatch-reopen@example.test>", "thread:dispatch-reopen", time.Now().UTC())
	transfer, err := db.StageMessageTransfer(ctx, user.ID, message.ID, destination.ID, "move", "")
	if err != nil {
		t.Fatal(err)
	}
	claim, claimed, err := db.ClaimMessageTransferDispatchForOwner(ctx, user.ID, transfer.ID, "same-process")
	if err != nil || !claimed {
		t.Fatalf("claim claimed=%v err=%v", claimed, err)
	}
	if err := db.FinishMessageTransferDispatch(ctx, user.ID, transfer.ID, claim); err != nil {
		t.Fatal(err)
	}

	var reopened atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, reopenErr := db.ReopenMessageTransferDispatchAfterProof(ctx, user.ID, transfer.ID, claim, "same-process", time.Time{})
			if reopenErr != nil {
				t.Errorf("reopen: %v", reopenErr)
				return
			}
			if ok {
				reopened.Add(1)
			}
		}()
	}
	wg.Wait()
	if reopened.Load() != 1 {
		t.Fatalf("successful concurrent reopens=%d, want 1", reopened.Load())
	}
	newClaim, claimed, err := db.ClaimMessageTransferDispatchForOwner(ctx, user.ID, transfer.ID, "same-process")
	if err != nil || !claimed || newClaim.Attempt != claim.Attempt+1 {
		t.Fatalf("new claim=%+v claimed=%v err=%v", newClaim, claimed, err)
	}
	if ok, err := db.ReopenMessageTransferDispatchAfterProof(ctx, user.ID, transfer.ID, claim, "other-process", time.Time{}); err != nil || ok {
		t.Fatalf("stale proof reopened newer claim ok=%v err=%v", ok, err)
	}
}

func TestMessageTransferStaleSameOwnerClaimReopensAfterCutoff(t *testing.T) {
	ctx := context.Background()
	db, err := openTestStore(t)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	user := createPendingMoveTestUser(t, ctx, db, "dispatch-stale@example.test")
	account := createPendingMoveTestAccount(t, ctx, db, user, "primary")
	source := arrivalTestMailbox(t, ctx, db, user, account, "Source", 911)
	destination := arrivalTestMailbox(t, ctx, db, user, account, "Destination", 912)
	raw := []byte("Message-ID: <dispatch-stale@example.test>\r\n\r\nbody\r\n")
	message, _ := arrivalTestMessage(t, ctx, db, user, account, source, 1, raw,
		"<dispatch-stale@example.test>", "thread:dispatch-stale", time.Now().UTC())
	transfer, err := db.StageMessageTransfer(ctx, user.ID, message.ID, destination.ID, "move", "")
	if err != nil {
		t.Fatal(err)
	}
	claim, claimed, err := db.ClaimMessageTransferDispatchForOwner(ctx, user.ID, transfer.ID, "same-process")
	if err != nil || !claimed {
		t.Fatalf("claim claimed=%v err=%v", claimed, err)
	}
	// A live same-owner claim (dispatch_finished_at = 0) is protected while it
	// is younger than the cutoff.
	if ok, err := db.ReopenMessageTransferDispatchAfterProof(ctx, user.ID, transfer.ID, claim, "same-process",
		time.Now().Add(-time.Hour)); err != nil || ok {
		t.Fatalf("young same-owner claim reopened ok=%v err=%v", ok, err)
	}
	// Once its dispatch age passes the cutoff, the claim belongs to a goroutine
	// that died without settling and reconciliation may take it over.
	if ok, err := db.ReopenMessageTransferDispatchAfterProof(ctx, user.ID, transfer.ID, claim, "same-process",
		time.Now().Add(time.Hour)); err != nil || !ok {
		t.Fatalf("stale same-owner claim did not reopen ok=%v err=%v", ok, err)
	}
	if _, claimed, err := db.ClaimMessageTransferDispatchForOwner(ctx, user.ID, transfer.ID, "same-process"); err != nil || !claimed {
		t.Fatalf("re-claim after stale reopen claimed=%v err=%v", claimed, err)
	}
}

func TestReleaseUnattemptedMessageTransferDispatchRestoresUndispatchedState(t *testing.T) {
	ctx := context.Background()
	db, err := openTestStore(t)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	user := createPendingMoveTestUser(t, ctx, db, "dispatch-release@example.test")
	account := createPendingMoveTestAccount(t, ctx, db, user, "primary")
	source := arrivalTestMailbox(t, ctx, db, user, account, "Source", 921)
	destination := arrivalTestMailbox(t, ctx, db, user, account, "Destination", 922)
	raw := []byte("Message-ID: <dispatch-release@example.test>\r\n\r\nbody\r\n")
	message, _ := arrivalTestMessage(t, ctx, db, user, account, source, 1, raw,
		"<dispatch-release@example.test>", "thread:dispatch-release", time.Now().UTC())
	transfer, err := db.StageMessageTransfer(ctx, user.ID, message.ID, destination.ID, "move", "")
	if err != nil {
		t.Fatal(err)
	}
	claim, claimed, err := db.ClaimMessageTransferDispatchForOwner(ctx, user.ID, transfer.ID, "same-process")
	if err != nil || !claimed {
		t.Fatalf("claim claimed=%v err=%v", claimed, err)
	}
	released, err := db.ReleaseUnattemptedMessageTransferDispatch(ctx, user.ID, transfer.ID, claim)
	if err != nil || !released {
		t.Fatalf("release unattempted claim released=%v err=%v", released, err)
	}
	// The transfer is back to never-dispatched: the next attempt claims and
	// dispatches directly instead of reconciling against the server.
	refreshed, err := db.StageMessageTransfer(ctx, user.ID, message.ID, destination.ID, "move", "")
	if err != nil {
		t.Fatal(err)
	}
	if refreshed.ID != transfer.ID || !refreshed.DispatchedAt.IsZero() || refreshed.DispatchOwner != "" {
		t.Fatalf("released transfer = %+v, want undispatched original", refreshed)
	}
	// A finished claim must be refused: only a dispatch that provably never
	// reached the wire may be released this way.
	newClaim, claimed, err := db.ClaimMessageTransferDispatchForOwner(ctx, user.ID, transfer.ID, "same-process")
	if err != nil || !claimed {
		t.Fatalf("second claim claimed=%v err=%v", claimed, err)
	}
	if err := db.FinishMessageTransferDispatch(ctx, user.ID, transfer.ID, newClaim); err != nil {
		t.Fatal(err)
	}
	if released, err := db.ReleaseUnattemptedMessageTransferDispatch(ctx, user.ID, transfer.ID, newClaim); err != nil || released {
		t.Fatalf("finished claim released=%v err=%v, want refusal", released, err)
	}
}
