package store

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// seedSearchMessage creates one stored message the search rows can reference.
func seedSearchMessage(t *testing.T, ctx context.Context, db *Store, user User, account MailAccount, mailbox Mailbox, uid uint32, subject string) MessageRecord {
	t.Helper()
	path := fmt.Sprintf("users/%d/search-tests/uid-%d.eml", user.ID, uid)
	blob, err := db.CreateBlob(ctx, BlobRecord{UserID: user.ID, Kind: "message", Path: path, SHA256: fmt.Sprintf("%064d", uid), Size: 1})
	if err != nil {
		t.Fatal(err)
	}
	msg, err := db.CreateMessage(ctx, CreateMessage{
		UserID: user.ID, AccountID: account.ID, MailboxID: mailbox.ID, BlobID: blob.ID,
		MessageIDHeader: fmt.Sprintf("<search-%d@example.test>", uid),
		CanonicalSHA256: fmt.Sprintf("%064d", uid), MessageIDHash: fmt.Sprintf("hash-%d", uid),
		ThreadKey: fmt.Sprintf("thread-%d", uid), Subject: subject,
		FromAddr: "sender@example.test", Date: time.Now(), InternalDate: time.Now(),
		UID: uid, UIDValidity: mailbox.UIDValidity, Size: 1, BlobPath: path,
	})
	if err != nil {
		t.Fatal(err)
	}
	return msg
}

