package store

import (
	"context"
	"testing"
	"time"

	"rolltop/backend/mailparse"
)

// outgoingCopyFixture is one tenant with an Inbox and a Sent folder, plus a
// received message to compare against, so an assertion says which mail a list
// drew from rather than only how many rows came back.
type outgoingCopyFixture struct {
	db      *Store
	user    User
	account MailAccount
	inbox   Mailbox
	sent    Mailbox
	blob    BlobRecord
	base    time.Time
}

func newOutgoingCopyFixture(t *testing.T) outgoingCopyFixture {
	t.Helper()
	ctx := context.Background()
	db, err := openTestStore(t)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	user, account, inbox, blob := testMailbox(t, ctx, db)
	sent, err := db.GetOrCreateMailboxWithRole(ctx, user.ID, account.ID, "Sent", "sent")
	if err != nil {
		t.Fatal(err)
	}
	return outgoingCopyFixture{db: db, user: user, account: account, inbox: inbox, sent: sent, blob: blob,
		base: time.Unix(1700000000, 0)}
}

func (f outgoingCopyFixture) create(t *testing.T, mailbox Mailbox, uid uint32, header, from string) MessageRecord {
	t.Helper()
	message, err := f.db.CreateMessage(context.Background(), CreateMessage{
		UserID: f.user.ID, AccountID: f.account.ID, MailboxID: mailbox.ID, BlobID: f.blob.ID,
		MessageIDHeader: header, Subject: header, FromAddr: from,
		Category: mailparse.CategoryRelevant,
		Date:     f.base.Add(time.Duration(uid) * time.Minute), UID: uid, BlobPath: f.blob.Path,
	})
	if err != nil {
		t.Fatal(err)
	}
	return message
}

