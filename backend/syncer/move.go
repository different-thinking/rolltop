// File overview: Local mailbox-location refresh helpers for messages copied or moved remotely.

package syncer

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

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

// moveRunBatchSize bounds how many messages one IMAP command moves. It stays
// below the client's own per-request ceiling so a whole-filter delete is moved
// in steps the server will accept, and low enough that progress keeps moving
// while it happens.
const moveRunBatchSize = 250

// moveRunBatchBytes bounds a gathered batch by the message text it is holding
// rather than only by how many messages that is. Mail sizes span four orders of
// magnitude, so a count-only limit would let one folder of large messages decide
// how much memory a move needs.
const moveRunBatchBytes = 8 << 20

// moveRunProgressInterval bounds how often a run writes its progress. A move
// that used to cost a database write and a broadcast per message now reports on
// a clock instead, so the indicators still move without the reporting outlasting
// the moves it reports.
const moveRunProgressInterval = 500 * time.Millisecond

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
//
// It runs under the same held connection and batched commands a background move
// run uses, so a handful of messages costs one login rather than one per
// message. The first failure still ends it: the caller answers a request that
// promised the whole selection.
func (s *Service) MoveMessages(ctx context.Context, userID int64, messageIDs []int64, destMailboxID int64) (int, error) {
	ids := uniqueMessageIDs(messageIDs)
	if len(ids) == 0 {
		return 0, errors.New("no messages selected")
	}
	dest, err := s.Store.GetMailboxForUser(ctx, userID, destMailboxID)
	if err != nil {
		return 0, err
	}
	executor := s.openMoveSessionExecutor(userID, dest.AccountID)
	defer executor.close()
	moved := 0
	var failure error
	walkErr := s.moveMessagesInBatches(ctx, userID, ids, destMailboxID, s.observeMessageMove, executor.dispatcher(),
		func(result moveMessageResult) bool {
			if result.Err != nil {
				if failure == nil {
					failure = result.Err
				}
				return false
			}
			if !result.Vanished {
				moved++
			}
			return true
		})
	if failure != nil {
		return moved, failure
	}
	return moved, walkErr
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
	executor := s.openMoveSessionExecutor(userID, dest.AccountID)
	go s.runMoveMessages(context.Background(), userID, ids, destMailboxID, dest.Name, run.ID, progress, executor, onDone)
	return run, nil
}

