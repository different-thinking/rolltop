// File overview: Local mailbox-location refresh helpers for messages copied or moved remotely.

package syncer

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"

	"rolltop/backend/plugins"
	"rolltop/backend/store"
)

// MoveSyncRunMarker is the LatestNewFrom value a move run carries. Progress
// displays use it to tell a user-initiated move from background mirroring.
const MoveSyncRunMarker = "rolltop:move"

// moveRunConsecutiveFailureLimit ends a move that has started failing
// systemically — a refused login, a dropped connection, a revoked account —
// rather than attempting every remaining message against a server that is
// rejecting all of them. Scattered per-message failures reset the counter and
// never reach it.
const moveRunConsecutiveFailureLimit = 25

type messageMoveNotifier func(context.Context, plugins.MessageMoveContext)

func uniqueMessageIDs(ids []int64) []int64 {
	seen := map[int64]bool{}
	out := make([]int64, 0, len(ids))
	for _, id := range ids {
		if id <= 0 || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

// MoveMessages moves several local messages through IMAP and updates local metadata when each move succeeds.
func (s *Service) MoveMessages(ctx context.Context, userID int64, messageIDs []int64, destMailboxID int64) (int, error) {
	ids := uniqueMessageIDs(messageIDs)
	if len(ids) == 0 {
		return 0, errors.New("no messages selected")
	}
	moved := 0
	for _, id := range ids {
		if err := s.MoveMessage(ctx, userID, id, destMailboxID); err != nil {
			return moved, err
		}
		moved++
	}
	return moved, nil
}

// StartMoveMessages runs a large move as a background sync run so the HTTP request can return quickly.
func (s *Service) StartMoveMessages(ctx context.Context, userID int64, messageIDs []int64, destMailboxID int64, onDone func()) (store.SyncRun, error) {
	if s.Fetcher == nil {
		return store.SyncRun{}, errors.New("sync fetcher is not configured")
	}
	ids := uniqueMessageIDs(messageIDs)
	if len(ids) == 0 {
		return store.SyncRun{}, errors.New("no messages selected")
	}
	dest, err := s.Store.GetMailboxForUser(ctx, userID, destMailboxID)
	if err != nil {
		return store.SyncRun{}, err
	}
	run, err := s.Store.CreateSyncRun(ctx, userID, dest.AccountID)
	if err != nil {
		return store.SyncRun{}, err
	}
	progress := store.SyncProgress{
		MessagesTotal:    len(ids),
		MailboxesTotal:   1,
		CurrentMailbox:   "Moving to " + dest.Name,
		LatestNewFrom:    MoveSyncRunMarker,
		LatestNewSubject: "Moving messages",
	}
	if err := s.Store.UpdateSyncRunProgress(ctx, userID, run.ID, progress); err != nil {
		return store.SyncRun{}, err
	}
	s.notify(userID)
	// A batch reuses one login when the fetcher can hold a connection open. The
	// executor is nil otherwise, which moves the run back to connecting per
	// message rather than failing it.
	var executor *moveSessionExecutor
	if _, ok := s.Fetcher.(MoveSessionFetcher); ok {
		executor = &moveSessionExecutor{service: s, userID: userID, accountID: dest.AccountID}
	}
	go s.runMoveMessages(context.Background(), userID, ids, destMailboxID, dest.Name, run.ID, progress, executor, onDone)
	return run, nil
}

func (s *Service) runMoveMessages(ctx context.Context, userID int64, ids []int64, destMailboxID int64, destName string, runID int64, progress store.SyncProgress, executor *moveSessionExecutor, onDone func()) {
	status := "ok"
	errText := ""
	defer executor.close()
	defer func() {
		if ctx.Err() != nil && status == "ok" {
			status = "interrupted"
			errText = "Server stopped before this move finished."
		}
		if status == "ok" {
			progress.MailboxesDone = 1
		}
		if err := s.Store.FinishSyncRun(context.Background(), userID, runID, status, progress, errText); err != nil {
			log.Printf("finish move run user_id=%d run_id=%d: %v", userID, runID, err)
		}
		s.notify(userID)
		// A partial move still needs source/destination refreshes and must release
		// any foreground scheduler guard owned by the caller.
		if onDone != nil {
			onDone()
		}
	}()
	// A whole-filter delete resolves thousands of IDs into one run, and single
	// messages drop out of that snapshot on their own: a mailbox generation that
	// changed since, a UID the server no longer has, a folder resynced while the
	// run was working. Those belong to one message, so each is recorded and
	// stepped over. Failing the whole run on the first left every remaining
	// message where it was, with a finished run the sidebar does not show.
	failures := 0
	consecutiveFailures := 0
	firstFailure := ""
	// A typed nil would read as a usable executor once boxed, so the fallback
	// stays an untyped nil interface.
	var dispatcher MoveReceiptFetcher
	if executor != nil {
		dispatcher = executor
	}
	for _, id := range ids {
		select {
		case <-ctx.Done():
			return
		default:
		}
		var failure error
		msg, err := s.Store.GetMessageForUser(ctx, userID, id)
		switch {
		case err == nil:
			progress.CurrentMailbox = "Moving to " + destName
			progress.CurrentUID = msg.UID
			if moveErr := s.moveMessageVia(ctx, userID, id, destMailboxID, s.observeMessageMove, dispatcher); moveErr != nil {
				failure = moveErr
			} else {
				progress.MessagesStored++
			}
		case store.IsNotFound(err):
			// A row a resync has already moved or dropped has nothing left to
			// move, so it counts as handled rather than as a failure. Nothing was
			// attempted, so this costs no progress write: a re-run over thousands
			// of stale IDs would otherwise write a row per message it skips.
			progress.MessagesSeen++
			continue
		default:
			failure = err
		}
		// The move re-reads the message and its mailboxes, so the row can go away
		// between the check above and the move itself. That is the same "already
		// gone" case, not a failure, and it must not count towards the streak
		// that stops the run.
		if failure != nil && store.IsNotFound(failure) {
			if _, recheck := s.Store.GetMessageForUser(ctx, userID, id); store.IsNotFound(recheck) {
				failure = nil
			}
		}
		progress.MessagesSeen++
		if failure != nil {
			failures++
			consecutiveFailures++
			if firstFailure == "" {
				firstFailure = failure.Error()
			}
			progress.MessagesSkipped = failures
			log.Printf("move run skipped message user_id=%d run_id=%d message_id=%d: %v", userID, runID, id, failure)
		} else {
			consecutiveFailures = 0
		}
		if err := s.Store.UpdateSyncRunProgress(ctx, userID, runID, progress); err != nil {
			status = "failed"
			errText = err.Error()
			return
		}
		// Per-message progress uses the lightweight event path, like the sync
		// loop does: the completed move already invalidated the cached mail
		// pages, so this only has to move the progress indicators.
		s.notifyProgress(userID)
		if consecutiveFailures >= moveRunConsecutiveFailureLimit {
			break
		}
	}
	if failures > 0 {
		status = "failed"
		errText = moveRunFailureSummary(progress.MessagesStored, len(ids), failures,
			len(ids)-progress.MessagesSeen, firstFailure)
	}
}

// moveRunFailureSummary describes a move that left messages behind. It is the
// only account the user gets of a background move that did not finish, so it
// leads with how much of the batch actually moved and accounts for every
// message it did not: a run that stopped early never reached most of them.
func moveRunFailureSummary(moved, total, failures, notAttempted int, firstFailure string) string {
	summary := fmt.Sprintf("Moved %d of %d messages; %d could not be moved", moved, total, failures)
	if notAttempted > 0 {
		summary += fmt.Sprintf(" and %d were not attempted", notAttempted)
	}
	summary += "."
	if notAttempted > 0 {
		summary += " The move stopped early after repeated failures; run it again to continue."
	}
	if strings.TrimSpace(firstFailure) != "" {
		summary += " First failure: " + firstFailure
	}
	return summary
}

// moveSessionExecutor dispatches a run's moves over one held connection,
// opening it on first use. Every message in a run belongs to the destination's
// account, because a move across accounts is rejected before dispatch.
type moveSessionExecutor struct {
	service   *Service
	userID    int64
	accountID int64
	session   MoveSession
}

// MoveMessageWithReceipt satisfies MoveReceiptFetcher. The account is already
// fixed by the session, so the one named per message is ignored.
func (e *moveSessionExecutor) MoveMessageWithReceipt(ctx context.Context, _ store.MailAccount,
	sourceMailbox, destMailbox string, uid uint32, expectedSourceUIDValidity uint32) (*MoveReceipt, error) {
	if e.session == nil {
		opener, ok := e.service.Fetcher.(MoveSessionFetcher)
		if !ok {
			return nil, errors.New("IMAP fetcher cannot hold a connection open for a batch of moves")
		}
		account, err := e.service.Store.GetMailAccountForUser(ctx, e.userID, e.accountID)
		if err != nil {
			return nil, err
		}
		session, err := opener.OpenMoveSession(ctx, account)
		if err != nil {
			return nil, err
		}
		e.session = session
	}
	receipt, err := e.session.MoveMessageWithReceipt(ctx, sourceMailbox, destMailbox, uid, expectedSourceUIDValidity)
	if err != nil {
		// The held connection may itself be why this failed, and one dead socket
		// must not take the rest of the batch down with it. Drop it so the next
		// message starts from a fresh login.
		e.close()
	}
	return receipt, err
}

func (e *moveSessionExecutor) close() {
	if e == nil || e.session == nil {
		return
	}
	session := e.session
	e.session = nil
	if err := session.Close(); err != nil {
		log.Printf("close move session user_id=%d account_id=%d: %v", e.userID, e.accountID, err)
	}
}

// MoveMessage moves one message through IMAP using its account, source mailbox, destination mailbox, and UID.
func (s *Service) MoveMessage(ctx context.Context, userID, messageID, destMailboxID int64) error {
	return s.moveMessage(ctx, userID, messageID, destMailboxID, s.observeMessageMove)
}

func (s *Service) moveMessage(ctx context.Context, userID, messageID, destMailboxID int64, notifyMove messageMoveNotifier) error {
	return s.moveMessageVia(ctx, userID, messageID, destMailboxID, notifyMove, nil)
}

// moveMessageVia moves one message, optionally dispatching the remote command
// through a caller-owned executor. A batch passes one that holds a single
// connection open; a lone move passes nothing and connects for itself.
func (s *Service) moveMessageVia(ctx context.Context, userID, messageID, destMailboxID int64, notifyMove messageMoveNotifier, executor MoveReceiptFetcher) error {
	if s.Fetcher == nil {
		return errors.New("sync fetcher is not configured")
	}
	msg, err := s.Store.GetMessageForUser(ctx, userID, messageID)
	if err != nil {
		return err
	}
	account, err := s.Store.GetMailAccountForUser(ctx, userID, msg.AccountID)
	if err != nil {
		return err
	}
	source, err := s.Store.GetMailboxForUser(ctx, userID, msg.MailboxID)
	if err != nil {
		return err
	}
	dest, err := s.Store.GetMailboxForUser(ctx, userID, destMailboxID)
	if err != nil {
		return err
	}
	if dest.AccountID != msg.AccountID || source.AccountID != msg.AccountID || account.ID != msg.AccountID {
		return errors.New("destination mailbox does not belong to this message account")
	}
	if strings.EqualFold(strings.TrimSpace(source.Name), strings.TrimSpace(dest.Name)) {
		return nil
	}
	messageUIDValidity, err := s.Store.GetMessageUIDValidityForUser(ctx, userID, msg.ID)
	if err != nil {
		return err
	}
	if messageUIDValidity <= 0 || messageUIDValidity > int64(^uint32(0)) || source.UIDValidity <= 0 || messageUIDValidity != source.UIDValidity {
		return errors.New("message source mailbox generation changed; refresh before moving")
	}
	receiptFetcher := executor
	if receiptFetcher == nil {
		capable, ok := s.Fetcher.(MoveReceiptFetcher)
		if !ok {
			return errors.New("IMAP fetcher cannot prove the source mailbox generation for move")
		}
		receiptFetcher = capable
	}
	transfer, err := s.Store.StageMessageTransfer(ctx, userID, msg.ID, dest.ID, "move", "")
	if err != nil {
		return err
	}
	if transfer.DestinationMailboxID != dest.ID {
		return errors.New("message move is already targeting another mailbox")
	}
	if transfer.State == "succeeded" || transfer.State == "consumed" {
		return s.completeMovedMessageLocally(ctx, userID, msg, dest, transfer.ID)
	}
	if !transfer.DispatchedAt.IsZero() {
		if !messageTransferCanReconcile(transfer) {
			return errors.New("message move is already awaiting remote reconciliation")
		}
		checker, ok := s.Fetcher.(UIDValidityExistenceFetcher)
		if !ok {
			return errors.New("IMAP fetcher cannot reconcile an interrupted move")
		}
		exists, selectedUIDValidity, checkErr := checker.UIDExistsWithValidity(ctx, account, source.Name, msg.UID)
		if checkErr != nil {
			return errors.Join(errors.New("reconcile interrupted message move"), checkErr)
		}
		if selectedUIDValidity != uint32(messageUIDValidity) {
			return errors.New("message move remains pending because the source mailbox generation changed")
		}
		if !exists {
			if err := s.Store.MarkMessageTransferSucceeded(ctx, userID, transfer.ID, 0, 0); err != nil {
				return err
			}
			if notifyMove != nil {
				notifyMove(ctx, messageMoveContext(msg, source, dest))
			}
			return s.completeMovedMessageLocally(ctx, userID, msg, dest, transfer.ID)
		}
		reopened, reopenErr := s.Store.ReopenMessageTransferDispatchAfterProof(ctx, userID, transfer.ID,
			messageTransferClaim(transfer), processMessageTransferOwner)
		if reopenErr != nil {
			return reopenErr
		}
		if !reopened {
			return errors.New("message move is already awaiting remote reconciliation")
		}
	}
	claim, claimed, err := s.Store.ClaimMessageTransferDispatchForOwner(ctx, userID, transfer.ID, processMessageTransferOwner)
	if err != nil {
		return err
	}
	if !claimed {
		return errors.New("message move is already awaiting remote reconciliation")
	}
	var receipt *MoveReceipt
	receipt, err = receiptFetcher.MoveMessageWithReceipt(ctx, account, source.Name, dest.Name,
		msg.UID, uint32(messageUIDValidity))
	if err != nil {
		if !IsMoveOutcomeUnknown(err) {
			if markErr := s.Store.MarkMessageTransferFailed(ctx, userID, transfer.ID); markErr != nil {
				return errors.Join(err, markErr)
			}
		} else if finishErr := s.Store.FinishMessageTransferDispatch(ctx, userID, transfer.ID, claim); finishErr != nil {
			return errors.Join(err, finishErr)
		}
		return err
	}
	var destinationUID uint32
	var destinationUIDValidity int64
	if receipt != nil {
		destinationUID = receipt.DestinationUID
		destinationUIDValidity = int64(receipt.DestinationUIDValidity)
	}
	if err := s.Store.MarkMessageTransferSucceeded(ctx, userID, transfer.ID, destinationUID, destinationUIDValidity); err != nil {
		finishErr := s.Store.FinishMessageTransferDispatch(context.WithoutCancel(ctx), userID, transfer.ID, claim)
		if errors.Is(finishErr, store.ErrNotFound) {
			finishErr = nil
		}
		return errors.Join(err, finishErr)
	}
	if notifyMove != nil {
		notifyMove(ctx, messageMoveContext(msg, source, dest))
	}
	return s.completeMovedMessageLocally(ctx, userID, msg, dest, transfer.ID)
}

func messageMoveContext(msg store.MessageRecord, source, destination store.Mailbox) plugins.MessageMoveContext {
	bodyPreview := ""
	bodyPreviewTruncated := false
	if !msg.IsEncrypted {
		bodyPreview = store.MessageBodyPreview(msg.BodyText, store.DefaultMessageBodyPreviewBytes)
		bodyPreviewTruncated = len(bodyPreview) < len(msg.BodyText)
	}
	return plugins.MessageMoveContext{
		UserID:                 msg.UserID,
		MessageID:              msg.ID,
		MessageIDHeader:        msg.MessageIDHeader,
		ThreadKey:              msg.ThreadKey,
		AccountID:              msg.AccountID,
		SourceMailboxID:        source.ID,
		SourceMailboxName:      source.Name,
		SourceMailboxRole:      source.Role,
		DestinationMailboxID:   destination.ID,
		DestinationMailboxName: destination.Name,
		DestinationMailboxRole: destination.Role,
		UID:                    msg.UID,
		Date:                   msg.Date,
		InternalDate:           msg.InternalDate,
		From:                   msg.FromAddr,
		To:                     msg.ToAddr,
		CC:                     msg.CCAddr,
		Subject:                msg.Subject,
		BodyPreview:            bodyPreview,
		BodyPreviewTruncated:   bodyPreviewTruncated,
		HasHTML:                strings.TrimSpace(msg.BodyHTML) != "",
		IsRead:                 msg.IsRead,
		IsStarred:              msg.IsStarred,
		HasAttachments:         msg.HasAttachments,
		IsEncrypted:            msg.IsEncrypted,
		IsSigned:               msg.IsSigned,
	}
}

func (s *Service) observeMessageMove(ctx context.Context, event plugins.MessageMoveContext) {
	backendPlugins, err := s.enabledBackendPlugins(ctx)
	if err != nil {
		// Do not include the error text: loader errors can contain environment or
		// plugin-owned details that do not belong in application logs.
		log.Printf("message move observer discovery failed user_id=%d message_id=%d error_type=%T", event.UserID, event.MessageID, err)
		return
	}
	dispatchMessageMoveObservers(ctx, syncPluginHost{s: s}, backendPlugins, event)
}

func dispatchMessageMoveObservers(ctx context.Context, host plugins.BackendHost, backendPlugins []plugins.BackendPlugin, event plugins.MessageMoveContext) {
	for _, backendPlugin := range backendPlugins {
		hook, ok := backendPlugin.(plugins.MessageMoveObserver)
		if !ok {
			continue
		}
		pluginID := hook.ID()
		err, panicked := callMessageMoveObserver(ctx, hook, host, event)
		switch {
		case panicked:
			// Never log the recovered value; plugin panics can contain message data.
			log.Printf("message move observer panicked plugin_id=%q user_id=%d message_id=%d account_id=%d source_mailbox_id=%d destination_mailbox_id=%d",
				pluginID, event.UserID, event.MessageID, event.AccountID, event.SourceMailboxID, event.DestinationMailboxID)
		case err != nil && !errors.Is(err, plugins.ErrUnsupported):
			// Error type is sufficient for diagnostics without risking body or
			// credential material embedded in a plugin-owned error string.
			log.Printf("message move observer failed plugin_id=%q user_id=%d message_id=%d account_id=%d source_mailbox_id=%d destination_mailbox_id=%d error_type=%T",
				pluginID, event.UserID, event.MessageID, event.AccountID, event.SourceMailboxID, event.DestinationMailboxID, err)
		}
	}
}

func callMessageMoveObserver(ctx context.Context, hook plugins.MessageMoveObserver, host plugins.BackendHost, event plugins.MessageMoveContext) (err error, panicked bool) {
	defer func() {
		if recover() != nil {
			err = nil
			panicked = true
		}
	}()
	return hook.ObserveMessageMove(ctx, host, event), false
}

func (s *Service) completeMovedMessageLocally(ctx context.Context, userID int64, msg store.MessageRecord, destination store.Mailbox, transferID int64) error {
	if !mailboxReceivesNewMailNotifications(destination) {
		if err := s.Store.TerminalizeMessageTransferWithoutArrival(ctx, userID, transferID); err != nil {
			return err
		}
	}
	return s.cleanupMovedMessage(ctx, userID, msg)
}

func (s *Service) cleanupMovedMessage(ctx context.Context, userID int64, msg store.MessageRecord) error {
	if err := s.Store.DeleteMessageForUser(ctx, userID, msg.ID); err != nil && !store.IsNotFound(err) {
		log.Printf("cleanup moved message user_id=%d message_id=%d: %v", userID, msg.ID, err)
		return err
	}
	if s.Search != nil {
		if err := s.Search.DeleteMessage(ctx, msg.UserID, msg.ID); err != nil {
			log.Printf("cleanup moved search document user_id=%d message_id=%d: %v", userID, msg.ID, err)
		}
	}
	if _, err := s.deleteUnreferencedBlob(ctx, userID, msg.BlobID, msg.BlobPath); err != nil {
		log.Printf("cleanup moved blob record user_id=%d message_id=%d: %v", userID, msg.ID, err)
	}
	s.notify(userID)
	return nil
}
