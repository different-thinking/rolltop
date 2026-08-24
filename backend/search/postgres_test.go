package search

import (
	"context"
	"fmt"
	"testing"
	"time"

	"rolltop/backend/store"
	"rolltop/backend/store/storetest"
)

func openPostgresSearchFixtures(t *testing.T) (*Service, *store.Store, store.User, store.Mailbox) {
	t.Helper()
	db, err := storetest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	user, err := db.CreateUser(ctx, "pg-search@example.test", "PG", "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	account, err := db.CreateMailAccount(ctx, store.MailAccount{
		UserID: user.ID, Label: "Test", Email: "pg-search@example.test",
		Host: "imap.example.test", Port: 993, Username: "u", EncryptedPassword: "x",
	})
	if err != nil {
		t.Fatal(err)
	}
	mailbox, err := db.GetOrCreateMailbox(ctx, user.ID, account.ID, "INBOX")
	if err != nil {
		t.Fatal(err)
	}
	svc := OpenPostgresBackend(db)
	t.Cleanup(func() { _ = svc.Close() })
	return svc, db, user, mailbox
}

// newPostgresSearchTenant adds a second tenant to an open fixture, for the
// assertions that only mean something with somebody else's mail in the same
// tables.
func newPostgresSearchTenant(t *testing.T, db *store.Store, email string) (store.User, store.Mailbox) {
	t.Helper()
	ctx := context.Background()
	user, err := db.CreateUser(ctx, email, "Other", "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	account, err := db.CreateMailAccount(ctx, store.MailAccount{
		UserID: user.ID, Label: "Test", Email: email,
		Host: "imap.example.test", Port: 993, Username: "u", EncryptedPassword: "x",
	})
	if err != nil {
		t.Fatal(err)
	}
	mailbox, err := db.GetOrCreateMailbox(ctx, user.ID, account.ID, "INBOX")
	if err != nil {
		t.Fatal(err)
	}
	return user, mailbox
}

func seedPostgresSearchMessage(t *testing.T, db *store.Store, user store.User, mailbox store.Mailbox, uid uint32, subject, body string) store.MessageRecord {
	t.Helper()
	ctx := context.Background()
	path := fmt.Sprintf("users/%d/pg-search/uid-%d.eml", user.ID, uid)
	blob, err := db.CreateBlob(ctx, store.BlobRecord{UserID: user.ID, Kind: "message", Path: path, SHA256: fmt.Sprintf("%064d", uid), Size: 1})
	if err != nil {
		t.Fatal(err)
	}
	msg, err := db.CreateMessage(ctx, store.CreateMessage{
		UserID: user.ID, AccountID: mailbox.AccountID, MailboxID: mailbox.ID, BlobID: blob.ID,
		MessageIDHeader: fmt.Sprintf("<pg-search-%d@example.test>", uid),
		CanonicalSHA256: fmt.Sprintf("%064d", uid), MessageIDHash: fmt.Sprintf("pg-hash-%d", uid),
		ThreadKey: fmt.Sprintf("pg-thread-%d", uid), Subject: subject, BodyText: body,
		FromAddr: "alice@example.test", ToAddr: "bob@example.test",
		Date: time.Now(), InternalDate: time.Now(),
		UID: uid, UIDValidity: mailbox.UIDValidity, Size: 1, BlobPath: path,
	})
	if err != nil {
		t.Fatal(err)
	}
	return msg
}

// seedPostgresSearchMessageFrom is seedPostgresSearchMessage for the ranking
// assertions, which only mean something when the sender and the date differ
// between the messages being ranked.
func seedPostgresSearchMessageFrom(t *testing.T, db *store.Store, user store.User, mailbox store.Mailbox, uid uint32, subject, body, from string, date time.Time) store.MessageRecord {
	t.Helper()
	ctx := context.Background()
	path := fmt.Sprintf("users/%d/pg-search/uid-%d.eml", user.ID, uid)
	blob, err := db.CreateBlob(ctx, store.BlobRecord{UserID: user.ID, Kind: "message", Path: path, SHA256: fmt.Sprintf("%064d", uid), Size: 1})
	if err != nil {
		t.Fatal(err)
	}
	msg, err := db.CreateMessage(ctx, store.CreateMessage{
		UserID: user.ID, AccountID: mailbox.AccountID, MailboxID: mailbox.ID, BlobID: blob.ID,
		MessageIDHeader: fmt.Sprintf("<pg-search-%d@example.test>", uid),
		CanonicalSHA256: fmt.Sprintf("%064d", uid), MessageIDHash: fmt.Sprintf("pg-hash-%d", uid),
		ThreadKey: fmt.Sprintf("pg-thread-%d", uid), Subject: subject, BodyText: body,
		FromAddr: from, ToAddr: "bob@example.test",
		Date: date, InternalDate: date,
		UID: uid, UIDValidity: mailbox.UIDValidity, Size: 1, BlobPath: path,
	})
	if err != nil {
		t.Fatal(err)
	}
	return msg
}

