// File overview: Tests for cross-account duplicate copy detection: which copy
// wins, which stays visible when the evidence is thin, and what happens to the
// pointer when the original is deleted.

package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

type duplicateFixture struct {
	db        *Store
	ctx       context.Context
	userID    int64
	original  int64
	aggregate int64
	// Mailboxes by account, all INBOX unless a test asks for another folder.
	originalInbox  int64
	aggregateInbox int64
}

func newDuplicateFixture(t *testing.T) duplicateFixture {
	t.Helper()
	ctx := context.Background()
	dataDir := filepath.Join(t.TempDir(), "data")
	db, err := OpenServer(filepath.Join(dataDir, "rolltop.db"), dataDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	user, err := db.CreateUser(ctx, "owner@example.test", "Owner", "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	original, err := db.CreateMailAccount(ctx, MailAccount{
		UserID: user.ID, Email: "info@firma.test", Host: "imap.firma.test", Port: 993,
		Username: "info@firma.test", EncryptedPassword: "secret", UseTLS: true, Mailbox: "*",
	})
	if err != nil {
		t.Fatal(err)
	}
	aggregate, err := db.CreateMailAccount(ctx, MailAccount{
		UserID: user.ID, Email: "owner@gmail.test", Host: "imap.gmail.test", Port: 993,
		Username: "owner@gmail.test", EncryptedPassword: "secret", UseTLS: true, Mailbox: "*",
	})
	if err != nil {
		t.Fatal(err)
	}
	originalInbox, err := db.GetOrCreateMailbox(ctx, user.ID, original.ID, "INBOX")
	if err != nil {
		t.Fatal(err)
	}
	aggregateInbox, err := db.GetOrCreateMailbox(ctx, user.ID, aggregate.ID, "INBOX")
	if err != nil {
		t.Fatal(err)
	}
	return duplicateFixture{
		db: db, ctx: ctx, userID: user.ID,
		original: original.ID, aggregate: aggregate.ID,
		originalInbox: originalInbox.ID, aggregateInbox: aggregateInbox.ID,
	}
}

