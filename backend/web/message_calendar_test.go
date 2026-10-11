package web

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"rolltop/backend/blob"
	"rolltop/backend/store"
)

// An Outlook invitation: the calendar is a body alternative with no file name,
// so it never becomes an attachment row and has to be found in the message.
const outlookInvitation = "From: Max Muster <max@example.de>\r\n" +
	"To: owner@example.test\r\n" +
	"Subject: Abstimmung\r\n" +
	"Date: Mon, 12 Oct 2026 08:00:00 +0200\r\n" +
	"Content-Class: urn:content-classes:calendarmessage\r\n" +
	"MIME-Version: 1.0\r\n" +
	"Content-Type: multipart/alternative; boundary=\"b1\"\r\n" +
	"\r\n" +
	"--b1\r\n" +
	"Content-Type: text/plain; charset=\"utf-8\"\r\n" +
	"\r\n" +
	"Wann: Dienstag, 24. November 2026 14:00-15:00\r\n" +
	"--b1\r\n" +
	"Content-Type: text/calendar; charset=\"utf-8\"; method=REQUEST\r\n" +
	"Content-Transfer-Encoding: base64\r\n" +
	"\r\n" +
	"%BASE64%\r\n" +
	"--b1--\r\n"

const outlookCalendar = "BEGIN:VCALENDAR\r\n" +
	"METHOD:REQUEST\r\n" +
	"VERSION:2.0\r\n" +
	"BEGIN:VEVENT\r\n" +
	"ORGANIZER;CN=Max Muster:mailto:max@example.de\r\n" +
	"ATTENDEE;CN=Owner;RSVP=TRUE:mailto:owner@example.test\r\n" +
	"SUMMARY:Abstimmung Projekt\r\n" +
	"DTSTART;TZID=W. Europe Standard Time:20261124T140000\r\n" +
	"DTEND;TZID=W. Europe Standard Time:20261124T150000\r\n" +
	"UID:outlook-uid-1\r\n" +
	"LOCATION:Besprechungsraum 2\r\n" +
	"END:VEVENT\r\n" +
	"END:VCALENDAR\r\n"

