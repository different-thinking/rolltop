package imapclient

import (
	"context"
	"testing"

	"rolltop/backend/googletoken"
	"rolltop/backend/syncer"
)

// A folder that is not on the account is refused the same way forever, so the
// sync has to be able to tell that refusal apart from a folder it merely could
// not read this time.
func TestMailboxStatusReportsAFolderTheServerDoesNotHave(t *testing.T) {
	server := startFakeIMAPServer(t, "good-token")
	server.setMissingMailboxes("[Gmail]/Gesendet")
	fetcher := &Fetcher{Tokens: &googletoken.StubTokenSource{Tokens: []string{"good-token"}}}
	account := server.account(t)

	_, err := fetcher.MailboxStatus(context.Background(), account, "[Gmail]/Gesendet")
	if err == nil {
		t.Fatal("status on a missing folder succeeded, want a refusal")
	}
	if !syncer.IsMailboxGone(err) {
		t.Fatalf("status error = %v, want it marked as a folder the account does not have", err)
	}

	status, err := fetcher.MailboxStatus(context.Background(), account, "INBOX")
	if err != nil {
		t.Fatalf("status on a folder the account has: %v", err)
	}
	if status.UIDNext != 12 || status.UIDValidity != 44 {
		t.Fatalf("status = %+v, want the server's UIDNEXT and UIDVALIDITY", status)
	}
}

// Only the refusals that mean "no such folder" may be treated as one: anything
// else is a failure the account still has to report.
func TestMailboxMissingReadsTheServerWording(t *testing.T) {
	missing := []string{
		"Mailbox doesn't exist: [Gmail]/Gesendet (0.002 + 0.000 secs).",
		"Unknown Mailbox: [Gmail]/Sent Mail (Failure)",
		"[NONEXISTENT] No such mailbox",
		// The folder named between the two halves of the answer.
		"Mailbox [Gmail]/Gesendet does not exist",
		"Folder \"Archiv\" doesn't exist on this server",
	}
	for _, text := range missing {
		if !mailboxMissing(errorString(text)) {
			t.Fatalf("%q was not recognized as a missing folder", text)
		}
	}
	present := []string{
		"[SERVERBUG] Internal error occurred. Refer to server log for more information.",
		"Mailbox is locked by another session",
		"[OVERQUOTA] Quota exceeded",
		// A folder is named, but what is absent is not the folder.
		"Cannot open mailbox: index file not found",
		"Unknown command while a folder is selected",
	}
	for _, text := range present {
		if mailboxMissing(errorString(text)) {
			t.Fatalf("%q was read as a missing folder", text)
		}
	}
}

type errorString string

func (e errorString) Error() string { return string(e) }
