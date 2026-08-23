// File overview: Emptying a Trash folder. This is the only operation that deletes mail
// on the IMAP server; everything else in sync moves messages between folders.

package syncer

import (
	"context"
	"errors"
	"fmt"
	"log"

	"rolltop/backend/store"
)

// EmptyTrashSyncRunMarker is the LatestNewFrom value an empty-trash run carries.
// Progress displays use it to tell a user-initiated purge from background
// mirroring, the same way a move run is recognised.
const EmptyTrashSyncRunMarker = "rolltop:empty-trash"

// emptyTrashBatchSize bounds one STORE/EXPUNGE pair. It stays below the IMAP
// client's own per-request ceiling so a full Trash folder is emptied in steps
// the server will accept, and so progress moves while it happens.
const emptyTrashBatchSize = 250

// ErrEmptyTrashUnsupported reports that this deployment's IMAP client cannot
// delete remote mail, which is a capability question rather than a failure of
// the request.
var ErrEmptyTrashUnsupported = errors.New("this IMAP connection cannot delete messages on the server")

// StartEmptyTrash permanently deletes everything in one Trash folder as a
// background sync run, so the HTTP request can return immediately.
//
// The folder is emptied as the server currently sees it, not as the local
// mirror does: mail that was never mirrored (an account with a sync start date,
// a folder still syncing) is part of what the user asked to throw away.
func (s *Service) StartEmptyTrash(ctx context.Context, userID, mailboxID int64, onDone func()) (store.SyncRun, error) {
	if s.Fetcher == nil {
		return store.SyncRun{}, errors.New("sync fetcher is not configured")
	}
	if _, ok := s.Fetcher.(ExpungeFetcher); !ok {
		return store.SyncRun{}, ErrEmptyTrashUnsupported
	}
	mailbox, err := s.Store.GetMailboxForUser(ctx, userID, mailboxID)
	if err != nil {
		return store.SyncRun{}, err
	}
	if mailbox.Role != "trash" {
		return store.SyncRun{}, errors.New("only a Trash folder can be emptied")
	}
	account, err := s.Store.GetMailAccountForUser(ctx, userID, mailbox.AccountID)
	if err != nil {
		return store.SyncRun{}, err
	}
	run, err := s.Store.CreateSyncRun(ctx, userID, mailbox.AccountID)
	if err != nil {
		return store.SyncRun{}, err
	}
	progress := store.SyncProgress{
		MailboxesTotal:   1,
		CurrentMailbox:   "Emptying " + mailbox.Name,
		LatestNewFrom:    EmptyTrashSyncRunMarker,
		LatestNewSubject: "Deleting messages on the server",
	}
	if err := s.Store.UpdateSyncRunProgress(ctx, userID, run.ID, progress); err != nil {
		return store.SyncRun{}, err
	}
	s.notify(userID)
	go s.runEmptyTrash(context.Background(), userID, account, mailbox, run.ID, progress, onDone)
	return run, nil
}

func (s *Service) runEmptyTrash(ctx context.Context, userID int64, account store.MailAccount, mailbox store.Mailbox,
	runID int64, progress store.SyncProgress, onDone func()) {
	status := "ok"
	errText := ""
	defer func() {
		if ctx.Err() != nil && status == "ok" {
			status = "interrupted"
			errText = "Server stopped before this folder was emptied."
		}
		if status == "ok" {
			progress.MailboxesDone = 1
		}
		if err := s.Store.FinishSyncRun(context.Background(), userID, runID, status, progress, errText); err != nil {
			log.Printf("finish empty trash run user_id=%d run_id=%d: %v", userID, runID, err)
		}
		s.notify(userID)
		if onDone != nil {
			onDone()
		}
	}()
	deleted, err := s.emptyTrashRemotely(ctx, userID, account, mailbox, runID, &progress)
	if err != nil {
		status = "failed"
		errText = err.Error()
		// Whatever did go is still gone remotely, so the local mirror is cleaned
		// up for it either way. A failure here only means the folder is not empty.
		log.Printf("empty trash user_id=%d run_id=%d mailbox_id=%d deleted=%d: %v", userID, runID, mailbox.ID, deleted, err)
	}
	if deleted == 0 {
		return
	}
	// One reconciliation removes the local rows, search documents, and blobs for
	// everything the server confirmed gone. Reusing the sync path keeps a single
	// definition of what "this message is no longer on the server" does locally.
	if err := s.reconcileMailboxUIDs(ctx, userID, account, mailbox); err != nil {
		log.Printf("reconcile emptied trash user_id=%d run_id=%d mailbox_id=%d: %v", userID, runID, mailbox.ID, err)
		if status == "ok" {
			status = "failed"
			errText = "Deleted " + fmt.Sprint(deleted) + " messages on the server, but the local copy could not be cleaned up. The next sync will finish it."
		}
	}
}

