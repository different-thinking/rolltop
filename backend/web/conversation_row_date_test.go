package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"rolltop/backend/store"
)

// rowDateTenant is one owner's mail: an Inbox, a Sent folder, and the user the
// list requests are made as.
type rowDateTenant struct {
	user    store.User
	account store.MailAccount
	inbox   store.Mailbox
	sent    store.Mailbox
}

// rowDateFixture is a store and server several tenants share, so a list page
// can be asserted to hold one owner's mail and none of the other's.
type rowDateFixture struct {
	db     *store.Store
	server *Server
	base   time.Time
	// seq numbers blobs and UIDs across every tenant, which keeps the paths and
	// Message-IDs distinct without each tenant carrying a counter of its own.
	seq int
}

func newRowDateFixture(t *testing.T) *rowDateFixture {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "rolltop.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return &rowDateFixture{
		db:     db,
		server: &Server{store: db, mailListCache: newMailListCache()},
		base:   time.Unix(1700000000, 0).UTC(),
	}
}

// tenant adds an owner with the two folders these lists are read from.
func (f *rowDateFixture) tenant(t *testing.T, email string) rowDateTenant {
	t.Helper()
	ctx := context.Background()
	user, err := f.db.CreateUser(ctx, email, "Rows", "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	account, err := f.db.UpsertMailAccount(ctx, store.MailAccount{
		UserID: user.ID, Email: user.Email, Host: "imap.example.test", Port: 993,
		Username: user.Email, EncryptedPassword: "encrypted", UseTLS: true, Mailbox: "INBOX",
	})
	if err != nil {
		t.Fatal(err)
	}
	inbox, err := f.db.GetOrCreateMailbox(ctx, user.ID, account.ID, "INBOX")
	if err != nil {
		t.Fatal(err)
	}
	sent, err := f.db.GetOrCreateMailboxWithRole(ctx, user.ID, account.ID, "Sent", "sent")
	if err != nil {
		t.Fatal(err)
	}
	return rowDateTenant{user: user, account: account, inbox: inbox, sent: sent}
}

// file stores one message at a fixed distance from the fixture's base instant,
// under a caller-chosen thread key so a reply can be tied to what it answers.
func (f *rowDateFixture) file(t *testing.T, owner rowDateTenant, mailbox store.Mailbox, subject, from, threadKey string, offset time.Duration) store.MessageRecord {
	t.Helper()
	ctx := context.Background()
	f.seq++
	blob, err := f.db.CreateBlob(ctx, store.BlobRecord{
		UserID: owner.user.ID, Kind: "message", Path: fmt.Sprintf("users/%d/row-%d.eml", owner.user.ID, f.seq),
		SHA256: fmt.Sprintf("row-%d", f.seq), Size: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	message, err := f.db.CreateMessage(ctx, store.CreateMessage{
		UserID: owner.user.ID, AccountID: owner.account.ID, MailboxID: mailbox.ID, BlobID: blob.ID,
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

// page reads one list as one owner, which is the only way a browser route ever
// names a tenant: from the session, never from the URL.
func (f *rowDateFixture) page(t *testing.T, owner rowDateTenant, target string) []apiConversation {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req = req.WithContext(context.WithValue(req.Context(), userContextKey, currentUser{User: owner.user}))
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
	owner := f.tenant(t, "rows@example.test")
	answered := f.file(t, owner, owner.inbox, "Old question", "ada@example.test", "thread-answered", -200*time.Hour)
	reply := f.file(t, owner, owner.sent, "Re: Old question", "rows@example.test", "thread-answered", time.Hour)
	recent := f.file(t, owner, owner.inbox, "Recent note", "grace@example.test", "thread-recent", -time.Hour)

	// The neighbour's mail is deliberately the same shape - same thread keys,
	// same senders, same instants - so nothing but the tenant column can keep the
	// two apart, and a list that leaked one into the other would be visible as an
	// extra row rather than as a coincidence.
	neighbour := f.tenant(t, "neighbour@example.test")
	neighbourAnswered := f.file(t, neighbour, neighbour.inbox, "Old question", "ada@example.test", "thread-answered", -200*time.Hour)
	neighbourReply := f.file(t, neighbour, neighbour.sent, "Re: Old question", "neighbour@example.test", "thread-answered", time.Hour)
	neighbourRecent := f.file(t, neighbour, neighbour.inbox, "Recent note", "grace@example.test", "thread-recent", -time.Hour)
	foreign := []int64{neighbourAnswered.ID, neighbourReply.ID, neighbourRecent.ID}

	for _, view := range []string{"/api/mail?page=1", "/api/mail?page=1&view=inbox", "/api/mail?page=1&view=relevant"} {
		t.Run(view, func(t *testing.T) {
			conversations := f.page(t, owner, view)
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
			assertNoForeignMessages(t, view, conversations, foreign)
		})
	}

	// The Sent view is built from the folder role, so there the reply is the row
	// and its own date is what places it.
	sentRows := f.page(t, owner, "/api/mail?page=1&view=sent")
	if len(sentRows) != 1 || sentRows[0].Message.ID != reply.ID {
		t.Fatalf("sent view = %+v, want the reply %d", sentRows, reply.ID)
	}
	if got := listDate(t, sentRows[0]); !got.Equal(reply.Date.UTC()) {
		t.Fatalf("sent row list date = %s, want %s", got, reply.Date.UTC())
	}
	assertNoForeignMessages(t, "sent", sentRows, foreign)

	// Read from the other side, the neighbour sees their own mail and the same
	// row rule, which is what makes the assertions above about isolation rather
	// than about one tenant simply having no mail.
	neighbourRows := f.page(t, neighbour, "/api/mail?page=1&view=inbox")
	if len(neighbourRows) != 2 || neighbourRows[1].Message.ID != neighbourAnswered.ID {
		t.Fatalf("neighbour inbox = %+v, want their own answered row %d", neighbourRows, neighbourAnswered.ID)
	}
	assertNoForeignMessages(t, "neighbour inbox", neighbourRows, []int64{answered.ID, reply.ID, recent.ID})
}

// assertNoForeignMessages fails when a page names another tenant's mail, in the
// row itself or in the thread ids a batch action would reach.
func assertNoForeignMessages(t *testing.T, view string, conversations []apiConversation, foreign []int64) {
	t.Helper()
	for _, conversation := range conversations {
		if slices.Contains(foreign, conversation.Message.ID) {
			t.Fatalf("%s showed another tenant's message %d", view, conversation.Message.ID)
		}
		for _, id := range conversation.MessageIDs {
			if slices.Contains(foreign, id) {
				t.Fatalf("%s reached another tenant's message %d through %d", view, id, conversation.Message.ID)
			}
		}
	}
}

// listDate reads the instant a row is placed by, which is what the browser
// groups its date sections on and prints in the row.
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
