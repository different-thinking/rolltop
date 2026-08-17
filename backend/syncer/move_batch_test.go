// File overview: Background move-run resilience tests. A whole-filter delete
// resolves thousands of message IDs into one run, so the run's behaviour when
// individual messages cannot be moved is what decides whether the delete
// happens at all.

package syncer

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"rolltop/backend/store"
)

// selectiveMoveFetcher fails the moves the test names and records how many
// remote attempts the run actually made.
type selectiveMoveFetcher struct {
	*moveTestFetcher
	failAll  error
	attempts int
}

func (f *selectiveMoveFetcher) MoveMessageWithReceipt(ctx context.Context, account store.MailAccount, source, destination string, uid uint32, validity uint32) (*MoveReceipt, error) {
	f.attempts++
	if f.failAll != nil {
		return nil, f.failAll
	}
	return f.moveTestFetcher.MoveMessageWithReceipt(ctx, account, source, destination, uid, validity)
}

// sessionMoveFetcher can hold a connection open, and counts how often a run
// actually logs in rather than reusing what it already has.
type sessionMoveFetcher struct {
	*moveTestFetcher
	opens    int
	closes   int
	moves    int
	failUIDs map[uint32]error
}

func (f *sessionMoveFetcher) OpenMoveSession(_ context.Context, account store.MailAccount) (MoveSession, error) {
	f.opens++
	return &fakeMoveSession{fetcher: f, account: account}, nil
}

type fakeMoveSession struct {
	fetcher *sessionMoveFetcher
	account store.MailAccount
	closed  bool
}

func (s *fakeMoveSession) MoveMessageWithReceipt(ctx context.Context, source, destination string, uid uint32, validity uint32) (*MoveReceipt, error) {
	if s.closed {
		return nil, errors.New("move session is closed")
	}
	s.fetcher.moves++
	if err := s.fetcher.failUIDs[uid]; err != nil {
		return nil, err
	}
	return s.fetcher.moveTestFetcher.MoveMessageWithReceipt(ctx, s.account, source, destination, uid, validity)
}

func (s *fakeMoveSession) Close() error {
	s.closed = true
	s.fetcher.closes++
	return nil
}

