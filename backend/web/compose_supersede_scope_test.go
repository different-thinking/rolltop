// File overview: What a compose tab asks to retire is checked before it is
// trashed, and a raw message file is only removed while nothing owns it.

package web

import (
	"context"
	"testing"
	"time"

	"rolltop/backend/blob"
	"rolltop/backend/store"
	"rolltop/backend/store/storetest"
	"rolltop/backend/syncer"
)

// TestSupersedeDraftLeavesNonDraftMessagesAlone covers the stale-ID case: a
// compose tab can hold a draft_message_id for minutes, and by the time an
// autosave fires that draft may have been sent or moved. Trashing whatever the
// ID now names would move ordinary mail, locally and on the server.
func TestSupersedeDraftLeavesNonDraftMessagesAlone(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db, err := storetest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	user, err := db.CreateUser(ctx, "scope@example.test", "Me", "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	account, err := db.UpsertMailAccount(ctx, store.MailAccount{
		UserID: user.ID, Email: "scope@example.test", Host: "imap.example.test", Port: 993,
		Username: "scope@example.test", EncryptedPassword: "encrypted", UseTLS: true, Mailbox: "INBOX",
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
	// The move this must not make has to be one that would otherwise go
	// through: both folders carry a live generation and the message names the
	// INBOX one, so the only thing standing between a stale id and the Trash is
	// the folder-role check itself.
	const inboxUIDValidity = 333
	if err := db.UpdateMailboxRemoteStatus(ctx, user.ID, inbox.ID, 0, 0, 1, inboxUIDValidity); err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateMailboxRemoteStatus(ctx, user.ID, trash.ID, 0, 0, 1, 999); err != nil {
		t.Fatal(err)
	}
	blobStore := blob.New(dir)
	fetcher := &draftMoveFetcher{captureAppendFetcher: &captureAppendFetcher{nextUID: 1, uidValidity: inboxUIDValidity}}
	server := &Server{
		store: db, blobs: blobStore,
		syncer:     &syncer.Service{Store: db, Blobs: blobStore, Fetcher: fetcher},
		syncRunner: syncer.NewRunner(nil),
	}

	message := createComposeScopeMessage(t, ctx, db, user.ID, account.ID, inbox.ID, "inbox.eml", 11, inboxUIDValidity)
	server.supersedeDraft(ctx, user.ID, message.ID)

	kept, err := db.GetMessageForUser(ctx, user.ID, message.ID)
	if err != nil {
		t.Fatalf("a stale draft id trashed an ordinary INBOX message: %v", err)
	}
	if kept.MailboxID != inbox.ID {
		t.Fatalf("message mailbox_id = %d, want INBOX (%d)", kept.MailboxID, inbox.ID)
	}
}

// TestDeleteUnreferencedRawMessageKeepsOwnedFiles pins the cleanup that runs
// when a Sent copy could not be recorded. The path it removes may already have
// been written for another message -- the syncer fetching the copy that was
// just appended lands on the same one -- and deleting the file then takes it
// away from a message that is still readable through it.
func TestDeleteUnreferencedRawMessageKeepsOwnedFiles(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db, err := storetest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	user, err := db.CreateUser(ctx, "cleanup@example.test", "Me", "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	account, err := db.UpsertMailAccount(ctx, store.MailAccount{
		UserID: user.ID, Email: "cleanup@example.test", Host: "imap.example.test", Port: 993,
		Username: "cleanup@example.test", EncryptedPassword: "encrypted", UseTLS: true, Mailbox: "INBOX",
	})
	if err != nil {
		t.Fatal(err)
	}
	sent, err := db.GetOrCreateMailboxWithRole(ctx, user.ID, account.ID, "Sent", "sent")
	if err != nil {
		t.Fatal(err)
	}
	blobStore := blob.New(dir)
	server := &Server{store: db, blobs: blobStore}

	saved, err := blobStore.SaveRawMessage(user.ID, account.ID, sent.Name, 42, []byte("From: me@example.test\r\n\r\nsent copy\r\n"))
	if err != nil {
		t.Fatal(err)
	}
	owner := createComposeScopeMessage(t, ctx, db, user.ID, account.ID, sent.ID, saved.Path, 42, 0)

	if err := server.deleteUnreferencedRawMessage(ctx, user.ID, saved.Path); err != nil {
		t.Fatal(err)
	}
	file, err := blobStore.OpenUserBlob(user.ID, saved.Path)
	if err != nil {
		t.Fatalf("cleanup removed a raw message another row still owns: %v", err)
	}
	_ = file.Close()
	if _, err := db.GetBlobByPathForUser(ctx, user.ID, saved.Path); err != nil {
		t.Fatalf("cleanup removed the blob row of an owned raw message: %v", err)
	}

	// Once nothing points at the path, the same call is what stops a failed
	// send from leaving the file behind for good.
	if err := db.DeleteMessageForUser(ctx, user.ID, owner.ID); err != nil {
		t.Fatal(err)
	}
	if err := server.deleteUnreferencedRawMessage(ctx, user.ID, saved.Path); err != nil {
		t.Fatal(err)
	}
	if _, err := blobStore.OpenUserBlob(user.ID, saved.Path); err == nil {
		t.Fatal("cleanup left an unreferenced raw message on disk")
	}
	if _, err := db.GetBlobByPathForUser(ctx, user.ID, saved.Path); !store.IsNotFound(err) {
		t.Fatalf("blob row err = %v, want not found", err)
	}
}

func createComposeScopeMessage(t *testing.T, ctx context.Context, db *store.Store, userID, accountID, mailboxID int64, blobPath string, uid uint32, uidValidity int64) store.MessageRecord {
	t.Helper()
	blobRec, err := db.CreateBlob(ctx, store.BlobRecord{
		UserID: userID, Kind: "message", Path: blobPath, SHA256: blobPath, Size: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	message, err := db.CreateMessage(ctx, store.CreateMessage{
		UserID: userID, AccountID: accountID, MailboxID: mailboxID, BlobID: blobRec.ID,
		MessageIDHeader: "<scope-" + blobPath + "@example.test>", Subject: "Scope",
		FromAddr: "sender@example.test", ToAddr: "me@example.test",
		Date: time.Now().UTC(), InternalDate: time.Now().UTC(), UID: uid, UIDValidity: uidValidity, Size: 1, BlobPath: blobPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	return message
}
