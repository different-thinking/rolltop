package web

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"rolltop/backend/blob"
	"rolltop/backend/icalendar"
	"rolltop/backend/mailparse"
	"rolltop/backend/smtpclient"
	"rolltop/backend/store"
	"rolltop/backend/store/storetest"
	"rolltop/backend/syncer"
)

type calendarReplyFixture struct {
	server     *Server
	sender     *captureSender
	db         *store.Store
	owner      store.User
	other      store.User
	invitation store.MessageRecord
	sent       store.Mailbox
}

func invitationRaw(organizer string) string {
	calendar := "BEGIN:VCALENDAR\r\n" +
		"METHOD:REQUEST\r\n" +
		"BEGIN:VEVENT\r\n" +
		"UID:reply-uid-1\r\n" +
		"SEQUENCE:1\r\n" +
		"DTSTART:20261124T130000Z\r\n" +
		"DTEND:20261124T140000Z\r\n" +
		"SUMMARY:Abstimmung\r\n" +
		"ORGANIZER;CN=Max Muster:mailto:" + organizer + "\r\n" +
		"ATTENDEE;PARTSTAT=NEEDS-ACTION;CN=Me:mailto:me@example.test\r\n" +
		"ATTENDEE;PARTSTAT=ACCEPTED:mailto:else@example.test\r\n" +
		"END:VEVENT\r\n" +
		"END:VCALENDAR\r\n"
	return "From: Max Muster <" + organizer + ">\r\n" +
		"To: me@example.test\r\n" +
		"Subject: Einladung: Abstimmung\r\n" +
		"Message-ID: <invite-1@example.de>\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: multipart/alternative; boundary=\"b\"\r\n\r\n" +
		"--b\r\nContent-Type: text/plain\r\n\r\nInvitation\r\n" +
		"--b\r\nContent-Type: text/calendar; method=REQUEST; charset=utf-8\r\nContent-Transfer-Encoding: base64\r\n\r\n" +
		base64.StdEncoding.EncodeToString([]byte(calendar)) + "\r\n" +
		"--b--\r\n"
}

