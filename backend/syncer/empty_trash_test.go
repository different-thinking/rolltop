// File overview: Coverage for emptying a Trash folder: what is deleted remotely, what the
// local mirror does about it, and which folders may be emptied at all.

package syncer

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"rolltop/backend/store"
	"rolltop/backend/store/storetest"
)

const emptyTrashUIDValidity uint32 = 900

type emptyTrashExpungeCall struct {
	mailbox     string
	uids        []uint32
	uidValidity uint32
}

// emptyTrashFetcher is a Trash folder on a fake server. Expunging removes UIDs
// from it, so the snapshot the reconciliation takes afterwards reports what the
// server would really have left.
type emptyTrashFetcher struct {
	uids []uint32
	// highestUID is the largest UID this folder has ever reported. UIDNEXT never
	// decreases on a real server, so an emptied folder still refuses to place
	// new mail at the UIDs it just deleted — and reconciliation depends on that:
	// a local row at or above UIDNEXT is one it must not remove.
	highestUID uint32
	// keep names UIDs the server refuses to remove, so a partial delete can be
	// distinguished from a finished one.
	keep         []uint32
	expungeErr   error
	expungeCalls []emptyTrashExpungeCall
	snapshots    int
}

func (f *emptyTrashFetcher) ListMailboxes(context.Context, store.MailAccount) ([]MailboxInfo, error) {
	return nil, errors.New("unexpected ListMailboxes call")
}

func (f *emptyTrashFetcher) MailboxStatus(context.Context, store.MailAccount, string) (MailboxStatus, error) {
	return MailboxStatus{}, errors.New("unexpected MailboxStatus call")
}

func (f *emptyTrashFetcher) UIDs(context.Context, store.MailAccount, string) ([]uint32, error) {
	return slices.Clone(f.uids), nil
}

func (f *emptyTrashFetcher) FetchMailbox(context.Context, store.MailAccount, string, uint32, func(FetchedMessage) error) error {
	return errors.New("unexpected FetchMailbox call")
}

func (f *emptyTrashFetcher) FetchMessage(context.Context, store.MailAccount, string, uint32) (FetchedMessage, error) {
	return FetchedMessage{}, errors.New("unexpected FetchMessage call")
}

func (f *emptyTrashFetcher) AppendMessage(context.Context, store.MailAccount, string, []byte, string, time.Time) (FetchedMessage, error) {
	return FetchedMessage{}, errors.New("unexpected AppendMessage call")
}

func (f *emptyTrashFetcher) SetSeen(context.Context, store.MailAccount, string, uint32, bool) error {
	return errors.New("unexpected SetSeen call")
}

func (f *emptyTrashFetcher) SeenUIDs(context.Context, store.MailAccount, string) ([]uint32, error) {
	return nil, errors.New("unexpected SeenUIDs call")
}

func (f *emptyTrashFetcher) SetFlagged(context.Context, store.MailAccount, string, uint32, bool) error {
	return errors.New("unexpected SetFlagged call")
}

func (f *emptyTrashFetcher) FlaggedUIDs(context.Context, store.MailAccount, string) ([]uint32, error) {
	return nil, errors.New("unexpected FlaggedUIDs call")
}

func (f *emptyTrashFetcher) MoveMessage(context.Context, store.MailAccount, string, string, uint32) error {
	return errors.New("unexpected MoveMessage call")
}

func (f *emptyTrashFetcher) SnapshotMailboxUIDs(context.Context, store.MailAccount, string) (MailboxUIDSnapshot, error) {
	f.snapshots++
	for _, uid := range f.uids {
		if uid > f.highestUID {
			f.highestUID = uid
		}
	}
	next := f.highestUID + 1
	return MailboxUIDSnapshot{UIDs: slices.Clone(f.uids), UIDValidity: emptyTrashUIDValidity, UIDNext: next + 100}, nil
}

func (f *emptyTrashFetcher) ExpungeMessages(_ context.Context, _ store.MailAccount, mailbox string, uids []uint32, uidValidity uint32) ([]uint32, error) {
	f.expungeCalls = append(f.expungeCalls, emptyTrashExpungeCall{mailbox: mailbox, uids: slices.Clone(uids), uidValidity: uidValidity})
	if f.expungeErr != nil {
		return nil, f.expungeErr
	}
	gone := make([]uint32, 0, len(uids))
	for _, uid := range uids {
		if slices.Contains(f.keep, uid) {
			continue
		}
		gone = append(gone, uid)
		f.uids = slices.DeleteFunc(f.uids, func(existing uint32) bool { return existing == uid })
	}
	return gone, nil
}

type emptyTrashFixture struct {
	store   *store.Store
	service *Service
	fetcher *emptyTrashFetcher
	userID  int64
	account store.MailAccount
	trash   store.Mailbox
	inbox   store.Mailbox
	// messages maps remote UID to the local row mirroring it.
	messages map[uint32]store.MessageRecord
}

