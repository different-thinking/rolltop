// File overview: Sync reconciliation between local rows and remote IMAP mailbox state.

package syncer

import (
	"context"
	"fmt"

	"rolltop/backend/store"
)

// reconcileMailboxUIDs treats IMAP as the source of truth for membership in a
// folder. If a UID disappears remotely because it was deleted or moved out, the
// local message/search row disappears too, and the raw blob is removed when safe.
//
// heartbeat, when non-nil, is called once per stale message after its blob is
// settled. Each one is its own transaction pair, so a folder emptied by the
// thousand can spend minutes here after the remote side is already done; a
// caller with a sync run to keep alive uses this to keep publishing progress
// rather than leaving the run looking stalled at 100%. A nil heartbeat costs
// nothing extra, for callers with no run to report against.
func (s *Service) reconcileMailboxUIDs(ctx context.Context, userID int64, account store.MailAccount, mailbox store.Mailbox, heartbeat func(context.Context) error) error {
	var (
		stale []store.ExpungedMessage
		err   error
	)
	if snapshotFetcher, ok := s.Fetcher.(MailboxUIDSnapshotFetcher); ok {
		snapshot, snapshotErr := snapshotFetcher.SnapshotMailboxUIDs(ctx, account, mailbox.Name)
		if snapshotErr != nil {
			return fmt.Errorf("reconcile mailbox %q UID snapshot: %w", mailbox.Name, snapshotErr)
		}
		stale, err = s.Store.DeleteMessagesMissingUIDsAndRecordExpunges(ctx, userID, account.ID,
			mailbox.ID, snapshot.UIDs, snapshot.UIDValidity, snapshot.UIDNext, nil)
	} else {
		// Legacy fetchers cannot bind UIDs to a selected UIDVALIDITY. Preserve
		// local mirror cleanup, but never create evidence that could suppress a
		// later Inbox delivery notification.
		uids, uidErr := s.Fetcher.UIDs(ctx, account, mailbox.Name)
		if uidErr != nil {
			return fmt.Errorf("reconcile mailbox %q UIDs: %w", mailbox.Name, uidErr)
		}
		stale, err = s.Store.DeleteMessagesMissingUIDs(ctx, userID, account.ID, mailbox.ID, uids)
	}
	if err != nil {
		return err
	}
	if s.Search != nil && len(stale) > 0 {
		// One commit for the whole reconciliation. Emptying a Trash folder passes
		// through here with everything the server confirmed gone, and removing
		// those documents one at a time is one index commit per message.
		staleIDs := make([]int64, 0, len(stale))
		for _, msg := range stale {
			staleIDs = append(staleIDs, msg.ID)
		}
		if err := s.Search.DeleteMessages(ctx, userID, staleIDs); err != nil {
			return err
		}
	}
	for _, msg := range stale {
		if _, err := s.deleteUnreferencedBlob(ctx, userID, msg.BlobID, msg.BlobPath); err != nil {
			return err
		}
		if heartbeat == nil {
			continue
		}
		if err := heartbeat(ctx); err != nil {
			return err
		}
	}
	if len(stale) > 0 {
		s.notify(userID)
	}
	return nil
}
