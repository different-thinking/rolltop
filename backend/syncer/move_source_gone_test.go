// File overview: What a move does when the folder it is moving out of turns out
// not to hold the message any more. That answer used to be a failed run and a
// notice the user could neither act on nor get rid of.

package syncer

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"rolltop/backend/store"
)

// goneSourceMoveFetcher is a server that refuses the UIDs its source folder no
// longer holds, and can list what that folder really holds so the walk has
// something to reconcile against.
type goneSourceMoveFetcher struct {
	*moveTestFetcher
	sourceMailbox string
	// remaining is what a UID listing of the source folder returns.
	remaining   []uint32
	uidNext     uint32
	batchMoves  int
	singleMoves int
	snapshots   int
}

func (f *goneSourceMoveFetcher) holds(uid uint32) bool {
	return slices.Contains(f.remaining, uid)
}

func (f *goneSourceMoveFetcher) take(uid uint32) {
	f.remaining = slices.DeleteFunc(f.remaining, func(existing uint32) bool { return existing == uid })
}

func (f *goneSourceMoveFetcher) SnapshotMailboxUIDs(_ context.Context, _ store.MailAccount, mailbox string) (MailboxUIDSnapshot, error) {
	f.snapshots++
	if mailbox != f.sourceMailbox {
		return MailboxUIDSnapshot{UIDValidity: 701, UIDNext: 1}, nil
	}
	return MailboxUIDSnapshot{
		UIDs:        slices.Clone(f.remaining),
		UIDValidity: moveTestSourceUIDValidity,
		UIDNext:     f.uidNext,
	}, nil
}

// MoveMessageWithReceipt is the path a lone move takes: no held connection, so
// the fetcher itself answers.
func (f *goneSourceMoveFetcher) MoveMessageWithReceipt(_ context.Context, account store.MailAccount,
	source, destination string, uid uint32, _ uint32) (*MoveReceipt, error) {
	f.singleMoves++
	if !f.holds(uid) {
		return nil, SourceUIDGone(fmt.Errorf("source mailbox %q no longer contains UID %d; refresh before moving", source, uid))
	}
	f.take(uid)
	f.moveCalls = append(f.moveCalls,
		moveTestCall{account: account, source: source, destination: destination, uid: uid})
	return nil, nil
}

func (f *goneSourceMoveFetcher) OpenMoveSession(_ context.Context, account store.MailAccount) (MoveSession, error) {
	return &goneSourceMoveSession{fetcher: f, account: account}, nil
}

type goneSourceMoveSession struct {
	fetcher *goneSourceMoveFetcher
	account store.MailAccount
}

func (s *goneSourceMoveSession) MoveMessageWithReceipt(_ context.Context, source, destination string,
	uid uint32, _ uint32) (*MoveReceipt, error) {
	s.fetcher.singleMoves++
	if !s.fetcher.holds(uid) {
		return nil, SourceUIDGone(fmt.Errorf("source mailbox %q no longer contains UID %d; refresh before moving", source, uid))
	}
	s.fetcher.take(uid)
	s.fetcher.moveCalls = append(s.fetcher.moveCalls,
		moveTestCall{account: s.account, source: source, destination: destination, uid: uid})
	return nil, nil
}

func (s *goneSourceMoveSession) MoveMessagesWithReceipts(_ context.Context, source, destination string,
	uids []uint32, _ uint32) ([]MoveOutcome, error) {
	s.fetcher.batchMoves++
	outcomes := make([]MoveOutcome, 0, len(uids))
	for _, uid := range uids {
		if !s.fetcher.holds(uid) {
			outcomes = append(outcomes, MoveOutcome{
				UID: uid,
				Err: SourceUIDGone(fmt.Errorf("source mailbox %q no longer contains UID %d; refresh before moving", source, uid)),
			})
			continue
		}
		s.fetcher.take(uid)
		s.fetcher.moveCalls = append(s.fetcher.moveCalls,
			moveTestCall{account: s.account, source: source, destination: destination, uid: uid})
		outcomes = append(outcomes, MoveOutcome{UID: uid})
	}
	return outcomes, nil
}

func (s *goneSourceMoveSession) Close() error { return nil }

