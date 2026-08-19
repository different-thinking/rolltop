// File overview: What Send and archive does after the reply is away: the message
// the reply answers is filed in the account's own Archive folder, and every
// reason it cannot be leaves the reply sent and the message where it was.

package web

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"
	"time"

	"rolltop/backend/blob"
	"rolltop/backend/store"
	"rolltop/backend/store/storetest"
	"rolltop/backend/syncer"
)

// archiveMoveFetcher accepts the move and answers with the COPYUID metadata a
// real server returns, which is what lets the move prove the source generation
// rather than refusing for want of a receipt.
type archiveMoveFetcher struct {
	syncer.Fetcher
}

func (f *archiveMoveFetcher) MoveMessageWithReceipt(_ context.Context, _ store.MailAccount, _ string, _ string, uid uint32, expectedSourceUIDValidity uint32) (*syncer.MoveReceipt, error) {
	return &syncer.MoveReceipt{DestinationUIDValidity: expectedSourceUIDValidity, DestinationUID: uid}, nil
}

type archiveAfterSendFixture struct {
	server  *Server
	db      *store.Store
	ctx     context.Context
	owner   store.User
	inbox   store.Mailbox
	archive store.Mailbox
	message store.MessageRecord
}

func newArchiveAfterSendFixture(t *testing.T, mapArchive bool) archiveAfterSendFixture {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	db, err := storetest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	blobStore := blob.New(dir)

	owner, err := db.CreateUser(ctx, "reply-owner@example.test", "Owner", "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	account, err := db.CreateMailAccount(ctx, store.MailAccount{
		UserID: owner.ID, Email: "owner@example.test", Host: "imap.example.test", Port: 993,
		Username: "owner@example.test", EncryptedPassword: "secret", UseTLS: true, Mailbox: "*",
	})
	if err != nil {
		t.Fatal(err)
	}
	inbox, err := db.GetOrCreateMailbox(ctx, owner.ID, account.ID, "INBOX")
	if err != nil {
		t.Fatal(err)
	}
	archive, err := db.GetOrCreateMailbox(ctx, owner.ID, account.ID, "Archive")
	if err != nil {
		t.Fatal(err)
	}
	if mapArchive {
		prefs := store.DefaultSwipePreferences(owner.ID)
		prefs.ArchiveMailboxes = []store.SwipeArchiveMailbox{{AccountID: account.ID, MailboxID: archive.ID}}
		if _, err := db.SaveSwipePreferences(ctx, prefs); err != nil {
			t.Fatal(err)
		}
	}
	// A move refuses to run against a mailbox generation the message does not
	// belong to, so the fixture gives the source folder and its message the same
	// UIDVALIDITY a synced mailbox would have.
	const uidValidity = uint32(42)
	if err := db.UpdateMailboxRemoteStatus(ctx, owner.ID, inbox.ID, 1, 0, 2, uidValidity); err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateMailboxRemoteStatus(ctx, owner.ID, archive.ID, 0, 0, 1, uidValidity); err != nil {
		t.Fatal(err)
	}
	blobRecord, err := db.CreateBlob(ctx, store.BlobRecord{
		UserID: owner.ID, Kind: "message",
		Path: filepath.Join("blobs", "reply", "a"), SHA256: "reply-sha-a", Size: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	message, err := db.CreateMessage(ctx, store.CreateMessage{
		UserID: owner.ID, AccountID: account.ID, MailboxID: inbox.ID, BlobID: blobRecord.ID,
		MessageIDHeader: "<question@partner.test>", ThreadKey: "msgid:<question@partner.test>",
		Subject: "Question", FromAddr: "sender@partner.test", ToAddr: "owner@example.test",
		Date: now, InternalDate: now, UID: 1, UIDValidity: int64(uidValidity), Size: 10, BlobPath: blobRecord.Path,
	})
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{
		store: db, blobs: blobStore,
		syncer:    &syncer.Service{Store: db, Blobs: blobStore, Fetcher: &archiveMoveFetcher{}},
		masterKey: bytes.Repeat([]byte{7}, 32), events: newEventHub(),
	}
	return archiveAfterSendFixture{server: server, db: db, ctx: ctx, owner: owner, inbox: inbox, archive: archive, message: message}
}

// stillInInbox reports whether the message the reply answered is where it was.
// A completed move drops the local row and leaves the destination folder to
// mirror it back on its next sync, so "gone from the Inbox" is what an archive
// looks like from here, not "now carries the Archive mailbox id".
func (f archiveAfterSendFixture) stillInInbox(t *testing.T) bool {
	t.Helper()
	current, err := f.db.GetMessageForUser(f.ctx, f.owner.ID, f.message.ID)
	if store.IsNotFound(err) {
		return false
	}
	if err != nil {
		t.Fatal(err)
	}
	return current.MailboxID == f.inbox.ID
}

// The button's whole promise: the reply goes out and the message it answers
// leaves the list, filed in the folder every other Archive action uses.
func TestSendAndArchiveFilesTheMessageTheReplyAnswers(t *testing.T) {
	f := newArchiveAfterSendFixture(t, true)
	folder := f.server.archiveRepliedMessage(f.ctx, f.owner.ID, composeForm{
		InReplyToID: f.message.ID, ArchiveAfterSend: true,
	})
	if folder != f.archive.Name {
		t.Fatalf("archived into %q, want %q", folder, f.archive.Name)
	}
	if f.stillInInbox(t) {
		t.Fatal("message is still in the Inbox after Send and archive")
	}
}

// Send is the other half of the pair and has to leave the conversation alone.
func TestPlainSendLeavesTheMessageWhereItIs(t *testing.T) {
	f := newArchiveAfterSendFixture(t, true)
	if folder := f.server.archiveRepliedMessage(f.ctx, f.owner.ID, composeForm{InReplyToID: f.message.ID}); folder != "" {
		t.Fatalf("plain send archived into %q, want nothing moved", folder)
	}
	if !f.stillInInbox(t) {
		t.Fatal("plain send moved the message out of the Inbox")
	}
}

// An account with no Archive folder chosen still sends. Reporting the send as a
// failure because there was nowhere to file the original would be a worse answer
// than saying nothing: the reply is gone either way and cannot be unsent.
func TestSendAndArchiveWithoutAnArchiveFolderStillSends(t *testing.T) {
	f := newArchiveAfterSendFixture(t, false)
	if folder := f.server.archiveRepliedMessage(f.ctx, f.owner.ID, composeForm{
		InReplyToID: f.message.ID, ArchiveAfterSend: true,
	}); folder != "" {
		t.Fatalf("archived into %q, want nothing moved when no Archive folder is chosen", folder)
	}
	if !f.stillInInbox(t) {
		t.Fatal("message left the Inbox even though nothing could be archived")
	}
}

// A compose that answers nothing -- a new message, a forward -- has no message
// to file, whatever the flag says.
func TestSendAndArchiveWithoutAReplyTargetMovesNothing(t *testing.T) {
	f := newArchiveAfterSendFixture(t, true)
	if folder := f.server.archiveRepliedMessage(f.ctx, f.owner.ID, composeForm{ArchiveAfterSend: true}); folder != "" {
		t.Fatalf("archived into %q, want nothing moved without a message to answer", folder)
	}
	if !f.stillInInbox(t) {
		t.Fatal("message left the Inbox even though nothing could be archived")
	}
}
