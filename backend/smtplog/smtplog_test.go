package smtplog

import (
	"errors"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestRecordingRedactsCredentialsAndBodies(t *testing.T) {
	recorder := NewRecorder()
	recording := recorder.Start(Session{UserID: 1, Kind: KindSend})
	recording.Client("AUTH PLAIN AHVzZXJAZXhhbXBsZS50ZXN0AGh1bnRlcjI=")
	recording.Secret()
	recording.Body(4096)
	recording.Client("MAIL FROM:<user@example.test>")
	recording.Finish(nil)

	session := recording.Snapshot()
	transcript := strings.Join(linesOf(session), "\n")
	for _, secret := range []string{"AHVzZXJAZXhhbXBsZS50ZXN0AGh1bnRlcjI=", "hunter2"} {
		if strings.Contains(transcript, secret) {
			t.Fatalf("transcript carried a credential:\n%s", transcript)
		}
	}
	if !strings.Contains(transcript, "AUTH PLAIN") {
		t.Fatalf("transcript dropped the mechanism, which is what tells a reader what was attempted:\n%s", transcript)
	}
	if !strings.Contains(transcript, "4096 bytes") {
		t.Fatalf("transcript dropped the message size:\n%s", transcript)
	}
	if !strings.Contains(transcript, "MAIL FROM:<user@example.test>") {
		t.Fatalf("transcript dropped an ordinary command:\n%s", transcript)
	}
}

// A reply is one record even when the server sent it across several lines, so
// a hostile envelope address or server banner cannot forge extra entries.
func TestRecordingKeepsOneUtteranceToOneLine(t *testing.T) {
	recorder := NewRecorder()
	recording := recorder.Start(Session{UserID: 1})
	recording.Server(250, "smtp.example.test\nAUTH PLAIN\nSTARTTLS")
	session := recording.Snapshot()
	if len(session.Lines) != 1 {
		t.Fatalf("lines = %d, want 1: %#v", len(session.Lines), session.Lines)
	}
	for _, want := range []string{"250", "AUTH PLAIN", "STARTTLS"} {
		if !strings.Contains(session.Lines[0].Text, want) {
			t.Fatalf("reply %q lost %q", session.Lines[0].Text, want)
		}
	}
}

func TestRecorderScopesSessionsToTheirUser(t *testing.T) {
	recorder := NewRecorder()
	mine := recorder.Start(Session{UserID: 1, Host: "mine.example.test"})
	theirs := recorder.Start(Session{UserID: 2, Host: "theirs.example.test"})
	mine.Finish(nil)
	theirs.Finish(errors.New("535 authentication failed"))

	sessions := recorder.Sessions(1, 0, 10)
	if len(sessions) != 1 || sessions[0].Host != "mine.example.test" {
		t.Fatalf("user 1 read %#v, want only their own session", sessions)
	}
	if _, ok := recorder.Session(1, theirs.Ref()); ok {
		t.Fatal("one user read another user's session by id")
	}
	if _, ok := recorder.Session(2, theirs.Ref()); !ok {
		t.Fatal("the owning user could not read their own session by id")
	}
}

func TestRecorderKeepsTheNewestSessionsPerUser(t *testing.T) {
	recorder := NewRecorder()
	for i := range sessionsPerUser + 5 {
		recording := recorder.Start(Session{UserID: 7, Host: "host", Port: i})
		recording.Finish(nil)
	}
	sessions := recorder.Sessions(7, 0, 100)
	if len(sessions) != sessionsPerUser {
		t.Fatalf("kept %d sessions, want %d", len(sessions), sessionsPerUser)
	}
	if sessions[0].Port != sessionsPerUser+4 {
		t.Fatalf("newest session port = %d, want the last attempt", sessions[0].Port)
	}
}

// Narrowing to one server has to happen while walking the tail, not on a page
// of it: an account whose attempt has been pushed out of the newest few by a
// different account is still recorded, and answering "nothing here" for it is
// the failure this panel exists to end.
func TestSessionsNarrowBeyondTheNewestPage(t *testing.T) {
	recorder := NewRecorder()
	recorder.Start(Session{UserID: 5, AccountID: 8, Host: "eight.example.test"}).Finish(nil)
	for range sessionsPerUser - 1 {
		recorder.Start(Session{UserID: 5, AccountID: 9, Host: "nine.example.test"}).Finish(nil)
	}
	sessions := recorder.Sessions(5, 8, 3)
	if len(sessions) != 1 || sessions[0].Host != "eight.example.test" {
		t.Fatalf("narrowed answer = %#v, want the one attempt on account 8", sessions)
	}
	if all := recorder.Sessions(5, 0, 3); len(all) != 3 {
		t.Fatalf("unnarrowed answer = %d sessions, want the requested 3", len(all))
	}
}

// A mail server's reply is not required to be valid UTF-8, and one stray byte
// must not cost the rest of a long line.
func TestTruncateKeepsWhatFitsAroundAnInvalidByte(t *testing.T) {
	line := "550 " + strings.Repeat("a", 10) + "\xff" + strings.Repeat("b", maxLineBytes)
	got := truncateLine(line)
	if len(got) < maxLineBytes {
		t.Fatalf("truncated to %d bytes, want the whole bound: %q", len(got), got)
	}
	if !strings.HasSuffix(got, "...") {
		t.Fatalf("truncated line does not say it was cut: %q", got)
	}
}

// A cut that lands inside a rune backs off to the boundary rather than leaving
// half of it.
func TestTruncateCutsOnRuneBoundaries(t *testing.T) {
	line := strings.Repeat("a", maxLineBytes-1) + "ü" + "tail"
	got := strings.TrimSuffix(truncateLine(line), "...")
	if !utf8.ValidString(got) {
		t.Fatalf("truncated line ends inside a rune: %q", got)
	}
	if got != strings.Repeat("a", maxLineBytes-1) {
		t.Fatalf("truncated line = %q, want the text before the split rune", got)
	}
}

func TestRecordingBoundsItsTranscript(t *testing.T) {
	recorder := NewRecorder()
	recording := recorder.Start(Session{UserID: 1})
	for range linesPerSession + 10 {
		recording.Note("a line")
	}
	session := recording.Snapshot()
	if len(session.Lines) != linesPerSession {
		t.Fatalf("lines = %d, want %d", len(session.Lines), linesPerSession)
	}
	if !session.Truncated {
		t.Fatal("a transcript that hit its bound did not say so")
	}
}

// A nil recorder is what a Sender built without one has, and every call on the
// way to the wire must survive it.
func TestNilRecorderRecordsNothing(t *testing.T) {
	var recorder *Recorder
	recording := recorder.Start(Session{UserID: 1})
	recording.Client("EHLO localhost")
	recording.Server(250, "ok")
	recording.Secret()
	recording.Body(10)
	recording.Note("note")
	recording.Finish(errors.New("boom"))
	if recording.Ref() != 0 {
		t.Fatalf("nil recorder handed out session id %d", recording.Ref())
	}
	if sessions := recorder.Sessions(1, 0, 10); sessions != nil {
		t.Fatalf("nil recorder returned %#v", sessions)
	}
}

func linesOf(session Session) []string {
	out := make([]string, 0, len(session.Lines))
	for _, line := range session.Lines {
		out = append(out, line.Direction+" "+line.Text)
	}
	return out
}