// A Gmail account that mirrors All Mail holds what arrived and what it sent in
// one folder, so the folder cannot answer "is this mail waiting on the reader".
// The message answers it instead: a copy of what this Rolltop sent stays out of
// the whole-account lists, and the folder it is in still shows it.
func TestOwnOutgoingCopyStaysOutOfTheWholeAccountLists(t *testing.T) {
	ctx := context.Background()
	f := newOutgoingCopyFixture(t)
	// The label view a Gmail reader turns on: an ordinary folder as far as the
	// mirror is concerned, holding sent mail alongside everything else.
	allMail, err := f.db.GetOrCreateMailbox(ctx, f.user.ID, f.account.ID, "[Gmail]/All Mail")
	if err != nil {
		t.Fatal(err)
	}
	if !allMail.ShowInAllMail {
		t.Fatal("a folder with no role stayed out of All Mail")
	}
	received := f.create(t, f.inbox, 1, "<received@example.test>", "ada@example.test")
	if err := f.db.RecordOutgoingMessageID(ctx, f.user.ID, f.account.ID, "<forwarded@example.test>"); err != nil {
		t.Fatal(err)
	}
	forwarded := f.create(t, allMail, 2, "<forwarded@example.test>", "mail@example.test")

	for _, tt := range []struct {
		name string
		list func() ([]MessageRecord, error)
	}{
		{"all mail", func() ([]MessageRecord, error) {
			return f.db.ListLatestThreadMessagesForUser(ctx, f.user.ID, 10, 0, ThreadListNewestFirst)
		}},
		{"inbox", func() ([]MessageRecord, error) {
			return f.db.ListUnarchivedLatestThreadMessagesForUser(ctx, f.user.ID, 10, 0, ThreadListNewestFirst)
		}},
		{"relevant", func() ([]MessageRecord, error) {
			return f.db.ListCategoryLatestThreadMessagesForUser(ctx, f.user.ID, mailparse.CategoryRelevant, 10, 0, ThreadListNewestFirst)
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			messages, err := tt.list()
			if err != nil {
				t.Fatal(err)
			}
			if len(messages) != 1 || messages[0].ID != received.ID {
				t.Fatalf("%s = %v, want only the received message %d", tt.name, messageIDsOf(messages), received.ID)
			}
		})
	}

	// A whole-view selection covers exactly what its list showed, or "delete
	// everything here" reaches mail the list kept out of sight.
	scope, err := f.db.ListUnarchivedMailScopeMessagesForUser(ctx, f.user.ID, ScopeFilter{}, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(scope) != 1 || scope[0].ID != received.ID {
		t.Fatalf("inbox scope = %+v, want only %d", scope, received.ID)
	}
	counts, err := f.db.CountMessagesByCategoryForUser(ctx, f.user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if counts[mailparse.CategoryRelevant].Total != 1 {
		t.Fatalf("relevant badge = %+v, want the outgoing copy uncounted", counts[mailparse.CategoryRelevant])
	}

	// Keeping it out of the combined lists is not hiding it: the folder the
	// server put it in still holds it, which is where a reader goes looking.
	folder, err := f.db.ListLatestThreadMessagesForMailbox(ctx, f.user.ID, allMail.ID, 10, 0, ThreadListNewestFirst)
	if err != nil {
		t.Fatal(err)
	}
	if len(folder) != 1 || folder[0].ID != forwarded.ID {
		t.Fatalf("folder view = %v, want the outgoing copy %d", messageIDsOf(folder), forwarded.ID)
	}
}

// The All Mail flag is the reader's to set, and a reader who puts Sent back into
// the combined lists is asking for exactly this mail. The Sent role is therefore
// exempt: what the ledger recognises is the copy that came back somewhere else.
func TestOwnOutgoingCopyInSentStaysVisibleWhenSentOptsIntoAllMail(t *testing.T) {
	ctx := context.Background()
	f := newOutgoingCopyFixture(t)
	if err := f.db.UpdateMailboxSettings(ctx, f.user.ID, f.sent.ID, MailboxSettings{
		SyncMode: f.sent.SyncMode, Role: "sent", Icon: f.sent.Icon,
		ShowInSidebar: true, ShowInAllMail: true, IncludeInSearch: true,
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.db.RecordOutgoingMessageID(ctx, f.user.ID, f.account.ID, "<written@example.test>"); err != nil {
		t.Fatal(err)
	}
	written := f.create(t, f.sent, 3, "<written@example.test>", "mail@example.test")

	all, err := f.db.ListLatestThreadMessagesForUser(ctx, f.user.ID, 10, 0, ThreadListNewestFirst)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || all[0].ID != written.ID {
		t.Fatalf("all mail = %v, want the Sent copy %d", messageIDsOf(all), written.ID)
	}
}

// A forward to another account of the same reader is mail that account really
// received. The ledger is keyed by the account that sent it for that reason:
// the sending account's copy is recognised, the receiving account's is not.
func TestMailDeliveredToAnotherAccountIsNotAnOutgoingCopy(t *testing.T) {
	ctx := context.Background()
	f := newOutgoingCopyFixture(t)
	second, err := f.db.UpsertMailAccount(ctx, MailAccount{
		UserID: f.user.ID, Email: "books@example.test", Host: "imap.example.test", Port: 993,
		Username: "books", EncryptedPassword: "secret", UseTLS: true, Mailbox: "INBOX",
	})
	if err != nil {
		t.Fatal(err)
	}
	secondInbox, err := f.db.GetOrCreateMailboxWithRole(ctx, f.user.ID, second.ID, "INBOX", "inbox")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.RecordOutgoingMessageID(ctx, f.user.ID, f.account.ID, "<forwarded@example.test>"); err != nil {
		t.Fatal(err)
	}
	delivered, err := f.db.CreateMessage(ctx, CreateMessage{
		UserID: f.user.ID, AccountID: second.ID, MailboxID: secondInbox.ID, BlobID: f.blob.ID,
		MessageIDHeader: "<forwarded@example.test>", Subject: "Fwd: Receipt", FromAddr: "mail@example.test",
		Category: mailparse.CategoryRelevant, Date: f.base, UID: 9, BlobPath: f.blob.Path,
	})
	if err != nil {
		t.Fatal(err)
	}

	all, err := f.db.ListLatestThreadMessagesForUser(ctx, f.user.ID, 10, 0, ThreadListNewestFirst)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || all[0].ID != delivered.ID {
		t.Fatalf("all mail = %v, want the delivered message %d", messageIDsOf(all), delivered.ID)
	}
}