func TestPostgresBackendWritePath(t *testing.T) {
	svc, db, user, mailbox := openPostgresSearchFixtures(t)
	ctx := context.Background()
	if !svc.PostgresBackend() {
		t.Fatal("service does not report the postgres backend")
	}

	first := seedPostgresSearchMessage(t, db, user, mailbox, 1, "Steuerbescheid 2026", "der bescheid liegt bei")
	second := seedPostgresSearchMessage(t, db, user, mailbox, 2, "Angebot Serverschrank", "das angebot gilt bis freitag")

	if err := svc.IndexMessages(ctx, []MessageIndexDocument{
		{Message: first, Attachments: []AttachmentDoc{{Filename: "bescheid.pdf", ContentType: "application/pdf", Text: "steuernummer 12/345"}}},
		{Message: second},
	}); err != nil {
		t.Fatalf("index: %v", err)
	}

	count, err := svc.CountUserMessages(ctx, user.ID)
	if err != nil || count != 2 {
		t.Fatalf("count = %d, err = %v, want 2", count, err)
	}
	perMailbox, err := svc.CountMailboxMessages(ctx, user.ID, mailbox.ID)
	if err != nil || perMailbox != 2 {
		t.Fatalf("mailbox count = %d, err = %v, want 2", perMailbox, err)
	}
	indexed, err := svc.MessageIDsIndexed(ctx, user.ID, []int64{first.ID, second.ID, 424242})
	if err != nil {
		t.Fatalf("indexed: %v", err)
	}
	if !indexed[first.ID] || !indexed[second.ID] || indexed[424242] {
		t.Fatalf("indexed = %v", indexed)
	}
	ids, err := svc.MailboxMessageIDs(ctx, user.ID, mailbox.ID)
	if err != nil || len(ids) != 2 {
		t.Fatalf("mailbox ids = %v, err = %v", ids, err)
	}
	if bytes, ok := svc.PerUserIndexBytes(user.ID); !ok || bytes <= 0 {
		t.Fatalf("per-user bytes = %d, ok = %v, want > 0", bytes, ok)
	}

	var batches []int
	if err := svc.DeleteMessagesWithProgress(ctx, user.ID, []int64{first.ID}, func(n int) error {
		batches = append(batches, n)
		return nil
	}); err != nil {
		t.Fatalf("delete with progress: %v", err)
	}
	if len(batches) != 1 || batches[0] != 1 {
		t.Fatalf("progress batches = %v, want [1]", batches)
	}
	purged, err := svc.PurgeMailboxWithProgress(ctx, user.ID, mailbox.ID, nil)
	if err != nil || purged != 1 {
		t.Fatalf("purge = %d, err = %v, want 1", purged, err)
	}
	if err := svc.IndexMessage(ctx, second, nil); err != nil {
		t.Fatalf("reindex: %v", err)
	}
	if err := svc.DropUser(ctx, user.ID); err != nil {
		t.Fatalf("drop user: %v", err)
	}
	count, err = svc.CountUserMessages(ctx, user.ID)
	if err != nil || count != 0 {
		t.Fatalf("count after drop = %d, err = %v, want 0", count, err)
	}
}

// TestPostgresBackendProjectsBoundedTexts pins the projection contract: the
// same bounded text streams the Bleve document carries, weighted A-D.
func TestPostgresBackendProjectsBoundedTexts(t *testing.T) {
	doc := MessageIndexDocument{
		Message: store.MessageRecord{
			ID: 1, UserID: 1, Subject: "Vertragsverlängerung Rechenzentrum",
			FromAddr: "Anna Beispiel <anna@firma-beispiel.de>", ToAddr: "bob@example.test",
			BodyText: "die verlängerung des vertrags",
		},
		Attachments: []AttachmentDoc{{Filename: "vertrag.pdf", ContentType: "application/pdf", Text: "laufzeit 24 monate"}},
	}
	a, b, c, d := buildMessageSearchTexts(doc)
	if a == "" || b == "" || c == "" || d == "" {
		t.Fatalf("streams = %q %q %q %q, want all non-empty", a, b, c, d)
	}
	if want := "die verlängerung des vertrags"; c != want {
		t.Fatalf("body stream = %q, want %q", c, want)
	}

	encrypted := doc
	encrypted.Message.IsEncrypted = true
	a, b, c, d = buildMessageSearchTexts(encrypted)
	if c != "" || d != "" {
		t.Fatalf("encrypted message leaked body %q or attachments %q into the index", c, d)
	}
	if a == "" || b == "" {
		t.Fatal("encrypted message lost its envelope streams")
	}
}