func newEmptyTrashFixture(t *testing.T, uids []uint32) emptyTrashFixture {
	t.Helper()
	ctx := context.Background()
	db, err := storetest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	user, err := db.CreateUser(ctx, "empty-trash@example.test", "Empty Trash", "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	account, err := db.CreateMailAccount(ctx, store.MailAccount{
		UserID: user.ID, Email: "empty-trash@example.test", Host: "imap.example.test", Port: 993,
		Username: "empty-trash", EncryptedPassword: "encrypted-test-value", UseTLS: true, Mailbox: store.DefaultMailboxPattern,
	})
	if err != nil {
		t.Fatal(err)
	}
	inbox, err := db.GetOrCreateMailbox(ctx, user.ID, account.ID, "INBOX")
	if err != nil {
		t.Fatal(err)
	}
	trash, err := db.GetOrCreateMailboxWithRole(ctx, user.ID, account.ID, "Trash", "trash")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateMailboxRemoteStatus(ctx, user.ID, trash.ID, len(uids), 0, 9000, emptyTrashUIDValidity); err != nil {
		t.Fatal(err)
	}
	trash, err = db.GetMailboxForUser(ctx, user.ID, trash.ID)
	if err != nil {
		t.Fatal(err)
	}
	messages := map[uint32]store.MessageRecord{}
	for _, uid := range uids {
		blob, err := db.CreateBlob(ctx, store.BlobRecord{
			UserID: user.ID, Kind: "message-remote",
			Path:   filepath.Join("users", "empty-trash", "message-"+string(rune('a'+int(uid%26)))+".eml"),
			SHA256: "sha-" + string(rune('a'+int(uid%26))), Size: 10,
		})
		if err != nil {
			t.Fatal(err)
		}
		date := time.Date(2026, 2, 3, 9, 0, 0, 0, time.UTC)
		message, err := db.CreateMessage(ctx, store.CreateMessage{
			UserID: user.ID, AccountID: account.ID, MailboxID: trash.ID, BlobID: blob.ID,
			MessageIDHeader: "<trash-" + string(rune('a'+int(uid%26))) + "@example.test>", Subject: "Deleted mail",
			FromAddr: "sender@example.test", Date: date, InternalDate: date,
			UID: uid, UIDValidity: int64(emptyTrashUIDValidity), BlobPath: blob.Path,
		})
		if err != nil {
			t.Fatal(err)
		}
		messages[uid] = message
	}
	fetcher := &emptyTrashFetcher{uids: slices.Clone(uids)}
	// Retries are part of what these tests exercise; the pause between them is
	// not, and the production one would add seconds to every failing case.
	service := &Service{Store: db, Fetcher: fetcher, emptyTrashRetryDelay: time.Microsecond}
	return emptyTrashFixture{
		store: db, service: service, fetcher: fetcher,
		userID: user.ID, account: account, trash: trash, inbox: inbox, messages: messages,
	}
}

