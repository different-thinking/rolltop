// File overview: Sync-side cleanup after an atomic mailbox UIDVALIDITY reset.

package syncer

import (
	"context"
	"errors"
	"log"
	"time"

	"rolltop/backend/store"
)

// mailboxGenerationIndexPurgeTimeout bounds the mailbox-scoped search purge a
// generation reset owes. The purge runs on a context detached from the caller
// so a cancellation that ends the sync turn cannot skip it.
const mailboxGenerationIndexPurgeTimeout = 2 * time.Minute

// ArrivalUIDFloorAfterConfirmedUID returns the first UID that could have arrived
// after a confirmed APPEND. It refuses to wrap the IMAP UID space to zero.
func ArrivalUIDFloorAfterConfirmedUID(uid uint32) (uint32, error) {
	if uid == 0 {
		return 0, errors.New("confirmed APPEND UID is zero")
	}
	if uid == ^uint32(0) {
		return 0, errors.New("confirmed APPEND UID exhausted the UID space")
	}
	return uid + 1, nil
}

// ResetMailboxGenerationIfNeeded purges local rows and cached artifacts when a
// positive remote UIDVALIDITY cannot prove the current mailbox checkpoint and
// message rows belong to the same IMAP generation. arrivalUIDFloor must be the
// first UID that was not present when the remote generation was observed.
func (s *Service) ResetMailboxGenerationIfNeeded(ctx context.Context, userID int64, account store.MailAccount,
	mailbox store.Mailbox, remoteUIDValidity, arrivalUIDFloor uint32,
) (bool, error) {
	if remoteUIDValidity == 0 {
		return false, nil
	}
	_, reset, err := s.Store.ResetMailboxForRemoteGeneration(ctx, userID, account.ID, mailbox.ID,
		remoteUIDValidity, arrivalUIDFloor)
	if err != nil {
		return reset, err
	}
	if !reset {
		return false, nil
	}
	// A UIDVALIDITY reset makes every existing document for this mailbox stale.
	// Clearing that bounded mailbox scope prevents reused UIDs from surfacing old
	// mail. The purge runs before the recovery signal and on a detached context,
	// because the signal cancels this tenant's other sync work and used to cancel
	// the discovering job itself — which skipped this purge with nothing ever
	// retrying it: the next pass sees a clean reset and never reaches this branch.
	if s.Search != nil {
		purgeCtx, cancelPurge := context.WithTimeout(context.WithoutCancel(ctx), mailboxGenerationIndexPurgeTimeout)
		_, purgeErr := s.Search.PurgeMailbox(purgeCtx, userID, mailbox.ID)
		cancelPurge()
		if purgeErr != nil {
			// The index is derived state and the message rows are already gone,
			// so a failed purge must not fail the reset. Mark the folder's
			// coverage unverified so the repair path removes the stale documents
			// instead of leaving them to surface reused UIDs as old mail.
			log.Printf("purge mailbox search index after generation reset user_id=%d mailbox_id=%d: %v",
				userID, mailbox.ID, purgeErr)
			if markErr := s.Store.MarkMailboxSearchIndexRepairRequired(context.WithoutCancel(ctx), userID, mailbox.ID); markErr != nil {
				log.Printf("mark mailbox search repair after failed generation purge user_id=%d mailbox_id=%d: %v",
					userID, mailbox.ID, markErr)
			}
		}
	}
	if s.MailboxGenerationRecoveryStarted != nil {
		s.MailboxGenerationRecoveryStarted(ctx, userID)
	}
	return true, nil
}
