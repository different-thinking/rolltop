// File overview: Attachment row replacement keeps /attachments/<id> URLs valid.

package store

import (
	"context"
	"testing"
	"time"
)

// TestReplaceAttachmentsForMessageKeepsIDsStable is the regression for the
// preview and download 404s: reindexing reparses the same message, and the old
// delete-then-insert handed every attachment a new ID, so an open mail view was
// left holding URLs the server no longer knew.
func TestReplaceAttachmentsForMessageKeepsIDsStable(t *testing.T) {
	ctx := context.Background()
	db := mustOpenTestStore(t)
	defer db.Close()

	user, err := db.CreateUser(ctx, "attachment-replace@example.test", "Owner", "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	message := createIndexedMessageForResetTest(t, ctx, db, user, "INBOX", true, 1)
	files := []Attachment{
		{UserID: user.ID, MessageID: message.ID, BlobID: message.BlobID, Filename: "invoice.pdf", ContentType: "application/pdf", Size: 120},
		{UserID: user.ID, MessageID: message.ID, BlobID: message.BlobID, Filename: "logo.png", ContentType: "image/png", ContentID: "logo@example.test", IsInline: true, Size: 30},
	}
	first, err := db.ReplaceAttachmentsForMessage(ctx, user.ID, message.ID, files)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 2 {
		t.Fatalf("attachments = %d, want 2", len(first))
	}

	second, err := db.ReplaceAttachmentsForMessage(ctx, user.ID, message.ID, files)
	if err != nil {
		t.Fatal(err)
	}
	if len(second) != len(first) {
		t.Fatalf("reindexed attachments = %d, want %d", len(second), len(first))
	}
	for i := range first {
		if second[i].ID != first[i].ID {
			t.Fatalf("attachment %d changed ID %d -> %d; preview and download URLs go stale", i, first[i].ID, second[i].ID)
		}
		if second[i].Filename != first[i].Filename || second[i].IsInline != first[i].IsInline {
			t.Fatalf("attachment %d changed identity: %+v -> %+v", i, first[i], second[i])
		}
	}
	for _, att := range second {
		fetched, err := db.GetAttachmentForUser(ctx, user.ID, att.ID)
		if err != nil {
			t.Fatalf("attachment %d is no longer readable: %v", att.ID, err)
		}
		if fetched.MessageID != message.ID {
			t.Fatalf("attachment %d message = %d, want %d", att.ID, fetched.MessageID, message.ID)
		}
	}
}

// TestReplaceAttachmentsForMessageUpdatesMetadataInPlace covers a reparse that
// really did change: rows keep their IDs but carry the new metadata.
func TestReplaceAttachmentsForMessageUpdatesMetadataInPlace(t *testing.T) {
	ctx := context.Background()
	db := mustOpenTestStore(t)
	defer db.Close()

	user, err := db.CreateUser(ctx, "attachment-replace-update@example.test", "Owner", "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	message := createIndexedMessageForResetTest(t, ctx, db, user, "INBOX", true, 1)
	before, err := db.ReplaceAttachmentsForMessage(ctx, user.ID, message.ID, []Attachment{
		{UserID: user.ID, MessageID: message.ID, BlobID: message.BlobID, Filename: "old.bin", ContentType: "application/octet-stream", Size: 10},
	})
	if err != nil {
		t.Fatal(err)
	}
	after, err := db.ReplaceAttachmentsForMessage(ctx, user.ID, message.ID, []Attachment{
		{UserID: user.ID, MessageID: message.ID, BlobID: message.BlobID, Filename: "invoice.pdf", ContentType: "application/pdf", Size: 4096},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 1 || after[0].ID != before[0].ID {
		t.Fatalf("row was replaced instead of updated: %+v -> %+v", before, after)
	}
	if after[0].Filename != "invoice.pdf" || after[0].ContentType != "application/pdf" || after[0].Size != 4096 {
		t.Fatalf("metadata not updated: %+v", after[0])
	}
}

// TestReplaceAttachmentsForMessageAddsAndRemovesRows checks the two cases that
// still change the row set, so a message whose parts really did change is not
// left with stale rows.
func TestReplaceAttachmentsForMessageAddsAndRemovesRows(t *testing.T) {
	ctx := context.Background()
	db := mustOpenTestStore(t)
	defer db.Close()

	user, err := db.CreateUser(ctx, "attachment-replace-count@example.test", "Owner", "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	message := createIndexedMessageForResetTest(t, ctx, db, user, "INBOX", true, 1)
	row := func(name string) Attachment {
		return Attachment{UserID: user.ID, MessageID: message.ID, BlobID: message.BlobID, Filename: name, ContentType: "application/pdf", Size: 1}
	}
	one, err := db.ReplaceAttachmentsForMessage(ctx, user.ID, message.ID, []Attachment{row("a.pdf")})
	if err != nil {
		t.Fatal(err)
	}
	grown, err := db.ReplaceAttachmentsForMessage(ctx, user.ID, message.ID, []Attachment{row("a.pdf"), row("b.pdf")})
	if err != nil {
		t.Fatal(err)
	}
	if len(grown) != 2 {
		t.Fatalf("attachments after growth = %d, want 2", len(grown))
	}
	if grown[0].ID != one[0].ID {
		t.Fatalf("existing row changed ID %d -> %d", one[0].ID, grown[0].ID)
	}
	shrunk, err := db.ReplaceAttachmentsForMessage(ctx, user.ID, message.ID, []Attachment{row("a.pdf")})
	if err != nil {
		t.Fatal(err)
	}
	if len(shrunk) != 1 || shrunk[0].ID != one[0].ID {
		t.Fatalf("attachments after shrink = %+v, want the original row only", shrunk)
	}
	if _, err := db.GetAttachmentForUser(ctx, user.ID, grown[1].ID); !IsNotFound(err) {
		t.Fatalf("surplus row error = %v, want not found", err)
	}
	if _, err := db.ReplaceAttachmentsForMessage(ctx, user.ID, message.ID, nil); err != nil {
		t.Fatal(err)
	}
	remaining, err := db.ListAttachmentsForMessage(ctx, user.ID, message.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 0 {
		t.Fatalf("attachments after empty replace = %d, want 0", len(remaining))
	}
}

// TestReplaceAttachmentsForMessageIsTenantScoped keeps the isolation rule: one
// tenant's replace never reaches another tenant's rows.
func TestReplaceAttachmentsForMessageIsTenantScoped(t *testing.T) {
	ctx := context.Background()
	db := mustOpenTestStore(t)
	defer db.Close()

	owner, err := db.CreateUser(ctx, "attachment-replace-owner@example.test", "Owner", "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	other, err := db.CreateUser(ctx, "attachment-replace-other@example.test", "Other", "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	ownerMessage := createIndexedMessageForResetTest(t, ctx, db, owner, "INBOX", true, 1)
	ownerRows, err := db.ReplaceAttachmentsForMessage(ctx, owner.ID, ownerMessage.ID, []Attachment{
		{UserID: owner.ID, MessageID: ownerMessage.ID, BlobID: ownerMessage.BlobID, Filename: "owned.pdf", ContentType: "application/pdf", Size: 7},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ReplaceAttachmentsForMessage(ctx, other.ID, ownerMessage.ID, nil); err != nil {
		t.Fatal(err)
	}
	kept, err := db.GetAttachmentForUser(ctx, owner.ID, ownerRows[0].ID)
	if err != nil {
		t.Fatalf("another tenant's replace removed the owner's attachment: %v", err)
	}
	if kept.Filename != "owned.pdf" {
		t.Fatalf("owner attachment = %+v, want owned.pdf", kept)
	}
	if !kept.CreatedAt.After(time.Time{}) {
		t.Fatalf("owner attachment lost its created_at: %+v", kept)
	}
}
