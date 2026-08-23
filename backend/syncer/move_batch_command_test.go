// File overview: Batched move-run tests. A whole-filter delete used to cost a
// SELECT, a UID SEARCH and a UID MOVE per message; these cover what a run does
// now that one command carries a whole folder's worth of them.

package syncer

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"rolltop/backend/search"
	"rolltop/backend/store"
)

// batchMoveCommand is one remote command a batching session issued.
type batchMoveCommand struct {
	source      string
	destination string
	uids        []uint32
}

// batchSessionMoveFetcher holds a connection open and can move a whole set with
// one command, which is what the production IMAP client does.
type batchSessionMoveFetcher struct {
	*moveTestFetcher
	opens    int
	closes   int
	commands []batchMoveCommand
	singles  int
	// failUIDs names UIDs the server refuses individually. The rest of the
	// batch still moves, which is the per-message reporting a batch owes.
	failUIDs map[uint32]error
	// failBatch refuses whole commands that carry more than one UID.
	failBatch error
	// onCommand runs while a command is in flight, which is where a client
	// disconnecting cancels the context a batch was claimed under.
	onCommand func()
}

func (f *batchSessionMoveFetcher) OpenMoveSession(_ context.Context, account store.MailAccount) (MoveSession, error) {
	f.opens++
	return &fakeBatchMoveSession{fetcher: f, account: account}, nil
}

type fakeBatchMoveSession struct {
	fetcher *batchSessionMoveFetcher
	account store.MailAccount
	closed  bool
}

func (s *fakeBatchMoveSession) MoveMessageWithReceipt(ctx context.Context, source, destination string, uid uint32, validity uint32) (*MoveReceipt, error) {
	if s.closed {
		return nil, errors.New("move session is closed")
	}
	s.fetcher.singles++
	s.fetcher.commands = append(s.fetcher.commands, batchMoveCommand{source: source, destination: destination, uids: []uint32{uid}})
	if err := s.fetcher.failUIDs[uid]; err != nil {
		return nil, err
	}
	return s.fetcher.moveTestFetcher.MoveMessageWithReceipt(ctx, s.account, source, destination, uid, validity)
}

func (s *fakeBatchMoveSession) MoveMessagesWithReceipts(ctx context.Context, source, destination string,
	uids []uint32, validity uint32) ([]MoveOutcome, error) {
	if s.closed {
		return nil, errors.New("move session is closed")
	}
	s.fetcher.commands = append(s.fetcher.commands, batchMoveCommand{
		source: source, destination: destination, uids: append([]uint32(nil), uids...),
	})
	if s.fetcher.onCommand != nil {
		s.fetcher.onCommand()
	}
	if s.fetcher.failBatch != nil && len(uids) > 1 {
		return nil, s.fetcher.failBatch
	}
	outcomes := make([]MoveOutcome, 0, len(uids))
	for _, uid := range uids {
		if err := s.fetcher.failUIDs[uid]; err != nil {
			outcomes = append(outcomes, MoveOutcome{UID: uid, Err: err})
			continue
		}
		receipt, err := s.fetcher.moveTestFetcher.MoveMessageWithReceipt(ctx, s.account, source, destination, uid, validity)
		outcomes = append(outcomes, MoveOutcome{UID: uid, Receipt: receipt, Err: err})
	}
	return outcomes, nil
}

func (s *fakeBatchMoveSession) Close() error {
	s.closed = true
	s.fetcher.closes++
	return nil
}

func newBatchMoveFixture(t *testing.T) (moveTestFixture, *batchSessionMoveFetcher) {
	t.Helper()
	fixture := newMoveTestFixture(t)
	fetcher := &batchSessionMoveFetcher{moveTestFetcher: fixture.fetcher, failUIDs: map[uint32]error{}}
	fixture.service.Fetcher = fetcher
	return fixture, fetcher
}

