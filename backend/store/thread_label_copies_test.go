// File overview: Tests for collapsing a mirrored label view's second copy out of
// a rendered thread - which copy is drawn, which physical rows the drawn one
// still stands for, and the placements that are left exactly as they are.

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

// collapseThread runs the whole path a rendered thread takes: load every row,
// then decide what to draw.
func (f *labelViewFixture) collapseThread(t *testing.T, msg MessageRecord) ThreadCopyCollapse {
	t.Helper()
	thread, err := f.db.ListThreadMessagesForUser(f.ctx, f.userID, msg)
	if err != nil {
		t.Fatal(err)
	}
	collapse, err := f.db.CollapseLabelViewCopies(f.ctx, f.userID, thread)
	if err != nil {
		t.Fatal(err)
	}
	return collapse
}

func threadIDs(messages []MessageRecord) []int64 {
	out := make([]int64, 0, len(messages))
	for _, msg := range messages {
		out = append(out, msg.ID)
	}
	return out
}

func sameIDs(got, want []int64) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// The thread query keeps returning every physical row. Read state, star state
// and conversation-level moves are all decided from it, so collapsing there
// would take rows away from callers that have to reach them.
func TestThreadQueryStillReturnsEveryCopy(t *testing.T) {
	f := newLabelViewFixture(t)
	sent := time.Now().UTC().Truncate(time.Second)
	inbox := f.storeCopy(t, f.inbox, "<milan@crowdfarming.test>", "msgid:milan", sent)
	view := f.storeCopy(t, f.allMail, "<milan@crowdfarming.test>", "msgid:milan", sent)

	thread, err := f.db.ListThreadMessagesForUser(f.ctx, f.userID, inbox)
	if err != nil {
		t.Fatal(err)
	}
	if got := threadIDs(thread); !sameIDs(got, []int64{inbox.ID, view.ID}) {
		t.Fatalf("thread = %v, want both physical rows %v", got, []int64{inbox.ID, view.ID})
	}
}

// The copy All Mail holds of an Inbox message is the same delivery listed a
// second time, so the thread draws one message rather than two - and the drawn
// row says which physical rows it stands for.
func TestCollapseDrawsOneRowForAnInboxMessageAllMailRepeats(t *testing.T) {
	f := newLabelViewFixture(t)
	sent := time.Now().UTC().Truncate(time.Second)
	inbox := f.storeCopy(t, f.inbox, "<milan@crowdfarming.test>", "msgid:milan", sent)
	view := f.storeCopy(t, f.allMail, "<milan@crowdfarming.test>", "msgid:milan", sent)

	collapse := f.collapseThread(t, inbox)
	if got := threadIDs(collapse.Messages); !sameIDs(got, []int64{inbox.ID}) {
		t.Fatalf("drawn = %v, want only the Inbox copy %d", got, inbox.ID)
	}
	if got := collapse.CopyIDs[inbox.ID]; !sameIDs(got, []int64{inbox.ID, view.ID}) {
		t.Fatalf("copy ids = %v, want both rows %v", got, []int64{inbox.ID, view.ID})
	}
	if collapse.StandIn[view.ID] != inbox.ID {
		t.Fatalf("stand-in for the hidden copy = %d, want the Inbox copy %d", collapse.StandIn[view.ID], inbox.ID)
	}
}

// Real folders are places the reader filed the message and are never decided
// away, not even when a view of the same message is in the thread beside them.
func TestCollapseKeepsEveryRealFolderCopy(t *testing.T) {
	f := newLabelViewFixture(t)
	label := f.mailbox(t, "Rechnungen", "")
	sent := time.Now().UTC().Truncate(time.Second)
	inbox := f.storeCopy(t, f.inbox, "<invoice@crowdfarming.test>", "msgid:invoice", sent)
	filed := f.storeCopy(t, label, "<invoice@crowdfarming.test>", "msgid:invoice", sent)
	view := f.storeCopy(t, f.allMail, "<invoice@crowdfarming.test>", "msgid:invoice", sent)

	collapse := f.collapseThread(t, inbox)
	if got := threadIDs(collapse.Messages); !sameIDs(got, []int64{inbox.ID, filed.ID}) {
		t.Fatalf("drawn = %v, want both filed copies %v", got, []int64{inbox.ID, filed.ID})
	}
	if collapse.StandIn[view.ID] != inbox.ID {
		t.Fatalf("stand-in for the hidden view copy = %d, want %d", collapse.StandIn[view.ID], inbox.ID)
	}
}

// Two real folders with no view among them are left entirely alone.
func TestCollapseLeavesTwoRealFoldersAlone(t *testing.T) {
	f := newLabelViewFixture(t)
	label := f.mailbox(t, "Rechnungen", "")
	sent := time.Now().UTC().Truncate(time.Second)
	inbox := f.storeCopy(t, f.inbox, "<invoice@crowdfarming.test>", "msgid:invoice", sent)
	filed := f.storeCopy(t, label, "<invoice@crowdfarming.test>", "msgid:invoice", sent)

	collapse := f.collapseThread(t, inbox)
	if got := threadIDs(collapse.Messages); !sameIDs(got, []int64{inbox.ID, filed.ID}) {
		t.Fatalf("drawn = %v, want both rows %v", got, []int64{inbox.ID, filed.ID})
	}
	if len(collapse.CopyIDs) != 0 || len(collapse.StandIn) != 0 {
		t.Fatalf("collapse hid something: copies=%v standIn=%v", collapse.CopyIDs, collapse.StandIn)
	}
}

