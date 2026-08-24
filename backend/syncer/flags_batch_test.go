// File overview: Batched flag-push grouping, generation proof, and pending-clear tests.

package syncer

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"rolltop/backend/store"
	"rolltop/backend/store/storetest"
)

type flagBatchCall struct {
	mailbox     string
	uidValidity uint32
	changes     MailboxFlagChanges
}

// flagBatchTestFetcher only implements the batch capability; every legacy
// per-message call is an error so a regression back to one login per message
// fails loudly.
type flagBatchTestFetcher struct {
	moveTestFetcher
	applied bool
	err     error
	calls   []flagBatchCall
}

func (f *flagBatchTestFetcher) ApplyFlagChangesWithUIDValidity(_ context.Context, _ store.MailAccount,
	mailbox string, expectedUIDValidity uint32, changes MailboxFlagChanges) (bool, error) {
	f.calls = append(f.calls, flagBatchCall{mailbox: mailbox, uidValidity: expectedUIDValidity, changes: changes})
	return f.applied, f.err
}

type flagBatchFixture struct {
	store   *store.Store
	service *Service
	fetcher *flagBatchTestFetcher
	userID  int64
	account store.MailAccount
	inbox   store.Mailbox
	archive store.Mailbox
}

func newFlagBatchFixture(t *testing.T) flagBatchFixture {
	t.Helper()
	ctx := context.Background()
	db, err := storetest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	user, err := db.CreateUser(ctx, "flag-batch@example.test", "Flag Batch", "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	account, err := db.CreateMailAccount(ctx, store.MailAccount{
		UserID: user.ID, Email: "flag-batch@example.test", Host: "imap.example.test", Port: 993,
		Username: "flag-batch", EncryptedPassword: "encrypted-test-value", UseTLS: true, Mailbox: store.DefaultMailboxPattern,
	})
	if err != nil {
		t.Fatal(err)
	}
	inbox := flagBatchMailbox(t, ctx, db, user.ID, account.ID, "INBOX", 500)
	archive := flagBatchMailbox(t, ctx, db, user.ID, account.ID, "Archive", 501)
	fetcher := &flagBatchTestFetcher{applied: true}
	return flagBatchFixture{
		store:   db,
		service: &Service{Store: db, Fetcher: fetcher},
		fetcher: fetcher,
		userID:  user.ID,
		account: account,
		inbox:   inbox,
		archive: archive,
	}
}

func flagBatchMailbox(t *testing.T, ctx context.Context, db *store.Store, userID, accountID int64, name string, uidValidity uint32) store.Mailbox {
	t.Helper()
	mailbox, err := db.GetOrCreateMailbox(ctx, userID, accountID, name)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateMailboxRemoteStatus(ctx, userID, mailbox.ID, 1, 0, 100, uidValidity); err != nil {
		t.Fatal(err)
	}
	mailbox, err = db.GetMailboxForUser(ctx, userID, mailbox.ID)
	if err != nil {
		t.Fatal(err)
	}
	return mailbox
}