func (s *Service) runMoveMessages(ctx context.Context, userID int64, ids []int64, destMailboxID int64, destName string, runID int64, progress store.SyncProgress, executor *moveSessionExecutor, onDone func()) {
	status := "ok"
	errText := ""
	defer func() {
		// Give the held connection back before the run reports completion.
		// Finishing releases the caller's foreground reservation and queues the
		// destination refresh, and neither should open a second connection to
		// this account while this run still owns one.
		executor.close()
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
	progress.CurrentMailbox = "Moving to " + destName
	// Progress used to be written and broadcast once per message. A run that
	// moves thousands then spends thousands of database writes and progress
	// events describing work that now costs a share of one IMAP command, and the
	// writes outlast the moves they report. The tally is kept per message and
	// published on an interval instead, so the indicators still move while the
	// run works without the run paying for each step of it.
	unpublished := false
	progressWriteFailed := false
	lastPublished := time.Now()
	publish := func(force bool) bool {
		if !unpublished || (!force && time.Since(lastPublished) < moveRunProgressInterval) {
			return true
		}
		unpublished = false
		lastPublished = time.Now()
		if err := s.Store.UpdateSyncRunProgress(ctx, userID, runID, progress); err != nil {
			status = "failed"
			errText = err.Error()
			progressWriteFailed = true
			return false
		}
		// Progress uses the lightweight event path, like the sync loop does: the
		// completed moves already invalidated the cached mail pages, so this only
		// has to move the progress indicators.
		s.notifyProgress(userID)
		return true
	}
	walkErr := s.moveMessagesInBatches(ctx, userID, ids, destMailboxID, s.observeMessageMove, executor.dispatcher(),
		func(result moveMessageResult) bool {
			progress.MessagesSeen++
			if result.Vanished {
				// Nothing was attempted, so this costs no progress write: a re-run
				// over thousands of stale IDs would otherwise report every message
				// it steps over, and none of them moved.
				return consecutiveFailures < moveRunConsecutiveFailureLimit
			}
			if result.UID > 0 {
				progress.CurrentUID = result.UID
			}
			unpublished = true
			switch {
			case result.Err != nil:
				failures++
				consecutiveFailures++
				if firstFailure == "" {
					firstFailure = result.Err.Error()
				}
				progress.MessagesSkipped = failures
				log.Printf("move run skipped message user_id=%d run_id=%d message_id=%d: %v",
					userID, runID, result.MessageID, result.Err)
			default:
				progress.MessagesStored++
				consecutiveFailures = 0
			}
			return publish(false) && consecutiveFailures < moveRunConsecutiveFailureLimit
		})
	// A run whose progress could not be written has already said so; nothing
	// below may overwrite that with an account of the moves themselves.
	if progressWriteFailed || !publish(true) {
		return
	}
	if walkErr != nil {
		status = "failed"
		errText = walkErr.Error()
		return
	}
	if failures > 0 {
		status = "failed"
		errText = moveRunFailureSummary(progress.MessagesStored, len(ids), failures,
			len(ids)-progress.MessagesSeen, firstFailure)
	}
}

// moveMessageResult is what one message in a move walk turned into. A vanished
// row was gone before anything was attempted, which is handled rather than
// failed: it is not a message this run moved and not one it left behind.
type moveMessageResult struct {
	MessageID int64
	UID       uint32
	Vanished  bool
	Err       error
}

// moveMessagesInBatches walks message IDs in order, gathers the ones leaving the
// same source mailbox generation, and moves each gathering with a single IMAP
// command. record sees every message's own outcome, in the order the IDs were
// given, and returns false to stop the walk.
//
// Batching is what makes a large move finish. Each message used to cost its own
// SELECT, UID SEARCH and UID MOVE — three network round trips, repeated
// thousands of times against a server that is also rate limiting them. A batch
// pays those three once for up to moveRunBatchSize messages. A dispatcher that
// cannot batch keeps the old rhythm rather than losing anything: it gathers one
// message at a time, so a run that starts failing systemically still gives up
// after the same number of attempts it always did.
func (s *Service) moveMessagesInBatches(ctx context.Context, userID int64, ids []int64, destMailboxID int64,
	notifyMove messageMoveNotifier, executor MoveReceiptFetcher, record func(moveMessageResult) bool) error {
	if len(ids) == 0 {
		return nil
	}
	dispatcher, err := s.moveDispatcher(executor)
	if err != nil {
		return err
	}
	batchSize := 1
	if _, ok := dispatcher.(batchMoveFetcher); ok {
		batchSize = moveRunBatchSize
		// One command names one source mailbox, and a selection made in All Mail
		// interleaves folders by date. Walking that order would gather one
		// message per command and give the batching away, so the run visits the
		// selection a folder at a time.
		ids = s.groupMessageIDsBySourceMailbox(ctx, userID, ids)
	}
	announce := s.moveAnnouncer(userID, false)
	defer announce.flush()
	batch := make([]*preparedMove, 0, batchSize)
	batchUIDs := make(map[uint32]struct{}, batchSize)
	batchBytes := 0
	keepGoing := true
	// flush moves everything gathered so far and reports each message's outcome.
	// Whatever it dispatched is always applied and always recorded, even once
	// record has asked to stop: a claimed dispatch nobody records leaves a
	// transfer the next attempt has to reconcile against the server.
	flush := func() {
		if len(batch) == 0 {
			return
		}
		dispatched := batch
		batch = make([]*preparedMove, 0, batchSize)
		batchUIDs = make(map[uint32]struct{}, batchSize)
		batchBytes = 0
		for i, failure := range s.dispatchPreparedMoves(ctx, userID, dispatcher, dispatched, notifyMove, announce) {
			prepared := dispatched[i]
			if !record(s.moveResultFor(ctx, userID, prepared.messageID, prepared.msg.UID, failure)) {
				keepGoing = false
			}
		}
		// One announcement for the batch, after every one of its messages has
		// been settled locally.
		announce.flush()
	}
	for _, id := range ids {
		if !keepGoing {
			break
		}
		select {
		case <-ctx.Done():
			flush()
			return nil
		default:
		}
		prepared, err := s.prepareMessageMove(ctx, userID, id, destMailboxID, notifyMove, announce)
		if prepared == nil {
			// This message is already settled — moved by an earlier attempt,
			// already where it was asked to go, or gone. Its outcome belongs
			// ahead of whatever is still gathered.
			flush()
			if !record(s.moveResultFor(ctx, userID, id, 0, err)) {
				keepGoing = false
			}
			continue
		}
		// One command names one source mailbox generation, and names each UID
		// once. A gathering that cannot take this message moves first.
		_, duplicateUID := batchUIDs[prepared.msg.UID]
		if len(batch) > 0 && (duplicateUID || !batch[0].sharesSourceGeneration(prepared)) {
			flush()
		}
		batch = append(batch, prepared)
		batchUIDs[prepared.msg.UID] = struct{}{}
		batchBytes += prepared.textBytes()
		// A message already claimed for dispatch is never left gathered: if the
		// walk is stopping, it moves now rather than being abandoned mid-transfer.
		if len(batch) >= batchSize || batchBytes >= moveRunBatchBytes || !keepGoing {
			flush()
		}
	}
	flush()
	return nil
}

// groupMessageIDsBySourceMailbox reorders a selection so messages leaving the
// same folder sit together, keeping the caller's order within each folder and
// leaving ids it cannot place where they were. It is a stable regrouping, not a
// filter: every id the caller gave comes back exactly once.
func (s *Service) groupMessageIDsBySourceMailbox(ctx context.Context, userID int64, ids []int64) []int64 {
	if len(ids) < 2 {
		return ids
	}
	mailboxes, err := s.Store.ListMessageSourceMailboxesForUser(ctx, userID, ids)
	if err != nil {
		// Grouping only decides how well the run batches, so a failed lookup
		// costs speed and nothing else. The walk still moves every message.
		log.Printf("group move by source mailbox user_id=%d messages=%d: %v", userID, len(ids), err)
		return ids
	}
	var order []int64
	grouped := map[int64][]int64{}
	var unplaced []int64
	for _, id := range ids {
		mailboxID, known := mailboxes[id]
		if !known {
			// A row that is already gone has no folder to be grouped under. It
			// still has to be walked, so the run can report it as handled.
			unplaced = append(unplaced, id)
			continue
		}
		if _, seen := grouped[mailboxID]; !seen {
			order = append(order, mailboxID)
		}
		grouped[mailboxID] = append(grouped[mailboxID], id)
	}
	out := make([]int64, 0, len(ids))
	for _, mailboxID := range order {
		out = append(out, grouped[mailboxID]...)
	}
	return append(out, unplaced...)
}

// moveResultFor turns one message's failure into the walk's view of it. A row
// that is really gone was moved or dropped by something else and has nothing
// left to move, so it counts as handled — but proving that costs a read, so it
// is only checked for a failure that says the row was missing.
func (s *Service) moveResultFor(ctx context.Context, userID, messageID int64, uid uint32, failure error) moveMessageResult {
	result := moveMessageResult{MessageID: messageID, UID: uid, Err: failure}
	if failure == nil || !store.IsNotFound(failure) {
		return result
	}
	if _, recheck := s.Store.GetMessageForUser(ctx, userID, messageID); store.IsNotFound(recheck) {
		return moveMessageResult{MessageID: messageID, UID: uid, Vanished: true}
	}
	return result
}

// dispatchPreparedMoves runs the remote half for a batch of prepared moves and
// records what the server did with each. It returns one entry per prepared
// move, in order, nil where the message moved.
func (s *Service) dispatchPreparedMoves(ctx context.Context, userID int64, dispatcher MoveReceiptFetcher,
	batch []*preparedMove, notifyMove messageMoveNotifier, announce *moveAnnouncer) []error {
	failures := make([]error, len(batch))
	if len(batch) == 0 {
		return failures
	}
	outcomes, batchErr := s.moveBatchOutcomes(ctx, dispatcher, batch)
	for i, prepared := range batch {
		var receipt *MoveReceipt
		failure := batchErr
		if batchErr == nil {
			receipt, failure = outcomes[i].Receipt, outcomes[i].Err
		}
		failures[i] = s.applyMoveOutcome(ctx, userID, prepared, receipt, failure, notifyMove, announce)
	}
	return failures
}

// moveBatchOutcomes issues the remote command and returns one outcome per
// prepared move, in order. A non-nil error means the command did not run at
// all, which is one answer for every message in the batch.
func (s *Service) moveBatchOutcomes(ctx context.Context, dispatcher MoveReceiptFetcher, batch []*preparedMove) ([]MoveOutcome, error) {
	batcher, canBatch := dispatcher.(batchMoveFetcher)
	if !canBatch || len(batch) == 1 {
		return s.moveOneAtATimeOutcomes(ctx, dispatcher, batch), nil
	}
	uids := make([]uint32, len(batch))
	for i, prepared := range batch {
		uids[i] = prepared.msg.UID
	}
	head := batch[0]
	moved, err := batcher.MoveMessagesWithReceipts(ctx, head.account, head.source.Name, head.dest.Name,
		uids, head.sourceUIDValidity)
	if errors.Is(err, errMoveSessionCannotBatch) {
		// The connection this run holds cannot name a set in one command. That
		// is a capability answer, not a failed move, so the batch is repeated one
		// message at a time rather than recorded as refused.
		return s.moveOneAtATimeOutcomes(ctx, dispatcher, batch), nil
	}
	if err != nil {
		return nil, err
	}
	outcomes := make([]MoveOutcome, len(batch))
	byUID := make(map[uint32]MoveOutcome, len(moved))
	for _, outcome := range moved {
		byUID[outcome.UID] = outcome
	}
	for i, prepared := range batch {
		outcome, answered := byUID[prepared.msg.UID]
		if !answered {
			// The server's answer did not account for this UID. Nothing proves it
			// stayed and nothing proves it moved, so it is left for reconciliation
			// rather than recorded as either.
			outcome = MoveOutcome{
				UID: prepared.msg.UID,
				Err: MoveOutcomeUnknown(fmt.Errorf("move of UID %d was not accounted for in the batch response", prepared.msg.UID)),
			}
		}
		outcomes[i] = outcome
	}
	return outcomes, nil
}

// moveOneAtATimeOutcomes issues a command per message. It is what a dispatcher
// without the batch capability does, and what a batch falls back to when the
// held connection turns out not to have it.
func (s *Service) moveOneAtATimeOutcomes(ctx context.Context, dispatcher MoveReceiptFetcher, batch []*preparedMove) []MoveOutcome {
	outcomes := make([]MoveOutcome, len(batch))
	for i, prepared := range batch {
		receipt, err := dispatcher.MoveMessageWithReceipt(ctx, prepared.account, prepared.source.Name,
			prepared.dest.Name, prepared.msg.UID, prepared.sourceUIDValidity)
		outcomes[i] = MoveOutcome{UID: prepared.msg.UID, Receipt: receipt, Err: err}
	}
	return outcomes
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

// openMoveSessionExecutor returns the executor a batch of moves dispatches
// through, or nil when this deployment's fetcher cannot hold a connection open
// — which moves the batch back to connecting per message rather than failing it.
func (s *Service) openMoveSessionExecutor(userID, accountID int64) *moveSessionExecutor {
	if s == nil || s.Fetcher == nil {
		return nil
	}
	if _, ok := s.Fetcher.(MoveSessionFetcher); !ok {
		return nil
	}
	return &moveSessionExecutor{service: s, userID: userID, accountID: accountID}
}

// dispatcher hands the executor to a caller that takes an interface. A typed
// nil would read as a usable executor once boxed, so a missing one stays an
// untyped nil interface.
func (e *moveSessionExecutor) dispatcher() MoveReceiptFetcher {
	if e == nil {
		return nil
	}
	return e
}

// batchMoveFetcher is the optional capability behind a batched dispatch: it
// moves several UIDs out of one source mailbox generation with one command.
type batchMoveFetcher interface {
	MoveMessagesWithReceipts(ctx context.Context, account store.MailAccount, sourceMailbox string, destMailbox string,
		uids []uint32, expectedSourceUIDValidity uint32) ([]MoveOutcome, error)
}

// open returns the executor's connection, logging in on first use.
func (e *moveSessionExecutor) open(ctx context.Context) (MoveSession, error) {
	if e.session != nil {
		return e.session, nil
	}
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
	return session, nil
}

// MoveMessageWithReceipt satisfies MoveReceiptFetcher. The account is already
// fixed by the session, so the one named per message is ignored.
func (e *moveSessionExecutor) MoveMessageWithReceipt(ctx context.Context, _ store.MailAccount,
	sourceMailbox, destMailbox string, uid uint32, expectedSourceUIDValidity uint32) (*MoveReceipt, error) {
	session, err := e.open(ctx)
	if err != nil {
		return nil, err
	}
	receipt, err := session.MoveMessageWithReceipt(ctx, sourceMailbox, destMailbox, uid, expectedSourceUIDValidity)
	if err != nil {
		// The held connection may itself be why this failed, and one dead socket
		// must not take the rest of the batch down with it. Drop it so the next
		// message starts from a fresh login.
		e.close()
	}
	return receipt, err
}

// MoveMessagesWithReceipts satisfies batchMoveFetcher when the held connection
// can move a whole set with one command. A session without that capability
// reports it, and the caller falls back to one command per message.
func (e *moveSessionExecutor) MoveMessagesWithReceipts(ctx context.Context, _ store.MailAccount,
	sourceMailbox, destMailbox string, uids []uint32, expectedSourceUIDValidity uint32) ([]MoveOutcome, error) {
	session, err := e.open(ctx)
	if err != nil {
		return nil, err
	}
	batcher, ok := session.(BatchMoveSession)
	if !ok {
		return nil, errMoveSessionCannotBatch
	}
	outcomes, err := batcher.MoveMessagesWithReceipts(ctx, sourceMailbox, destMailbox, uids, expectedSourceUIDValidity)
	if err != nil {
		// The held connection may itself be why this failed, and one dead socket
		// must not take the rest of the run down with it. Drop it so the next
		// batch starts from a fresh login.
		e.close()
	}
	return outcomes, err
}

// errMoveSessionCannotBatch reports a held connection that cannot move a set in
// one command. It is a capability answer rather than a failed move, so the
// caller repeats the batch one message at a time instead of recording it.
var errMoveSessionCannotBatch = errors.New("IMAP move session cannot move a batch with one command")

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
	dispatcher, err := s.moveDispatcher(executor)
	if err != nil {
		return err
	}
	announce := s.moveAnnouncer(userID, true)
	prepared, err := s.prepareMessageMove(ctx, userID, messageID, destMailboxID, notifyMove, announce)
	if prepared == nil {
		return err
	}
	receipt, moveErr := dispatcher.MoveMessageWithReceipt(ctx, prepared.account, prepared.source.Name,
		prepared.dest.Name, prepared.msg.UID, prepared.sourceUIDValidity)
	return s.applyMoveOutcome(ctx, userID, prepared, receipt, moveErr, notifyMove, announce)
}

// moveDispatcher resolves what will issue the remote command: a caller-owned
// executor holding one connection, or the fetcher connecting for itself. A
// fetcher that cannot report the destination UID cannot move at all, and that
// is answered before anything is staged.
func (s *Service) moveDispatcher(executor MoveReceiptFetcher) (MoveReceiptFetcher, error) {
	if s.Fetcher == nil {
		return nil, errors.New("sync fetcher is not configured")
	}
	if executor != nil {
		return executor, nil
	}
	capable, ok := s.Fetcher.(MoveReceiptFetcher)
	if !ok {
		return nil, errors.New("IMAP fetcher cannot prove the source mailbox generation for move")
	}
	return capable, nil
}

// preparedMove is one message that has passed every local check and owns its
// dispatch claim. All that is left is the remote command and recording what it
// did, which is what lets many messages share one command.
type preparedMove struct {
	messageID         int64
	msg               store.MessageRecord
	account           store.MailAccount
	source            store.Mailbox
	dest              store.Mailbox
	sourceUIDValidity uint32
	transfer          store.MessageTransfer
	claim             store.MessageTransferDispatchClaim
}

// textBytes is how much message text this prepared move is holding. A gathering
// is bounded by the sum of them as well as by how many messages it has.
func (p *preparedMove) textBytes() int {
	return len(p.msg.BodyText) + len(p.msg.BodyHTML)
}

// sharesSourceGeneration reports whether both messages leave the same mailbox
// under the same generation, which is what one UID MOVE command can name.
func (p *preparedMove) sharesSourceGeneration(other *preparedMove) bool {
	return p.source.ID == other.source.ID && p.dest.ID == other.dest.ID &&
		p.sourceUIDValidity == other.sourceUIDValidity && p.account.ID == other.account.ID
}

// prepareMessageMove takes one message as far as it can go without talking to
// the server: it proves the move is allowed, stages the transfer, settles a
// transfer an earlier attempt left behind, and claims the dispatch.
//
// A nil prepared move means nothing is left to dispatch — the message is
// already where it was asked to go, an earlier attempt already moved it, or the
// error says why it cannot be moved at all.
func (s *Service) prepareMessageMove(ctx context.Context, userID, messageID, destMailboxID int64,
	notifyMove messageMoveNotifier, announce *moveAnnouncer) (*preparedMove, error) {
	if s.Fetcher == nil {
		return nil, errors.New("sync fetcher is not configured")
	}
	msg, err := s.Store.GetMessageForUser(ctx, userID, messageID)
	if err != nil {
		return nil, err
	}
	account, err := s.Store.GetMailAccountForUser(ctx, userID, msg.AccountID)
	if err != nil {
		return nil, err
	}
	source, err := s.Store.GetMailboxForUser(ctx, userID, msg.MailboxID)
	if err != nil {
		return nil, err
	}
	dest, err := s.Store.GetMailboxForUser(ctx, userID, destMailboxID)
	if err != nil {
		return nil, err
	}
	if dest.AccountID != msg.AccountID || source.AccountID != msg.AccountID || account.ID != msg.AccountID {
		return nil, errors.New("destination mailbox does not belong to this message account")
	}
	if strings.EqualFold(strings.TrimSpace(source.Name), strings.TrimSpace(dest.Name)) {
		return nil, nil
	}
	messageUIDValidity, err := s.Store.GetMessageUIDValidityForUser(ctx, userID, msg.ID)
	if err != nil {
		return nil, err
	}
	if messageUIDValidity <= 0 || messageUIDValidity > int64(^uint32(0)) || source.UIDValidity <= 0 || messageUIDValidity != source.UIDValidity {
		return nil, errors.New("message source mailbox generation changed; refresh before moving")
	}
	transfer, err := s.Store.StageMessageTransfer(ctx, userID, msg.ID, dest.ID, "move", "")
	if err != nil {
		return nil, err
	}
	if transfer.DestinationMailboxID != dest.ID {
		return nil, errors.New("message move is already targeting another mailbox")
	}
	if transfer.State == "succeeded" || transfer.State == "consumed" {
		return nil, s.completeMovedMessageLocally(ctx, userID, msg, dest, transfer.ID, announce)
	}
	if !transfer.DispatchedAt.IsZero() {
		if !messageTransferCanReconcile(transfer) {
			return nil, errors.New("message move is already awaiting remote reconciliation")
		}
		checker, ok := s.Fetcher.(UIDValidityExistenceFetcher)
		if !ok {
			return nil, errors.New("IMAP fetcher cannot reconcile an interrupted move")
		}
		exists, selectedUIDValidity, checkErr := checker.UIDExistsWithValidity(ctx, account, source.Name, msg.UID)
		if checkErr != nil {
			return nil, errors.Join(errors.New("reconcile interrupted message move"), checkErr)
		}
		if selectedUIDValidity != uint32(messageUIDValidity) {
			return nil, errors.New("message move remains pending because the source mailbox generation changed")
		}
		if !exists {
			if err := s.Store.MarkMessageTransferSucceeded(ctx, userID, transfer.ID, 0, 0); err != nil {
				return nil, err
			}
			if notifyMove != nil {
				notifyMove(ctx, messageMoveContext(msg, source, dest))
			}
			return nil, s.completeMovedMessageLocally(ctx, userID, msg, dest, transfer.ID, announce)
		}
		reopened, reopenErr := s.Store.ReopenMessageTransferDispatchAfterProof(ctx, userID, transfer.ID,
			messageTransferClaim(transfer), processMessageTransferOwner)
		if reopenErr != nil {
			return nil, reopenErr
		}
		if !reopened {
			return nil, errors.New("message move is already awaiting remote reconciliation")
		}
	}
	claim, claimed, err := s.Store.ClaimMessageTransferDispatchForOwner(ctx, userID, transfer.ID, processMessageTransferOwner)
	if err != nil {
		return nil, err
	}
	if !claimed {
		return nil, errors.New("message move is already awaiting remote reconciliation")
	}
	return &preparedMove{
		messageID:         messageID,
		msg:               msg,
		account:           account,
		source:            source,
		dest:              dest,
		sourceUIDValidity: uint32(messageUIDValidity),
		transfer:          transfer,
		claim:             claim,
	}, nil
}

// applyMoveOutcome records what the server did with one prepared move and
// brings the local mirror in line with it.
func (s *Service) applyMoveOutcome(ctx context.Context, userID int64, prepared *preparedMove,
	receipt *MoveReceipt, moveErr error, notifyMove messageMoveNotifier, announce *moveAnnouncer) error {
	if moveErr != nil {
		if !IsMoveOutcomeUnknown(moveErr) {
			if markErr := s.Store.MarkMessageTransferFailed(ctx, userID, prepared.transfer.ID); markErr != nil {
				return errors.Join(moveErr, markErr)
			}
		} else if finishErr := s.Store.FinishMessageTransferDispatch(ctx, userID, prepared.transfer.ID, prepared.claim); finishErr != nil {
			return errors.Join(moveErr, finishErr)
		}
		return moveErr
	}
	var destinationUID uint32
	var destinationUIDValidity int64
	if receipt != nil {
		destinationUID = receipt.DestinationUID
		destinationUIDValidity = int64(receipt.DestinationUIDValidity)
	}
	if err := s.Store.MarkMessageTransferSucceeded(ctx, userID, prepared.transfer.ID, destinationUID, destinationUIDValidity); err != nil {
		finishErr := s.Store.FinishMessageTransferDispatch(context.WithoutCancel(ctx), userID, prepared.transfer.ID, prepared.claim)
		if errors.Is(finishErr, store.ErrNotFound) {
			finishErr = nil
		}
		return errors.Join(err, finishErr)
	}
	if notifyMove != nil {
		notifyMove(ctx, messageMoveContext(prepared.msg, prepared.source, prepared.dest))
	}
	return s.completeMovedMessageLocally(ctx, userID, prepared.msg, prepared.dest, prepared.transfer.ID, announce)
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

func (s *Service) completeMovedMessageLocally(ctx context.Context, userID int64, msg store.MessageRecord, destination store.Mailbox, transferID int64, announce *moveAnnouncer) error {
	if !mailboxReceivesNewMailNotifications(destination) {
		if err := s.Store.TerminalizeMessageTransferWithoutArrival(ctx, userID, transferID); err != nil {
			return err
		}
	}
	return s.cleanupMovedMessage(ctx, userID, msg, announce)
}

func (s *Service) cleanupMovedMessage(ctx context.Context, userID int64, msg store.MessageRecord, announce *moveAnnouncer) error {
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
	announce.changed()
	return nil
}

// moveAnnouncer decides when the local changes a move made are announced. A
// lone move announces its own the moment it lands. A run announces once per
// batch instead: announcing per message asks every open browser to reload, and
// keeps the All Mail cache re-warming itself for as long as the run works —
// which is the load a large move used to put on everything else the reader was
// doing.
type moveAnnouncer struct {
	service    *Service
	userID     int64
	perMessage bool
	pending    bool
}

func (s *Service) moveAnnouncer(userID int64, perMessage bool) *moveAnnouncer {
	return &moveAnnouncer{service: s, userID: userID, perMessage: perMessage}
}

// changed records that this user's mail moved.
func (a *moveAnnouncer) changed() {
	if a == nil {
		return
	}
	a.pending = true
	if a.perMessage {
		a.flush()
	}
}

// flush announces whatever has changed since the last announcement.
func (a *moveAnnouncer) flush() {
	if a == nil || !a.pending {
		return
	}
	a.pending = false
	a.service.notify(a.userID)
}
