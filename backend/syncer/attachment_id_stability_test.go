// File overview: Reindexing a message must not renumber its attachments.

package syncer

import (
	"context"
	"encoding/base64"
	"fmt"
	"path/filepath"
	"testing"

	"rolltop/backend/blob"
	"rolltop/backend/search"
	"rolltop/backend/store"
)

// TestIndexAttachmentsForMessageKeepsAttachmentIDs is the regression for the
// preview and download 404s. The attachment index worker reparses the stored
// message, and rewriting its rows used to hand every attachment a new ID while
// the open mail view still linked to the old one - so /attachments/<id>/download
// and the attachment_preview route both answered 404 for mail that had not
// changed at all.
func TestIndexAttachmentsForMessageKeepsAttachmentIDs(t *testing.T) {
	fixture := newMoveTestFixture(t)
	ctx := context.Background()
	dir := t.TempDir()
	searchService, err := search.Open(filepath.Join(dir, "bleve"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = searchService.Close() })
	blobStore := blob.New(dir)
	fixture.service.Search = searchService
	fixture.service.Blobs = blobStore

	message := createPendingAttachmentIndexMessage(t, ctx, fixture, fixture.message.UID+7)
	message = storeRawForMessage(t, ctx, fixture, blobStore, message, rawMessageWithAttachments(message.UID))

	if err := fixture.service.IndexAttachmentsForMessage(ctx, message); err != nil {
		t.Fatal(err)
	}
	before, err := fixture.store.ListAttachmentsForMessage(ctx, fixture.userID, message.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != 2 {
		t.Fatalf("attachments = %d, want 2", len(before))
	}

	// The queue reindexes the same message more than once - stall recovery and a
	// re-enabled folder both put a row back on it - so the second pass is the
	// one an already-rendered mail view has to survive.
	if err := fixture.store.MarkMessageAttachmentIndexPending(ctx, fixture.userID, message.ID); err != nil {
		t.Fatal(err)
	}
	reloaded, err := fixture.store.GetMessageForUser(ctx, fixture.userID, message.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.service.IndexAttachmentsForMessage(ctx, reloaded); err != nil {
		t.Fatal(err)
	}
	after, err := fixture.store.ListAttachmentsForMessage(ctx, fixture.userID, message.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("attachments after reindex = %d, want %d", len(after), len(before))
	}
	for i := range before {
		if after[i].ID != before[i].ID {
			t.Fatalf("attachment %q changed ID %d -> %d; its preview and download URLs 404", before[i].Filename, before[i].ID, after[i].ID)
		}
		if after[i].Filename != before[i].Filename {
			t.Fatalf("attachment %d filename %q -> %q", before[i].ID, before[i].Filename, after[i].Filename)
		}
	}
	for _, att := range before {
		if _, err := fixture.store.GetAttachmentForUser(ctx, fixture.userID, att.ID); err != nil {
			t.Fatalf("attachment %d is no longer served after reindex: %v", att.ID, err)
		}
	}
}

// TestIndexAttachmentsForMessageDropsRowsAParseNoLongerFinds is the other half
// of the contract: rows follow the parse down as well as across. A message
// whose attachments a reparse no longer yields -- a security plugin dropping
// them for mail stored before it was enabled -- must lose its rows, or the
// parts this pass decided not to store stay downloadable by ID.
func TestIndexAttachmentsForMessageDropsRowsAParseNoLongerFinds(t *testing.T) {
	fixture := newMoveTestFixture(t)
	ctx := context.Background()
	dir := t.TempDir()
	searchService, err := search.Open(filepath.Join(dir, "bleve"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = searchService.Close() })
	blobStore := blob.New(dir)
	fixture.service.Search = searchService
	fixture.service.Blobs = blobStore

	message := createPendingAttachmentIndexMessage(t, ctx, fixture, fixture.message.UID+8)
	message = storeRawForMessage(t, ctx, fixture, blobStore, message, rawMessageWithAttachments(message.UID))
	if err := fixture.service.IndexAttachmentsForMessage(ctx, message); err != nil {
		t.Fatal(err)
	}
	stored, err := fixture.store.ListAttachmentsForMessage(ctx, fixture.userID, message.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 2 {
		t.Fatalf("attachments = %d, want 2", len(stored))
	}

	plain := []byte(fmt.Sprintf("From: sender@example.test\r\nTo: receiver@example.test\r\n"+
		"Subject: Attachment UID %d\r\nMessage-ID: <attachment-%d@example.test>\r\n\r\nno attachments any more\r\n",
		message.UID, message.UID))
	message = storeRawForMessage(t, ctx, fixture, blobStore, message, plain)
	if err := fixture.store.MarkMessageAttachmentIndexPending(ctx, fixture.userID, message.ID); err != nil {
		t.Fatal(err)
	}
	reloaded, err := fixture.store.GetMessageForUser(ctx, fixture.userID, message.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.service.IndexAttachmentsForMessage(ctx, reloaded); err != nil {
		t.Fatal(err)
	}
	remaining, err := fixture.store.ListAttachmentsForMessage(ctx, fixture.userID, message.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 0 {
		t.Fatalf("attachments after the parse stopped finding them = %+v, want none", remaining)
	}
	for _, att := range stored {
		if _, err := fixture.store.GetAttachmentForUser(ctx, fixture.userID, att.ID); !store.IsNotFound(err) {
			t.Fatalf("attachment %d is still served by ID: err = %v", att.ID, err)
		}
	}
}

func storeRawForMessage(t *testing.T, ctx context.Context, fixture moveTestFixture, blobStore *blob.Store, message store.MessageRecord, raw []byte) store.MessageRecord {
	t.Helper()
	saved, err := blobStore.SaveRawMessage(fixture.userID, fixture.account.ID, fixture.source.Name, message.UID, raw)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := fixture.store.RetainMessageBlob(ctx, fixture.userID, message.ID, store.BlobRecord{
		Path: saved.Path, SHA256: saved.SHA256, Size: saved.Size,
	})
	if err != nil {
		t.Fatal(err)
	}
	return stored
}

func rawMessageWithAttachments(uid uint32) []byte {
	pdf := base64.StdEncoding.EncodeToString([]byte("%PDF-1.4\nattachment body\n%%EOF\n"))
	png := base64.StdEncoding.EncodeToString([]byte("\x89PNG\r\n\x1a\ninline body"))
	return []byte(fmt.Sprintf("From: sender@example.test\r\n"+
		"To: receiver@example.test\r\n"+
		"Subject: Attachment UID %d\r\n"+
		"Message-ID: <attachment-%d@example.test>\r\n"+
		"MIME-Version: 1.0\r\n"+
		"Content-Type: multipart/mixed; boundary=\"rolltop\"\r\n"+
		"\r\n"+
		"--rolltop\r\n"+
		"Content-Type: text/plain; charset=utf-8\r\n"+
		"\r\n"+
		"body with an attachment\r\n"+
		"--rolltop\r\n"+
		"Content-Type: application/pdf; name=\"invoice.pdf\"\r\n"+
		"Content-Disposition: attachment; filename=\"invoice.pdf\"\r\n"+
		"Content-Transfer-Encoding: base64\r\n"+
		"\r\n%s\r\n"+
		"--rolltop\r\n"+
		"Content-Type: image/png; name=\"logo.png\"\r\n"+
		"Content-Disposition: inline; filename=\"logo.png\"\r\n"+
		"Content-ID: <logo@example.test>\r\n"+
		"Content-Transfer-Encoding: base64\r\n"+
		"\r\n%s\r\n"+
		"--rolltop--\r\n", uid, uid, pdf, png))
}