func (f flagBatchFixture) message(t *testing.T, mailbox store.Mailbox, uid uint32, uidValidity int64, read bool) store.MessageRecord {
	t.Helper()
	ctx := context.Background()
	date := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	blob, err := f.store.CreateBlob(ctx, store.BlobRecord{
		UserID: f.userID, Kind: "message-remote",
		Path:   "users/flag-batch/" + mailbox.Name + "-" + string(rune('a'+uid)) + ".eml",
		SHA256: "test-" + mailbox.Name + string(rune('a'+uid)), Size: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	msg, err := f.store.CreateMessage(ctx, store.CreateMessage{
		UserID: f.userID, AccountID: f.account.ID, MailboxID: mailbox.ID, BlobID: blob.ID,
		MessageIDHeader: "<flag-batch-" + mailbox.Name + "-" + strings.Repeat("x", int(uid%3)) + string(rune('a'+uid)) + "@example.test>",
		ThreadKey:       "thread:flag-batch",
		Subject:         "flag batch", FromAddr: "sender@example.test", ToAddr: "flag-batch@example.test",
		Date: date, InternalDate: date, UID: uid, UIDValidity: uidValidity, Size: 100,
		BodyText: "body", IsRead: read,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.MarkMessageReadForUser(ctx, f.userID, msg.ID, read, true); err != nil {
		t.Fatal(err)
	}
	msg, err = f.store.GetMessageForUser(ctx, f.userID, msg.ID)
	if err != nil {
		t.Fatal(err)
	}
	return msg
}

func TestPushPendingReadStateBatchesPerMailboxAndClearsPending(t *testing.T) {
	fixture := newFlagBatchFixture(t)
	ctx := context.Background()
	read := fixture.message(t, fixture.inbox, 1, 500, true)
	unread := fixture.message(t, fixture.inbox, 2, 500, false)
	other := fixture.message(t, fixture.archive, 3, 501, true)

	if err := fixture.service.PushPendingReadState(ctx, fixture.userID, 500); err != nil {
		t.Fatal(err)
	}
	if len(fixture.fetcher.calls) != 2 {
		t.Fatalf("batched flag calls = %d, want one per mailbox", len(fixture.fetcher.calls))
	}
	byMailbox := map[string]flagBatchCall{}
	for _, call := range fixture.fetcher.calls {
		byMailbox[call.mailbox] = call
	}
	inboxCall := byMailbox["INBOX"]
	if inboxCall.uidValidity != 500 || len(inboxCall.changes.SetSeen) != 1 || inboxCall.changes.SetSeen[0] != read.UID ||
		len(inboxCall.changes.ClearSeen) != 1 || inboxCall.changes.ClearSeen[0] != unread.UID {
		t.Fatalf("INBOX call = %+v", inboxCall)
	}
	archiveCall := byMailbox["Archive"]
	if archiveCall.uidValidity != 501 || len(archiveCall.changes.SetSeen) != 1 || archiveCall.changes.SetSeen[0] != other.UID {
		t.Fatalf("Archive call = %+v", archiveCall)
	}
	remaining, err := fixture.store.ListMessagesWithReadSyncPending(ctx, fixture.userID, 500)
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 0 {
		t.Fatalf("read-sync-pending rows after push = %d, want 0", len(remaining))
	}
}

func TestPushPendingReadStateLeavesFlagsPendingWhenGenerationMismatch(t *testing.T) {
	fixture := newFlagBatchFixture(t)
	ctx := context.Background()
	// The mailbox generation moves after the message was stored: the push
	// must skip it (it stays pending) without issuing any STORE for it.
	stale := fixture.message(t, fixture.inbox, 4, 500, true)
	if err := fixture.store.UpdateMailboxRemoteStatus(ctx, fixture.userID, fixture.inbox.ID, 1, 0, 100, 502); err != nil {
		t.Fatal(err)
	}
	if err := fixture.service.PushPendingReadState(ctx, fixture.userID, 500); err != nil {
		t.Fatal(err)
	}
	if len(fixture.fetcher.calls) != 0 {
		t.Fatalf("batched flag calls = %d, want none for a stale generation", len(fixture.fetcher.calls))
	}
	remaining, err := fixture.store.ListMessagesWithReadSyncPending(ctx, fixture.userID, 500)
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 1 || remaining[0].ID != stale.ID {
		t.Fatalf("pending rows = %+v, want the stale message still queued", remaining)
	}
}

func TestPushPendingReadStateNotAppliedKeepsFlagsPending(t *testing.T) {
	fixture := newFlagBatchFixture(t)
	fixture.fetcher.applied = false
	ctx := context.Background()
	fixture.message(t, fixture.inbox, 5, 500, true)
	if err := fixture.service.PushPendingReadState(ctx, fixture.userID, 500); err != nil {
		t.Fatal(err)
	}
	remaining, err := fixture.store.ListMessagesWithReadSyncPending(ctx, fixture.userID, 500)
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 1 {
		t.Fatalf("pending rows after unapplied push = %d, want 1", len(remaining))
	}
}

func TestPushPendingReadStateOneFolderFailureDoesNotBlockOthers(t *testing.T) {
	fixture := newFlagBatchFixture(t)
	fixture.fetcher.err = errors.New("mailbox is broken")
	ctx := context.Background()
	fixture.message(t, fixture.inbox, 6, 500, true)
	fixture.message(t, fixture.archive, 7, 501, true)
	err := fixture.service.PushPendingReadState(ctx, fixture.userID, 500)
	if err == nil || !strings.Contains(err.Error(), "mailbox is broken") {
		t.Fatalf("push error = %v, want the folder failure reported", err)
	}
	// Both folders were still attempted: one folder's failure is that
	// folder's problem.
	if len(fixture.fetcher.calls) != 2 {
		t.Fatalf("batched flag calls = %d, want both folders attempted", len(fixture.fetcher.calls))
	}
}
