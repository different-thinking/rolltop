// File overview: Emptying a Trash folder. This is the only operation that deletes mail
// on the IMAP server; everything else in sync moves messages between folders.

package syncer

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

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

// emptyTrashBatchAttempts bounds how often one batch is tried when the
// connection it deletes over dies. A full Trash folder is tens of batches over
// many minutes, and a mail host dropping that connection partway through is
// ordinary rather than exceptional. Repeating a batch is safe: it names the
// same UIDs, and a UID the server has already removed reads back as gone rather
// than as a failure.
const emptyTrashBatchAttempts = 3

// emptyTrashBatchGiveUp is how many batches in a row may exhaust their attempts
// before the purge stops. One batch the server will not take must not strand
// the thousands of messages behind it; a connection that is dead for good must
// not be logged into again once per remaining batch.
const emptyTrashBatchGiveUp = 2

// defaultEmptyTrashRetryDelay is the wait before the second attempt on a batch,
// doubling for the one after it. A host that closed the connection because the
// purge was going too fast for it must not be met with an immediate reconnect,
// and a throttle that lasts a few seconds must not end the purge.
const defaultEmptyTrashRetryDelay = 2 * time.Second

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
//
// releaseForeground, when set, is called once the remote deletion is settled
// (deleted, failed, or interrupted) and before local cleanup starts — freeing
// whatever exclusive foreground slot the caller is holding so it does not also
// block every other foreground mail action (sending, moving) for however long
// a large folder's local reconciliation takes. onDone runs once at the very
// end, after that reconciliation, for work that wants the finished state — a
// mailbox listing refresh, for instance.
func (s *Service) StartEmptyTrash(ctx context.Context, userID, mailboxID int64, releaseForeground, onDone func()) (store.SyncRun, error) {
	return s.startTrashPurge(ctx, userID, mailboxID, time.Time{}, releaseForeground, onDone)
}

// StartTrashRetentionPurge deletes the mail one Trash folder has held since
// before a cutoff, and leaves everything newer in place. It is the scheduled
// half of the same operation StartEmptyTrash performs by hand, and it deletes
// on the server in exactly the same way: the folder is listed live, its
// generation is proved, and local rows follow only for the UIDs the server
// reports gone.
//
// What it selects is narrower, and only in the safe direction. The messages are
// the ones this mirror recorded arriving in that folder before the cutoff
// (Store.ListTrashRetentionUIDs), intersected with what the folder actually
// holds now. Mail the mirror has never seen has no measurable stay and is left
// alone; emptying the folder by hand still takes all of it.
func (s *Service) StartTrashRetentionPurge(ctx context.Context, userID, mailboxID int64, before time.Time, releaseForeground, onDone func()) (store.SyncRun, error) {
	if before.IsZero() {
		return store.SyncRun{}, errors.New("a retention purge needs the moment to delete before")
	}
	return s.startTrashPurge(ctx, userID, mailboxID, before, releaseForeground, onDone)
}

// startTrashPurge is both of the above. A zero cutoff empties the folder; a
// cutoff narrows it to the mail that has been there long enough.
func (s *Service) startTrashPurge(ctx context.Context, userID, mailboxID int64, before time.Time, releaseForeground, onDone func()) (store.SyncRun, error) {
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
	label := "Emptying " + mailbox.Name
	subject := "Deleting messages on the server"
	if !before.IsZero() {
		label = "Clearing old mail from " + mailbox.Name
		subject = "Deleting mail this folder has kept long enough"
	}
	progress := store.SyncProgress{
		MailboxesTotal:   1,
		CurrentMailbox:   label,
		LatestNewFrom:    EmptyTrashSyncRunMarker,
		LatestNewSubject: subject,
	}
	if err := s.Store.UpdateSyncRunProgress(ctx, userID, run.ID, progress); err != nil {
		s.failSyncRunInit(userID, run.ID, progress, err)
		return store.SyncRun{}, err
	}
	s.notify(userID)
	go s.runEmptyTrash(s.backgroundContext(), userID, account, mailbox, run.ID, before, progress, releaseForeground, onDone)
	return run, nil
}

