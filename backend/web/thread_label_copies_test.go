// File overview: Tests for what a rendered thread does with the second copy a
// mirrored label view holds - that every physical row is still marked read, that
// only one of them is drawn, and that the drawn row carries the rows behind it.

package web

import (
	"context"
	"fmt"
	"testing"
	"time"

	"rolltop/backend/store"
	"rolltop/backend/store/storetest"
)

type threadCopyEnv struct {
	server  *Server
	db      *store.Store
	ctx     context.Context
	user    store.User
	account int64
	inbox   int64
	allMail int64
	uid     uint32
}

func newThreadCopyEnv(t *testing.T) *threadCopyEnv {
	t.Helper()
	ctx := context.Background()
	db, err := storetest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	user, err := db.CreateUser(ctx, "thread-copies@example.test", "Owner", "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	account, err := db.CreateMailAccount(ctx, store.MailAccount{
		UserID: user.ID, Email: "owner@gmail.test", Host: "imap.gmail.test", Port: 993,
		Username: "owner@gmail.test", EncryptedPassword: "secret", UseTLS: true, Mailbox: "*",
	})
	if err != nil {
		t.Fatal(err)
	}
	env := &threadCopyEnv{
		server: &Server{store: db, masterKey: []byte("12345678901234567890123456789012")},
		db:     db, ctx: ctx, user: user, account: account.ID,
	}
	env.inbox = env.mailbox(t, "INBOX", "inbox")
	env.allMail = env.mailbox(t, "[Gmail]/Alle Nachrichten", "all")
	t.Cleanup(func() { waitForWebPushWorker(t, env.server, user.ID) })
	return env
}

func (e *threadCopyEnv) mailbox(t *testing.T, name, role string) int64 {
	t.Helper()
	mailbox, err := e.db.GetOrCreateMailbox(e.ctx, e.user.ID, e.account, name)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.db.UpdateMailboxSettings(e.ctx, e.user.ID, mailbox.ID, store.MailboxSettings{
		SyncMode: "auto", Role: role, ShowInSidebar: true, ShowInAllMail: true, IncludeInSearch: true,
	}); err != nil {
		t.Fatal(err)
	}
	return mailbox.ID
}

func (e *threadCopyEnv) storeCopy(t *testing.T, mailboxID int64, header string, sent time.Time) store.MessageRecord {
	t.Helper()
	e.uid++
	uid := e.uid
	blob, err := e.db.CreateBlob(e.ctx, store.BlobRecord{
		UserID: e.user.ID, Kind: "message",
		Path:   fmt.Sprintf("blobs/%d", uid),
		SHA256: fmt.Sprintf("sha-%d", uid),
		Size:   10,
	})
	if err != nil {
		t.Fatal(err)
	}
	msg, err := e.db.CreateMessage(e.ctx, store.CreateMessage{
		UserID: e.user.ID, AccountID: e.account, MailboxID: mailboxID, BlobID: blob.ID,
		MessageIDHeader: header, ThreadKey: "msgid:thread",
		Subject:  "Deine Adoption",
		FromAddr: "hallo@crowdfarming.test", ToAddr: "owner@gmail.test",
		BodyText: "Erhalte die erste Lieferung frueher.",
		Date:     sent, InternalDate: sent, UID: uid, Size: 10, BlobPath: blob.Path,
	})
	if err != nil {
		t.Fatal(err)
	}
	return msg
}

// Opening the conversation has to mark the copy it hides read as well. The
// lists AND read state across the copies, so a copy left unread keeps the
// conversation unread in the list the reader just read it from - and a label
// view defaults to never syncing, so nothing would come along to correct it.
func TestRenderedThreadMarksTheHiddenCopyReadToo(t *testing.T) {
	env := newThreadCopyEnv(t)
	sent := time.Now().UTC().Truncate(time.Second)
	inbox := env.storeCopy(t, env.inbox, "<milan@crowdfarming.test>", sent)
	view := env.storeCopy(t, env.allMail, "<milan@crowdfarming.test>", sent)

	views, _, err := env.server.threadViewsForMessage(env.ctx, currentUser{User: env.user}, inbox, false, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(views) != 1 {
		t.Fatalf("thread drew %d messages, want one", len(views))
	}
	for _, id := range []int64{inbox.ID, view.ID} {
		stored, err := env.db.GetMessageForUser(env.ctx, env.user.ID, id)
		if err != nil {
			t.Fatal(err)
		}
		if !stored.IsRead {
			t.Fatalf("message %d is still unread after the thread was rendered", id)
		}
	}
}

// The drawn message names the rows it stands for, which is what lets Mark
// Unread reach the copy the thread hid.
func TestRenderedThreadNamesTheCopiesBehindTheDrawnMessage(t *testing.T) {
	env := newThreadCopyEnv(t)
	sent := time.Now().UTC().Truncate(time.Second)
	inbox := env.storeCopy(t, env.inbox, "<milan@crowdfarming.test>", sent)
	view := env.storeCopy(t, env.allMail, "<milan@crowdfarming.test>", sent)

	views, _, err := env.server.threadViewsForMessage(env.ctx, currentUser{User: env.user}, inbox, false, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(views) != 1 {
		t.Fatalf("thread drew %d messages, want one", len(views))
	}
	got := views[0].CopyIDs
	if len(got) != 2 || got[0] != inbox.ID || got[1] != view.ID {
		t.Fatalf("copy ids = %v, want both rows %v", got, []int64{inbox.ID, view.ID})
	}
}

// The lists reach the All Mail copy on their own: it is mirrored after the
// Inbox copy and so holds the higher id, which breaks the tie when a
// conversation picks the row it prints. Opening it has to render a thread the
// reader can read, and the message the view reports has to be in it.
func TestRenderedThreadFollowsAnOpenedHiddenCopyToItsStandIn(t *testing.T) {
	env := newThreadCopyEnv(t)
	sent := time.Now().UTC().Truncate(time.Second)
	inbox := env.storeCopy(t, env.inbox, "<milan@crowdfarming.test>", sent)
	view := env.storeCopy(t, env.allMail, "<milan@crowdfarming.test>", sent)

	views, reported, err := env.server.threadViewsForMessage(env.ctx, currentUser{User: env.user}, view, false, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(views) != 1 {
		t.Fatalf("thread drew %d messages, want one", len(views))
	}
	if views[0].Message.ID != inbox.ID {
		t.Fatalf("drawn message = %d, want the Inbox copy %d", views[0].Message.ID, inbox.ID)
	}
	if reported.ID != inbox.ID {
		t.Fatalf("reported message = %d, want the row the thread actually drew %d", reported.ID, inbox.ID)
	}
	if !views[0].Expanded {
		t.Fatal("the message the reader opened was not expanded")
	}
}
