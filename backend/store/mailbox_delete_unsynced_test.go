package store

import (
	"context"
	"testing"
	"time"
)

// The only rows this may remove are the ones no server ever answered for. A
// folder that holds mail, or whose status has been read, is a real folder --
// one bad answer from a server must not be able to take it away.
func TestDeleteUnsyncedMailboxOnlyRemovesRowsNoServerAnsweredFor(t *testing.T) {
	ctx := context.Background()
	db, err := openTestStore(t)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	user, account, inbox, blob := testMailbox(t, ctx, db)
	if _, err := db.CreateMessage(ctx, CreateMessage{
		UserID: user.ID, AccountID: account.ID, MailboxID: inbox.ID, BlobID: blob.ID,
		MessageIDHeader: "<held@example.test>", Date: time.Now().UTC(), InternalDate: time.Now().UTC(),
		UID: 1, Size: blob.Size, BlobPath: blob.Path,
	}); err != nil {
		t.Fatal(err)
	}
	deleted, err := db.DeleteUnsyncedMailbox(ctx, user.ID, account.ID, inbox.Name)
	if err != nil {
		t.Fatal(err)
	}
	if deleted {
		t.Fatal("a folder holding mail was deleted")
	}

	answered, err := db.GetOrCreateMailbox(ctx, user.ID, account.ID, "Archiv")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateMailboxRemoteStatus(ctx, user.ID, answered.ID, 0, 0, 1, 9); err != nil {
		t.Fatal(err)
	}
	deleted, err = db.DeleteUnsyncedMailbox(ctx, user.ID, account.ID, answered.Name)
	if err != nil {
		t.Fatal(err)
	}
	if deleted {
		t.Fatal("an empty folder the server had answered for was deleted")
	}
}

// A folder recorded from a name alone can still be picked as an identity's Sent
// default -- "[Gmail]/Sent Mail" carries the Sent role by name. Removing the row
// without the pointer would leave compose filing into a folder that is gone.
func TestDeleteUnsyncedMailboxClearsIdentityPointers(t *testing.T) {
	ctx := context.Background()
	db, err := openTestStore(t)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	user, account, _, _ := testMailbox(t, ctx, db)
	phantom, err := db.GetOrCreateMailbox(ctx, user.ID, account.ID, "[Gmail]/Sent Mail")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.EnsureMeContactForEmail(ctx, user.ID, "mail@example.test", "Mail"); err != nil {
		t.Fatal(err)
	}
	if err := db.EnsureMailIdentityForEmail(ctx, user.ID, "mail@example.test"); err != nil {
		t.Fatal(err)
	}
	identities, err := db.ListMailIdentitiesForUser(ctx, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(identities) == 0 {
		t.Fatal("no identity to point at the folder")
	}
	identity := identities[0]
	identity.IMAPAccountID = account.ID
	identity.SentMailboxID = phantom.ID
	if _, err := db.UpdateMailIdentityForUser(ctx, user.ID, identity); err != nil {
		t.Fatal(err)
	}

	deleted, err := db.DeleteUnsyncedMailbox(ctx, user.ID, account.ID, phantom.Name)
	if err != nil {
		t.Fatal(err)
	}
	if !deleted {
		t.Fatal("a folder no server answered for was kept")
	}
	if _, err := db.GetMailbox(ctx, user.ID, account.ID, phantom.Name); !IsNotFound(err) {
		t.Fatalf("mailbox lookup = %v, want not found", err)
	}
	updated, err := db.GetMailIdentityForUser(ctx, user.ID, identity.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.SentMailboxID != 0 {
		t.Fatalf("identity sent mailbox = %d, want the pointer cleared", updated.SentMailboxID)
	}
}