func storeRawTestMessage(t *testing.T, env *googleTestEnv, blobs *blob.Store, user store.User, uid uint32, raw string) store.MessageRecord {
	t.Helper()
	ctx := context.Background()
	account, err := env.db.UpsertMailAccount(ctx, store.MailAccount{
		UserID: user.ID, Email: user.Email, Host: "imap.example.test", Port: 993,
		Username: user.Email, EncryptedPassword: "encrypted", UseTLS: true, Mailbox: "INBOX",
	})
	if err != nil {
		t.Fatal(err)
	}
	mailbox, err := env.db.GetOrCreateMailbox(ctx, user.ID, account.ID, "INBOX")
	if err != nil {
		t.Fatal(err)
	}
	saved, err := blobs.SaveRawMessage(user.ID, account.ID, "INBOX", uid, []byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	record, err := env.db.CreateBlob(ctx, store.BlobRecord{
		UserID: user.ID, Kind: "message", Path: saved.Path, SHA256: saved.SHA256, Size: saved.Size,
	})
	if err != nil {
		t.Fatal(err)
	}
	message, err := env.db.CreateMessage(ctx, store.CreateMessage{
		UserID: user.ID, AccountID: account.ID, MailboxID: mailbox.ID, BlobID: record.ID,
		FromAddr: "Max Muster <max@example.de>", Subject: "Abstimmung",
		Date: time.Now().UTC(), InternalDate: time.Now().UTC(),
		UID: uid, BlobPath: saved.Path, Size: int64(len(raw)),
	})
	if err != nil {
		t.Fatal(err)
	}
	return message
}

func base64Lines(data string) string {
	encoded := base64.StdEncoding.EncodeToString([]byte(data))
	var b strings.Builder
	for len(encoded) > 76 {
		b.WriteString(encoded[:76] + "\r\n")
		encoded = encoded[76:]
	}
	b.WriteString(encoded)
	return b.String()
}

func TestMessageCalendarReadsAnOutlookInvitation(t *testing.T) {
	env := newGoogleTestEnv(t)
	blobs := blob.New(t.TempDir())
	env.server.blobs = blobs
	raw := strings.Replace(outlookInvitation, "%BASE64%", base64Lines(outlookCalendar), 1)
	message := storeRawTestMessage(t, env, blobs, env.owner, 41, raw)

	connection := env.connect(t, env.owner)
	calendar := storedCalendar(t, env, env.owner, connection.ID, store.CalendarAccessRoleOwner)
	start := time.Date(2026, 11, 24, 13, 0, 0, 0, time.UTC)
	if _, err := env.db.UpsertCalendarEvent(context.Background(), env.owner.ID, store.CalendarEvent{
		CalendarID: calendar.ID, ExternalID: "from-invite", ETag: "etag", ICalUID: "outlook-uid-1",
		Summary: "Abstimmung Projekt", StartAt: start, EndAt: start.Add(time.Hour),
		MyResponse: store.CalendarResponseNeedsAction,
	}); err != nil {
		t.Fatal(err)
	}

	if !env.server.messageCarriesCalendar(context.Background(), env.owner.ID, message, nil, true) {
		t.Fatal("an inline text/calendar part was not noticed")
	}
	if env.server.messageCarriesCalendar(context.Background(), env.owner.ID, message, nil, false) {
		t.Fatal("a collapsed message without a calendar attachment row was scanned")
	}

	response := env.send(t, env.owner, http.MethodGet, "/api/messages/"+strconvInt64(message.ID)+"/calendar", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var payload apiMessageCalendar
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if payload.Method != "REQUEST" || len(payload.Events) != 1 {
		t.Fatalf("payload = %+v", payload)
	}
	event := payload.Events[0]
	if event.Summary != "Abstimmung Projekt" || event.Location != "Besprechungsraum 2" {
		t.Fatalf("event = %+v", event)
	}
	if event.StartAt != "2026-11-24T13:00:00Z" || event.EndAt != "2026-11-24T14:00:00Z" || event.TimeZone != "Europe/Berlin" {
		t.Fatalf("bounds = %s - %s (%s)", event.StartAt, event.EndAt, event.TimeZone)
	}
	if event.Organizer == nil || event.Organizer.Email != "max@example.de" || len(event.Attendees) != 1 {
		t.Fatalf("people = %+v / %+v", event.Organizer, event.Attendees)
	}
	if len(event.InCalendars) != 1 || event.InCalendars[0].CalendarID != calendar.ID || event.InCalendars[0].CalendarName != "Work" {
		t.Fatalf("in calendars = %+v", event.InCalendars)
	}
	// No calendar provider is wired into this server, so there is nothing a
	// new event could be written through.
	if payload.CanAdd {
		t.Fatal("can_add without a calendar provider")
	}
}

// The message id is guessable, and the calendar route reads the raw message:
// another tenant's id has to read as a message that does not exist, and the
// owner's calendars must not be consulted for somebody else's invitation.
func TestMessageCalendarIsScopedToTheSignedInUser(t *testing.T) {
	env := newGoogleTestEnv(t)
	blobs := blob.New(t.TempDir())
	env.server.blobs = blobs
	raw := strings.Replace(outlookInvitation, "%BASE64%", base64Lines(outlookCalendar), 1)
	message := storeRawTestMessage(t, env, blobs, env.owner, 42, raw)

	response := env.send(t, env.other, http.MethodGet, "/api/messages/"+strconvInt64(message.ID)+"/calendar", nil)
	if response.Code != http.StatusNotFound {
		t.Fatalf("other tenant status=%d body=%s", response.Code, response.Body.String())
	}
	var payload struct {
		Code string `json:"code"`
	}
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil || payload.Code != messageGoneCode {
		t.Fatalf("other tenant payload = %+v (%v)", payload, err)
	}
}

func TestMessageCalendarAnswersEmptyForPlainMail(t *testing.T) {
	env := newGoogleTestEnv(t)
	blobs := blob.New(t.TempDir())
	env.server.blobs = blobs
	message := storeRawTestMessage(t, env, blobs, env.owner, 43, "From: a@example.test\r\nSubject: Hi\r\nContent-Type: text/plain\r\n\r\nHello\r\n")

	if env.server.messageCarriesCalendar(context.Background(), env.owner.ID, message, nil, true) {
		t.Fatal("plain mail reported a calendar")
	}
	attachments := []store.Attachment{{Filename: "termin.ics", ContentType: "application/octet-stream"}}
	if !env.server.messageCarriesCalendar(context.Background(), env.owner.ID, message, attachments, false) {
		t.Fatal("an .ics attachment row was not enough")
	}
	response := env.send(t, env.owner, http.MethodGet, "/api/messages/"+strconvInt64(message.ID)+"/calendar", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var payload apiMessageCalendar
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Events) != 0 {
		t.Fatalf("events = %+v", payload.Events)
	}
}
