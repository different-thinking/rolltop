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
	"rolltop/backend/store/storetest"
)

// TestCategoryBackfillReadsStoredMailAndAlwaysDrains covers the property the
// whole worker depends on: a candidate is always filed, even when its raw
// message is missing or unreadable. A row that stayed pending would be picked up
// on every pass and the backfill would never finish.
func TestCategoryBackfillReadsStoredMailAndAlwaysDrains(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	db, err := storetest.Open(t)
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

// TestCategoryRePassKeepsWhatItCannotImprove covers the other half of the
// backfill's contract. A message an older classifier generation filed is
// re-read to see whether the rules now say something better; when its stored
// message is gone -- blob retention prunes raw messages and clears blob_path
// with them, which on a default install is most of a mailbox -- there is
// nothing better to say, and the answer its headers earned it has to survive.
func TestCategoryRePassKeepsWhatItCannotImprove(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	db, err := storetest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	user, err := db.CreateUser(ctx, "repass@example.test", "Repass", "hash", false)
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
	blobs := blob.New(root)
	create := func(uid uint32, from, name, raw, category string) store.MessageRecord {
		t.Helper()
		rel := ""
		if raw != "" {
			rel = filepath.Join("users", fmt.Sprintf("%d", user.ID), "blobs", name)
			abs := filepath.Join(root, rel)
			if err := os.MkdirAll(filepath.Dir(abs), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(abs, []byte(raw), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		blobRecord, err := db.CreateBlob(ctx, store.BlobRecord{
			UserID: user.ID, Kind: "message", Path: filepath.Join("meta", fmt.Sprintf("%d", uid)),
			SHA256: fmt.Sprintf("sha-%d", uid), Size: int64(len(raw)),
		})
		if err != nil {
			t.Fatal(err)
		}
		message, err := db.CreateMessage(ctx, store.CreateMessage{
			UserID: user.ID, AccountID: account.ID, MailboxID: mailbox.ID, BlobID: blobRecord.ID,
			MessageIDHeader: fmt.Sprintf("<repass-%d@example.test>", uid), FromAddr: from,
			Category: category, Date: time.Unix(1700000000, 0), UID: uid, BlobPath: rel,
		})
		if err != nil {
			t.Fatal(err)
		}
		return message
	}
	service := &Service{Store: db, Blobs: blobs}

	// A candidate the worker cannot read at all, standing for every row whose
	// raw message has aged out.
	pruned := create(1, "Shop <offers@example.test>", "", "", mailparse.CategoryNewsletters)
	kept := service.categorizeStoredMessage(user.ID, store.CategoryCandidate{
		ID: pruned.ID, FromAddr: "Shop <offers@example.test>", Category: mailparse.CategoryNewsletters,
	})
	if kept != mailparse.CategoryNewsletters {
		t.Fatalf("unreadable filed message = %q, want the category it already had (%q)", kept, mailparse.CategoryNewsletters)
	}
	// An unfiled message in the same state still has to come out with
	// something, or the pass would select it forever.
	guessed := service.categorizeStoredMessage(user.ID, store.CategoryCandidate{
		ID: pruned.ID, FromAddr: "no-reply@example.test",
	})
	if guessed != mailparse.CategoryNotifications {
		t.Fatalf("unreadable pending message = %q, want the address fallback %q", guessed, mailparse.CategoryNotifications)
	}

	// And a filed message that can still be read is re-decided, which is the
	// point of the pass.
	readable := create(2, "Telco <no-reply@telco.test>", "invoice.eml",
		"From: Telco <no-reply@telco.test>\r\nSubject: Ihre Rechnung\r\nAuto-Submitted: auto-generated\r\n\r\nDanke.\r\n",
		mailparse.CategoryNotifications)
	candidates, err := db.ListMessagesNeedingCategory(ctx, user.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 0 {
		t.Fatalf("candidates before a generation bump = %+v, want none", candidates)
	}
	refiled := service.categorizeStoredMessage(user.ID, store.CategoryCandidate{
		ID: readable.ID, FromAddr: "Telco <no-reply@telco.test>", BlobPath: readable.BlobPath,
		Category: mailparse.CategoryNotifications,
	})
	if refiled != mailparse.CategoryInvoices {
		t.Fatalf("re-read filed message = %q, want %q", refiled, mailparse.CategoryInvoices)
	}
}