// newGoneSourceFixture puts uids in the source folder locally and tells the
// server which of them are really still there.
func newGoneSourceFixture(t *testing.T, uids []uint32, stillOnServer []uint32) (moveTestFixture, *goneSourceMoveFetcher, []int64) {
	t.Helper()
	fixture := newMoveTestFixture(t)
	ids := make([]int64, 0, len(uids)+1)
	// The fixture's own message is UID 42 and is not part of this selection.
	for _, uid := range uids {
		ids = append(ids, addMoveTestMessage(t, fixture, uid).ID)
	}
	highest := uint32(42)
	for _, uid := range uids {
		if uid > highest {
			highest = uid
		}
	}
	fetcher := &goneSourceMoveFetcher{
		moveTestFetcher: fixture.fetcher,
		sourceMailbox:   fixture.source.Name,
		remaining:       append([]uint32{42}, stillOnServer...),
		uidNext:         highest + 1,
	}
	fixture.service.Fetcher = fetcher
	return fixture, fetcher, ids
}

// pacedSourcePlan is a source folder large enough that reconciliation is paced
// rather than run on every turn, which is the case a stale row survives.
func pacedSourcePlan(fixture moveTestFixture) MailboxPlan {
	return MailboxPlan{Name: fixture.source.Name, Status: MailboxStatus{Messages: inlineMetadataSyncLimit + 1}}
}

// requireSourceReconcileDue reports whether the next sync turn of the source
// folder will reconcile it rather than wait out the large-folder pace.
func sourceReconcileDue(fixture moveTestFixture) bool {
	_, due := fixture.service.mailboxReconcileDue(fixture.userID, fixture.source, pacedSourcePlan(fixture))
	return due
}

// A message the source folder no longer holds is not a move that failed: there
// is nothing there to move. The run finishes clean rather than leaving a notice
// the user cannot act on.
func TestRunMoveMessagesTreatsAGoneSourceUIDAsHandled(t *testing.T) {
	fixture, fetcher, ids := newGoneSourceFixture(t, []uint32{43, 44, 45, 46}, []uint32{43})

	finished := waitForMoveRun(t, fixture, ids)

	if finished.Status != "ok" || finished.Error != "" {
		t.Fatalf("run status=%q error=%q, want a move with nothing left to do to finish clean",
			finished.Status, finished.Error)
	}
	if finished.MessagesStored != 1 {
		t.Fatalf("run stored=%d, want the one message the folder still held", finished.MessagesStored)
	}
	if finished.MessagesSkipped != 0 {
		t.Fatalf("run skipped=%d, want messages the folder no longer holds not counted as failures",
			finished.MessagesSkipped)
	}
	if fetcher.batchMoves == 0 {
		t.Fatalf("batch moves = %d, want the selection dispatched as a batch", fetcher.batchMoves)
	}
}

// The mirror is what was out of date, so the move makes sure it is put right:
// the source folder becomes due for reconciliation, which is what removes the
// rows for mail it no longer holds.
//
// The move does not list the folder itself. It is holding one connection for
// the run, and a foreground move would keep the browser waiting on a
// full-folder listing; the refresh a move already queues has a connection for
// that folder, and this only takes away the reason it would skip.
func TestRunMoveMessagesMakesTheSourceDueForReconciliation(t *testing.T) {
	fixture, fetcher, ids := newGoneSourceFixture(t, []uint32{43, 44, 45}, []uint32{43})
	// The folder was reconciled a moment ago, so its pace would otherwise skip
	// it for the next half hour.
	fixture.service.recordMailboxReconciled(fixture.userID, fixture.source)
	if sourceReconcileDue(fixture) {
		t.Fatal("the source folder was already due before the move, so this proves nothing")
	}

	waitForMoveRun(t, fixture, ids)

	if !sourceReconcileDue(fixture) {
		t.Fatal("the source folder is still waiting out its pace, so its stale rows survive the move")
	}
	if fetcher.snapshots != 0 {
		t.Fatalf("mailbox listings = %d, want the move not to open a second connection of its own", fetcher.snapshots)
	}
	// Only the message the folder really held was moved on the server.
	movedUIDs := make([]uint32, 0, len(fetcher.moveCalls))
	for _, call := range fetcher.moveCalls {
		movedUIDs = append(movedUIDs, call.uid)
	}
	if !slices.Equal(movedUIDs, []uint32{43}) {
		t.Fatalf("moved UIDs = %v, want only the one the folder still held", movedUIDs)
	}
}

