package icalendar

import (
	"strings"
	"testing"
	"time"
)

func TestBuildReplyNamesTheEventAndOnlyTheAnsweringAttendee(t *testing.T) {
	cal, err := Parse(crlf(`
BEGIN:VCALENDAR
METHOD:REQUEST
BEGIN:VEVENT
UID:series-1@example.de
SEQUENCE:3
RECURRENCE-ID;TZID=Europe/Berlin:20261124T140000
DTSTART;TZID=Europe/Berlin:20261124T150000
DTEND;TZID=Europe/Berlin:20261124T160000
SUMMARY:Abstimmung\, Projekt
ORGANIZER;CN="Muster, Max":mailto:max@example.de
ATTENDEE;PARTSTAT=NEEDS-ACTION;CN=Owner:mailto:owner@example.test
ATTENDEE;PARTSTAT=ACCEPTED;CN=Someone:mailto:someone@example.test
END:VEVENT
END:VCALENDAR
`))
	if err != nil {
		t.Fatal(err)
	}
	event := cal.Events[0]
	if event.Attendees[0].PartStat != "NEEDS-ACTION" || event.Attendees[1].PartStat != "ACCEPTED" {
		t.Fatalf("attendees = %+v", event.Attendees)
	}
	now := time.Date(2026, 10, 12, 9, 30, 0, 0, time.UTC)
	reply, err := BuildReply(event, Person{Name: "Owner", Email: "owner@example.test"}, PartStatAccepted, now)
	if err != nil {
		t.Fatal(err)
	}
	text := string(reply)
	for _, want := range []string{
		"METHOD:REPLY\r\n",
		"UID:series-1@example.de\r\n",
		"SEQUENCE:3\r\n",
		"DTSTAMP:20261012T093000Z\r\n",
		"RECURRENCE-ID:20261124T130000Z\r\n",
		"DTSTART:20261124T140000Z\r\n",
		"DTEND:20261124T150000Z\r\n",
		"SUMMARY:Abstimmung\\, Projekt\r\n",
		"ORGANIZER;CN=\"Muster, Max\":mailto:max@example.de\r\n",
		"ATTENDEE;PARTSTAT=ACCEPTED;CN=Owner:mailto:owner@example.test\r\n",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("reply lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "someone@example.test") {
		t.Fatalf("reply restates another attendee:\n%s", text)
	}

	// The reply reads back as what it says.
	back, err := Parse(reply)
	if err != nil {
		t.Fatal(err)
	}
	if back.Method != "REPLY" || len(back.Events) != 1 || back.Events[0].Attendees[0].PartStat != PartStatAccepted {
		t.Fatalf("read back = %+v", back)
	}
	if back.Events[0].Summary != "Abstimmung, Projekt" {
		t.Fatalf("summary = %q", back.Events[0].Summary)
	}
}

func TestBuildReplyRefusesWhatCannotBeAnswered(t *testing.T) {
	me := Person{Email: "owner@example.test"}
	organizer := Person{Email: "max@example.de"}
	if _, err := BuildReply(Event{Organizer: organizer}, me, PartStatAccepted, time.Now()); err != ErrNotAnswerable {
		t.Fatalf("no UID: err = %v", err)
	}
	if _, err := BuildReply(Event{UID: "x"}, me, PartStatAccepted, time.Now()); err != ErrNotAnswerable {
		t.Fatalf("no organizer: err = %v", err)
	}
	if _, err := BuildReply(Event{UID: "x", Organizer: organizer}, me, "MAYBE", time.Now()); err == nil {
		t.Fatal("unknown status accepted")
	}
}

func TestWriteFoldedKeepsCharactersWhole(t *testing.T) {
	var b strings.Builder
	long := "SUMMARY:" + strings.Repeat("Größenänderung ", 12)
	writeFolded(&b, long)
	for _, line := range strings.Split(strings.TrimSuffix(b.String(), "\r\n"), "\r\n") {
		if len(line) > 75 {
			t.Fatalf("line of %d octets: %q", len(line), line)
		}
	}
	back := strings.Join(unfold([]byte(b.String())), "")
	if back != long {
		t.Fatalf("unfolded = %q", back)
	}
}
