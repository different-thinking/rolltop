// File overview: Saving over an existing draft must not pile up duplicate
// copies in Drafts -- the replacement is appended and stored first, then the
// draft it replaces is retired.

package web

import (
	"context"
	"testing"

	"rolltop/backend/blob"
	"rolltop/backend/store"
	"rolltop/backend/store/storetest"
	"rolltop/backend/syncer"
)

// draftMoveFetcher answers a move with the COPYUID metadata a real server
// returns, which is what lets supersedeDraft's move prove the source
// generation rather than refusing for want of a receipt.
type draftMoveFetcher struct {
	*captureAppendFetcher
}

func (f *draftMoveFetcher) MoveMessageWithReceipt(_ context.Context, _ store.MailAccount, _ string, _ string, uid uint32, expectedSourceUIDValidity uint32) (*syncer.MoveReceipt, error) {
	return &syncer.MoveReceipt{DestinationUIDValidity: expectedSourceUIDValidity, DestinationUID: uid}, nil
}

func TestSaveComposeDraftSupersedesThePreviousDraft(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db, err := storetest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	user, err := db.CreateUser(ctx, "me@example.test", "Me", "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	account, err := db.UpsertMailAccount(ctx, store.MailAccount{
		UserID:            user.ID,
		Email:             "me@example.test",
		Host:              "imap.example.test",
		Port:              993,
		Username:          "me@example.test",
		EncryptedPassword: "encrypted",
		UseTLS:            true,
		Mailbox:           "INBOX",
	})
	if err != nil {
		t.Fatal(err)
	}
	drafts, err := db.GetOrCreateMailboxWithRole(ctx, user.ID, account.ID, "Drafts", "drafts")
	if err != nil {
		t.Fatal(err)
	}
	trash, err := db.GetOrCreateMailboxWithRole(ctx, user.ID, account.ID, "Trash", "trash")
	if err != nil {
		t.Fatal(err)
	}
	const draftsUIDValidity = 333
	if err := db.UpdateMailboxRemoteStatus(ctx, user.ID, drafts.ID, 0, 0, 1, draftsUIDValidity); err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateMailboxRemoteStatus(ctx, user.ID, trash.ID, 0, 0, 1, 999); err != nil {
		t.Fatal(err)
	}
	contact, err := db.CreateContact(ctx, user.ID, store.Contact{
		DisplayName: "Me",
		IsMe:        true,
		IsPrimary:   true,
		Emails:      []store.ContactEmail{{Email: "me@example.test", IsPrimary: true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.EnsureMailIdentityForEmail(ctx, user.ID, "me@example.test"); err != nil {
		t.Fatal(err)
	}
	blobStore := blob.New(dir)
	fetcher := &draftMoveFetcher{captureAppendFetcher: &captureAppendFetcher{nextUID: 1, uidValidity: draftsUIDValidity}}
	service := &syncer.Service{Store: db, Blobs: blobStore, Fetcher: fetcher}
	server := &Server{store: db, blobs: blobStore, syncer: service, syncRunner: syncer.NewRunner(nil)}

	first, err := server.saveComposeDraft(ctx, currentUser{User: user}, composeForm{
		To: "recipient@example.test", Subject: "Unfinished", Body: "v1", FromIdentityID: contact.Emails[0].ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if first.MailboxID != drafts.ID {
		t.Fatalf("first draft mailbox_id = %d, want Drafts (%d)", first.MailboxID, drafts.ID)
	}

	second, err := server.saveComposeDraft(ctx, currentUser{User: user}, composeForm{
		To: "recipient@example.test", Subject: "Unfinished", Body: "v2", FromIdentityID: contact.Emails[0].ID,
		DraftMessageID: first.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if second.ID == first.ID {
		t.Fatal("expected a new local row for the replacement draft")
	}
	if second.MailboxID != drafts.ID {
		t.Fatalf("replacement draft mailbox_id = %d, want Drafts (%d)", second.MailboxID, drafts.ID)
	}

	// A completed move drops the local row for the same reason every other move
	// does: the mirror follows the server on the destination folder's next sync
	// rather than keeping a stale row updated in place. "Gone from Drafts" is
	// what a superseded draft looks like here, not "now carries the Trash id".
	if _, err := db.GetMessageForUser(ctx, user.ID, first.ID); !store.IsNotFound(err) {
		t.Fatalf("old draft lookup err = %v, want not found: saving over a draft must not leave the previous copy behind", err)
	}

	drafts, err = db.GetMailboxForUser(ctx, user.ID, drafts.ID)
	if err != nil {
		t.Fatal(err)
	}
	remaining, err := db.ListMailboxScopeMessagesForUser(ctx, user.ID, drafts.ID, store.ScopeFilter{}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 1 || remaining[0].ID != second.ID {
		t.Fatalf("Drafts contents = %+v, want only the replacement draft %d", remaining, second.ID)
	}
}

// draft_message_id arrives from the client and everything it reaches ends in
// Trash, so it has to name a draft. Loading one into the composer already
// refuses anything else; retiring one refused nothing, and a stale or wrong id
// filed whichever message it named.
func TestSaveComposeDraftRefusesToRetireAMessageOutsideDrafts(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db, err := storetest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	user, err := db.CreateUser(ctx, "me@example.test", "Me", "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	account, err := db.UpsertMailAccount(ctx, store.MailAccount{
		UserID: user.ID, Email: "me@example.test", Host: "imap.example.test", Port: 993,
		Username: "me@example.test", EncryptedPassword: "encrypted", UseTLS: true, Mailbox: "INBOX",
	})
	if err != nil {
		t.Fatal(err)
	}
	drafts, err := db.GetOrCreateMailboxWithRole(ctx, user.ID, account.ID, "Drafts", "drafts")
	if err != nil {
		t.Fatal(err)
	}
	trash, err := db.GetOrCreateMailboxWithRole(ctx, user.ID, account.ID, "Trash", "trash")
	if err != nil {
		t.Fatal(err)
	}
	inbox, err := db.GetOrCreateMailbox(ctx, user.ID, account.ID, "INBOX")
	if err != nil {
		t.Fatal(err)
	}
	const draftsUIDValidity = 333
	if err := db.UpdateMailboxRemoteStatus(ctx, user.ID, drafts.ID, 0, 0, 1, draftsUIDValidity); err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateMailboxRemoteStatus(ctx, user.ID, trash.ID, 0, 0, 1, 999); err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateMailboxRemoteStatus(ctx, user.ID, inbox.ID, 0, 0, 1, 444); err != nil {
		t.Fatal(err)
	}
	contact, err := db.CreateContact(ctx, user.ID, store.Contact{
		DisplayName: "Me", IsMe: true, IsPrimary: true,
		Emails: []store.ContactEmail{{Email: "me@example.test", IsPrimary: true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.EnsureMailIdentityForEmail(ctx, user.ID, "me@example.test"); err != nil {
		t.Fatal(err)
	}

	// An ordinary received message, which is what a wrong id most likely names.
	blobStore := blob.New(dir)
	received, err := db.CreateBlob(ctx, store.BlobRecord{
		UserID: user.ID, Kind: "message", Path: "users/1/inbox/1.eml", SHA256: "aa", Size: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	keep, err := db.CreateMessage(ctx, store.CreateMessage{
		UserID: user.ID, AccountID: account.ID, MailboxID: inbox.ID, BlobID: received.ID,
		MessageIDHeader: "<keep@example.test>", CanonicalSHA256: "aa", MessageIDHash: "keep",
		ThreadKey: "keep", Subject: "Contract", BodyText: "please keep me",
		FromAddr: "sender@example.test", ToAddr: "me@example.test",
		UID: 7, UIDValidity: 444, Size: 1, BlobPath: "users/1/inbox/1.eml",
	})
	if err != nil {
		t.Fatal(err)
	}

	fetcher := &draftMoveFetcher{captureAppendFetcher: &captureAppendFetcher{nextUID: 1, uidValidity: draftsUIDValidity}}
	service := &syncer.Service{Store: db, Blobs: blobStore, Fetcher: fetcher}
	server := &Server{store: db, blobs: blobStore, syncer: service, syncRunner: syncer.NewRunner(nil)}

	saved, err := server.saveComposeDraft(ctx, currentUser{User: user}, composeForm{
		To: "recipient@example.test", Subject: "Unfinished", Body: "v1",
		FromIdentityID: contact.Emails[0].ID, DraftMessageID: keep.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if saved.MailboxID != drafts.ID {
		t.Fatalf("draft mailbox_id = %d, want Drafts (%d): the refusal must not cost the save", saved.MailboxID, drafts.ID)
	}
	still, err := db.GetMessageForUser(ctx, user.ID, keep.ID)
	if err != nil {
		t.Fatalf("received message lookup err = %v, want it untouched in the Inbox", err)
	}
	if still.MailboxID != inbox.ID {
		t.Fatalf("received message mailbox_id = %d, want it left in the Inbox (%d)", still.MailboxID, inbox.ID)
	}
}