func searchTestFixtures(t *testing.T, ctx context.Context, db *Store) (User, MailAccount, Mailbox) {
	t.Helper()
	user, err := db.CreateUser(ctx, "search-rows@example.test", "Search", "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	account, err := db.CreateMailAccount(ctx, MailAccount{UserID: user.ID, Label: "Test", Email: "search-rows@example.test", Host: "imap.example.test", Port: 993, Username: "u", EncryptedPassword: "x"})
	if err != nil {
		t.Fatal(err)
	}
	mailbox, err := db.GetOrCreateMailbox(ctx, user.ID, account.ID, "INBOX")
	if err != nil {
		t.Fatal(err)
	}
	return user, account, mailbox
}

func TestMessageSearchRowsRoundTrip(t *testing.T) {
	ctx := context.Background()
	db := mustOpenTestStore(t)
	user, account, mailbox := searchTestFixtures(t, ctx, db)
	first := seedSearchMessage(t, ctx, db, user, account, mailbox, 1, "Quartalsbericht Rechnung")
	second := seedSearchMessage(t, ctx, db, user, account, mailbox, 2, "Sitzungsprotokoll")

	docs := []MessageSearchDoc{
		{MessageID: first.ID, UserID: user.ID, TextA: "Quartalsbericht Rechnung", TextB: "sender@example.test", TextC: "die rechnung liegt bei", TextD: "rechnung.pdf"},
		{MessageID: second.ID, UserID: user.ID, TextA: "Sitzungsprotokoll", TextB: "sender@example.test", TextC: "protokoll der sitzung", TextD: ""},
	}
	if err := db.UpsertMessageSearch(ctx, user.ID, docs); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	count, err := db.CountMessageSearchForUser(ctx, user.ID)
	if err != nil || count != 2 {
		t.Fatalf("count = %d, err = %v, want 2", count, err)
	}
	perMailbox, err := db.CountMessageSearchForMailbox(ctx, user.ID, mailbox.ID)
	if err != nil || perMailbox != 2 {
		t.Fatalf("mailbox count = %d, err = %v, want 2", perMailbox, err)
	}

	// The vector is weighted and searchable: subject terms carry weight A.
	var hits int
	if err := db.db.QueryRowContext(ctx, `SELECT count(*) FROM message_search
		WHERE user_id = ? AND tsv @@ to_tsquery('simple', 'rechnung')`, user.ID).Scan(&hits); err != nil {
		t.Fatalf("query tsv: %v", err)
	}
	if hits != 1 {
		t.Fatalf("tsquery hits = %d, want 1", hits)
	}
	var weightedHits int
	if err := db.db.QueryRowContext(ctx, `SELECT count(*) FROM message_search
		WHERE user_id = ? AND tsv @@ to_tsquery('simple', 'quartalsbericht:A')`, user.ID).Scan(&weightedHits); err != nil {
		t.Fatalf("query weighted tsv: %v", err)
	}
	if weightedHits != 1 {
		t.Fatalf("weighted tsquery hits = %d, want 1", weightedHits)
	}

	// Re-upserting replaces the vector rather than duplicating the row.
	docs[0].TextA = "Ersetzter Betreff"
	if err := db.UpsertMessageSearch(ctx, user.ID, docs[:1]); err != nil {
		t.Fatalf("re-upsert: %v", err)
	}
	if err := db.db.QueryRowContext(ctx, `SELECT count(*) FROM message_search
		WHERE user_id = ? AND tsv @@ to_tsquery('simple', 'quartalsbericht')`, user.ID).Scan(&hits); err != nil {
		t.Fatalf("query replaced tsv: %v", err)
	}
	if hits != 0 {
		t.Fatalf("replaced subject still matches, hits = %d", hits)
	}

	present, err := db.MessageSearchPresence(ctx, user.ID, []int64{first.ID, second.ID, 999999})
	if err != nil {
		t.Fatalf("presence: %v", err)
	}
	if !present[first.ID] || !present[second.ID] || present[999999] {
		t.Fatalf("presence = %v", present)
	}
	ids, err := db.MessageSearchMailboxIDs(ctx, user.ID, mailbox.ID)
	if err != nil || len(ids) != 2 {
		t.Fatalf("mailbox ids = %v, err = %v", ids, err)
	}
	bytes, err := db.MessageSearchBytes(ctx, user.ID)
	if err != nil || bytes <= 0 {
		t.Fatalf("bytes = %d, err = %v, want > 0", bytes, err)
	}

	deleted, err := db.DeleteMessageSearch(ctx, user.ID, []int64{first.ID})
	if err != nil || deleted != 1 {
		t.Fatalf("delete = %d, err = %v, want 1", deleted, err)
	}
	purged, err := db.PurgeMessageSearchForMailbox(ctx, user.ID, mailbox.ID)
	if err != nil || purged != 1 {
		t.Fatalf("purge = %d, err = %v, want 1", purged, err)
	}
	count, err = db.CountMessageSearchForUser(ctx, user.ID)
	if err != nil || count != 0 {
		t.Fatalf("count after purge = %d, err = %v, want 0", count, err)
	}
}

func TestMessageSearchRowsFollowMessageDeletion(t *testing.T) {
	ctx := context.Background()
	db := mustOpenTestStore(t)
	user, account, mailbox := searchTestFixtures(t, ctx, db)
	msg := seedSearchMessage(t, ctx, db, user, account, mailbox, 7, "Kaskadentest")
	if err := db.UpsertMessageSearch(ctx, user.ID, []MessageSearchDoc{
		{MessageID: msg.ID, UserID: user.ID, TextA: "Kaskadentest"},
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := db.DeleteMessageForUser(ctx, user.ID, msg.ID); err != nil {
		t.Fatalf("delete message: %v", err)
	}
	count, err := db.CountMessageSearchForUser(ctx, user.ID)
	if err != nil || count != 0 {
		t.Fatalf("count after message delete = %d, err = %v, want 0 via cascade", count, err)
	}
}

func TestMessageSearchScopesByUser(t *testing.T) {
	ctx := context.Background()
	db := mustOpenTestStore(t)
	user, account, mailbox := searchTestFixtures(t, ctx, db)
	other, err := db.CreateUser(ctx, "search-other@example.test", "Other", "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	msg := seedSearchMessage(t, ctx, db, user, account, mailbox, 3, "Mandantengrenze")
	if err := db.UpsertMessageSearch(ctx, user.ID, []MessageSearchDoc{
		{MessageID: msg.ID, UserID: user.ID, TextA: "Mandantengrenze"},
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if count, err := db.CountMessageSearchForUser(ctx, other.ID); err != nil || count != 0 {
		t.Fatalf("other tenant count = %d, err = %v, want 0", count, err)
	}
	if deleted, err := db.DeleteMessageSearch(ctx, other.ID, []int64{msg.ID}); err != nil || deleted != 0 {
		t.Fatalf("cross-tenant delete = %d, err = %v, want 0", deleted, err)
	}
	if err := db.DropMessageSearchForUser(ctx, other.ID); err != nil {
		t.Fatalf("drop other: %v", err)
	}
	if count, err := db.CountMessageSearchForUser(ctx, user.ID); err != nil || count != 1 {
		t.Fatalf("count after cross-tenant ops = %d, err = %v, want 1", count, err)
	}
	if mismatch := db.UpsertMessageSearch(ctx, user.ID, []MessageSearchDoc{
		{MessageID: msg.ID, UserID: other.ID, TextA: "falscher Mandant"},
	}); mismatch == nil {
		t.Fatal("cross-tenant doc in a batch was accepted")
	}
}
