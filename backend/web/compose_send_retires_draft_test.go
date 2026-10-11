// File overview: Sending a message composed from a saved draft retires that
// draft, so it does not stay in Drafts looking like mail still waiting to go.

package web

import (
	"context"
	"testing"

	"rolltop/backend/blob"
	"rolltop/backend/store"
	"rolltop/backend/store/storetest"
	"rolltop/backend/syncer"
)

func TestSendComposeRetiresTheDraftItWasComposedFrom(t *testing.T) {
	ctx := context.Background()
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
	if _, err := db.CreateSMTPAccount(ctx, store.SMTPAccount{UserID: user.ID, Label: "SMTP", Host: "smtp.example.test", Port: 587, Username: "me@example.test", EncryptedPassword: "encrypted", UseTLS: true}); err != nil {
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
	sent, err := db.GetOrCreateMailboxWithRole(ctx, user.ID, account.ID, "Sent", "sent")
	if err != nil {
		t.Fatal(err)
	}
	const uidValidity = 333
	for _, mailbox := range []store.Mailbox{drafts, trash, sent} {
		if err := db.UpdateMailboxRemoteStatus(ctx, user.ID, mailbox.ID, 0, 0, 1, uidValidity); err != nil {
			t.Fatal(err)
		}
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
	blobStore := blob.New(t.TempDir())
	fetcher := &draftMoveFetcher{captureAppendFetcher: &captureAppendFetcher{nextUID: 1, uidValidity: uidValidity}}
	service := &syncer.Service{Store: db, Blobs: blobStore, Fetcher: fetcher}
	sender := &captureSender{}
	server := &Server{store: db, blobs: blobStore, sender: sender, syncer: service, syncRunner: syncer.NewRunner(nil)}

	draft, err := server.saveComposeDraft(ctx, currentUser{User: user}, composeForm{
		To: "recipient@example.test", Subject: "Almost done", Body: "draft", FromIdentityID: contact.Emails[0].ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if draft.MailboxID != drafts.ID {
		t.Fatalf("draft mailbox_id = %d, want Drafts (%d)", draft.MailboxID, drafts.ID)
	}

	sentMsg, err := server.sendCompose(ctx, currentUser{User: user}, composeForm{
		To: "recipient@example.test", Subject: "Almost done", Body: "final", FromIdentityID: contact.Emails[0].ID,
		DraftMessageID: draft.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if sender.count != 1 {
		t.Fatalf("SMTP send count = %d, want 1", sender.count)
	}
	if sentMsg.MailboxID != sent.ID {
		t.Fatalf("sent copy mailbox_id = %d, want Sent (%d)", sentMsg.MailboxID, sent.ID)
	}
	if _, err := db.GetMessageForUser(ctx, user.ID, draft.ID); !store.IsNotFound(err) {
		t.Fatalf("draft lookup err = %v, want not found: a sent draft must not stay in Drafts", err)
	}
	remaining, err := db.ListMailboxScopeMessagesForUser(ctx, user.ID, drafts.ID, store.ScopeFilter{}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 0 {
		t.Fatalf("Drafts still holds %d messages after the send, want none", len(remaining))
	}
}