func (f duplicateFixture) storeMessage(t *testing.T, accountID, mailboxID int64, uid uint32, messageID, to string) MessageRecord {
	t.Helper()
	blob, err := f.db.CreateBlob(f.ctx, BlobRecord{
		UserID: f.userID,
		Kind:   "message",
		Path:   filepath.Join("blobs", messageID, string(rune('a'+int(uid)))),
		SHA256: messageID + string(rune('a'+int(uid))),
		Size:   10,
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	msg, err := f.db.CreateMessage(f.ctx, CreateMessage{
		UserID: f.userID, AccountID: accountID, MailboxID: mailboxID, BlobID: blob.ID,
		MessageIDHeader: messageID, ThreadKey: "msgid:" + messageID,
		Subject: "Invoice", FromAddr: "sender@partner.test", ToAddr: to,
		Date: now, InternalDate: now, UID: uid, Size: 10, BlobPath: blob.Path,
	})
	if err != nil {
		t.Fatal(err)
	}
	return msg
}

func (f duplicateFixture) duplicatePointer(t *testing.T, messageID int64) int64 {
	t.Helper()
	db, err := f.db.dataDB(f.ctx, f.userID)
	if err != nil {
		t.Fatal(err)
	}
	var pointer int64
	if err := db.QueryRowContext(f.ctx, `SELECT duplicate_of_message_id FROM messages
		WHERE user_id = ? AND id = ?`, f.userID, messageID).Scan(&pointer); err != nil {
		t.Fatal(err)
	}
	return pointer
}

// The aggregating account fetched a copy of mail addressed to another account.
// Only the fetched copy is hidden, and it points at the addressed account's row.
func TestDuplicateScanHidesTheCopyTheAccountWasNotAddressedIn(t *testing.T) {
	f := newDuplicateFixture(t)
	original := f.storeMessage(t, f.original, f.originalInbox, 1, "<invoice@partner.test>", "info@firma.test")
	fetched := f.storeMessage(t, f.aggregate, f.aggregateInbox, 1, "<invoice@partner.test>", "info@firma.test")

	stats, err := f.db.RefreshDuplicateCopiesForUser(f.ctx, f.userID)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Groups != 1 || stats.Hidden != 1 {
		t.Fatalf("scan stats = %+v, want one group with one hidden copy", stats)
	}
	if pointer := f.duplicatePointer(t, fetched.ID); pointer != original.ID {
		t.Fatalf("fetched copy points at %d, want the addressed account's message %d", pointer, original.ID)
	}
	if pointer := f.duplicatePointer(t, original.ID); pointer != 0 {
		t.Fatalf("addressed copy points at %d, want it to stay visible", pointer)
	}
	messages, err := f.db.ListMessagesForUser(f.ctx, f.userID, 50, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 1 || messages[0].ID != original.ID {
		t.Fatalf("list returned %d messages, want only the addressed copy %d", len(messages), original.ID)
	}
	thread, err := f.db.ListThreadMessagesForUser(f.ctx, f.userID, original)
	if err != nil {
		t.Fatal(err)
	}
	if len(thread) != 1 {
		t.Fatalf("thread returned %d messages, want the duplicate collapsed away", len(thread))
	}
}

// Mail nobody was addressed in - a Bcc, or a mailing list - gives detection no
// way to tell the original from the copy. Both stay visible, because showing a
// message twice is recoverable and hiding the only copy is not.
func TestDuplicateScanKeepsBothCopiesWhenNoAccountWasAddressed(t *testing.T) {
	f := newDuplicateFixture(t)
	first := f.storeMessage(t, f.original, f.originalInbox, 2, "<list@partner.test>", "members@list.test")
	second := f.storeMessage(t, f.aggregate, f.aggregateInbox, 2, "<list@partner.test>", "members@list.test")

	if _, err := f.db.RefreshDuplicateCopiesForUser(f.ctx, f.userID); err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{first.ID, second.ID} {
		if pointer := f.duplicatePointer(t, id); pointer != 0 {
			t.Fatalf("message %d points at %d, want both copies visible", id, pointer)
		}
	}
}

// Both accounts addressed means the user really is on the message twice. That is
// two deliveries, not one delivery mirrored twice.
func TestDuplicateScanKeepsBothCopiesWhenBothAccountsWereAddressed(t *testing.T) {
	f := newDuplicateFixture(t)
	recipients := "info@firma.test, owner@gmail.test"
	first := f.storeMessage(t, f.original, f.originalInbox, 3, "<both@partner.test>", recipients)
	second := f.storeMessage(t, f.aggregate, f.aggregateInbox, 3, "<both@partner.test>", recipients)

	if _, err := f.db.RefreshDuplicateCopiesForUser(f.ctx, f.userID); err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{first.ID, second.ID} {
		if pointer := f.duplicatePointer(t, id); pointer != 0 {
			t.Fatalf("message %d points at %d, want both copies visible", id, pointer)
		}
	}
}

// A Sent copy is the user's own writing rather than a second delivery of someone
// else's message, so it is never hidden behind another account's row.
func TestDuplicateScanNeverHidesASentCopy(t *testing.T) {
	f := newDuplicateFixture(t)
	sent, err := f.db.GetOrCreateMailbox(f.ctx, f.userID, f.aggregate, "Sent")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.UpdateMailboxSettings(f.ctx, f.userID, sent.ID, MailboxSettings{
		SyncMode: "auto", Role: "sent", ShowInSidebar: true, ShowInAllMail: true, IncludeInSearch: true,
	}); err != nil {
		t.Fatal(err)
	}
	f.storeMessage(t, f.original, f.originalInbox, 4, "<reply@partner.test>", "info@firma.test")
	sentCopy := f.storeMessage(t, f.aggregate, sent.ID, 4, "<reply@partner.test>", "info@firma.test")

	if _, err := f.db.RefreshDuplicateCopiesForUser(f.ctx, f.userID); err != nil {
		t.Fatal(err)
	}
	if pointer := f.duplicatePointer(t, sentCopy.ID); pointer != 0 {
		t.Fatalf("sent copy points at %d, want it left alone", pointer)
	}
}

// Hiding a message behind a pointer is only safe while the pointer resolves.
// Deleting the original has to bring its copy back, whichever code path does the
// deleting, which is why the invariant lives in a trigger.
func TestDeletingTheOriginalRevealsItsHiddenCopy(t *testing.T) {
	f := newDuplicateFixture(t)
	original := f.storeMessage(t, f.original, f.originalInbox, 5, "<gone@partner.test>", "info@firma.test")
	fetched := f.storeMessage(t, f.aggregate, f.aggregateInbox, 5, "<gone@partner.test>", "info@firma.test")
	if _, err := f.db.RefreshDuplicateCopiesForUser(f.ctx, f.userID); err != nil {
		t.Fatal(err)
	}
	if pointer := f.duplicatePointer(t, fetched.ID); pointer != original.ID {
		t.Fatalf("fetched copy points at %d, want %d before the delete", pointer, original.ID)
	}
	if err := f.db.DeleteMessageForUser(f.ctx, f.userID, original.ID); err != nil {
		t.Fatal(err)
	}
	if pointer := f.duplicatePointer(t, fetched.ID); pointer != 0 {
		t.Fatalf("fetched copy still points at %d after its original was deleted", pointer)
	}
	messages, err := f.db.ListMessagesForUser(f.ctx, f.userID, 50, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 1 || messages[0].ID != fetched.ID {
		t.Fatalf("list returned %d messages, want the revealed copy %d", len(messages), fetched.ID)
	}
}

// Detection runs per message during sync, so a copy arriving after its original
// is hidden immediately rather than at the next full scan.
func TestIncrementalRefreshHidesACopyArrivingAfterItsOriginal(t *testing.T) {
	f := newDuplicateFixture(t)
	original := f.storeMessage(t, f.original, f.originalInbox, 6, "<later@partner.test>", "info@firma.test")
	fetched := f.storeMessage(t, f.aggregate, f.aggregateInbox, 6, "<later@partner.test>", "info@firma.test")

	if err := f.db.RefreshDuplicateCopiesForMessageID(f.ctx, f.userID, "<later@partner.test>"); err != nil {
		t.Fatal(err)
	}
	if pointer := f.duplicatePointer(t, fetched.ID); pointer != original.ID {
		t.Fatalf("fetched copy points at %d, want %d", pointer, original.ID)
	}
}

// Folder counters drive the sidebar badge, which is where a doubled mailbox is
// most obvious. A hidden copy counts for neither the total nor the unread badge.
func TestFolderCountersIgnoreHiddenDuplicates(t *testing.T) {
	f := newDuplicateFixture(t)
	f.storeMessage(t, f.original, f.originalInbox, 7, "<counted@partner.test>", "info@firma.test")
	f.storeMessage(t, f.aggregate, f.aggregateInbox, 7, "<counted@partner.test>", "info@firma.test")
	if _, err := f.db.RefreshDuplicateCopiesForUser(f.ctx, f.userID); err != nil {
		t.Fatal(err)
	}
	mailboxes, err := f.db.ListMailboxesForUser(f.ctx, f.userID)
	if err != nil {
		t.Fatal(err)
	}
	for _, mailbox := range mailboxes {
		if mailbox.AccountID != f.aggregate {
			continue
		}
		if mailbox.MessageCount != 0 || mailbox.UnreadCount != 0 {
			t.Fatalf("aggregating folder counts = %d/%d unread, want both 0",
				mailbox.MessageCount, mailbox.UnreadCount)
		}
	}
}

// Detection is per tenant. One user's accounts must never explain away another
// user's mail, even when both mirror the same Message-ID.
func TestDuplicateScanStaysInsideOneTenant(t *testing.T) {
	f := newDuplicateFixture(t)
	other, err := f.db.CreateUser(f.ctx, "other@example.test", "Other", "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	otherAccount, err := f.db.CreateMailAccount(f.ctx, MailAccount{
		UserID: other.ID, Email: "info@firma.test", Host: "imap.firma.test", Port: 993,
		Username: "info@firma.test", EncryptedPassword: "secret", UseTLS: true, Mailbox: "*",
	})
	if err != nil {
		t.Fatal(err)
	}
	otherInbox, err := f.db.GetOrCreateMailbox(f.ctx, other.ID, otherAccount.ID, "INBOX")
	if err != nil {
		t.Fatal(err)
	}
	blob, err := f.db.CreateBlob(f.ctx, BlobRecord{
		UserID: other.ID, Kind: "message", Path: "blobs/other", SHA256: "other", Size: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	otherMessage, err := f.db.CreateMessage(f.ctx, CreateMessage{
		UserID: other.ID, AccountID: otherAccount.ID, MailboxID: otherInbox.ID, BlobID: blob.ID,
		MessageIDHeader: "<shared@partner.test>", ThreadKey: "msgid:<shared@partner.test>",
		Subject: "Invoice", FromAddr: "sender@partner.test", ToAddr: "info@firma.test",
		Date: now, InternalDate: now, UID: 8, Size: 10, BlobPath: "blobs/other",
	})
	if err != nil {
		t.Fatal(err)
	}
	f.storeMessage(t, f.aggregate, f.aggregateInbox, 8, "<shared@partner.test>", "info@firma.test")

	if _, err := f.db.RefreshDuplicateCopiesForUser(f.ctx, f.userID); err != nil {
		t.Fatal(err)
	}
	otherDB, err := f.db.dataDB(f.ctx, other.ID)
	if err != nil {
		t.Fatal(err)
	}
	var pointer int64
	if err := otherDB.QueryRowContext(f.ctx, `SELECT duplicate_of_message_id FROM messages
		WHERE user_id = ? AND id = ?`, other.ID, otherMessage.ID).Scan(&pointer); err != nil {
		t.Fatal(err)
	}
	if pointer != 0 {
		t.Fatalf("other tenant's message points at %d, want it untouched", pointer)
	}
	counts, err := f.db.CountHiddenDuplicateCopiesForUser(f.ctx, f.userID)
	if err != nil {
		t.Fatal(err)
	}
	if len(counts) != 0 {
		t.Fatalf("hidden counts = %v, want nothing hidden across tenants", counts)
	}
}

// A copy the aggregating account fetched must not ring the phone a second time:
// the original's own arrival already produced that notification.
func TestHiddenDuplicateCopyProducesNoNewMailEvent(t *testing.T) {
	f := newDuplicateFixture(t)
	original := f.storeMessage(t, f.original, f.originalInbox, 9, "<ping@partner.test>", "info@firma.test")
	fetched := f.storeMessage(t, f.aggregate, f.aggregateInbox, 9, "<ping@partner.test>", "info@firma.test")
	if _, err := f.db.RefreshDuplicateCopiesForUser(f.ctx, f.userID); err != nil {
		t.Fatal(err)
	}
	if _, created, err := f.db.RecordNewMailEvent(f.ctx, f.userID, original); err != nil {
		t.Fatal(err)
	} else if !created {
		t.Fatal("the addressed copy produced no new-mail event")
	}
	_, created, err := f.db.RecordNewMailEvent(f.ctx, f.userID, fetched)
	if err != nil {
		t.Fatal(err)
	}
	if created {
		t.Fatal("the fetched duplicate produced a second new-mail event")
	}
}