// emptyTrashRemotely deletes the folder's current contents in batches and
// returns how many messages the server confirmed gone.
func (s *Service) emptyTrashRemotely(ctx context.Context, userID int64, account store.MailAccount, mailbox store.Mailbox,
	runID int64, progress *store.SyncProgress) (int, error) {
	expunger, ok := s.Fetcher.(ExpungeFetcher)
	if !ok {
		return 0, ErrEmptyTrashUnsupported
	}
	uids, uidValidity, err := s.trashSnapshot(ctx, account, mailbox)
	if err != nil {
		return 0, err
	}
	if len(uids) == 0 {
		return 0, nil
	}
	progress.MessagesTotal = len(uids)
	if err := s.updateSyncProgress(ctx, userID, runID, *progress); err != nil {
		return 0, err
	}
	// A full Trash folder is tens of batches, and reconnecting for each one is
	// most of what the purge costs. Hold one login for all of them when the
	// fetcher can; the session is nil otherwise, which connects per batch rather
	// than failing the purge.
	session := s.openExpungeSession(ctx, userID, account)
	defer session.close()
	deleted := 0
	for start := 0; start < len(uids); start += emptyTrashBatchSize {
		if err := ctx.Err(); err != nil {
			return deleted, err
		}
		end := min(start+emptyTrashBatchSize, len(uids))
		gone, err := session.expunge(ctx, expunger, account, mailbox.Name, uids[start:end], uidValidity)
		deleted += len(gone)
		progress.MessagesSeen += end - start
		progress.MessagesStored = deleted
		progress.MessagesSkipped = progress.MessagesSeen - deleted
		if err != nil {
			return deleted, err
		}
		if err := s.updateSyncProgress(ctx, userID, runID, *progress); err != nil {
			return deleted, err
		}
	}
	if deleted < len(uids) {
		return deleted, fmt.Errorf("the server kept %d of %d messages in %s", len(uids)-deleted, len(uids), mailbox.Name)
	}
	return deleted, nil
}

// expungeSessionHolder keeps the connection one purge deletes over, opening it
// on first use and giving it back when the purge ends.
type expungeSessionHolder struct {
	service *Service
	userID  int64
	account store.MailAccount
	session ExpungeSession
}

// openExpungeSession returns the holder a purge deletes through, or nil when
// this deployment's fetcher cannot hold a connection open.
func (s *Service) openExpungeSession(ctx context.Context, userID int64, account store.MailAccount) *expungeSessionHolder {
	if s == nil || s.Fetcher == nil || ctx.Err() != nil {
		return nil
	}
	if _, ok := s.Fetcher.(ExpungeSessionFetcher); !ok {
		return nil
	}
	return &expungeSessionHolder{service: s, userID: userID, account: account}
}

// expunge deletes one batch, over the held connection when there is one and
// through the fetcher's own connection otherwise.
func (h *expungeSessionHolder) expunge(ctx context.Context, expunger ExpungeFetcher, account store.MailAccount,
	mailbox string, uids []uint32, expectedUIDValidity uint32) ([]uint32, error) {
	if h == nil {
		return expunger.ExpungeMessages(ctx, account, mailbox, uids, expectedUIDValidity)
	}
	if h.session == nil {
		opener, ok := h.service.Fetcher.(ExpungeSessionFetcher)
		if !ok {
			return expunger.ExpungeMessages(ctx, account, mailbox, uids, expectedUIDValidity)
		}
		session, err := opener.OpenExpungeSession(ctx, h.account)
		if err != nil {
			return nil, err
		}
		h.session = session
	}
	gone, err := h.session.ExpungeMessages(ctx, mailbox, uids, expectedUIDValidity)
	if err != nil {
		// The held connection may itself be why this failed. Drop it so a retry
		// of this purge starts from a fresh login rather than a dead socket.
		h.close()
	}
	return gone, err
}

func (h *expungeSessionHolder) close() {
	if h == nil || h.session == nil {
		return
	}
	session := h.session
	h.session = nil
	if err := session.Close(); err != nil {
		log.Printf("close expunge session user_id=%d account_id=%d: %v", h.userID, h.account.ID, err)
	}
}

// trashSnapshot lists what the folder currently holds, bound to the mailbox
// generation those UIDs belong to. Both values must come from one selected
// session: a UID is only meaningful under the UIDVALIDITY it was listed with,
// and that pairing is what stops a recreated folder from being deleted from.
func (s *Service) trashSnapshot(ctx context.Context, account store.MailAccount, mailbox store.Mailbox) ([]uint32, uint32, error) {
	snapshotFetcher, ok := s.Fetcher.(MailboxUIDSnapshotFetcher)
	if !ok {
		return nil, 0, errors.New("this IMAP connection cannot prove which mailbox generation it is deleting from")
	}
	snapshot, err := snapshotFetcher.SnapshotMailboxUIDs(ctx, account, mailbox.Name)
	if err != nil {
		return nil, 0, fmt.Errorf("list %s before emptying it: %w", mailbox.Name, err)
	}
	if snapshot.UIDValidity == 0 {
		return nil, 0, fmt.Errorf("%s reported no mailbox generation; refresh the folder and try again", mailbox.Name)
	}
	// The sync start date deliberately limits what is downloaded, never what is
	// deleted: emptying the Trash empties it, including mail this mirror never
	// held. That is why the full UID list is used here rather than Fetchable().
	if mailbox.UIDValidity > 0 && uint32(mailbox.UIDValidity) != snapshot.UIDValidity {
		log.Printf("empty trash sees a new mailbox generation account_id=%d mailbox=%q stored=%d remote=%d",
			account.ID, mailbox.Name, mailbox.UIDValidity, snapshot.UIDValidity)
	}
	return snapshot.UIDs, snapshot.UIDValidity, nil
}
