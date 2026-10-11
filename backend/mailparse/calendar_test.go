package mailparse

import (
	"strings"
	"testing"
)

func TestCalendarPartsFindsInlineAndAttachedCalendars(t *testing.T) {
	raw := "From: a@example.test\r\n" +
		"Subject: Invitation\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: multipart/mixed; boundary=\"outer\"\r\n" +
		"\r\n" +
		"--outer\r\n" +
		"Content-Type: multipart/alternative; boundary=\"inner\"\r\n" +
		"\r\n" +
		"--inner\r\n" +
		"Content-Type: text/plain; charset=UTF-8\r\n" +
		"\r\n" +
		"You are invited.\r\n" +
		"--inner\r\n" +
		"Content-Type: text/calendar; charset=ISO-8859-1; method=REQUEST\r\n" +
		"Content-Transfer-Encoding: quoted-printable\r\n" +
		"\r\n" +
		"BEGIN:VCALENDAR\r\nSUMMARY:M=FCller\r\nEND:VCALENDAR\r\n" +
		"--inner--\r\n" +
		"--outer\r\n" +
		"Content-Type: application/pdf; name=\"agenda.pdf\"\r\n" +
		"Content-Disposition: attachment; filename=\"agenda.pdf\"\r\n" +
		"\r\n" +
		strings.Repeat("%PDF-noise ", 1000) + "\r\n" +
		"--outer\r\n" +
		"Content-Type: application/octet-stream; name=\"invite.ics\"\r\n" +
		"Content-Disposition: attachment; filename=\"invite.ics\"\r\n" +
		"Content-Transfer-Encoding: base64\r\n" +
		"\r\n" +
		"QkVHSU46VkNBTEVOREFSDQpFTkQ6VkNBTEVOREFSDQo=\r\n" +
		"--outer--\r\n"
	parts, err := CalendarParts(strings.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if len(parts) != 2 {
		t.Fatalf("parts = %d, want the inline calendar and invite.ics", len(parts))
	}
	if !strings.Contains(string(parts[0].Data), "SUMMARY:Müller") {
		t.Fatalf("inline part = %q, want it decoded from its charset", parts[0].Data)
	}
	if parts[1].Filename != "invite.ics" || !strings.HasPrefix(string(parts[1].Data), "BEGIN:VCALENDAR") {
		t.Fatalf("attachment = %+v", parts[1])
	}
}

func TestCalendarPartsIgnoresPlainMail(t *testing.T) {
	parts, err := CalendarParts(strings.NewReader("From: a@example.test\r\nContent-Type: text/plain\r\n\r\nBEGIN:VCALENDAR in a sentence\r\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(parts) != 0 {
		t.Fatalf("parts = %+v", parts)
	}
}

func TestIsCalendarPart(t *testing.T) {
	for _, tc := range []struct {
		contentType, filename string
		want                  bool
	}{
		{"text/calendar; method=REQUEST", "", true},
		{"application/ics", "", true},
		{"application/octet-stream", "Termin.ICS", true},
		{"text/plain", "notes.txt", false},
		{"application/pdf", "agenda.pdf", false},
	} {
		if got := IsCalendarPart(tc.contentType, tc.filename); got != tc.want {
			t.Errorf("IsCalendarPart(%q, %q) = %v", tc.contentType, tc.filename, got)
		}
	}
}

func TestCalendarPartsRejoinsCharacterSplitAcrossFold(t *testing.T) {
	raw := "From: a@example.test\r\nContent-Type: text/calendar; charset=utf-8\r\n\r\n" +
		"BEGIN:VCALENDAR\r\nSUMMARY:Caf\xc3\r\n \xa9\r\nEND:VCALENDAR\r\n"
	parts, err := CalendarParts(strings.NewReader(raw))
	if err != nil || len(parts) != 1 {
		t.Fatalf("parts = %d (%v)", len(parts), err)
	}
	if !strings.Contains(string(parts[0].Data), "SUMMARY:Café") {
		t.Fatalf("data = %q", parts[0].Data)
	}
}
