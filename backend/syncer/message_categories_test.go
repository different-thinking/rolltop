package syncer

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"rolltop/backend/blob"
	"rolltop/backend/mailparse"
	"rolltop/backend/store"
)

// TestCategoryBackfillReadsStoredMailAndAlwaysDrains covers the property the
// whole worker depends on: a candidate is always filed, even when its raw
// message is missing or unreadable. A row that stayed pending would be picked up
// on every pass and the backfill would never finish.
func TestCategoryBackfillReadsStoredMailAndAlwaysDrains(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	db, err := store.Open(filepath.Join(root, "rolltop.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	user, err := db.CreateUser(ctx, "categories@example.test", "Categories", "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	account, err := db.UpsertMailAccount(ctx, store.MailAccount{
		UserID: user.ID, Email: user.Email, Host: "imap.example.test", Port: 993,
		Username: user.Email, EncryptedPassword: "encrypted", UseTLS: true, Mailbox: "INBOX",
	})
	if err != nil {
		t.Fatal(err)
	}
	mailbox, err := db.GetOrCreateMailbox(ctx, user.ID, account.ID, "INBOX")
	if err != nil {
		t.Fatal(err)
	}

	store2 := blob.New(root)
	writeMessage := func(name, raw string) string {
		t.Helper()
		if raw == "" {
			return ""
		}
		rel := filepath.Join("users", fmt.Sprintf("%d", user.ID), "blobs", name)
		abs := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(abs), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte(raw), 0o600); err != nil {
			t.Fatal(err)
		}
		return rel
	}
	create := func(uid uint32, from, name, raw string) store.MessageRecord {
		t.Helper()
		rel := writeMessage(name, raw)
		blobRecord, err := db.CreateBlob(ctx, store.BlobRecord{
			UserID: user.ID, Kind: "message", Path: filepath.Join("meta", name), SHA256: name, Size: int64(len(raw)),
		})
		if err != nil {
			t.Fatal(err)
		}
		message, err := db.CreateMessage(ctx, store.CreateMessage{
			UserID: user.ID, AccountID: account.ID, MailboxID: mailbox.ID, BlobID: blobRecord.ID,
			MessageIDHeader: fmt.Sprintf("<backfill-%d@example.test>", uid), FromAddr: from,
			Date: time.Unix(1700000000, 0), UID: uid, BlobPath: rel,
		})
		if err != nil {
			t.Fatal(err)
		}
		return message
	}

	list := create(1, "News <news@example.test>", "list.eml",
		"From: News <news@example.test>\r\nList-Id: <news.example.test>\r\nList-Post: <mailto:news@example.test>\r\n\r\nhi\r\n")
	newsletter := create(2, "Shop <offers@example.test>", "newsletter.eml",
		"From: Shop <offers@example.test>\r\nList-Unsubscribe: <https://example.test/x>\r\n\r\nhi\r\n")
	// No stored body at all: the sender is the only evidence left.
	bodyless := create(3, "no-reply@example.test", "", "")
	// A stored file that is not a message at all still has to be filed.
	garbage := create(4, "Ada <ada@example.test>", "garbage.eml", "this is not a message")

	service := &Service{Store: db, Blobs: store2}
	classified, err := service.ClassifyPendingCategoriesForUser(ctx, user.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if classified != 4 {
		t.Fatalf("classified = %d, want 4", classified)
	}
	want := map[int64]string{
		list.ID:       mailparse.CategoryForums,
		newsletter.ID: mailparse.CategoryNewsletters,
		bodyless.ID:   mailparse.CategoryNotifications,
		garbage.ID:    mailparse.CategoryRelevant,
	}
	for id, category := range want {
		message, err := db.GetMessageForUser(ctx, user.ID, id)
		if err != nil {
			t.Fatal(err)
		}
		if message.Category != category {
			t.Fatalf("message %d category = %q, want %q", id, message.Category, category)
		}
	}
	if remaining, err := db.CountMessagesNeedingCategory(ctx, user.ID); err != nil || remaining != 0 {
		t.Fatalf("remaining = %d err=%v, want 0", remaining, err)
	}
	// A second pass has nothing to do, which is what makes the worker stop.
	if classified, err := service.ClassifyPendingCategoriesForUser(ctx, user.ID, 10); err != nil || classified != 0 {
		t.Fatalf("second pass classified = %d err=%v, want 0", classified, err)
	}
}