func (f emptyTrashFixture) runEmpty(t *testing.T) store.SyncRun {
	t.Helper()
	ctx := context.Background()
	run, err := f.store.CreateSyncRun(ctx, f.userID, f.account.ID)
	if err != nil {
		t.Fatal(err)
	}
	f.service.runEmptyTrash(ctx, f.userID, f.account, f.trash, run.ID, time.Time{}, store.SyncProgress{}, nil, nil)
	finished, err := f.store.GetSyncRunForUser(ctx, f.userID, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	return finished
}

func TestEmptyTrashDeletesRemoteMailAndTheLocalMirror(t *testing.T) {
	fixture := newEmptyTrashFixture(t, []uint32{11, 12, 13})
	ctx := context.Background()

	run := fixture.runEmpty(t)

	if run.Status != "ok" {
		t.Fatalf("run status = %q (%s), want ok", run.Status, run.Error)
	}
	if len(fixture.fetcher.expungeCalls) != 1 {
		t.Fatalf("expunge calls = %d, want one batch", len(fixture.fetcher.expungeCalls))
	}
	call := fixture.fetcher.expungeCalls[0]
	if call.mailbox != fixture.trash.Name || call.uidValidity != emptyTrashUIDValidity {
		t.Fatalf("expunge call = %+v, want the trash folder under its own generation", call)
	}
	if !slices.Equal(call.uids, []uint32{11, 12, 13}) {
		t.Fatalf("expunged uids = %v, want every UID the folder held", call.uids)
	}
	if len(fixture.fetcher.uids) != 0 {
		t.Fatalf("server still holds %v after emptying the trash", fixture.fetcher.uids)
	}
	for uid, message := range fixture.messages {
		if _, err := fixture.store.GetMessageForUser(ctx, fixture.userID, message.ID); !store.IsNotFound(err) {
			t.Fatalf("local row for uid %d = %v, want it removed with the remote message", uid, err)
		}
	}
}

// TestEmptyTrashReleasesForegroundBeforeLocalCleanup guards the fix for a
// purge that blocked every other foreground mail action — sending, moving —
// for as long as its local reconciliation took. releaseForeground must run
// once the remote delete is settled, strictly before onDone, which only runs
// once local cleanup (reconcileMailboxUIDs) has finished.
func TestEmptyTrashReleasesForegroundBeforeLocalCleanup(t *testing.T) {
	fixture := newEmptyTrashFixture(t, []uint32{61, 62})
	ctx := context.Background()
	run, err := fixture.store.CreateSyncRun(ctx, fixture.userID, fixture.account.ID)
	if err != nil {
		t.Fatal(err)
	}

	var order []string
	fixture.service.runEmptyTrash(ctx, fixture.userID, fixture.account, fixture.trash, run.ID, time.Time{}, store.SyncProgress{},
		func() { order = append(order, "release") },
		func() { order = append(order, "done") })

	if len(order) != 2 || order[0] != "release" || order[1] != "done" {
		t.Fatalf("callback order = %v, want [release done]: releasing the foreground reservation only after local cleanup blocks every other foreground mail action for as long as that cleanup takes", order)
	}
}

// TestEmptyTrashReportsACleanupPhaseAfterDeleting guards against a run that
// looks stuck at 100%: local reconciliation after a large purge is its own
// per-message transaction pair and can run long after the server confirms
// everything gone, so the run's own label must move to say so.
func TestEmptyTrashReportsACleanupPhaseAfterDeleting(t *testing.T) {
	fixture := newEmptyTrashFixture(t, []uint32{41, 42})

	run := fixture.runEmpty(t)

	if run.Status != "ok" {
		t.Fatalf("run status = %q (%s), want ok", run.Status, run.Error)
	}
	want := "Cleaning up " + fixture.trash.Name
	if run.CurrentMailbox != want {
		t.Fatalf("current_mailbox = %q, want %q so local cleanup does not look identical to the batch still running", run.CurrentMailbox, want)
	}
}

func TestEmptyTrashKeepsLocalRowsForMailTheServerRefusedToDelete(t *testing.T) {
	fixture := newEmptyTrashFixture(t, []uint32{21, 22})
	fixture.fetcher.keep = []uint32{22}
	ctx := context.Background()

	run := fixture.runEmpty(t)

	if run.Status != "failed" {
		t.Fatalf("run status = %q, want failed when the server kept a message", run.Status)
	}
	if _, err := fixture.store.GetMessageForUser(ctx, fixture.userID, fixture.messages[21].ID); !store.IsNotFound(err) {
		t.Fatalf("local row for the deleted message = %v, want it removed", err)
	}
	kept, err := fixture.store.GetMessageForUser(ctx, fixture.userID, fixture.messages[22].ID)
	if err != nil {
		t.Fatalf("local row for the message the server kept: %v", err)
	}
	if kept.UID != 22 {
		t.Fatalf("kept row uid = %d, want 22", kept.UID)
	}
}

func TestEmptyTrashLeavesTheMirrorAloneWhenNothingWasDeleted(t *testing.T) {
	fixture := newEmptyTrashFixture(t, []uint32{31})
	fixture.fetcher.expungeErr = errors.New("server said no")
	ctx := context.Background()

	run := fixture.runEmpty(t)

	if run.Status != "failed" {
		t.Fatalf("run status = %q, want failed", run.Status)
	}
	if _, err := fixture.store.GetMessageForUser(ctx, fixture.userID, fixture.messages[31].ID); err != nil {
		t.Fatalf("local row after a failed expunge = %v, want it untouched", err)
	}
	// A failed delete must not reconcile: the local mirror is the only remaining
	// copy of mail that is still on the server. The two listings are the one
	// taken before the expunge and the recount of what the folder still holds.
	if fixture.fetcher.snapshots != 2 {
		t.Fatalf("snapshots = %d, want the listing before the expunge and the recount after it, and no reconciliation",
			fixture.fetcher.snapshots)
	}
}

func TestStartEmptyTrashOnlyAcceptsATrashFolder(t *testing.T) {
	fixture := newEmptyTrashFixture(t, []uint32{41})
	ctx := context.Background()

	if _, err := fixture.service.StartEmptyTrash(ctx, fixture.userID, fixture.inbox.ID, nil, nil); err == nil {
		t.Fatal("emptying the inbox was accepted")
	}
}

func TestStartEmptyTrashRequiresAFetcherThatCanDelete(t *testing.T) {
	fixture := newEmptyTrashFixture(t, []uint32{51})
	fixture.service.Fetcher = &moveTestFetcher{}
	ctx := context.Background()

	_, err := fixture.service.StartEmptyTrash(ctx, fixture.userID, fixture.trash.ID, nil, nil)
	if !errors.Is(err, ErrEmptyTrashUnsupported) {
		t.Fatalf("error = %v, want ErrEmptyTrashUnsupported", err)
	}
}
