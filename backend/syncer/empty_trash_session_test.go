// File overview: A full Trash folder is emptied in batches. These cover what
// those batches share — one login rather than one each — and what happens when
// the connection they share dies mid-purge.

package syncer

import (
	"context"
	"errors"
	"slices"
	"testing"

	"rolltop/backend/store"
)

// sessionExpungeFetcher can hold a connection open for a purge and counts how
// often one actually logs in rather than reusing what it already has.
type sessionExpungeFetcher struct {
	*emptyTrashFetcher
	opens   int
	closes  int
	batches int
	// failAfter refuses the batch at this index and every one after it, as a
	// dropped socket would.
	failAfter int
	failErr   error
}

func (f *sessionExpungeFetcher) OpenExpungeSession(_ context.Context, _ store.MailAccount) (ExpungeSession, error) {
	f.opens++
	return &fakeExpungeSession{fetcher: f}, nil
}

type fakeExpungeSession struct {
	fetcher *sessionExpungeFetcher
	closed  bool
}

func (s *fakeExpungeSession) ExpungeMessages(ctx context.Context, mailbox string, uids []uint32, uidValidity uint32) ([]uint32, error) {
	if s.closed {
		return nil, errors.New("expunge session is closed")
	}
	index := s.fetcher.batches
	s.fetcher.batches++
	if s.fetcher.failErr != nil && index >= s.fetcher.failAfter {
		return nil, s.fetcher.failErr
	}
	return s.fetcher.emptyTrashFetcher.ExpungeMessages(ctx, store.MailAccount{}, mailbox, uids, uidValidity)
}

func (s *fakeExpungeSession) Close() error {
	s.closed = true
	s.fetcher.closes++
	return nil
}

func trashUIDs(count int) []uint32 {
	uids := make([]uint32, 0, count)
	for i := 0; i < count; i++ {
		uids = append(uids, uint32(i+1))
	}
	return uids
}

// A full Trash folder is emptied in batches, and reconnecting for each of them
// is most of what the purge costs. They share one login.
func TestEmptyTrashSharesOneConnectionAcrossItsBatches(t *testing.T) {
	uids := trashUIDs(emptyTrashBatchSize*2 + 7)
	fixture := newEmptyTrashFixture(t, uids)
	fetcher := &sessionExpungeFetcher{emptyTrashFetcher: fixture.fetcher}
	fixture.service.Fetcher = fetcher

	finished := fixture.runEmpty(t)

	if finished.Status != "ok" || finished.Error != "" {
		t.Fatalf("run status=%q error=%q, want a clean purge", finished.Status, finished.Error)
	}
	if fetcher.batches != 3 {
		t.Fatalf("expunge batches = %d, want 3 for %d messages", fetcher.batches, len(uids))
	}
	if fetcher.opens != 1 {
		t.Fatalf("logins = %d, want every batch to share one", fetcher.opens)
	}
	if fetcher.closes != 1 {
		t.Fatalf("session closes = %d, want the held connection given back once", fetcher.closes)
	}
	if finished.MessagesStored != len(uids) {
		t.Fatalf("run stored=%d, want every message deleted", finished.MessagesStored)
	}
	for uid, message := range fixture.messages {
		if _, err := fixture.store.GetMessageForUser(context.Background(), fixture.userID, message.ID); !store.IsNotFound(err) {
			t.Fatalf("local row for UID %d survived the purge: %v", uid, err)
		}
	}
}

// A purge that dies partway still keeps what it did delete: the local mirror is
// reconciled for the batches that landed, and the run says the rest are still
// there.
func TestEmptyTrashKeepsTheDeletedHalfWhenTheConnectionDies(t *testing.T) {
	uids := trashUIDs(emptyTrashBatchSize + 5)
	fixture := newEmptyTrashFixture(t, uids)
	fetcher := &sessionExpungeFetcher{
		emptyTrashFetcher: fixture.fetcher,
		failAfter:         1,
		failErr:           errors.New("connection reset"),
	}
	fixture.service.Fetcher = fetcher

	finished := fixture.runEmpty(t)

	if finished.Status != "failed" || finished.Error == "" {
		t.Fatalf("run status=%q error=%q, want the purge to report that it stopped", finished.Status, finished.Error)
	}
	if fetcher.closes != 1 {
		t.Fatalf("session closes = %d, want the dead connection dropped once", fetcher.closes)
	}
	// The first batch really was deleted on the server, so its local rows go.
	for _, uid := range uids[:emptyTrashBatchSize] {
		message := fixture.messages[uid]
		if _, err := fixture.store.GetMessageForUser(context.Background(), fixture.userID, message.ID); !store.IsNotFound(err) {
			t.Fatalf("local row for deleted UID %d survived: %v", uid, err)
		}
	}
	// The rest are still on the server, so they are still in the mirror.
	for _, uid := range uids[emptyTrashBatchSize:] {
		if !slices.Contains(fixture.fetcher.uids, uid) {
			t.Fatalf("UID %d was deleted after the connection died", uid)
		}
		message := fixture.messages[uid]
		if _, err := fixture.store.GetMessageForUser(context.Background(), fixture.userID, message.ID); err != nil {
			t.Fatalf("local row for undeleted UID %d was dropped: %v", uid, err)
		}
	}
}

// A fetcher that cannot hold a connection open still empties the folder, one
// connection per batch, rather than failing the purge.
func TestEmptyTrashFallsBackWithoutSessionSupport(t *testing.T) {
	uids := trashUIDs(emptyTrashBatchSize + 3)
	fixture := newEmptyTrashFixture(t, uids)
	if _, ok := fixture.service.Fetcher.(ExpungeSessionFetcher); ok {
		t.Fatal("fixture fetcher unexpectedly supports expunge sessions")
	}

	finished := fixture.runEmpty(t)

	if finished.Status != "ok" || finished.MessagesStored != len(uids) {
		t.Fatalf("run status=%q stored=%d, want the fallback to empty the folder",
			finished.Status, finished.MessagesStored)
	}
	if len(fixture.fetcher.expungeCalls) != 2 {
		t.Fatalf("expunge calls = %d, want one per batch", len(fixture.fetcher.expungeCalls))
	}
}