func addMoveTestMessage(t *testing.T, fixture moveTestFixture, uid uint32) store.MessageRecord {
	t.Helper()
	ctx := context.Background()
	blob, err := fixture.store.CreateBlob(ctx, store.BlobRecord{
		UserID: fixture.userID, Kind: "message-remote",
		Path: fmt.Sprintf("users/move-hook/batch-%d.eml", uid), SHA256: fmt.Sprintf("batch-%d", uid), Size: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	date := time.Date(2026, 7, 14, 10, 30, 0, 0, time.UTC)
	message, err := fixture.store.CreateMessage(ctx, store.CreateMessage{
		UserID: fixture.userID, AccountID: fixture.account.ID, MailboxID: fixture.source.ID, BlobID: blob.ID,
		MessageIDHeader: fmt.Sprintf("<batch-%d@example.test>", uid), ThreadKey: fmt.Sprintf("thread:batch-%d", uid),
		Subject: "Batch move", FromAddr: "sender@example.test", ToAddr: "move-hook@example.test",
		Date: date, InternalDate: date, UID: uid, UIDValidity: int64(moveTestSourceUIDValidity), Size: 900,
		BodyText: "body",
	})
	if err != nil {
		t.Fatal(err)
	}
	return message
}

// waitForMoveRun runs one background move to completion and returns the
// finished run row.
func waitForMoveRun(t *testing.T, fixture moveTestFixture, ids []int64) store.SyncRun {
	t.Helper()
	done := make(chan struct{})
	run, err := fixture.service.StartMoveMessages(context.Background(), fixture.userID, ids, fixture.destination.ID, func() {
		close(done)
	})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("background move did not finish")
	}
	finished, err := fixture.store.GetSyncRunForUser(context.Background(), fixture.userID, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	return finished
}

// A message whose local mailbox generation no longer matches its folder is
// rejected before dispatch. In a whole-filter delete that is one row out of
// thousands, and it must not strand the rest of the batch.
func TestRunMoveMessagesStepsOverUnmovableMessages(t *testing.T) {
	fixture := newMoveTestFixture(t)
	ids := []int64{fixture.message.ID}
	for uid := uint32(43); uid <= 46; uid++ {
		ids = append(ids, addMoveTestMessage(t, fixture, uid).ID)
	}
	// The first message in the batch is the unmovable one, so a run that gives up
	// on the first failure moves nothing at all.
	if _, err := fixture.store.DB().Exec(`UPDATE messages SET uid_validity = 0 WHERE user_id = ? AND id = ?`,
		fixture.userID, fixture.message.ID); err != nil {
		t.Fatal(err)
	}

	finished := waitForMoveRun(t, fixture, ids)

	if finished.MessagesStored != 4 || finished.MessagesSeen != 5 || finished.MessagesSkipped != 1 {
		t.Fatalf("run progress stored=%d seen=%d skipped=%d, want 4/5/1",
			finished.MessagesStored, finished.MessagesSeen, finished.MessagesSkipped)
	}
	if len(fixture.fetcher.moveCalls) != 4 {
		t.Fatalf("remote move calls = %d, want the 4 movable messages", len(fixture.fetcher.moveCalls))
	}
	for _, id := range ids[1:] {
		if _, err := fixture.store.GetMessageForUser(context.Background(), fixture.userID, id); !store.IsNotFound(err) {
			t.Fatalf("movable message %d was left behind: %v", id, err)
		}
	}
	if _, err := fixture.store.GetMessageForUser(context.Background(), fixture.userID, fixture.message.ID); err != nil {
		t.Fatalf("unmovable message was dropped locally: %v", err)
	}
	// The run has to say what it left behind: it is the only account the user
	// gets of a delete that did not fully happen.
	if finished.Status != "failed" {
		t.Fatalf("run status = %q, want failed", finished.Status)
	}
	if !strings.Contains(finished.Error, "Moved 4 of 5 messages") || !strings.Contains(finished.Error, "generation changed") {
		t.Fatalf("run error = %q, want a partial-move summary naming the first failure", finished.Error)
	}
}

// A run whose every move fails is hitting something systemic, and must not
// attempt thousands of logins against a server that is rejecting all of them.
func TestRunMoveMessagesStopsAfterRepeatedFailures(t *testing.T) {
	fixture := newMoveTestFixture(t)
	fetcher := &selectiveMoveFetcher{moveTestFetcher: fixture.fetcher, failAll: errors.New("imap login refused")}
	fixture.service.Fetcher = fetcher
	total := moveRunConsecutiveFailureLimit + 10
	ids := []int64{fixture.message.ID}
	for offset := 0; offset < total-1; offset++ {
		ids = append(ids, addMoveTestMessage(t, fixture, uint32(43+offset)).ID)
	}

	finished := waitForMoveRun(t, fixture, ids)

	if fetcher.attempts != moveRunConsecutiveFailureLimit {
		t.Fatalf("remote attempts = %d, want the consecutive-failure limit %d", fetcher.attempts, moveRunConsecutiveFailureLimit)
	}
	if finished.Status != "failed" || finished.MessagesStored != 0 {
		t.Fatalf("run status=%q stored=%d, want a failed run that moved nothing", finished.Status, finished.MessagesStored)
	}
	if !strings.Contains(finished.Error, "stopped early") {
		t.Fatalf("run error = %q, want it to report stopping early", finished.Error)
	}
	// Everything past the streak was never tried, and the summary has to say so
	// rather than leaving the remainder of the batch unaccounted for.
	notAttempted := total - moveRunConsecutiveFailureLimit
	if !strings.Contains(finished.Error, fmt.Sprintf("%d were not attempted", notAttempted)) {
		t.Fatalf("run error = %q, want it to account for the %d unattempted messages", finished.Error, notAttempted)
	}
}

// Rows that vanish between the run resolving its IDs and reaching them are not
// failures, so they must neither be written per message nor count towards the
// streak that stops the run.
func TestRunMoveMessagesTreatsVanishedRowsAsHandled(t *testing.T) {
	fixture := newMoveTestFixture(t)
	ids := []int64{fixture.message.ID}
	for uid := uint32(43); uid <= 45; uid++ {
		ids = append(ids, addMoveTestMessage(t, fixture, uid).ID)
	}
	// Delete far more rows than the consecutive-failure limit: if they were
	// scored as failures the run would stop before reaching the last message.
	gone := make([]int64, 0, moveRunConsecutiveFailureLimit+5)
	for offset := 0; offset < moveRunConsecutiveFailureLimit+5; offset++ {
		message := addMoveTestMessage(t, fixture, uint32(100+offset))
		gone = append(gone, message.ID)
		if err := fixture.store.DeleteMessageForUser(context.Background(), fixture.userID, message.ID); err != nil {
			t.Fatal(err)
		}
	}
	last := addMoveTestMessage(t, fixture, 200)
	ids = append(ids, gone...)
	ids = append(ids, last.ID)

	finished := waitForMoveRun(t, fixture, ids)

	if finished.Status != "ok" || finished.Error != "" {
		t.Fatalf("run status=%q error=%q, want vanished rows to leave a clean run", finished.Status, finished.Error)
	}
	if finished.MessagesStored != 5 || finished.MessagesSkipped != 0 {
		t.Fatalf("run stored=%d skipped=%d, want the 5 real messages moved and nothing counted as failed",
			finished.MessagesStored, finished.MessagesSkipped)
	}
	if _, err := fixture.store.GetMessageForUser(context.Background(), fixture.userID, last.ID); !store.IsNotFound(err) {
		t.Fatalf("the run stopped before the message past the vanished rows: %v", err)
	}
}

// A batch that moves cleanly still has to finish as a plain successful run.
func TestRunMoveMessagesReportsACleanBatchAsSucceeded(t *testing.T) {
	fixture := newMoveTestFixture(t)
	ids := []int64{fixture.message.ID}
	for uid := uint32(43); uid <= 45; uid++ {
		ids = append(ids, addMoveTestMessage(t, fixture, uid).ID)
	}

	finished := waitForMoveRun(t, fixture, ids)

	if finished.Status != "ok" || finished.Error != "" {
		t.Fatalf("run status=%q error=%q, want a clean run", finished.Status, finished.Error)
	}
	if finished.MessagesStored != 4 || finished.MessagesSkipped != 0 || finished.MailboxesDone != 1 {
		t.Fatalf("run progress stored=%d skipped=%d mailboxes_done=%d, want 4/0/1",
			finished.MessagesStored, finished.MessagesSkipped, finished.MailboxesDone)
	}
}

// A whole-filter delete moves thousands of messages. Reconnecting for each one
// dominates the run and is what mail hosts throttle, so a batch has to log in
// once and reuse that connection.
func TestRunMoveMessagesReusesOneConnectionForTheBatch(t *testing.T) {
	fixture := newMoveTestFixture(t)
	fetcher := &sessionMoveFetcher{moveTestFetcher: fixture.fetcher}
	fixture.service.Fetcher = fetcher
	ids := []int64{fixture.message.ID}
	for uid := uint32(43); uid <= 47; uid++ {
		ids = append(ids, addMoveTestMessage(t, fixture, uid).ID)
	}

	finished := waitForMoveRun(t, fixture, ids)

	if finished.Status != "ok" || finished.MessagesStored != 6 {
		t.Fatalf("run status=%q stored=%d, want all 6 moved cleanly", finished.Status, finished.MessagesStored)
	}
	if fetcher.opens != 1 {
		t.Fatalf("logins = %d, want the whole batch to share one", fetcher.opens)
	}
	if fetcher.moves != 6 {
		t.Fatalf("session moves = %d, want 6", fetcher.moves)
	}
	// The run owns the connection and has to give it back when it ends.
	if fetcher.closes != 1 {
		t.Fatalf("session closes = %d, want the held connection released once", fetcher.closes)
	}
}

// One dead connection must not take the rest of the batch with it: the run
// drops the session it was holding and logs in again for the next message.
func TestRunMoveMessagesReopensTheConnectionAfterAFailedMove(t *testing.T) {
	fixture := newMoveTestFixture(t)
	fetcher := &sessionMoveFetcher{
		moveTestFetcher: fixture.fetcher,
		failUIDs:        map[uint32]error{fixture.message.UID: errors.New("connection reset")},
	}
	fixture.service.Fetcher = fetcher
	ids := []int64{fixture.message.ID}
	for uid := uint32(43); uid <= 45; uid++ {
		ids = append(ids, addMoveTestMessage(t, fixture, uid).ID)
	}

	finished := waitForMoveRun(t, fixture, ids)

	if fetcher.opens != 2 {
		t.Fatalf("logins = %d, want a second one after the failure dropped the first", fetcher.opens)
	}
	if finished.MessagesStored != 3 || finished.MessagesSkipped != 1 {
		t.Fatalf("run stored=%d skipped=%d, want the 3 messages after the failure still moved",
			finished.MessagesStored, finished.MessagesSkipped)
	}
	for _, id := range ids[1:] {
		if _, err := fixture.store.GetMessageForUser(context.Background(), fixture.userID, id); !store.IsNotFound(err) {
			t.Fatalf("message %d after the failed one was left behind: %v", id, err)
		}
	}
}

// A fetcher that cannot hold a connection open still runs the batch, one
// connection per message, rather than failing it.
func TestRunMoveMessagesFallsBackWithoutSessionSupport(t *testing.T) {
	fixture := newMoveTestFixture(t)
	if _, ok := fixture.service.Fetcher.(MoveSessionFetcher); ok {
		t.Fatal("fixture fetcher unexpectedly supports move sessions")
	}
	ids := []int64{fixture.message.ID}
	for uid := uint32(43); uid <= 45; uid++ {
		ids = append(ids, addMoveTestMessage(t, fixture, uid).ID)
	}

	finished := waitForMoveRun(t, fixture, ids)

	if finished.Status != "ok" || finished.MessagesStored != 4 {
		t.Fatalf("run status=%q stored=%d, want the fallback to move all 4", finished.Status, finished.MessagesStored)
	}
	if len(fixture.fetcher.moveCalls) != 4 {
		t.Fatalf("remote move calls = %d, want one per message", len(fixture.fetcher.moveCalls))
	}
}
