package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"rolltop/backend/store"
)

// rowDateFixture is one tenant with an Inbox and a Sent folder, and a helper
// that files a message into either under a caller-chosen thread key.
type rowDateFixture struct {
	db      *store.Store
	user    store.User
	account store.MailAccount
	inbox   store.Mailbox
	sent    store.Mailbox
	server  *Server
	base    time.Time
	seq     int
}

func newRowDateFixture(t *testing.T) *rowDateFixture {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(filepath.Join(t.TempDir(), "rolltop.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	user, err := db.CreateUser(ctx, "rows@example.test", "Rows", "hash", false)
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
	inbox, err := db.GetOrCreateMailbox(ctx, user.ID, account.ID, "INBOX")
	if err != nil {
		t.Fatal(err)
	}
	sent, err := db.GetOrCreateMailboxWithRole(ctx, user.ID, account.ID, "Sent", "sent")
	if err != nil {
		t.Fatal(err)
	}
	return &rowDateFixture{
		db: db, user: user, account: account, inbox: inbox, sent: sent,
		server: &Server{store: db, mailListCache: newMailListCache()},
		base:   time.Unix(1700000000, 0).UTC(),
	}
}

func (f *rowDateFixture) file(t *testing.T, mailbox store.Mailbox, subject, from, threadKey string, offset time.Duration) store.MessageRecord {
	t.Helper()
	ctx := context.Background()
	f.seq++
	blob, err := f.db.CreateBlob(ctx, store.BlobRecord{
		UserID: f.user.ID, Kind: "message", Path: fmt.Sprintf("users/%d/row-%d.eml", f.user.ID, f.seq),
		SHA256: fmt.Sprintf("row-%d", f.seq), Size: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	message, err := f.db.CreateMessage(ctx, store.CreateMessage{
		UserID: f.user.ID, AccountID: f.account.ID, MailboxID: mailbox.ID, BlobID: blob.ID,
		MessageIDHeader: fmt.Sprintf("<row-%d@example.test>", f.seq),
		ThreadKey:       threadKey, Subject: subject, FromAddr: from,
		Category:     "relevant",
		Date:         f.base.Add(offset),
		InternalDate: f.base.Add(offset),
		UID:          uint32(f.seq), BlobPath: blob.Path,
	})
	if err != nil {
		t.Fatal(err)
	}
	return message
}

func (f *rowDateFixture) page(t *testing.T, target string) []apiConversation {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req = req.WithContext(context.WithValue(req.Context(), userContextKey, currentUser{User: f.user}))
	rec := httptest.NewRecorder()
	f.server.apiMail(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s status = %d body = %s", target, rec.Code, rec.Body.String())
	}
	var payload mailSortResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	return payload.Conversations
}

// A row is placed by the newest message the list itself holds, so that is the
// message it has to show. Threads are hydrated in full, which means an answered
// conversation carries a Sent reply the list excludes; taking the reply as the
// row would print today's date on a row sitting months down the page, and the
// date section headings group on exactly that date.
func TestListRowsShowTheMessageTheyAreSortedBy(t *testing.T) {
	f := newRowDateFixture(t)
	answered := f.file(t, f.inbox, "Old question", "ada@example.test", "thread-answered", -200*time.Hour)
	reply := f.file(t, f.sent, "Re: Old question", "rows@example.test", "thread-answered", time.Hour)
	recent := f.file(t, f.inbox, "Recent note", "grace@example.test", "thread-recent", -time.Hour)

	for _, view := range []string{"/api/mail?page=1", "/api/mail?page=1&view=inbox", "/api/mail?page=1&view=relevant"} {
		t.Run(view, func(t *testing.T) {
			conversations := f.page(t, view)
			if len(conversations) != 2 {
				t.Fatalf("%s = %d rows, want the two received conversations", view, len(conversations))
			}
			if conversations[0].Message.ID != recent.ID || conversations[1].Message.ID != answered.ID {
				t.Fatalf("rows = %d, %d, want %d then %d (the Sent reply %d must not stand in for a row)",
					conversations[0].Message.ID, conversations[1].Message.ID, recent.ID, answered.ID, reply.ID)
			}
			// The page order and the dates the rows print have to agree, or a
			// "Today" heading reopens below rows that are months older.
			first := listDate(t, conversations[0])
			second := listDate(t, conversations[1])
			if !first.After(second) {
				t.Fatalf("list dates = %s then %s, want them descending like the rows", first, second)
			}
			if !second.Equal(answered.Date.UTC()) {
				t.Fatalf("answered row list date = %s, want the received message's %s", second, answered.Date.UTC())
			}
			// The conversation is still whole behind the row: the reply counts,
			// and a batch action still reaches it.
			if conversations[1].Count != 2 {
				t.Fatalf("answered row count = %d, want both messages", conversations[1].Count)
			}
			if len(conversations[1].MessageIDs) != 2 {
				t.Fatalf("answered row message ids = %v, want the thread's two messages", conversations[1].MessageIDs)
			}
		})
	}

	// The Sent view is built from the folder role, so there the reply is the row
	// and its own date is what places it.
	sentRows := f.page(t, "/api/mail?page=1&view=sent")
	if len(sentRows) != 1 || sentRows[0].Message.ID != reply.ID {
		t.Fatalf("sent view = %+v, want the reply %d", sentRows, reply.ID)
	}
	if got := listDate(t, sentRows[0]); !got.Equal(reply.Date.UTC()) {
		t.Fatalf("sent row list date = %s, want %s", got, reply.Date.UTC())
	}
}

func listDate(t *testing.T, conversation apiConversation) time.Time {
	t.Helper()
	raw := conversation.ListDate
	if raw == "" {
		raw = conversation.Message.Date
	}
	parsed, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		t.Fatalf("parse list date %q: %v", raw, err)
	}
	return parsed.UTC()
}
