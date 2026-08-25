// File overview: Tests for collapsing a mirrored label view's second copy out of
// a rendered thread - which copy the thread keeps, which the reader gets when
// they opened the view's copy themselves, and the placements that are left
// exactly as they are.

package store

import (
	"context"
	"fmt"
	"testing"
	"time"
)

type labelViewFixture struct {
	db      *Store
	ctx     context.Context
	userID  int64
	account int64
	inbox   int64
	allMail int64
	uid     uint32
}

func newLabelViewFixture(t *testing.T) *labelViewFixture {
	t.Helper()
	ctx := context.Background()
	db := mustOpenTestStore(t)
	user, err := db.CreateUser(ctx, "owner@example.test", "Owner", "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	account, err := db.CreateMailAccount(ctx, MailAccount{
		UserID: user.ID, Email: "owner@gmail.test", Host: "imap.gmail.test", Port: 993,
		Username: "owner@gmail.test", EncryptedPassword: "secret", UseTLS: true, Mailbox: "*",
	})
	if err != nil {
		t.Fatal(err)
	}
	f := &labelViewFixture{db: db, ctx: ctx, userID: user.ID, account: account.ID}
	f.inbox = f.mailbox(t, "INBOX", "inbox")
	// Gmail advertises \All on this folder whatever the account's language is,
	// which is what the syncer turns into the role.
	f.allMail = f.mailbox(t, "[Gmail]/Alle Nachrichten", "all")
	return f
}

func (f *labelViewFixture) mailbox(t *testing.T, name, role string) int64 {
	t.Helper()
	mailbox, err := f.db.GetOrCreateMailbox(f.ctx, f.userID, f.account, name)
	if err != nil {
		t.Fatal(err)
	}
	if role == "" {
		return mailbox.ID
	}
	if err := f.db.UpdateMailboxSettings(f.ctx, f.userID, mailbox.ID, MailboxSettings{
		SyncMode: "auto", Role: role, ShowInSidebar: true, ShowInAllMail: true, IncludeInSearch: true,
	}); err != nil {
		t.Fatal(err)
	}
	return mailbox.ID
}

// storeCopy files one copy of a message in a folder. Two calls with the same
// header are the two rows one Gmail account holds for a single delivery.
func (f *labelViewFixture) storeCopy(t *testing.T, mailboxID int64, header, threadKey string, sent time.Time) MessageRecord {
	t.Helper()
	f.uid++
	uid := f.uid
	blob, err := f.db.CreateBlob(f.ctx, BlobRecord{
		UserID: f.userID, Kind: "message",
		Path:   fmt.Sprintf("blobs/%d", uid),
		SHA256: fmt.Sprintf("sha-%d", uid),
		Size:   10,
	})
	if err != nil {
		t.Fatal(err)
	}
	msg, err := f.db.CreateMessage(f.ctx, CreateMessage{
		UserID: f.userID, AccountID: f.account, MailboxID: mailboxID, BlobID: blob.ID,
		MessageIDHeader: header, ThreadKey: threadKey,
		Subject:  "Deine Adoption",
		FromAddr: "hallo@crowdfarming.test", ToAddr: "owner@gmail.test",
		Date: sent, InternalDate: sent, UID: uid, Size: 10, BlobPath: blob.Path,
	})
	if err != nil {
		t.Fatal(err)
	}
	return msg
}

func threadIDs(messages []MessageRecord) []int64 {
	out := make([]int64, 0, len(messages))
	for _, msg := range messages {
		out = append(out, msg.ID)
	}
	return out
}

// The reader opened the copy in the folder the mail was delivered to. The copy
// All Mail holds of it is the same delivery listed a second time, so the thread
// renders one message rather than two.
func TestThreadDropsTheAllMailCopyOfAnOpenedInboxMessage(t *testing.T) {
	f := newLabelViewFixture(t)
	sent := time.Now().UTC().Truncate(time.Second)
	inbox := f.storeCopy(t, f.inbox, "<milan@crowdfarming.test>", "msgid:milan", sent)
	f.storeCopy(t, f.allMail, "<milan@crowdfarming.test>", "msgid:milan", sent)

	thread, err := f.db.ListThreadMessagesForUser(f.ctx, f.userID, inbox)
	if err != nil {
		t.Fatal(err)
	}
	if len(thread) != 1 || thread[0].ID != inbox.ID {
		t.Fatalf("thread = %v, want only the Inbox copy %d", threadIDs(thread), inbox.ID)
	}
}