// A reader who mirrors two of Gmail's views holds the message in both and in no
// real folder at all - archived mail carries no label. One of them is drawn, so
// the message is neither repeated nor hidden.
//
// The second view is named in English on purpose. Gmail advertises \All on All
// Mail whatever the account's language is and the syncer stores that as a role,
// but \Flagged and \Important carry no role of their own and the attributes
// are not kept, so those two are recognized here only by name.
func TestCollapseDrawsOneRowWhenEveryCopyIsAView(t *testing.T) {
	f := newLabelViewFixture(t)
	starred := f.mailbox(t, "[Gmail]/Starred", "")
	sent := time.Now().UTC().Truncate(time.Second)
	inAllMail := f.storeCopy(t, f.allMail, "<archived@crowdfarming.test>", "msgid:archived", sent)
	inStarred := f.storeCopy(t, starred, "<archived@crowdfarming.test>", "msgid:archived", sent)

	collapse := f.collapseThread(t, inAllMail)
	if got := threadIDs(collapse.Messages); !sameIDs(got, []int64{inAllMail.ID}) {
		t.Fatalf("drawn = %v, want one of the two view copies", got)
	}
	if got := collapse.CopyIDs[inAllMail.ID]; !sameIDs(got, []int64{inAllMail.ID, inStarred.ID}) {
		t.Fatalf("copy ids = %v, want both view rows", got)
	}
}

// A message held only in All Mail is the only copy there is and keeps its row.
func TestCollapseKeepsAMessageHeldOnlyInAllMail(t *testing.T) {
	f := newLabelViewFixture(t)
	sent := time.Now().UTC().Truncate(time.Second)
	only := f.storeCopy(t, f.allMail, "<archived@crowdfarming.test>", "msgid:thread", sent)
	other := f.storeCopy(t, f.inbox, "<reply@crowdfarming.test>", "msgid:thread", sent.Add(time.Minute))

	collapse := f.collapseThread(t, only)
	if got := threadIDs(collapse.Messages); !sameIDs(got, []int64{only.ID, other.ID}) {
		t.Fatalf("drawn = %v, want both messages %v", got, []int64{only.ID, other.ID})
	}
}

// The drawn row carries the state of the copies behind it, the same way the
// conversation row the reader clicked was built from them: read only when every
// copy is read, starred when any of them is.
func TestCollapseMergesReadAndStarStateOntoTheDrawnRow(t *testing.T) {
	f := newLabelViewFixture(t)
	sent := time.Now().UTC().Truncate(time.Second)
	inbox := f.storeCopy(t, f.inbox, "<milan@crowdfarming.test>", "msgid:milan", sent)
	view := f.storeCopy(t, f.allMail, "<milan@crowdfarming.test>", "msgid:milan", sent)
	if err := f.db.MarkMessagesReadForUser(f.ctx, f.userID, []int64{inbox.ID}, true, false); err != nil {
		t.Fatal(err)
	}
	if err := f.db.MarkMessageStarredForUser(f.ctx, f.userID, view.ID, true, false); err != nil {
		t.Fatal(err)
	}

	collapse := f.collapseThread(t, inbox)
	if len(collapse.Messages) != 1 {
		t.Fatalf("drawn = %v, want one row", threadIDs(collapse.Messages))
	}
	drawn := collapse.Messages[0]
	if drawn.IsRead {
		t.Fatal("drawn row reads as read while the All Mail copy is still unread")
	}
	if !drawn.IsStarred {
		t.Fatal("drawn row reads as unstarred while the All Mail copy is starred")
	}
}

// Starring a drawn message has to reach the copy behind it, so the lookup the
// star handler uses names the view's row and nothing else.
func TestLabelViewCopyIDsNamesTheViewCopy(t *testing.T) {
	f := newLabelViewFixture(t)
	label := f.mailbox(t, "Rechnungen", "")
	sent := time.Now().UTC().Truncate(time.Second)
	inbox := f.storeCopy(t, f.inbox, "<invoice@crowdfarming.test>", "msgid:invoice", sent)
	f.storeCopy(t, label, "<invoice@crowdfarming.test>", "msgid:invoice", sent)
	view := f.storeCopy(t, f.allMail, "<invoice@crowdfarming.test>", "msgid:invoice", sent)

	copies, err := f.db.LabelViewCopyIDsForMessage(f.ctx, f.userID, inbox.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !sameIDs(copies, []int64{view.ID}) {
		t.Fatalf("copies = %v, want only the All Mail row %d", copies, view.ID)
	}
}

// A row without a Message-ID is not evidence of anything, and two of them are
// not evidence of each other.
func TestCollapseLeavesMessagesWithoutAMessageIDAlone(t *testing.T) {
	messages := []MessageRecord{
		{ID: 1, AccountID: 7, MailboxID: 100},
		{ID: 2, AccountID: 7, MailboxID: 200},
	}
	collapse := collapseLabelViewCopies(messages, threadCopyGroups(messages), map[int64]bool{200: true})
	if len(collapse.Messages) != 2 {
		t.Fatalf("drawn = %v, want both rows", threadIDs(collapse.Messages))
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
	collapse := collapseLabelViewCopies(messages, threadCopyGroups(messages), map[int64]bool{200: true})
	if len(collapse.Messages) != 2 {
		t.Fatalf("drawn = %v, want both accounts' rows", threadIDs(collapse.Messages))
	}
}