// addMoveTestMessageIn stores a message in a named source folder so a run can be
// given messages that do not all leave the same mailbox.
func addMoveTestMessageIn(t *testing.T, fixture moveTestFixture, mailbox store.Mailbox, uid uint32) store.MessageRecord {
	t.Helper()
	ctx := context.Background()
	blob, err := fixture.store.CreateBlob(ctx, store.BlobRecord{
		UserID: fixture.userID, Kind: "message-remote",
		Path:   fmt.Sprintf("users/move-hook/%s-%d.eml", mailbox.Name, uid),
		SHA256: fmt.Sprintf("%s-%d", mailbox.Name, uid), Size: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	date := time.Date(2026, 7, 14, 10, 30, 0, 0, time.UTC)
	message, err := fixture.store.CreateMessage(ctx, store.CreateMessage{
		UserID: fixture.userID, AccountID: fixture.account.ID, MailboxID: mailbox.ID, BlobID: blob.ID,
		MessageIDHeader: fmt.Sprintf("<%s-%d@example.test>", mailbox.Name, uid),
		ThreadKey:       fmt.Sprintf("thread:%s-%d", mailbox.Name, uid),
		Subject:         "Batch move", FromAddr: "sender@example.test", ToAddr: "move-hook@example.test",
		Date: date, InternalDate: date, UID: uid,
		UIDValidity: mailbox.UIDValidity, Size: 900, BodyText: "body",
	})
	if err != nil {
		t.Fatal(err)
	}
	return message
}

// A run that used to issue one remote command per message now issues one for the
// whole batch. That is the difference between three network round trips per
// message and three for all of them, which is what made a large move slow.
func TestRunMoveMessagesMovesTheWholeBatchWithOneCommand(t *testing.T) {
	fixture, fetcher := newBatchMoveFixture(t)
	ids := []int64{fixture.message.ID}
	wantUIDs := []uint32{fixture.message.UID}
	for uid := uint32(43); uid <= 47; uid++ {
		ids = append(ids, addMoveTestMessage(t, fixture, uid).ID)
		wantUIDs = append(wantUIDs, uid)
	}

	finished := waitForMoveRun(t, fixture, ids)

	if finished.Status != "ok" || finished.MessagesStored != 6 || finished.MessagesSkipped != 0 {
		t.Fatalf("run status=%q stored=%d skipped=%d, want all 6 moved cleanly",
			finished.Status, finished.MessagesStored, finished.MessagesSkipped)
	}
	if fetcher.opens != 1 || fetcher.closes != 1 {
		t.Fatalf("logins=%d closes=%d, want the batch to share one held connection", fetcher.opens, fetcher.closes)
	}
	if fetcher.singles != 0 {
		t.Fatalf("per-message commands = %d, want the batch to be moved with one command", fetcher.singles)
	}
	if len(fetcher.commands) != 1 {
		t.Fatalf("remote commands = %d, want 1 for the whole batch", len(fetcher.commands))
	}
	command := fetcher.commands[0]
	if command.source != fixture.source.Name || command.destination != fixture.destination.Name {
		t.Fatalf("command moved %q to %q, want %q to %q",
			command.source, command.destination, fixture.source.Name, fixture.destination.Name)
	}
	if len(command.uids) != len(wantUIDs) {
		t.Fatalf("command carried %d UIDs, want %d", len(command.uids), len(wantUIDs))
	}
	named := map[uint32]bool{}
	for _, uid := range command.uids {
		named[uid] = true
	}
	for _, uid := range wantUIDs {
		if !named[uid] {
			t.Fatalf("command did not name UID %d: %v", uid, command.uids)
		}
	}
	for _, id := range ids {
		if _, err := fixture.store.GetMessageForUser(context.Background(), fixture.userID, id); !store.IsNotFound(err) {
			t.Fatalf("message %d was left behind: %v", id, err)
		}
	}
}

// One command names one source mailbox generation, so a run over several folders
// batches per folder rather than per run.
func TestRunMoveMessagesBatchesPerSourceMailbox(t *testing.T) {
	fixture, fetcher := newBatchMoveFixture(t)
	ctx := context.Background()
	archive, err := fixture.store.GetOrCreateMailbox(ctx, fixture.userID, fixture.account.ID, "Archive")
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.store.UpdateMailboxRemoteStatus(ctx, fixture.userID, archive.ID, 0, 0, 60, 900); err != nil {
		t.Fatal(err)
	}
	archive, err = fixture.store.GetMailboxForUser(ctx, fixture.userID, archive.ID)
	if err != nil {
		t.Fatal(err)
	}
	// Interleave the folders the way a selection made in All Mail does: the list
	// is ordered by date, not by where the mail lives. Walking that order as
	// given would put one message in each command and give the batching away.
	ids := []int64{fixture.message.ID}
	ids = append(ids, addMoveTestMessageIn(t, fixture, archive, 50).ID)
	ids = append(ids, addMoveTestMessage(t, fixture, 43).ID)
	ids = append(ids, addMoveTestMessageIn(t, fixture, archive, 51).ID)

	finished := waitForMoveRun(t, fixture, ids)

	if finished.Status != "ok" || finished.MessagesStored != 4 {
		t.Fatalf("run status=%q stored=%d, want all 4 moved", finished.Status, finished.MessagesStored)
	}
	perSource := map[string]int{}
	for _, command := range fetcher.commands {
		perSource[command.source] += len(command.uids)
	}
	if perSource[fixture.source.Name] != 2 || perSource[archive.Name] != 2 {
		t.Fatalf("UIDs per source folder = %v, want 2 from each", perSource)
	}
	// One command per folder: a command names one source mailbox generation, and
	// there are two of them here whatever order the ids arrived in.
	if len(fetcher.commands) != 2 {
		t.Fatalf("remote commands = %d, want one per source folder: %+v", len(fetcher.commands), fetcher.commands)
	}
}

// A batch reports per message, not per batch: one UID the server refuses leaves
// every other message in the same command moved.
func TestRunMoveMessagesReportsBatchFailuresPerMessage(t *testing.T) {
	fixture, fetcher := newBatchMoveFixture(t)
	fetcher.failUIDs[44] = errors.New("mailbox is over quota")
	ids := []int64{fixture.message.ID}
	for uid := uint32(43); uid <= 46; uid++ {
		ids = append(ids, addMoveTestMessage(t, fixture, uid).ID)
	}

	finished := waitForMoveRun(t, fixture, ids)

	if finished.MessagesStored != 4 || finished.MessagesSkipped != 1 || finished.MessagesSeen != 5 {
		t.Fatalf("run stored=%d skipped=%d seen=%d, want the other four of five moved",
			finished.MessagesStored, finished.MessagesSkipped, finished.MessagesSeen)
	}
	if finished.Status != "failed" || !strings.Contains(finished.Error, "Moved 4 of 5 messages") ||
		!strings.Contains(finished.Error, "over quota") {
		t.Fatalf("run status=%q error=%q, want a partial-move summary naming the refusal", finished.Status, finished.Error)
	}
	if len(fetcher.commands) != 1 {
		t.Fatalf("remote commands = %d, want the refusal handled inside one command", len(fetcher.commands))
	}
}

// A connection that turns out not to be able to move a set is a capability
// answer, not a refusal: the batch is repeated one message at a time rather than
// recorded as failed.
func TestRunMoveMessagesFallsBackWhenTheSessionCannotBatch(t *testing.T) {
	fixture := newMoveTestFixture(t)
	fetcher := &sessionMoveFetcher{moveTestFetcher: fixture.fetcher}
	fixture.service.Fetcher = fetcher
	if _, ok := MoveSession(&fakeMoveSession{}).(BatchMoveSession); ok {
		t.Fatal("the plain fake session unexpectedly supports batched moves")
	}
	ids := []int64{fixture.message.ID}
	for uid := uint32(43); uid <= 45; uid++ {
		ids = append(ids, addMoveTestMessage(t, fixture, uid).ID)
	}

	finished := waitForMoveRun(t, fixture, ids)

	if finished.Status != "ok" || finished.MessagesStored != 4 {
		t.Fatalf("run status=%q stored=%d, want the fallback to move all 4", finished.Status, finished.MessagesStored)
	}
	if fetcher.opens != 1 {
		t.Fatalf("logins = %d, want the fallback to keep sharing one connection", fetcher.opens)
	}
	if fetcher.moves != 4 {
		t.Fatalf("session moves = %d, want one command per message", fetcher.moves)
	}
}

// A batch the server refuses outright is one answer for messages that each need
// their own, so nothing in it is recorded as moved.
func TestRunMoveMessagesRecordsARefusedBatchAgainstEveryMessage(t *testing.T) {
	fixture, fetcher := newBatchMoveFixture(t)
	fetcher.failBatch = errors.New("imap login refused")
	ids := []int64{fixture.message.ID}
	for uid := uint32(43); uid <= 45; uid++ {
		ids = append(ids, addMoveTestMessage(t, fixture, uid).ID)
	}

	finished := waitForMoveRun(t, fixture, ids)

	if finished.Status != "failed" || finished.MessagesStored != 0 || finished.MessagesSkipped != 4 {
		t.Fatalf("run status=%q stored=%d skipped=%d, want every message in the refused batch reported",
			finished.Status, finished.MessagesStored, finished.MessagesSkipped)
	}
	if !strings.Contains(finished.Error, "login refused") {
		t.Fatalf("run error = %q, want it to name the refusal", finished.Error)
	}
	for _, id := range ids {
		if _, err := fixture.store.GetMessageForUser(context.Background(), fixture.userID, id); err != nil {
			t.Fatalf("message %d was dropped locally after a refused move: %v", id, err)
		}
	}
}

// A handful of messages moved inline shares the run's held connection and its
// batching rather than logging in once per message.
func TestMoveMessagesSharesOneConnectionAndOneCommand(t *testing.T) {
	fixture, fetcher := newBatchMoveFixture(t)
	ids := []int64{fixture.message.ID}
	for uid := uint32(43); uid <= 45; uid++ {
		ids = append(ids, addMoveTestMessage(t, fixture, uid).ID)
	}

	moved, err := fixture.service.MoveMessages(context.Background(), fixture.userID, ids, fixture.destination.ID)
	if err != nil {
		t.Fatal(err)
	}

	if moved != 4 {
		t.Fatalf("moved = %d, want 4", moved)
	}
	if fetcher.opens != 1 || fetcher.closes != 1 {
		t.Fatalf("logins=%d closes=%d, want one held connection given back", fetcher.opens, fetcher.closes)
	}
	if len(fetcher.commands) != 1 || len(fetcher.commands[0].uids) != 4 {
		t.Fatalf("remote commands = %+v, want one carrying all four UIDs", fetcher.commands)
	}
}

// Every moved message used to announce itself. A reader watching their mail
// then had every open view reload once per message, and the All Mail cache
// re-warmed itself for as long as the run worked — which is the load a large
// move put on everything else they were doing. A batch announces once.
func TestRunMoveMessagesAnnouncesOncePerBatchRatherThanPerMessage(t *testing.T) {
	fixture, _ := newBatchMoveFixture(t)
	announcements := 0
	fixture.service.Notify = func(int64) { announcements++ }
	ids := []int64{fixture.message.ID}
	for uid := uint32(43); uid <= 50; uid++ {
		ids = append(ids, addMoveTestMessage(t, fixture, uid).ID)
	}

	finished := waitForMoveRun(t, fixture, ids)

	if finished.Status != "ok" || finished.MessagesStored != 9 {
		t.Fatalf("run status=%q stored=%d, want all 9 moved", finished.Status, finished.MessagesStored)
	}
	// The run announces when it starts, once for the batch it moved, and once
	// when it finishes. Anything approaching one per message is the old cost.
	if announcements > 4 {
		t.Fatalf("announcements = %d for 9 messages, want a handful rather than one each", announcements)
	}
	if announcements == 0 {
		t.Fatal("the run never announced that mail moved")
	}
}

// A lone move still announces itself the moment it lands: there is no batch to
// wait for, and the view that asked for it is waiting on the answer.
func TestMoveMessageStillAnnouncesImmediately(t *testing.T) {
	fixture := newMoveTestFixture(t)
	announcements := 0
	fixture.service.Notify = func(int64) { announcements++ }

	if err := fixture.service.MoveMessage(context.Background(), fixture.userID, fixture.message.ID, fixture.destination.ID); err != nil {
		t.Fatal(err)
	}

	if announcements != 1 {
		t.Fatalf("announcements = %d, want exactly one for one move", announcements)
	}
}

// A moved message's search document has to go with it. It used to go one index
// commit at a time, which for a folder's worth of mail is a folder's worth of
// commits; the batch now hands them over together. What must not change is that
// none of them are left behind.
func TestRunMoveMessagesClearsTheSearchDocumentsOfAWholeBatch(t *testing.T) {
	fixture, _ := newBatchMoveFixture(t)
	searchService, err := search.Open(filepath.Join(t.TempDir(), "bleve"))
	if err != nil {
		t.Fatal(err)
	}
	defer searchService.Close()
	fixture.service.Search = searchService
	ctx := context.Background()
	ids := []int64{fixture.message.ID}
	messages := []store.MessageRecord{fixture.message}
	for uid := uint32(43); uid <= 48; uid++ {
		message := addMoveTestMessage(t, fixture, uid)
		ids = append(ids, message.ID)
		messages = append(messages, message)
	}
	for _, message := range messages {
		if err := searchService.IndexMessage(ctx, message, nil); err != nil {
			t.Fatal(err)
		}
	}
	indexed, err := searchService.CountMailboxMessages(ctx, fixture.userID, fixture.source.ID)
	if err != nil {
		t.Fatal(err)
	}
	if indexed != len(messages) {
		t.Fatalf("indexed %d of %d messages before the move", indexed, len(messages))
	}

	finished := waitForMoveRun(t, fixture, ids)

	if finished.Status != "ok" || finished.MessagesStored != len(messages) {
		t.Fatalf("run status=%q stored=%d, want every message moved", finished.Status, finished.MessagesStored)
	}
	remaining, err := searchService.CountMailboxMessages(ctx, fixture.userID, fixture.source.ID)
	if err != nil {
		t.Fatal(err)
	}
	if remaining != 0 {
		t.Fatalf("%d search documents survived the batch that moved their messages", remaining)
	}
}

// A batch is claimed before it is dispatched, so a context cancelled while it is
// in flight strands every claim in it. Settling those claims has to outlive the
// context that failed them: a dispatch this process owns but never finished
// cannot be reconciled by anything short of a restart, and the messages behind
// it refuse every later move until then.
func TestMoveMessagesSettlesClaimedDispatchesAfterCancellation(t *testing.T) {
	fixture, fetcher := newBatchMoveFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// The reader closed the tab while the command was in flight.
	fetcher.onCommand = cancel
	fetcher.failBatch = context.Canceled
	ids := []int64{fixture.message.ID}
	for uid := uint32(43); uid <= 45; uid++ {
		ids = append(ids, addMoveTestMessage(t, fixture, uid).ID)
	}

	if _, err := fixture.service.MoveMessages(ctx, fixture.userID, ids, fixture.destination.ID); err == nil {
		t.Fatal("a move cancelled mid-command reported success")
	}

	if len(fetcher.commands) == 0 {
		t.Fatal("the test cancelled before anything was claimed, so it proves nothing")
	}
	// Every claim is settled, so the same messages can be moved again. A claim
	// left open reports itself as awaiting reconciliation instead.
	for _, id := range ids {
		transfer, err := fixture.store.StageMessageTransfer(context.Background(), fixture.userID, id,
			fixture.destination.ID, "move", "")
		if err != nil {
			t.Fatalf("message %d could not be staged again: %v", id, err)
		}
		if transfer.DispatchedAt.IsZero() {
			continue
		}
		if !messageTransferCanReconcile(transfer) {
			t.Fatalf("message %d is stranded on a dispatch this process owns and never finished", id)
		}
	}
}

// A cancelled request reaches the settlement with the command already sent and
// the server having moved the mail. Recording that has to outlive the
// cancellation: a move called failed because the request went away leaves the
// mirror holding rows for mail that is gone, and makes the next attempt pay a
// remote existence check per message to discover it.
func TestMoveMessagesRecordsASucceededMoveAfterCancellation(t *testing.T) {
	fixture, fetcher := newBatchMoveFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// The command lands; the reader closed the tab while it was in flight.
	fetcher.onCommand = cancel
	ids := []int64{fixture.message.ID}
	for uid := uint32(43); uid <= 45; uid++ {
		ids = append(ids, addMoveTestMessage(t, fixture, uid).ID)
	}

	moved, err := fixture.service.MoveMessages(ctx, fixture.userID, ids, fixture.destination.ID)
	if err != nil {
		t.Fatalf("a move the server completed reported failure: %v", err)
	}
	if moved != len(ids) {
		t.Fatalf("moved = %d, want all %d recorded", moved, len(ids))
	}

	if len(fetcher.commands) == 0 {
		t.Fatal("the test cancelled before anything was dispatched, so it proves nothing")
	}
	for _, id := range ids {
		if _, err := fixture.store.GetMessageForUser(context.Background(), fixture.userID, id); !store.IsNotFound(err) {
			t.Fatalf("message %d stayed in the mirror after the server moved it: %v", id, err)
		}
	}
}

// A walk can settle messages without dispatching anything: an earlier attempt
// already moved them remotely, and preparing them is what finishes the local
// half. That walk has no batch boundary, so what it settled has to reach the
// reader — and its search documents have to go — on the strength of the walk
// ending alone.
func TestRunMoveMessagesSettlesMessagesItNeverDispatches(t *testing.T) {
	fixture, fetcher := newBatchMoveFixture(t)
	searchService, err := search.Open(filepath.Join(t.TempDir(), "bleve"))
	if err != nil {
		t.Fatal(err)
	}
	defer searchService.Close()
	fixture.service.Search = searchService
	ctx := context.Background()
	ids := []int64{fixture.message.ID}
	messages := []store.MessageRecord{fixture.message}
	for uid := uint32(43); uid <= 45; uid++ {
		message := addMoveTestMessage(t, fixture, uid)
		ids = append(ids, message.ID)
		messages = append(messages, message)
	}
	// Every message already has a transfer the server accepted, which is what an
	// interrupted run leaves behind.
	for _, message := range messages {
		if err := searchService.IndexMessage(ctx, message, nil); err != nil {
			t.Fatal(err)
		}
		transfer, err := fixture.store.StageMessageTransfer(ctx, fixture.userID, message.ID, fixture.destination.ID, "move", "")
		if err != nil {
			t.Fatal(err)
		}
		if err := fixture.store.MarkMessageTransferSucceeded(ctx, fixture.userID, transfer.ID, 0, 0); err != nil {
			t.Fatal(err)
		}
	}
	announcements := 0
	fixture.service.Notify = func(int64) { announcements++ }

	finished := waitForMoveRun(t, fixture, ids)

	if len(fetcher.commands) != 0 {
		t.Fatalf("the run issued %d remote commands for messages the server had already moved", len(fetcher.commands))
	}
	if finished.Status != "ok" || finished.MessagesStored != len(messages) {
		t.Fatalf("run status=%q stored=%d, want every message settled", finished.Status, finished.MessagesStored)
	}
	for _, message := range messages {
		if _, err := fixture.store.GetMessageForUser(ctx, fixture.userID, message.ID); !store.IsNotFound(err) {
			t.Fatalf("message %d was left in the mirror: %v", message.ID, err)
		}
	}
	remaining, err := searchService.CountMailboxMessages(ctx, fixture.userID, fixture.source.ID)
	if err != nil {
		t.Fatal(err)
	}
	if remaining != 0 {
		t.Fatalf("%d search documents survived a run that settled their messages without dispatching", remaining)
	}
	if announcements == 0 {
		t.Fatal("a run that settled its messages without dispatching announced nothing")
	}
}