// The lists reach the All Mail copy on their own - it is mirrored after the
// Inbox copy and therefore carries the higher id, which is what breaks the tie
// when a conversation picks the row it prints. Opening it has to render a thread
// that contains it, so here the All Mail copy is the one that stays.
func TestThreadKeepsTheAllMailCopyTheReaderOpened(t *testing.T) {
	f := newLabelViewFixture(t)
	sent := time.Now().UTC().Truncate(time.Second)
	f.storeCopy(t, f.inbox, "<milan@crowdfarming.test>", "msgid:milan", sent)
	view := f.storeCopy(t, f.allMail, "<milan@crowdfarming.test>", "msgid:milan", sent)

	thread, err := f.db.ListThreadMessagesForUser(f.ctx, f.userID, view)
	if err != nil {
		t.Fatal(err)
	}
	if len(thread) != 1 || thread[0].ID != view.ID {
		t.Fatalf("thread = %v, want only the opened copy %d", threadIDs(thread), view.ID)
	}
}

// Every message of a real conversation is collapsed, not just the one that was
// opened.
func TestThreadCollapsesEveryMessageItHoldsTwice(t *testing.T) {
	f := newLabelViewFixture(t)
	first := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	second := first.Add(30 * time.Minute)
	opened := f.storeCopy(t, f.inbox, "<first@crowdfarming.test>", "msgid:first", first)
	f.storeCopy(t, f.allMail, "<first@crowdfarming.test>", "msgid:first", first)
	reply := f.storeCopy(t, f.inbox, "<second@crowdfarming.test>", "msgid:first", second)
	f.storeCopy(t, f.allMail, "<second@crowdfarming.test>", "msgid:first", second)

	thread, err := f.db.ListThreadMessagesForUser(f.ctx, f.userID, opened)
	if err != nil {
		t.Fatal(err)
	}
	want := []int64{opened.ID, reply.ID}
	if got := threadIDs(thread); len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("thread = %v, want the two Inbox copies %v", got, want)
	}
}

// A message the account holds only in All Mail - archived mail, which in Gmail
// is exactly mail that carries no other label - is still the only copy there is.
func TestThreadKeepsAMessageHeldOnlyInAllMail(t *testing.T) {
	f := newLabelViewFixture(t)
	sent := time.Now().UTC().Truncate(time.Second)
	only := f.storeCopy(t, f.allMail, "<archived@crowdfarming.test>", "msgid:archived", sent)
	f.storeCopy(t, f.inbox, "<other@crowdfarming.test>", "msgid:archived", sent.Add(time.Minute))

	thread, err := f.db.ListThreadMessagesForUser(f.ctx, f.userID, only)
	if err != nil {
		t.Fatal(err)
	}
	if len(thread) != 2 {
		t.Fatalf("thread = %v, want both messages", threadIDs(thread))
	}
}

// Two real folders of one account are two places the user filed the message,
// and neither is a view over the other. Collapsing them would take away a copy
// they can see and act on, so the thread leaves both exactly as they are.
func TestThreadKeepsCopiesTwoRealFoldersHold(t *testing.T) {
	f := newLabelViewFixture(t)
	label := f.mailbox(t, "Rechnungen", "")
	sent := time.Now().UTC().Truncate(time.Second)
	inbox := f.storeCopy(t, f.inbox, "<invoice@crowdfarming.test>", "msgid:invoice", sent)
	filed := f.storeCopy(t, label, "<invoice@crowdfarming.test>", "msgid:invoice", sent)

	thread, err := f.db.ListThreadMessagesForUser(f.ctx, f.userID, inbox)
	if err != nil {
		t.Fatal(err)
	}
	want := []int64{inbox.ID, filed.ID}
	if got := threadIDs(thread); len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("thread = %v, want both filed copies %v", got, want)
	}
}

// A row without a Message-ID is not evidence of anything, and two of them are
// not evidence of each other.
func TestCollapseLeavesMessagesWithoutAMessageIDAlone(t *testing.T) {
	messages := []MessageRecord{
		{ID: 1, AccountID: 7, MailboxID: 100},
		{ID: 2, AccountID: 7, MailboxID: 200},
	}
	labelView := map[int64]bool{200: true}
	if got := collapseLabelViewCopies(messages, labelView, 1); len(got) != 2 {
		t.Fatalf("collapsed to %v, want both rows kept", threadIDs(got))
	}
}

// Copies two accounts hold are two deliveries, which cross-account detection
// decides about. Collapsing them here would pre-empt that with a rule that has
// not looked at who the mail was addressed to.
func TestCollapseLeavesCopiesOfTwoAccountsAlone(t *testing.T) {
	messages := []MessageRecord{
		{ID: 1, AccountID: 7, MailboxID: 100, MessageIDHeader: "<x@test>"},
		{ID: 2, AccountID: 8, MailboxID: 200, MessageIDHeader: "<x@test>"},
	}
	labelView := map[int64]bool{200: true}
	if got := collapseLabelViewCopies(messages, labelView, 1); len(got) != 2 {
		t.Fatalf("collapsed to %v, want both accounts' rows kept", threadIDs(got))
	}
}
