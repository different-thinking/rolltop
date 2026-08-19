// File overview: Search batch consistency when messages move during indexing.

package syncer

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"rolltop/backend/search"
	"rolltop/backend/store"
	"rolltop/backend/store/storetest"
)

func TestFetchedSearchBatchRemovesMessageDeletedBeforeMetadataCommit(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db, err := storetest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	searchService, err := search.Open(filepath.Join(dir, "bleve"))
	if err != nil {
		t.Fatal(err)
	}
	defer searchService.Close()

	user, err := db.CreateUser(ctx, "batch-move@example.test", "Batch Move", "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	account, err := db.CreateMailAccount(ctx, store.MailAccount{
		UserID: user.ID, Email: user.Email, Host: "imap.example.test", Port: 993,
		Username: "batch-move", EncryptedPassword: "encrypted", UseTLS: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	mailbox, err := db.GetOrCreateMailbox(ctx, user.ID, account.ID, "Spam")
	if err != nil {
		t.Fatal(err)
	}
	blob, err := db.CreateBlob(ctx, store.BlobRecord{
		UserID: user.ID, Kind: "message-remote", Path: "remote/batch-move.eml", SHA256: "batch-move", Size: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	message, err := db.CreateMessage(ctx, store.CreateMessage{
		UserID: user.ID, AccountID: account.ID, MailboxID: mailbox.ID, BlobID: blob.ID,
		Subject: "Moving while indexing", FromAddr: "sender@example.test", UID: 10,
		Date: time.Now().UTC(), InternalDate: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}

	service := &Service{Store: db, Search: searchService}
	batch := newFetchedSearchIndexBatch(service)
	if err := batch.Add(ctx, &pendingFetchedSearchIndex{
		Document: search.MessageIndexDocument{Message: message},
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.DeleteMessageForUser(ctx, user.ID, message.ID); err != nil {
		t.Fatal(err)
	}
	if err := batch.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	indexed, err := searchService.MessageIDsIndexed(ctx, user.ID, []int64{message.ID})
	if err != nil {
		t.Fatal(err)
	}
	if indexed[message.ID] {
		t.Fatal("deleted message was resurrected in the search index")
	}
}

// A document count alone does not bound what a batch holds. Sync used to carry
// twenty-five whole message bodies and attachment extractions between commits,
// so one folder full of large mail decided how much memory the process needed.
func TestFetchedSearchBatchFlushesOnRetainedPayloadBudget(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db, err := storetest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	searchService, err := search.Open(filepath.Join(dir, "bleve"))
	if err != nil {
		t.Fatal(err)
	}
	defer searchService.Close()

	user, err := db.CreateUser(ctx, "batch-bytes@example.test", "Batch Bytes", "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	account, err := db.CreateMailAccount(ctx, store.MailAccount{
		UserID: user.ID, Email: user.Email, Host: "imap.example.test", Port: 993,
		Username: "batch-bytes", EncryptedPassword: "encrypted", UseTLS: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	mailbox, err := db.GetOrCreateMailbox(ctx, user.ID, account.ID, "INBOX")
	if err != nil {
		t.Fatal(err)
	}
	service := &Service{Store: db, Search: searchService}
	batch := newFetchedSearchIndexBatch(service)

	// Each message keeps the maximum indexable body and attachment text, so the
	// payload budget is spent long before the twenty-fifth sibling arrives.
	body := strings.Repeat("attachment sized body text ", 400*1024)
	messageIDs := make([]int64, 0, fetchedSearchIndexBatchSize)
	flushedAfter := 0
	for i := range fetchedSearchIndexBatchSize {
		blob, err := db.CreateBlob(ctx, store.BlobRecord{
			UserID: user.ID, Kind: "message-remote",
			Path: fmt.Sprintf("remote/batch-bytes-%d.eml", i), SHA256: fmt.Sprintf("batch-bytes-%d", i), Size: 1,
		})
		if err != nil {
			t.Fatal(err)
		}
		message, err := db.CreateMessage(ctx, store.CreateMessage{
			UserID: user.ID, AccountID: account.ID, MailboxID: mailbox.ID, BlobID: blob.ID,
			Subject: fmt.Sprintf("Large message %d", i), FromAddr: "sender@example.test", UID: uint32(20 + i),
			Date: time.Now().UTC(), InternalDate: time.Now().UTC(),
		})
		if err != nil {
			t.Fatal(err)
		}
		message.BodyText = body
		if err := batch.Add(ctx, &pendingFetchedSearchIndex{
			Document: search.MessageIndexDocument{
				Message: message,
				Attachments: []search.AttachmentDoc{
					{Filename: "report.pdf", ContentType: "application/pdf", Text: body},
				},
			},
		}); err != nil {
			t.Fatal(err)
		}
		messageIDs = append(messageIDs, message.ID)
		if batch.bytes > fetchedSearchIndexBatchBytes {
			t.Fatalf("batch holds %d bytes after message %d, want at most %d",
				batch.bytes, i, fetchedSearchIndexBatchBytes)
		}
		if batch.Empty() {
			flushedAfter = i + 1
			break
		}
	}
	if flushedAfter == 0 || flushedAfter >= fetchedSearchIndexBatchSize {
		t.Fatalf("batch committed after %d large messages, want the payload budget to commit first", flushedAfter)
	}
	indexed, err := searchService.MessageIDsIndexed(ctx, user.ID, messageIDs)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range messageIDs {
		if !indexed[id] {
			t.Fatalf("message %d was not committed when the payload budget was spent", id)
		}
	}
}

// Ordinary mail must still ride the twenty-five document cadence: flushing per
// message would pay a Bleve commit for every mail in a first sync.
func TestFetchedSearchBatchKeepsCountCadenceForOrdinaryMail(t *testing.T) {
	ctx := context.Background()
	db, err := storetest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	user, err := db.CreateUser(ctx, "batch-count@example.test", "Batch Count", "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	batch := newFetchedSearchIndexBatch(&Service{Store: db})
	for i := range fetchedSearchIndexBatchSize - 1 {
		if err := batch.Add(ctx, &pendingFetchedSearchIndex{
			Document: search.MessageIndexDocument{Message: store.MessageRecord{
				UserID: user.ID, ID: int64(i + 1), Subject: "Rechnung", BodyText: strings.Repeat("kurz ", 512),
			}},
		}); err != nil {
			t.Fatal(err)
		}
	}
	if batch.Empty() {
		t.Fatal("ordinary mail flushed before the batch was full")
	}
	if got := len(batch.items); got != fetchedSearchIndexBatchSize-1 {
		t.Fatalf("batch holds %d documents, want %d", got, fetchedSearchIndexBatchSize-1)
	}
}
