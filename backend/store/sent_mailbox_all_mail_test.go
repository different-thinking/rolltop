package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"

	"rolltop/backend/mailparse"
)

// sentListFixture is one tenant with an Inbox, a Sent folder, and a received
// message to compare against, so a list assertion says which of the two folders
// a view drew from rather than only how many rows came back.
type sentListFixture struct {
	db      *Store
	user    User
	account MailAccount
	inbox   Mailbox
	sent    Mailbox
	blob    BlobRecord
	base    time.Time
}

func newSentListFixture(t *testing.T) sentListFixture {
	t.Helper()
	ctx := context.Background()
	db, err := Open(filepath.Join(t.TempDir(), "rolltop.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	user, account, inbox, blob := testMailbox(t, ctx, db)
	sent, err := db.GetOrCreateMailboxWithRole(ctx, user.ID, account.ID, "Sent", "sent")
	if err != nil {
		t.Fatal(err)
	}
	return sentListFixture{db: db, user: user, account: account, inbox: inbox, sent: sent, blob: blob,
		base: time.Unix(1700000000, 0)}
}

func (f sentListFixture) create(t *testing.T, mailbox Mailbox, uid uint32, header, from string) MessageRecord {
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

// Mail the user wrote is not mail waiting on them. A Sent folder that opted into
// All Mail put every reply back in front of its author three times over - in All
// Mail, in Inbox, and in whichever category the reply's own headers earned it,
// which for a message to a person is Relevant.
func TestSentFolderStaysOutOfTheWholeAccountLists(t *testing.T) {
	ctx := context.Background()
	f := newSentListFixture(t)
	if f.sent.ShowInAllMail {
		t.Fatal("a discovered Sent folder opted into All Mail")
	}
	received := f.create(t, f.inbox, 1, "<received@example.test>", "ada@example.test")
	written := f.create(t, f.sent, 2, "<written@example.test>", "mail@example.test")

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

	// A whole-view selection has to cover exactly what its list showed, or
	// "delete everything here" would reach mail the list kept out of sight.
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
		t.Fatalf("relevant badge = %+v, want only the received message counted", counts[mailparse.CategoryRelevant])
	}

	// Keeping Sent out of the combined lists is not the same as hiding it: the
	// Sent view and the folder itself still hold everything the user wrote.
	role, err := f.db.ListRoleLatestThreadMessagesForUser(ctx, f.user.ID, "sent", 10, 0, ThreadListNewestFirst)
	if err != nil {
		t.Fatal(err)
	}
	if len(role) != 1 || role[0].ID != written.ID {
		t.Fatalf("sent view = %v, want %d", messageIDsOf(role), written.ID)
	}
	folder, err := f.db.ListLatestThreadMessagesForMailbox(ctx, f.user.ID, f.sent.ID, 10, 0, ThreadListNewestFirst)
	if err != nil {
		t.Fatal(err)
	}
	if len(folder) != 1 || folder[0].ID != written.ID {
		t.Fatalf("sent folder = %v, want %d", messageIDsOf(folder), written.ID)
	}
}

// The All Mail flag is the reader's to set. Unlike Junk, which the whole-account
// lists drop whatever it says, Sent is only defaulted out of them: a reader who
// wants their own mail back in the combined lists can have it, and must not lose
// that at the next restart to a migration or a startup seed running twice.
func TestSentFolderReturnedToAllMailSurvivesReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "rolltop.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	user, account, _, blob := testMailbox(t, ctx, db)
	sent, err := db.GetOrCreateMailboxWithRole(ctx, user.ID, account.ID, "Sent", "sent")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateMailboxSettings(ctx, user.ID, sent.ID, MailboxSettings{
		SyncMode: sent.SyncMode, Role: "sent", Icon: sent.Icon,
		ShowInSidebar: true, ShowInAllMail: true, IncludeInSearch: true,
	}); err != nil {
		t.Fatal(err)
	}
	written, err := db.CreateMessage(ctx, CreateMessage{
		UserID: user.ID, AccountID: account.ID, MailboxID: sent.ID, BlobID: blob.ID,
		MessageIDHeader: "<restored@example.test>", Subject: "Restored",
		FromAddr: "mail@example.test", Category: mailparse.CategoryRelevant,
		Date: time.Unix(1700000000, 0), UID: 5, BlobPath: blob.Path,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	mailbox, err := reopened.GetMailboxForUser(ctx, user.ID, sent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !mailbox.ShowInAllMail {
		t.Fatal("reopening the store took the Sent folder back out of All Mail")
	}
	all, err := reopened.ListLatestThreadMessagesForUser(ctx, user.ID, 10, 0, ThreadListNewestFirst)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || all[0].ID != written.ID {
		t.Fatalf("all mail = %v, want the restored Sent message %d", messageIDsOf(all), written.ID)
	}
}

// Accounts that were already mirrored carry a Sent folder created under the old
// default, so the backfill is what decides their behavior. It runs over the
// role rather than over folder names, because the role is what every list reads.
func TestSentAllMailMigrationBackfillsExistingFolders(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	ctx := context.Background()
	for _, statement := range []string{
		`CREATE TABLE mailboxes (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			user_id INTEGER NOT NULL,
			name TEXT NOT NULL,
			role TEXT NOT NULL DEFAULT '',
			show_in_all_mail INTEGER NOT NULL DEFAULT 1,
			updated_at INTEGER NOT NULL DEFAULT 0
		)`,
		`INSERT INTO mailboxes (id, user_id, name, role, show_in_all_mail, updated_at) VALUES
			(1, 1, '[Gmail]/Sent Mail', 'sent', 1, 10),
			(2, 1, 'INBOX', 'inbox', 1, 11),
			(3, 2, 'Sent Items', 'sent', 1, 12),
			(4, 2, 'Projects', '', 1, 13),
			(5, 2, 'Trash', 'trash', 0, 14)`,
	} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	for _, statement := range userSentMailboxAllMailMigrationSet().Statements {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatalf("apply %q: %v", statement, err)
		}
	}

	for _, tt := range []struct {
		id   int64
		want bool
	}{{1, false}, {2, true}, {3, false}, {4, true}, {5, false}} {
		var showInAllMail bool
		if err := db.QueryRowContext(ctx, `SELECT show_in_all_mail FROM mailboxes WHERE id = ?`, tt.id).Scan(&showInAllMail); err != nil {
			t.Fatal(err)
		}
		if showInAllMail != tt.want {
			t.Fatalf("mailbox %d show_in_all_mail = %t, want %t", tt.id, showInAllMail, tt.want)
		}
	}
	// The flag moved, so the row's own timestamp has to say so: the folder list
	// the browser caches is refreshed from it.
	var updated int64
	if err := db.QueryRowContext(ctx, `SELECT updated_at FROM mailboxes WHERE id = 1`).Scan(&updated); err != nil {
		t.Fatal(err)
	}
	if updated <= 10 {
		t.Fatalf("backfilled updated_at = %d, want the row restamped", updated)
	}
}