// A move that leaves the folder alone leaves its pace alone too, so an ordinary
// move does not make every large folder pay for a UID listing.
func TestRunMoveMessagesLeavesTheSourcePaceAloneWhenNothingIsStale(t *testing.T) {
	fixture, _, ids := newGoneSourceFixture(t, []uint32{43, 44}, []uint32{43, 44})
	fixture.service.recordMailboxReconciled(fixture.userID, fixture.source)

	finished := waitForMoveRun(t, fixture, ids)

	if finished.Status != "ok" || finished.MessagesStored != 2 {
		t.Fatalf("run status=%q stored=%d, want both messages moved", finished.Status, finished.MessagesStored)
	}
	if sourceReconcileDue(fixture) {
		t.Fatal("a move that found nothing stale still forced the source folder to be reconciled")
	}
}

// A move that really cannot be carried out still fails the run: only the "this
// folder does not hold it" answer is handled rather than reported.
func TestRunMoveMessagesStillReportsARefusedMove(t *testing.T) {
	fixture := newMoveTestFixture(t)
	ids := []int64{fixture.message.ID}
	fetcher := &sessionMoveFetcher{
		moveTestFetcher: fixture.fetcher,
		failUIDs:        map[uint32]error{42: errors.New("server refused the move")},
	}
	fixture.service.Fetcher = fetcher

	finished := waitForMoveRun(t, fixture, ids)

	if finished.Status != "failed" || !strings.Contains(finished.Error, "server refused the move") {
		t.Fatalf("run status=%q error=%q, want a refused move still reported",
			finished.Status, finished.Error)
	}
}

// The bulk path an inline move takes answers the same way as a background run.
func TestMoveMessagesTreatsAGoneSourceUIDAsHandled(t *testing.T) {
	fixture, fetcher, ids := newGoneSourceFixture(t, []uint32{43}, nil)
	fixture.service.recordMailboxReconciled(fixture.userID, fixture.source)
	ctx := context.Background()

	moved, err := fixture.service.MoveMessages(ctx, fixture.userID, ids, fixture.destination.ID)
	if err != nil {
		t.Fatalf("move of a message the folder no longer holds = %v, want it handled", err)
	}
	if moved != 0 {
		t.Fatalf("moved = %d, want nothing counted as moved", moved)
	}
	if fetcher.singleMoves != 1 {
		t.Fatalf("single moves = %d, want one attempt", fetcher.singleMoves)
	}
	if !sourceReconcileDue(fixture) {
		t.Fatal("the source folder is still waiting out its pace, so its stale row survives the move")
	}
}

// Dragging one message takes its own path, and the same server answer must not
// mean something different there: it used to come back as HTTP 502 for as long
// as the stale row existed, which is for as long as nothing reconciled it.
func TestMoveMessageTreatsAGoneSourceUIDAsHandled(t *testing.T) {
	fixture, fetcher, ids := newGoneSourceFixture(t, []uint32{43}, nil)
	fixture.service.recordMailboxReconciled(fixture.userID, fixture.source)
	ctx := context.Background()

	if err := fixture.service.MoveMessage(ctx, fixture.userID, ids[0], fixture.destination.ID); err != nil {
		t.Fatalf("single move of a message the folder no longer holds = %v, want it handled", err)
	}
	if fetcher.singleMoves != 1 {
		t.Fatalf("single moves = %d, want one attempt", fetcher.singleMoves)
	}
	if !sourceReconcileDue(fixture) {
		t.Fatal("the source folder is still waiting out its pace, so its stale row survives the move")
	}
}

// A single move the server actually refuses is still an error the caller sees.
func TestMoveMessageStillReportsARefusedMove(t *testing.T) {
	fixture := newMoveTestFixture(t)
	fixture.fetcher.moveErr = errors.New("server refused the move")
	ctx := context.Background()

	err := fixture.service.MoveMessage(ctx, fixture.userID, fixture.message.ID, fixture.destination.ID)
	if err == nil || !strings.Contains(err.Error(), "server refused the move") {
		t.Fatalf("single move error = %v, want a refused move still reported", err)
	}
}
