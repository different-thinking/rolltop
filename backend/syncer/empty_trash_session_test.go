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
	// fail decides, per attempt, whether the connection refuses this batch the
	// way a dropped socket would. It sees how many attempts came before it and
	// which UIDs this one names, so a test can kill one attempt, one batch, or
	// everything.
	fail func(attempt int, uids []uint32) error
}

// failEveryAttempt refuses every batch, as a connection that is dead for good does.
func failEveryAttempt(err error) func(int, []uint32) error {
	return func(int, []uint32) error { return err }
}

// failAttempt refuses exactly one attempt, as a connection dropped mid-purge does.
func failAttempt(target int, err error) func(int, []uint32) error {
	return func(attempt int, _ []uint32) error {
		if attempt == target {
			return err
		}
		return nil
	}
}

// failBatchStartingAt refuses every attempt on the one batch that begins with
// this UID, as a message the server will not delete does.
func failBatchStartingAt(uid uint32, err error) func(int, []uint32) error {
	return func(_ int, uids []uint32) error {
		if len(uids) > 0 && uids[0] == uid {
			return err
		}
		return nil
	}
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
	attempt := s.fetcher.batches
	s.fetcher.batches++
	if s.fetcher.fail != nil {
		if err := s.fetcher.fail(attempt, uids); err != nil {
			return nil, err
		}
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

// A dropped connection is what a long purge runs into, not what ends it: the
// batch is tried again on a fresh login and the folder still empties.
func TestEmptyTrashReconnectsWhenTheHeldConnectionDies(t *testing.T) {
	uids := trashUIDs(emptyTrashBatchSize*3 + 4)
	fixture := newEmptyTrashFixture(t, uids)
	fetcher := &sessionExpungeFetcher{
		emptyTrashFetcher: fixture.fetcher,
		fail:              failAttempt(1, errors.New("imap: connection closed")),
	}
	fixture.service.Fetcher = fetcher

	finished := fixture.runEmpty(t)

	if finished.Status != "ok" || finished.Error != "" {
		t.Fatalf("run status=%q error=%q, want the purge to survive one dropped connection",
			finished.Status, finished.Error)
	}
	if finished.MessagesStored != len(uids) {
		t.Fatalf("run stored=%d of %d, want every message deleted", finished.MessagesStored, len(uids))
	}
	if len(fixture.fetcher.uids) != 0 {
		t.Fatalf("server still holds %d messages after the purge", len(fixture.fetcher.uids))
	}
	if fetcher.opens != 2 {
		t.Fatalf("logins = %d, want a second one only for the batch that lost its connection", fetcher.opens)
	}
	if fetcher.closes != fetcher.opens {
		t.Fatalf("session closes = %d for %d logins, want every connection given back", fetcher.closes, fetcher.opens)
	}
	for uid, message := range fixture.messages {
		if _, err := fixture.store.GetMessageForUser(context.Background(), fixture.userID, message.ID); !store.IsNotFound(err) {
			t.Fatalf("local row for UID %d survived the purge: %v", uid, err)
		}
	}
}

// One batch the server will not take must not strand the thousands behind it:
// the rest of the folder is still emptied, and the run says what is left.
func TestEmptyTrashCarriesOnPastABatchItCannotDelete(t *testing.T) {
	uids := trashUIDs(emptyTrashBatchSize * 4)
	fixture := newEmptyTrashFixture(t, uids)
	fetcher := &sessionExpungeFetcher{
		emptyTrashFetcher: fixture.fetcher,
		fail:              failBatchStartingAt(uids[emptyTrashBatchSize], errors.New("server said no")),
	}
	fixture.service.Fetcher = fetcher

	finished := fixture.runEmpty(t)

	if finished.Status != "failed" || finished.Error == "" {
		t.Fatalf("run status=%q error=%q, want the purge to report the batch it lost",
			finished.Status, finished.Error)
	}
	wantDeleted := len(uids) - emptyTrashBatchSize
	if finished.MessagesStored != wantDeleted {
		t.Fatalf("run stored=%d, want the %d messages outside the refused batch", finished.MessagesStored, wantDeleted)
	}
	refused := uids[emptyTrashBatchSize : emptyTrashBatchSize*2]
	if !slices.Equal(fixture.fetcher.uids, refused) {
		t.Fatalf("server holds %v, want only the refused batch", fixture.fetcher.uids)
	}
	for _, uid := range refused {
		message := fixture.messages[uid]
		if _, err := fixture.store.GetMessageForUser(context.Background(), fixture.userID, message.ID); err != nil {
			t.Fatalf("local row for undeleted UID %d was dropped: %v", uid, err)
		}
	}
	for _, uid := range uids[emptyTrashBatchSize*2:] {
		message := fixture.messages[uid]
		if _, err := fixture.store.GetMessageForUser(context.Background(), fixture.userID, message.ID); !store.IsNotFound(err) {
			t.Fatalf("local row for UID %d behind the refused batch survived: %v", uid, err)
		}
	}
}

// A connection that is dead for good stops the purge instead of being logged
// into again once per remaining batch.
func TestEmptyTrashStopsRetryingAConnectionThatStaysDead(t *testing.T) {
	uids := trashUIDs(emptyTrashBatchSize * 6)
	fixture := newEmptyTrashFixture(t, uids)
	fetcher := &sessionExpungeFetcher{
		emptyTrashFetcher: fixture.fetcher,
		fail:              failEveryAttempt(errors.New("imap: connection closed")),
	}
	fixture.service.Fetcher = fetcher

	finished := fixture.runEmpty(t)

	if finished.Status != "failed" || finished.Error == "" {
		t.Fatalf("run status=%q error=%q, want a purge that could not start to fail", finished.Status, finished.Error)
	}
	wantAttempts := emptyTrashBatchAttempts * emptyTrashBatchGiveUp
	if fetcher.batches != wantAttempts {
		t.Fatalf("expunge attempts = %d, want %d rather than one set per remaining batch",
			fetcher.batches, wantAttempts)
	}
	if fetcher.closes != fetcher.opens {
		t.Fatalf("session closes = %d for %d logins, want every dead connection dropped", fetcher.closes, fetcher.opens)
	}
	if len(fixture.fetcher.uids) != len(uids) {
		t.Fatalf("server holds %d messages, want all %d still there", len(fixture.fetcher.uids), len(uids))
	}
}

// A purge that dies partway still keeps what it did delete: the local mirror is
// reconciled for the batches that landed, and the run says the rest are still
// there.
func TestEmptyTrashKeepsTheDeletedHalfWhenTheConnectionDies(t *testing.T) {
	uids := trashUIDs(emptyTrashBatchSize*3 + 5)
	fixture := newEmptyTrashFixture(t, uids)
	fetcher := &sessionExpungeFetcher{
		emptyTrashFetcher: fixture.fetcher,
		fail: func(attempt int, _ []uint32) error {
			if attempt == 0 {
				return nil
			}
			return errors.New("connection reset")
		},
	}
	fixture.service.Fetcher = fetcher

	finished := fixture.runEmpty(t)

	if finished.Status != "failed" || finished.Error == "" {
		t.Fatalf("run status=%q error=%q, want the purge to report that it stopped", finished.Status, finished.Error)
	}
	if fetcher.closes != fetcher.opens {
		t.Fatalf("session closes = %d for %d logins, want every dead connection dropped", fetcher.closes, fetcher.opens)
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