func (s *Service) runEmptyTrash(ctx context.Context, userID int64, account store.MailAccount, mailbox store.Mailbox,
	runID int64, before time.Time, progress store.SyncProgress, releaseForeground, onDone func()) {
	ctx, finishRun := s.beginRunCancellation(ctx, userID, runID)
	defer finishRun()
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
	deleted, err := s.emptyTrashRemotely(ctx, userID, account, mailbox, runID, before, &progress)
	if err != nil {
		status = "failed"
		errText = err.Error()
		// Whatever did go is still gone remotely, so the local mirror is cleaned
		// up for it either way. A failure here only means the folder is not empty.
		log.Printf("empty trash user_id=%d run_id=%d mailbox_id=%d deleted=%d: %v", userID, runID, mailbox.ID, deleted, err)
	}
	// The remote side is settled either way: what follows only touches the local
	// mirror. Releasing here, rather than holding it through however long that
	// mirror cleanup takes, is what keeps a large purge from also blocking every
	// other foreground mail action — sending, moving — for its whole duration.
	if releaseForeground != nil {
		releaseForeground()
	}
	if deleted == 0 {
		return
	}
	// One reconciliation removes the local rows, search documents, and blobs for
	// everything the server confirmed gone. Reusing the sync path keeps a single
	// definition of what "this message is no longer on the server" does locally.
	//
	// This is its own transaction pair per message, so a large Trash folder can
	// spend minutes here after every batch has already reported 100% deleted.
	// Without a heartbeat the run's own progress row goes stale for that whole
	// stretch: the sidebar looks frozen, and a run stuck past the stale-run
	// reconciler's window can be torn down out from under a goroutine that is
	// still working. The label changes so a user watching it can tell this is a
	// second phase, not the same batch stuck in place.
	progress.CurrentMailbox = "Cleaning up " + mailbox.Name
	reporter := s.syncProgressReporter(userID, runID, &progress)
	if err := reporter.commit(ctx); err != nil {
		log.Printf("empty trash cleanup progress user_id=%d run_id=%d mailbox_id=%d: %v", userID, runID, mailbox.ID, err)
	}
	if err := s.reconcileMailboxUIDs(ctx, userID, account, mailbox, func(hbCtx context.Context) error {
		if stepErr := reporter.step(hbCtx); stepErr != nil {
			if hbCtx.Err() != nil {
				return hbCtx.Err()
			}
			log.Printf("empty trash cleanup progress user_id=%d run_id=%d mailbox_id=%d: %v", userID, runID, mailbox.ID, stepErr)
		}
		return nil
	}); err != nil {
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
	runID int64, before time.Time, progress *store.SyncProgress) (int, error) {
	expunger, ok := s.Fetcher.(ExpungeFetcher)
	if !ok {
		return 0, ErrEmptyTrashUnsupported
	}
	uids, uidValidity, err := s.trashPurgeTargets(ctx, userID, account, mailbox, before)
	if err != nil {
		return 0, err
	}
	// A purge that named a cutoff means the UIDs it named and no others, so it
	// may not fall back to the expunge that takes everything flagged \Deleted
	// in the folder. Emptying the folder is the one case where that fallback is
	// what was asked for.
	scope := ExpungeWholeFolder
	if !before.IsZero() {
		scope = ExpungeNamedUIDsOnly
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
	// Closed again below before anything else logs in to this account, and here
	// as well so an early return cannot leave the connection held.
	defer session.close()
	deleted := 0
	failedInARow := 0
	var batchErr error
	for start := 0; start < len(uids); start += emptyTrashBatchSize {
		if err := ctx.Err(); err != nil {
			return deleted, err
		}
		end := min(start+emptyTrashBatchSize, len(uids))
		gone, err := s.expungeTrashBatch(ctx, session, expunger, userID, account, mailbox.Name, uids[start:end], uidValidity, scope)
		deleted += len(gone)
		progress.MessagesSeen += end - start
		progress.MessagesStored = deleted
		progress.MessagesSkipped = progress.MessagesSeen - deleted
		if err != nil {
			if ctx.Err() != nil {
				return deleted, err
			}
			// One batch the server would not take is not the whole folder. Carry
			// on so the rest is still emptied, and stop only once nothing is
			// getting through any more: giving up on the first dropped
			// connection leaves most of a large Trash folder behind.
			batchErr = err
			// A server that cannot expunge selectively will answer every
			// remaining batch the same way, and answering it once is the point:
			// the purge stops with mail intact rather than widening itself.
			if errors.Is(err, ErrSelectiveExpungeUnsupported) {
				break
			}
			failedInARow++
			log.Printf("empty trash batch failed user_id=%d run_id=%d mailbox_id=%d uids=%d-%d of %d: %v",
				userID, runID, mailbox.ID, start+1, end, len(uids), err)
			if failedInARow >= emptyTrashBatchGiveUp {
				break
			}
			continue
		}
		failedInARow = 0
		if err := s.updateSyncProgress(ctx, userID, runID, *progress); err != nil {
			return deleted, err
		}
	}
	// Give the connection back before the folder is listed again: a second
	// concurrent login to the same account is what holding one exists to avoid.
	session.close()
	// A capability the server does not have is the one failure that flagged
	// nothing, so there is no residue to recount and no folder listing worth
	// spending on it: what the batches reported is already the whole story.
	if batchErr != nil && !errors.Is(batchErr, ErrSelectiveExpungeUnsupported) {
		// What the batches confirmed is no longer the whole story. A batch that
		// failed after flagging its messages leaves them carrying \Deleted, and
		// on a server without UIDPLUS the only expunge available removes
		// everything so flagged — so a later batch may well have taken a failed
		// one with it, unverified and uncounted. Ask the folder what it still
		// holds rather than reporting the sum of the batches, which would tell a
		// user that messages the server did delete could not be deleted.
		//
		// A failed batch may also be left carrying \Deleted with nothing to
		// remove it. That is accepted here and nowhere else: those messages are
		// the ones the user asked to delete, in the folder they asked to empty,
		// and the next purge flags and expunges them again. What must not be
		// accepted is reporting them wrongly, which is what this recount is for.
		if kept, err := s.trashMessagesStillHeld(ctx, account, mailbox, uids, uidValidity); err != nil {
			log.Printf("recount emptied trash user_id=%d run_id=%d mailbox_id=%d: %v", userID, runID, mailbox.ID, err)
		} else {
			deleted = len(uids) - kept
			progress.MessagesStored = deleted
			progress.MessagesSkipped = progress.MessagesSeen - deleted
		}
	}
	if deleted < len(uids) {
		if batchErr != nil {
			return deleted, fmt.Errorf("%d of %d messages in %s could not be deleted: %w",
				len(uids)-deleted, len(uids), mailbox.Name, batchErr)
		}
		return deleted, fmt.Errorf("the server kept %d of %d messages in %s", len(uids)-deleted, len(uids), mailbox.Name)
	}
	return deleted, nil
}

// trashPurgeTargets names the UIDs one purge is going to delete, bound to the
// generation they were listed under.
//
// Without a cutoff that is the folder as the server currently holds it. With
// one it is the intersection of that live listing with the messages this mirror
// recorded arriving in the folder before the cutoff: the mirror is the only
// side that knows how long a message has been in the Trash, and the live
// listing is what proves the UIDs still mean what the mirror thinks and still
// belong to the generation being deleted from. Taking either one alone would be
// wrong in a different direction -- the mirror alone would delete by UIDs the
// server may have reassigned, and the server alone cannot tell old mail from
// mail thrown away this morning.
func (s *Service) trashPurgeTargets(ctx context.Context, userID int64, account store.MailAccount, mailbox store.Mailbox,
	before time.Time) ([]uint32, uint32, error) {
	uids, uidValidity, err := s.trashSnapshot(ctx, account, mailbox)
	if err != nil {
		return nil, 0, err
	}
	if before.IsZero() || len(uids) == 0 {
		return uids, uidValidity, nil
	}
	due, err := s.Store.ListTrashRetentionUIDs(ctx, userID, mailbox.ID, before, uidValidity)
	if err != nil {
		return nil, 0, err
	}
	if len(due) == 0 {
		return nil, uidValidity, nil
	}
	held := make(map[uint32]struct{}, len(uids))
	for _, uid := range uids {
		held[uid] = struct{}{}
	}
	out := make([]uint32, 0, len(due))
	for _, candidate := range due {
		if _, still := held[candidate.UID]; still {
			out = append(out, candidate.UID)
		}
	}
	return out, uidValidity, nil
}

// trashMessagesStillHeld counts how many of the messages a purge set out to
// delete the folder still holds. It is the truthful answer where the per-batch
// confirmations are not: batches are only independent of one another on a
// server with UIDPLUS.
//
// A folder that has been recreated since the purge started answers nothing
// about those UIDs, so it is an error rather than a count of zero.
func (s *Service) trashMessagesStillHeld(ctx context.Context, account store.MailAccount, mailbox store.Mailbox,
	uids []uint32, uidValidity uint32) (int, error) {
	current, currentValidity, err := s.trashSnapshot(ctx, account, mailbox)
	if err != nil {
		return 0, err
	}
	if currentValidity != uidValidity {
		return 0, fmt.Errorf("%s is now generation %d, not the %d this purge deleted from",
			mailbox.Name, currentValidity, uidValidity)
	}
	held := make(map[uint32]struct{}, len(current))
	for _, uid := range current {
		held[uid] = struct{}{}
	}
	kept := 0
	for _, uid := range uids {
		if _, still := held[uid]; still {
			kept++
		}
	}
	return kept, nil
}

// expungeTrashBatch deletes one batch, trying again on a fresh login when the
// connection it was deleting over died. The held connection is dropped by the
// failing attempt itself, so the next one starts from a new login rather than a
// dead socket.
func (s *Service) expungeTrashBatch(ctx context.Context, session *expungeSessionHolder, expunger ExpungeFetcher,
	userID int64, account store.MailAccount, mailbox string, uids []uint32, uidValidity uint32, scope ExpungeScope) ([]uint32, error) {
	var lastErr error
	for attempt := 1; attempt <= emptyTrashBatchAttempts; attempt++ {
		gone, err := session.expunge(ctx, expunger, account, mailbox, uids, uidValidity, scope)
		if err == nil {
			return gone, nil
		}
		lastErr = err
		// A fresh login answers a missing capability exactly as this one did.
		if errors.Is(err, ErrSelectiveExpungeUnsupported) {
			return nil, lastErr
		}
		if ctx.Err() != nil || attempt == emptyTrashBatchAttempts {
			return nil, lastErr
		}
		log.Printf("retry empty trash batch user_id=%d account_id=%d mailbox=%q messages=%d attempt=%d/%d: %v",
			userID, account.ID, mailbox, len(uids), attempt, emptyTrashBatchAttempts, err)
		if err := s.pauseBeforeEmptyTrashRetry(ctx, attempt); err != nil {
			return nil, lastErr
		}
	}
	return nil, lastErr
}

// pauseBeforeEmptyTrashRetry waits between the attempts on one batch, backing
// off further each time, and reports the caller's cancellation rather than
// sleeping through it.
func (s *Service) pauseBeforeEmptyTrashRetry(ctx context.Context, attempt int) error {
	delay := defaultEmptyTrashRetryDelay
	if s != nil && s.emptyTrashRetryDelay > 0 {
		delay = s.emptyTrashRetryDelay
	}
	for i := 1; i < attempt; i++ {
		delay *= 2
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
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
	mailbox string, uids []uint32, expectedUIDValidity uint32, scope ExpungeScope) ([]uint32, error) {
	if h == nil {
		return expunger.ExpungeMessages(ctx, account, mailbox, uids, expectedUIDValidity, scope)
	}
	if h.session == nil {
		opener, ok := h.service.Fetcher.(ExpungeSessionFetcher)
		if !ok {
			return expunger.ExpungeMessages(ctx, account, mailbox, uids, expectedUIDValidity, scope)
		}
		session, err := opener.OpenExpungeSession(ctx, h.account)
		if err != nil {
			return nil, err
		}
		h.session = session
	}
	gone, err := h.session.ExpungeMessages(ctx, mailbox, uids, expectedUIDValidity, scope)
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