func newCalendarReplyFixture(t *testing.T, organizer string) calendarReplyFixture {
	t.Helper()
	ctx := context.Background()
	db, err := storetest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	owner, err := db.CreateUser(ctx, "me@example.test", "Me", "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	other, err := db.CreateUser(ctx, "other@example.test", "Other", "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	account, err := db.UpsertMailAccount(ctx, store.MailAccount{
		UserID: owner.ID, Email: "me@example.test", Host: "imap.example.test", Port: 993,
		Username: "me@example.test", EncryptedPassword: "encrypted", UseTLS: true, Mailbox: "INBOX",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateSMTPAccount(ctx, store.SMTPAccount{UserID: owner.ID, Label: "SMTP", Host: "smtp.example.test", Port: 587, Username: "me@example.test", EncryptedPassword: "encrypted", UseTLS: true}); err != nil {
		t.Fatal(err)
	}
	inbox, err := db.GetOrCreateMailbox(ctx, owner.ID, account.ID, "INBOX")
	if err != nil {
		t.Fatal(err)
	}
	sent, err := db.GetOrCreateMailboxWithRole(ctx, owner.ID, account.ID, "Sent", "sent")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateMailboxRemoteStatus(ctx, owner.ID, sent.ID, 0, 0, 1, 444); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateContact(ctx, owner.ID, store.Contact{
		DisplayName: "Me", IsMe: true, IsPrimary: true,
		Emails: []store.ContactEmail{{Email: "me@example.test", IsPrimary: true}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.EnsureMailIdentityForEmail(ctx, owner.ID, "me@example.test"); err != nil {
		t.Fatal(err)
	}
	blobs := blob.New(t.TempDir())
	raw := invitationRaw(organizer)
	saved, err := blobs.SaveRawMessage(owner.ID, account.ID, "INBOX", 7, []byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	record, err := db.CreateBlob(ctx, store.BlobRecord{UserID: owner.ID, Kind: "message", Path: saved.Path, SHA256: saved.SHA256, Size: saved.Size})
	if err != nil {
		t.Fatal(err)
	}
	invitation, err := db.CreateMessage(ctx, store.CreateMessage{
		UserID: owner.ID, AccountID: account.ID, MailboxID: inbox.ID, BlobID: record.ID,
		MessageIDHeader: "<invite-1@example.de>", FromAddr: "Max Muster <" + organizer + ">",
		ToAddr: "me@example.test", Subject: "Einladung: Abstimmung",
		Date: time.Now().UTC(), InternalDate: time.Now().UTC(), UID: 7, BlobPath: saved.Path, Size: int64(len(raw)),
	})
	if err != nil {
		t.Fatal(err)
	}
	fetcher := &captureAppendFetcher{nextUID: 1, uidValidity: 444}
	sender := &captureSender{}
	server := &Server{
		store: db, blobs: blobs, sender: sender, events: newEventHub(),
		syncer: &syncer.Service{Store: db, Blobs: blobs, Fetcher: fetcher}, syncRunner: syncer.NewRunner(nil),
	}
	return calendarReplyFixture{server: server, sender: sender, db: db, owner: owner, other: other, invitation: invitation, sent: sent}
}

func (f calendarReplyFixture) send(t *testing.T, user store.User, method, target string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var payload []byte
	if body != nil {
		var err error
		if payload, err = json.Marshal(body); err != nil {
			t.Fatal(err)
		}
	}
	req := httptest.NewRequest(method, target, bytes.NewReader(payload))
	req = req.WithContext(context.WithValue(req.Context(), userContextKey, currentUser{User: user}))
	if method != http.MethodGet {
		const session = "calendar-reply-session"
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: session})
		req.Header.Set("X-CSRF-Token", f.server.csrfForBase(session))
		req.Header.Set("Content-Type", "application/json")
	}
	res := httptest.NewRecorder()
	f.server.handleAPI(res, req)
	return res
}

func (f calendarReplyFixture) path(suffix string) string {
	return "/api/messages/" + strconvInt64(f.invitation.ID) + "/calendar" + suffix
}

// sentReply reads the reply out of what was handed to SMTP, the way the
// organizer's client will.
func sentReply(t *testing.T, msg smtpclient.Message) icalendar.Calendar {
	t.Helper()
	raw, _, err := smtpclient.BuildRaw(msg)
	if err != nil {
		t.Fatal(err)
	}
	parts, err := mailparse.CalendarParts(bytes.NewReader(raw))
	if err != nil || len(parts) != 1 {
		t.Fatalf("calendar parts = %d (%v) in:\n%s", len(parts), err, raw)
	}
	if !strings.Contains(string(raw), "method=REPLY") {
		t.Fatalf("the calendar part does not say method=REPLY:\n%s", raw)
	}
	cal, err := icalendar.Parse(parts[0].Data)
	if err != nil {
		t.Fatal(err)
	}
	return cal
}

func TestInvitationIsAnsweredByMailToTheOrganizer(t *testing.T) {
	f := newCalendarReplyFixture(t, "max@example.de")

	res := f.send(t, f.owner, http.MethodGet, f.path(""), nil)
	if res.Code != http.StatusOK {
		t.Fatalf("GET status=%d body=%s", res.Code, res.Body.String())
	}
	var shown apiMessageCalendar
	if err := json.NewDecoder(res.Body).Decode(&shown); err != nil {
		t.Fatal(err)
	}
	if len(shown.Events) != 1 || !shown.Events[0].CanReply {
		t.Fatalf("events = %+v, want one answerable invitation", shown.Events)
	}
	if shown.Events[0].Attendees[0].Status != store.CalendarResponseNeedsAction || shown.Events[0].Attendees[1].Status != store.CalendarResponseAccepted {
		t.Fatalf("attendee statuses = %+v", shown.Events[0].Attendees)
	}

	res = f.send(t, f.owner, http.MethodPost, f.path("/reply"), map[string]any{
		"event_index": 0, "uid": "reply-uid-1", "response": "accepted",
	})
	if res.Code != http.StatusOK {
		t.Fatalf("POST status=%d body=%s", res.Code, res.Body.String())
	}
	if f.sender.count != 1 {
		t.Fatalf("SMTP sends = %d, want 1", f.sender.count)
	}
	msg := f.sender.msg
	if len(msg.To) != 1 || !strings.Contains(msg.To[0], "max@example.de") {
		t.Fatalf("to = %v, want the organizer", msg.To)
	}
	if msg.Subject != "Accepted: Abstimmung" || msg.InReplyTo != "<invite-1@example.de>" {
		t.Fatalf("subject = %q in-reply-to = %q", msg.Subject, msg.InReplyTo)
	}
	reply := sentReply(t, msg)
	if reply.Method != "REPLY" || len(reply.Events) != 1 {
		t.Fatalf("reply = %+v", reply)
	}
	event := reply.Events[0]
	if event.UID != "reply-uid-1" || event.Sequence != 1 || event.Organizer.Email != "max@example.de" {
		t.Fatalf("reply event = %+v", event)
	}
	if len(event.Attendees) != 1 || event.Attendees[0].Email != "me@example.test" || event.Attendees[0].PartStat != icalendar.PartStatAccepted {
		t.Fatalf("reply attendees = %+v, want only the reader, accepted", event.Attendees)
	}

	// The answer is kept in Sent like any other mail the reader sent.
	copies, err := f.db.ListMailboxScopeMessagesForUser(context.Background(), f.owner.ID, f.sent.ID, store.ScopeFilter{}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(copies) != 1 {
		t.Fatalf("Sent holds %d messages, want the answer", len(copies))
	}
}

func TestInvitationReplyRefusesWhatWasNotShown(t *testing.T) {
	f := newCalendarReplyFixture(t, "max@example.de")
	for _, tc := range []struct {
		name string
		body map[string]any
		want int
	}{
		{"unknown answer", map[string]any{"event_index": 0, "uid": "reply-uid-1", "response": "maybe"}, http.StatusBadRequest},
		{"other event", map[string]any{"event_index": 0, "uid": "something-else", "response": "declined"}, http.StatusConflict},
		{"index out of range", map[string]any{"event_index": 3, "uid": "reply-uid-1", "response": "declined"}, http.StatusConflict},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := f.send(t, f.owner, http.MethodPost, f.path("/reply"), tc.body)
			if res.Code != tc.want {
				t.Fatalf("status=%d body=%s, want %d", res.Code, res.Body.String(), tc.want)
			}
		})
	}
	if f.sender.count != 0 {
		t.Fatalf("SMTP sends = %d, want none", f.sender.count)
	}
}

// The message id is guessable: answering somebody else's invitation would
// send mail on their behalf, so it has to read as a message that is not there.
func TestInvitationReplyIsScopedToTheSignedInUser(t *testing.T) {
	f := newCalendarReplyFixture(t, "max@example.de")
	res := f.send(t, f.other, http.MethodPost, f.path("/reply"), map[string]any{
		"event_index": 0, "uid": "reply-uid-1", "response": "accepted",
	})
	if res.Code != http.StatusNotFound {
		t.Fatalf("other tenant status=%d body=%s", res.Code, res.Body.String())
	}
	if f.sender.count != 0 {
		t.Fatalf("SMTP sends = %d, want none", f.sender.count)
	}
}

// An invitation the reader sent themselves -- the copy in Sent, or one a list
// handed back -- has nobody to answer.
func TestOwnInvitationIsNotOfferedAnAnswer(t *testing.T) {
	f := newCalendarReplyFixture(t, "me@example.test")
	res := f.send(t, f.owner, http.MethodGet, f.path(""), nil)
	var shown apiMessageCalendar
	if err := json.NewDecoder(res.Body).Decode(&shown); err != nil {
		t.Fatal(err)
	}
	if len(shown.Events) != 1 || shown.Events[0].CanReply {
		t.Fatalf("events = %+v, want no answer offered", shown.Events)
	}
	res = f.send(t, f.owner, http.MethodPost, f.path("/reply"), map[string]any{
		"event_index": 0, "uid": "reply-uid-1", "response": "accepted",
	})
	if res.Code != http.StatusUnprocessableEntity || f.sender.count != 0 {
		t.Fatalf("status=%d sends=%d body=%s", res.Code, f.sender.count, res.Body.String())
	}
}
