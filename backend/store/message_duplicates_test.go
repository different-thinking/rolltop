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
	db, err := openTestStore(t)
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

// junkMailbox creates one account's Spam folder. The role is set by hand
// because the folder name alone does not carry it, and the role is what takes
// the folder out of the lists this test cares about.
func (f duplicateFixture) junkMailbox(t *testing.T, accountID int64, name string) int64 {
	t.Helper()
	mailbox, err := f.db.GetOrCreateMailbox(f.ctx, f.userID, accountID, name)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.UpdateMailboxSettings(f.ctx, f.userID, mailbox.ID, MailboxSettings{
		SyncMode: "auto", Role: "junk", ShowInSidebar: true, IncludeInSearch: true,
	}); err != nil {
		t.Fatal(err)
	}
	return mailbox.ID
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

	stats, err := f.db.RefreshDuplicateCopiesForUser(f.ctx, f.userID, "")
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
func TestDuplicateScanHidesACopyNoAddressCanAttribute(t *testing.T) {
	f := newDuplicateFixture(t)
	first := f.storeMessage(t, f.original, f.originalInbox, 2, "<list@partner.test>", "members@list.test")
	second := f.storeMessage(t, f.aggregate, f.aggregateInbox, 2, "<list@partner.test>", "members@list.test")

	stats, err := f.db.RefreshDuplicateCopiesForUser(f.ctx, f.userID, "")
	if err != nil {
		t.Fatal(err)
	}
	if pointer := f.duplicatePointer(t, first.ID); pointer != 0 {
		t.Fatalf("the copy that stays visible points at %d, want it kept as the original", pointer)
	}
	if pointer := f.duplicatePointer(t, second.ID); pointer != first.ID {
		t.Fatalf("second copy points at %d, want it hidden behind %d", pointer, first.ID)
	}
	// The reason has to say that placement decided it, not that a recipient
	// did: the two are different answers to "why is this copy the one I see".
	if got := stats.Outcomes[DuplicateGroupResolvedByPlacement]; got != 1 {
		t.Fatalf("placement-resolved groups=%d, want 1 (outcomes=%v)", got, stats.Outcomes)
	}
}

// Both accounts addressed still means one message. Which copy stays is then
// decided among the addressed accounts rather than by an account that only
// fetched the mail, so the survivor is one the sender actually wrote to.
func TestDuplicateScanKeepsOneCopyWhenBothAccountsWereAddressed(t *testing.T) {
	f := newDuplicateFixture(t)
	recipients := "info@firma.test, owner@gmail.test"
	first := f.storeMessage(t, f.original, f.originalInbox, 3, "<both@partner.test>", recipients)
	second := f.storeMessage(t, f.aggregate, f.aggregateInbox, 3, "<both@partner.test>", recipients)

	if _, err := f.db.RefreshDuplicateCopiesForUser(f.ctx, f.userID, ""); err != nil {
		t.Fatal(err)
	}
	if pointer := f.duplicatePointer(t, first.ID); pointer != 0 {
		t.Fatalf("the copy that stays visible points at %d, want it kept as the original", pointer)
	}
	if pointer := f.duplicatePointer(t, second.ID); pointer != first.ID {
		t.Fatalf("second copy points at %d, want it hidden behind %d", pointer, first.ID)
	}
}

// An account that merely fetched the mail must not outrank the account the
// sender wrote to, even when the fetched copy is the one in an Inbox. Placement
// only decides among the accounts a recipient named.
func TestDuplicateScanPrefersAnAddressedAccountOverAFetchedInboxCopy(t *testing.T) {
	f := newDuplicateFixture(t)
	archive, err := f.db.GetOrCreateMailbox(f.ctx, f.userID, f.original, "Archive")
	if err != nil {
		t.Fatal(err)
	}
	addressed := f.storeMessage(t, f.original, archive.ID, 4, "<filed@partner.test>", "info@firma.test")
	fetched := f.storeMessage(t, f.aggregate, f.aggregateInbox, 4, "<filed@partner.test>", "info@firma.test")

	if _, err := f.db.RefreshDuplicateCopiesForUser(f.ctx, f.userID, ""); err != nil {
		t.Fatal(err)
	}
	if pointer := f.duplicatePointer(t, addressed.ID); pointer != 0 {
		t.Fatalf("the addressed account's copy points at %d, want it kept as the original", pointer)
	}
	if pointer := f.duplicatePointer(t, fetched.ID); pointer != addressed.ID {
		t.Fatalf("fetched copy points at %d, want it hidden behind %d", pointer, addressed.ID)
	}
}

// Every copy in a folder the lists do not show is the one case where nothing can
// stand in: hiding either behind the other would take the message out of view
// entirely, so the group is left exactly as it is.
func TestDuplicateScanKeepsBothCopiesWhenNoCopyIsVisible(t *testing.T) {
	f := newDuplicateFixture(t)
	first := f.junkMailbox(t, f.original, "Spam")
	second := f.junkMailbox(t, f.aggregate, "Spam")
	filed := f.storeMessage(t, f.original, first, 5, "<junked@partner.test>", "info@firma.test")
	fetched := f.storeMessage(t, f.aggregate, second, 5, "<junked@partner.test>", "info@firma.test")

	stats, err := f.db.RefreshDuplicateCopiesForUser(f.ctx, f.userID, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{filed.ID, fetched.ID} {
		if pointer := f.duplicatePointer(t, id); pointer != 0 {
			t.Fatalf("message %d points at %d, want both copies left alone", id, pointer)
		}
	}
	if got := stats.Outcomes[DuplicateGroupOriginalNotVisible]; got != 1 {
		t.Fatalf("original-not-visible groups=%d, want 1 (outcomes=%v)", got, stats.Outcomes)
	}
}

// A scan that hides nothing has to be able to say why. Without the reason, the
// only thing a user can read from an unchanged count is that detection missed
// their duplicates, which sends them looking for a bug in a decision the rule
// made on purpose.
func TestDuplicateScanReportsWhyItLeftCopiesVisible(t *testing.T) {
	f := newDuplicateFixture(t)
	originalJunk := f.junkMailbox(t, f.original, "Spam")
	aggregateJunk := f.junkMailbox(t, f.aggregate, "Spam")
	f.storeMessage(t, f.original, originalJunk, 2, "<junked@partner.test>", "info@firma.test")
	f.storeMessage(t, f.aggregate, aggregateJunk, 2, "<junked@partner.test>", "info@firma.test")
	aggregateSent, err := f.db.GetOrCreateMailbox(f.ctx, f.userID, f.aggregate, "Sent")
	if err != nil {
		t.Fatal(err)
	}
	f.storeMessage(t, f.original, f.originalInbox, 3, "<written@partner.test>", "info@firma.test")
	f.storeMessage(t, f.aggregate, aggregateSent.ID, 3, "<written@partner.test>", "info@firma.test")

	stats, err := f.db.RefreshDuplicateCopiesForUser(f.ctx, f.userID, "")
	if err != nil {
		t.Fatal(err)
	}
	if stats.Hidden != 0 {
		t.Fatalf("hidden=%d, want 0", stats.Hidden)
	}
	if got := stats.Outcomes[DuplicateGroupOriginalNotVisible]; got != 1 {
		t.Fatalf("original-not-visible groups=%d, want 1 (outcomes=%v)", got, stats.Outcomes)
	}
	if got := stats.Outcomes[DuplicateGroupNothingToHide]; got != 1 {
		t.Fatalf("nothing-to-hide groups=%d, want 1 (outcomes=%v)", got, stats.Outcomes)
	}
}

// Two rows of one account holding the same Message-ID are one message filed in
// two of that account's folders. Detection never judges them, so the count is
// the only thing that separates them from copies it considered and dismissed.
func TestWithinAccountCopiesAreCountedRatherThanHidden(t *testing.T) {
	f := newDuplicateFixture(t)
	archive, err := f.db.GetOrCreateMailbox(f.ctx, f.userID, f.aggregate, "Archive")
	if err != nil {
		t.Fatal(err)
	}
	f.storeMessage(t, f.aggregate, f.aggregateInbox, 11, "<filed-twice@partner.test>", "owner@gmail.test")
	filed := f.storeMessage(t, f.aggregate, archive.ID, 12, "<filed-twice@partner.test>", "owner@gmail.test")

	if _, err := f.db.RefreshDuplicateCopiesForUser(f.ctx, f.userID, ""); err != nil {
		t.Fatal(err)
	}
	if pointer := f.duplicatePointer(t, filed.ID); pointer != 0 {
		t.Fatalf("within-account copy points at %d, want it left alone", pointer)
	}
	messages, err := f.db.CountWithinAccountDuplicatedMessagesForUser(f.ctx, f.userID)
	if err != nil {
		t.Fatal(err)
	}
	if messages != 1 {
		t.Fatalf("within-account duplicated messages=%d, want 1", messages)
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

	if _, err := f.db.RefreshDuplicateCopiesForUser(f.ctx, f.userID, ""); err != nil {
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
	if _, err := f.db.RefreshDuplicateCopiesForUser(f.ctx, f.userID, ""); err != nil {
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
	if _, err := f.db.RefreshDuplicateCopiesForUser(f.ctx, f.userID, ""); err != nil {
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

	if _, err := f.db.RefreshDuplicateCopiesForUser(f.ctx, f.userID, ""); err != nil {
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
	if _, err := f.db.RefreshDuplicateCopiesForUser(f.ctx, f.userID, ""); err != nil {
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

// A Spam-filed row is out of the decision in both directions. It cannot stand in
// as the original, because junk folders are forced out of All Mail and hiding
// the other copy behind it would leave the message reachable only from that Spam
// list - and it is not hidden either, because a hidden row is gone from its own
// folder's list too, and Spam is where the user or their server put that copy.
func TestDuplicateScanLeavesAJunkFiledCopyOutOfTheDecision(t *testing.T) {
	f := newDuplicateFixture(t)
	junk := f.junkMailbox(t, f.original, "Spam")
	filed := f.storeMessage(t, f.original, junk, 10, "<spam@partner.test>", "info@firma.test")
	fetched := f.storeMessage(t, f.aggregate, f.aggregateInbox, 10, "<spam@partner.test>", "info@firma.test")

	stats, err := f.db.RefreshDuplicateCopiesForUser(f.ctx, f.userID, "")
	if err != nil {
		t.Fatal(err)
	}
	if pointer := f.duplicatePointer(t, fetched.ID); pointer != 0 {
		t.Fatalf("copy points at a junk-filed original %d, want it left visible", pointer)
	}
	if pointer := f.duplicatePointer(t, filed.ID); pointer != 0 {
		t.Fatalf("the Spam copy points at %d, want it left in Spam where it was filed", pointer)
	}
	if got := stats.Outcomes[DuplicateGroupNothingToHide]; got != 1 {
		t.Fatalf("nothing-to-hide groups=%d, want 1 (outcomes=%v)", got, stats.Outcomes)
	}
	messages, err := f.db.ListMessagesForUser(f.ctx, f.userID, 50, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) == 0 {
		t.Fatal("both copies disappeared from the list, leaving the message only in Spam")
	}
}

// Releasing a group the rule no longer resolves runs through the same map the
// resolver returns, and the resolver returns nil for every group it declines.
// Sync calls this path for each stored message, so a nil map here panics a sync.
func TestIncrementalRefreshReleasesAGroupItNoLongerResolves(t *testing.T) {
	f := newDuplicateFixture(t)
	const header = "<alias@partner.test>"
	f.storeMessage(t, f.original, f.originalInbox, 11, header, "info@firma.test")
	fetched := f.storeMessage(t, f.aggregate, f.aggregateInbox, 11, header, "info@firma.test")
	if err := f.db.RefreshDuplicateCopiesForMessageID(f.ctx, f.userID, header); err != nil {
		t.Fatal(err)
	}
	if pointer := f.duplicatePointer(t, fetched.ID); pointer == 0 {
		t.Fatal("expected the fetched copy to start out hidden")
	}
	// Both folders leave All Mail, so no copy is left that could stand in as the
	// original and the group stops resolving.
	for _, mailboxID := range []int64{f.originalInbox, f.aggregateInbox} {
		if err := f.db.UpdateMailboxSettings(f.ctx, f.userID, mailboxID, MailboxSettings{
			SyncMode: "auto", Role: "inbox", ShowInSidebar: true, ShowInAllMail: false, IncludeInSearch: true,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.db.RefreshDuplicateCopiesForMessageID(f.ctx, f.userID, header); err != nil {
		t.Fatal(err)
	}
	if pointer := f.duplicatePointer(t, fetched.ID); pointer != 0 {
		t.Fatalf("copy still points at %d after the group stopped resolving", pointer)
	}
}

// Which copy stays is decided from folder placement whenever no recipient can
// decide it, and placement is the one thing the reader keeps changing. Archiving
// the copy they just read must not hand the role to the other account's Inbox
// row: read state lives on the row, so the message would come back unread in an
// account they were not even looking at.
func TestArchivingTheVisibleCopyDoesNotMoveTheOriginalToAnotherAccount(t *testing.T) {
	f := newDuplicateFixture(t)
	const header = "<listed@partner.test>"
	visible := f.storeMessage(t, f.original, f.originalInbox, 40, header, "members@list.test")
	other := f.storeMessage(t, f.aggregate, f.aggregateInbox, 40, header, "members@list.test")
	if _, err := f.db.RefreshDuplicateCopiesForUser(f.ctx, f.userID, ""); err != nil {
		t.Fatal(err)
	}
	if pointer := f.duplicatePointer(t, other.ID); pointer != visible.ID {
		t.Fatalf("second copy points at %d, want it hidden behind %d to start with", pointer, visible.ID)
	}

	archive, err := f.db.GetOrCreateMailbox(f.ctx, f.userID, f.original, "Archive")
	if err != nil {
		t.Fatal(err)
	}
	db, err := f.db.dataDB(f.ctx, f.userID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(f.ctx, `UPDATE messages SET mailbox_id = ? WHERE user_id = ? AND id = ?`,
		archive.ID, f.userID, visible.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.db.RefreshDuplicateCopiesForMessageID(f.ctx, f.userID, header); err != nil {
		t.Fatal(err)
	}
	if pointer := f.duplicatePointer(t, visible.ID); pointer != 0 {
		t.Fatalf("the archived copy now points at %d, want it kept as the original", pointer)
	}
	if pointer := f.duplicatePointer(t, other.ID); pointer != visible.ID {
		t.Fatalf("second copy points at %d after the archive, want it still hidden behind %d", pointer, visible.ID)
	}
}

// A folder the user took out of All Mail is a folder they still open. Hiding a
// copy filed there would take it out of that folder's own list too, so the one
// place they would go looking for it is where it stops being.
func TestDuplicateScanNeverHidesACopyOutOfAFolderTakenOutOfAllMail(t *testing.T) {
	f := newDuplicateFixture(t)
	kept, err := f.db.GetOrCreateMailbox(f.ctx, f.userID, f.aggregate, "Newsletters")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.UpdateMailboxSettings(f.ctx, f.userID, kept.ID, MailboxSettings{
		SyncMode: "auto", ShowInSidebar: true, ShowInAllMail: false, IncludeInSearch: true,
	}); err != nil {
		t.Fatal(err)
	}
	f.storeMessage(t, f.original, f.originalInbox, 41, "<filed-away@partner.test>", "info@firma.test")
	filed := f.storeMessage(t, f.aggregate, kept.ID, 41, "<filed-away@partner.test>", "info@firma.test")

	if _, err := f.db.RefreshDuplicateCopiesForUser(f.ctx, f.userID, ""); err != nil {
		t.Fatal(err)
	}
	if pointer := f.duplicatePointer(t, filed.ID); pointer != 0 {
		t.Fatalf("the filed copy points at %d, want it left in the folder it was filed in", pointer)
	}
}

// The cleanup moves copies into the Trash of the account holding them, which is
// only a safe thing to do to mail that account fetched. A copy of a message that
// named the account itself is a delivery that server made in its own right -
// hidden so the reader sees one message, not offered up for deletion.
func TestTrashCleanupSkipsACopyItsOwnAccountWasAddressedIn(t *testing.T) {
	f := newDuplicateFixture(t)
	f.storeMessage(t, f.original, f.originalInbox, 42, "<fetched@partner.test>", "info@firma.test")
	fetched := f.storeMessage(t, f.aggregate, f.aggregateInbox, 42, "<fetched@partner.test>", "info@firma.test")
	both := "info@firma.test, owner@gmail.test"
	f.storeMessage(t, f.original, f.originalInbox, 43, "<delivered@partner.test>", both)
	delivered := f.storeMessage(t, f.aggregate, f.aggregateInbox, 43, "<delivered@partner.test>", both)

	if _, err := f.db.RefreshDuplicateCopiesForUser(f.ctx, f.userID, ""); err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{fetched.ID, delivered.ID} {
		if pointer := f.duplicatePointer(t, id); pointer == 0 {
			t.Fatalf("copy %d is not hidden, so this test is not testing what it means to", id)
		}
	}
	copies, err := f.db.ListHiddenDuplicateCopiesForUser(f.ctx, f.userID, 50)
	if err != nil {
		t.Fatal(err)
	}
	selected := map[int64]bool{}
	for _, item := range copies {
		selected[item.ID] = true
	}
	if !selected[fetched.ID] {
		t.Fatalf("cleanup selection %v leaves out the fetched copy %d", selected, fetched.ID)
	}
	if selected[delivered.ID] {
		t.Fatalf("cleanup selection %v takes copy %d, which its own account was addressed in", selected, delivered.ID)
	}
}

// A tenant with more duplicate groups than one pass covers has to finish by
// repeating the call. The cursor is what makes the second pass see new groups.
func TestDuplicateScanResumesFromItsCursor(t *testing.T) {
	f := newDuplicateFixture(t)
	for i := 0; i < 3; i++ {
		header := "<paged" + string(rune('a'+i)) + "@partner.test>"
		f.storeMessage(t, f.original, f.originalInbox, uint32(20+i), header, "info@firma.test")
		f.storeMessage(t, f.aggregate, f.aggregateInbox, uint32(20+i), header, "info@firma.test")
	}
	first, err := f.db.RefreshDuplicateCopiesForUser(f.ctx, f.userID, "")
	if err != nil {
		t.Fatal(err)
	}
	if first.Groups != 3 || first.Hidden != 3 {
		t.Fatalf("first pass = %+v, want all three groups hidden", first)
	}
	if first.Truncated || first.NextHeader != "" {
		t.Fatalf("first pass reports more work as %+v, want it finished", first)
	}
	// Resuming past the first two groups must skip them instead of restarting.
	resumed, err := f.db.RefreshDuplicateCopiesForUser(f.ctx, f.userID, "<pagedb@partner.test>")
	if err != nil {
		t.Fatal(err)
	}
	if resumed.Groups != 1 {
		t.Fatalf("resumed pass saw %d groups, want only the one past the cursor", resumed.Groups)
	}
}

// Role and All Mail visibility decide whether a row may stand in as the
// original. Taking the addressed account's folder out of All Mail has to release
// the copies hiding behind it, not wait for someone to run a full rescan.
func TestChangingAFolderOutOfAllMailReleasesItsHiddenCopies(t *testing.T) {
	f := newDuplicateFixture(t)
	const header = "<visibility@partner.test>"
	f.storeMessage(t, f.original, f.originalInbox, 30, header, "info@firma.test")
	fetched := f.storeMessage(t, f.aggregate, f.aggregateInbox, 30, header, "info@firma.test")
	if _, err := f.db.RefreshDuplicateCopiesForUser(f.ctx, f.userID, ""); err != nil {
		t.Fatal(err)
	}
	if pointer := f.duplicatePointer(t, fetched.ID); pointer == 0 {
		t.Fatal("expected the fetched copy to start out hidden")
	}
	if err := f.db.UpdateMailboxSettings(f.ctx, f.userID, f.originalInbox, MailboxSettings{
		SyncMode: "auto", Role: "inbox", ShowInSidebar: true, ShowInAllMail: false, IncludeInSearch: true,
	}); err != nil {
		t.Fatal(err)
	}
	if pointer := f.duplicatePointer(t, fetched.ID); pointer != 0 {
		t.Fatalf("copy still points at %d after its original left All Mail", pointer)
	}
	// The row that left All Mail cannot stand in as the original any more, and it
	// is not hidden behind the other copy either: the user took that folder out
	// of the lists, which is not the same as asking for its mail to disappear.
	messages, err := f.db.ListMessagesForUser(f.ctx, f.userID, 50, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 2 {
		t.Fatalf("list returned %d messages, want both copies visible again", len(messages))
	}
}

// The pointer write happens after the copies were read, so the row picked as the
// original can be gone by then. Its delete trigger has already run, so nothing
// would clear a pointer written afterwards: the write has to refuse instead.
func TestPointerWriteRefusesAnOriginalThatVanishedMidScan(t *testing.T) {
	f := newDuplicateFixture(t)
	const header = "<vanished@partner.test>"
	original := f.storeMessage(t, f.original, f.originalInbox, 31, header, "info@firma.test")
	fetched := f.storeMessage(t, f.aggregate, f.aggregateInbox, 31, header, "info@firma.test")
	db, err := f.db.dataDB(f.ctx, f.userID)
	if err != nil {
		t.Fatal(err)
	}
	copies := []DuplicateCopy{
		{ID: original.ID, AccountID: f.original, MailboxID: f.originalInbox, MailboxRole: "inbox", ShowInAllMail: true, MessageID: header},
		{ID: fetched.ID, AccountID: f.aggregate, MailboxID: f.aggregateInbox, MailboxRole: "inbox", ShowInAllMail: true, MessageID: header},
	}
	// The delete stands in for the race: it lands between the read above and the
	// write below, exactly where the trigger can no longer help.
	if err := f.db.DeleteMessageForUser(f.ctx, f.userID, original.ID); err != nil {
		t.Fatal(err)
	}
	hidden, _, err := f.db.applyDuplicatePointers(f.ctx, db, f.userID, copies, map[int64]int64{fetched.ID: original.ID})
	if err != nil {
		t.Fatal(err)
	}
	if hidden != 0 {
		t.Fatalf("reported %d hidden copies, want the write refused", hidden)
	}
	if pointer := f.duplicatePointer(t, fetched.ID); pointer != 0 {
		t.Fatalf("copy points at deleted message %d, which nothing would ever clear", pointer)
	}
}

// A Junk folder can be switched back into All Mail by hand, but the whole-account
// lists drop it either way. A copy hidden behind such a row would sit behind a
// row those lists never show, so the message would be in no list at all.
func TestDuplicateScanNeverHidesBehindJunkThatOptedIntoAllMail(t *testing.T) {
	f := newDuplicateFixture(t)
	junk, err := f.db.GetOrCreateMailbox(f.ctx, f.userID, f.original, "Spam")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.UpdateMailboxSettings(f.ctx, f.userID, junk.ID, MailboxSettings{
		SyncMode: "auto", Role: "junk", ShowInSidebar: true, ShowInAllMail: true, IncludeInSearch: true,
	}); err != nil {
		t.Fatal(err)
	}
	filed := f.storeMessage(t, f.original, junk.ID, 11, "<optin@partner.test>", "info@firma.test")
	fetched := f.storeMessage(t, f.aggregate, f.aggregateInbox, 11, "<optin@partner.test>", "info@firma.test")

	if _, err := f.db.RefreshDuplicateCopiesForUser(f.ctx, f.userID, ""); err != nil {
		t.Fatal(err)
	}
	if pointer := f.duplicatePointer(t, fetched.ID); pointer != 0 {
		t.Fatalf("copy points at a junk-filed original %d, want it left visible", pointer)
	}
	if pointer := f.duplicatePointer(t, filed.ID); pointer != 0 {
		t.Fatalf("the Spam copy points at %d, want it left in Spam where it was filed", pointer)
	}
	// The whole-account list is where the failure would show: the junk row is
	// excluded by role, so hiding this row too would leave nothing.
	all, err := f.db.ListLatestThreadMessagesForUser(f.ctx, f.userID, 50, 0, ThreadListNewestFirst)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, message := range all {
		if message.ID == fetched.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("all mail = %v, want the visible copy %d: hiding it leaves the message only in Spam",
			messageIDsOf(all), fetched.ID)
	}
}
