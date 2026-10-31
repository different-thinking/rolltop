package main

import (
	"context"
	"testing"
	"time"

	"rolltop/backend/plugins"
)

// trashFromMailView is the mail view's Delete as the host performs it: the
// observer sees the move before the source row goes, then the row moves.
func trashFromMailView(t *testing.T, p *recipeBoxBackend, host *fakeHost, messageID, sourceID int64, sourceRole string, trashID int64) {
	t.Helper()
	ctx := context.Background()
	var accountID int64
	var header string
	if err := host.store.DB().QueryRowContext(ctx, `SELECT account_id, message_id_header FROM messages WHERE user_id = ? AND id = ?`, host.userID, messageID).
		Scan(&accountID, &header); err != nil {
		t.Fatal(err)
	}
	event := plugins.MessageMoveContext{
		UserID: host.userID, MessageID: messageID, MessageIDHeader: header, AccountID: accountID,
		SourceMailboxID: sourceID, SourceMailboxRole: sourceRole, DestinationMailboxID: trashID, DestinationMailboxRole: "trash",
	}
	if err := p.ObserveMessageMove(ctx, host, event); err != nil {
		t.Fatal(err)
	}
	if err := host.MoveMessage(ctx, host.userID, messageID, trashID); err != nil {
		t.Fatal(err)
	}
}

func TestDeletingANotesMailInTheMailViewDeletesTheNote(t *testing.T) {
	st := openRecipeStore(t)
	ctx := context.Background()
	f := newFixture(t, st, "view@example.test")
	trash, err := st.GetOrCreateMailboxWithRole(ctx, f.user.ID, f.account.ID, "Trash", "trash")
	if err != nil {
		t.Fatal(err)
	}
	me := "view@example.test"
	date := time.Date(2026, 5, 1, 9, 0, 0, 0, time.UTC)
	inboxCopy := createMessage(t, st, f.user.ID, f.account.ID, f.inbox.ID, "<view@x>", me, me, "Einkauf", "Milch", date)
	// The Sent copy is the newer row: the Sent folder synced after the Inbox.
	createMessage(t, st, f.user.ID, f.account.ID, f.sent.ID, "<view@x>", me, me, "Einkauf", "Milch", date)
	if _, err := saveSettings(ctx, st.DB(), f.user.ID, Settings{Enabled: true, OwnRecipientsOnly: true}); err != nil {
		t.Fatal(err)
	}
	p := testBackend()
	host := &fakeHost{t: t, store: st, userID: f.user.ID}
	drain(t, p, host, f.user.ID)
	answer := listNotes(t, p, host, "")
	if len(answer.Notes) != 1 {
		t.Fatalf("notes = %#v", answer.Notes)
	}
	// Open mail opens the copy Delete has to act on, not the newer Sent row.
	if got := answer.Notes[0].MessageID; got != inboxCopy.ID {
		t.Fatalf("note opens message %d, want the Inbox copy %d", got, inboxCopy.ID)
	}

	// A stranger's Trash move of their own mail leaves the reader's note alone.
	stranger := newFixture(t, st, "stranger@example.test")
	strangerTrash, err := st.GetOrCreateMailboxWithRole(ctx, stranger.user.ID, stranger.account.ID, "Trash", "trash")
	if err != nil {
		t.Fatal(err)
	}
	theirs := createMessage(t, st, stranger.user.ID, stranger.account.ID, stranger.inbox.ID, "<view@x>", me, me, "Einkauf", "Milch", date)
	trashFromMailView(t, p, &fakeHost{t: t, store: st, userID: stranger.user.ID}, theirs.ID, stranger.inbox.ID, "inbox", strangerTrash.ID)
	if got := listNotes(t, p, host, ""); len(got.Notes) != 1 {
		t.Fatalf("another reader's delete took the note: %#v", got.Notes)
	}

	trashFromMailView(t, p, host, inboxCopy.ID, f.inbox.ID, "inbox", trash.ID)
	if got := listNotes(t, p, host, ""); len(got.Notes) != 0 || got.Totals["open"] != 0 {
		t.Fatalf("notes after deleting the mail = %#v", got)
	}
	drain(t, p, host, f.user.ID)
	if got := listNotes(t, p, host, ""); len(got.Notes) != 0 {
		t.Fatalf("notes after the next round = %#v", got.Notes)
	}
}

func TestANoteStaysWhileACopyOutsideTrashHoldsIt(t *testing.T) {
	st := openRecipeStore(t)
	ctx := context.Background()
	f := newFixture(t, st, "held@example.test")
	trash, err := st.GetOrCreateMailboxWithRole(ctx, f.user.ID, f.account.ID, "Trash", "trash")
	if err != nil {
		t.Fatal(err)
	}
	archive, err := st.GetOrCreateMailbox(ctx, f.user.ID, f.account.ID, "Archiv")
	if err != nil {
		t.Fatal(err)
	}
	me := "held@example.test"
	date := time.Date(2026, 5, 1, 9, 0, 0, 0, time.UTC)
	inboxCopy := createMessage(t, st, f.user.ID, f.account.ID, f.inbox.ID, "<held@x>", me, me, "Notiz", "Text", date)
	archived := createMessage(t, st, f.user.ID, f.account.ID, archive.ID, "<held@x>", me, me, "Notiz", "Text", date)
	if _, err := saveSettings(ctx, st.DB(), f.user.ID, Settings{Enabled: true, OwnRecipientsOnly: true}); err != nil {
		t.Fatal(err)
	}
	p := testBackend()
	host := &fakeHost{t: t, store: st, userID: f.user.ID}
	drain(t, p, host, f.user.ID)
	if got := listNotes(t, p, host, ""); len(got.Notes) != 1 {
		t.Fatalf("notes = %#v", got.Notes)
	}
	trashFromMailView(t, p, host, inboxCopy.ID, f.inbox.ID, "inbox", trash.ID)
	if got := listNotes(t, p, host, ""); len(got.Notes) != 1 {
		t.Fatalf("a copy still in Archiv, yet the note went: %#v", got.Notes)
	}
	trashFromMailView(t, p, host, archived.ID, archive.ID, "", trash.ID)
	if got := listNotes(t, p, host, ""); len(got.Notes) != 0 {
		t.Fatalf("the last copy went to Trash, yet the note stays: %#v", got.Notes)
	}
}

func TestCopyHoldsNote(t *testing.T) {
	for _, tc := range []struct {
		name string
		c    storedCopy
		want bool
	}{
		{"inbox", storedCopy{AccountID: 1, MailboxName: "INBOX", MailboxRole: "inbox"}, true},
		{"own folder", storedCopy{AccountID: 1, MailboxName: "Notes"}, true},
		{"sent", storedCopy{AccountID: 2, MailboxName: "Sent", MailboxRole: "sent"}, false},
		{"trash", storedCopy{AccountID: 1, MailboxName: "Trash", MailboxRole: "trash"}, false},
		{"junk", storedCopy{AccountID: 1, MailboxName: "Junk", MailboxRole: "junk"}, false},
		{"gmail label view, same account", storedCopy{AccountID: 1, MailboxName: "[Gmail]/All Mail", MailboxRole: "all"}, false},
		{"gmail label view, other account", storedCopy{AccountID: 2, MailboxName: "[Gmail]/All Mail", MailboxRole: "all"}, true},
	} {
		if got := copyHoldsNote(tc.c, 1); got != tc.want {
			t.Errorf("%s: copyHoldsNote = %v, want %v", tc.name, got, tc.want)
		}
	}
}
