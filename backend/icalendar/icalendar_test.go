package icalendar

import (
	"strings"
	"testing"
	"time"
)

func crlf(s string) []byte {
	return []byte(strings.ReplaceAll(strings.TrimPrefix(s, "\n"), "\n", "\r\n"))
}

func TestParseGoogleInvitation(t *testing.T) {
	cal, err := Parse(crlf(`
BEGIN:VCALENDAR
PRODID:-//Google Inc//Google Calendar 70.9054//EN
VERSION:2.0
METHOD:REQUEST
BEGIN:VTIMEZONE
TZID:Europe/Berlin
END:VTIMEZONE
BEGIN:VEVENT
DTSTART;TZID=Europe/Berlin:20261012T100000
DTEND;TZID=Europe/Berlin:20261012T113000
UID:abc123@google.com
ORGANIZER;CN=Anna Example:mailto:anna@example.com
ATTENDEE;CUTYPE=INDIVIDUAL;ROLE=REQ-PARTICIPANT;PARTSTAT=NEEDS-ACTION;CN="Bob, the Builder";X-NUM-GUESTS=0:mailto:bob@example.com
ATTENDEE;CN=carol@example.com:mailto:carol@example.com
DESCRIPTION:Agenda:\n1. Budget\, plans\n2. Questions; answers
LOCATION:Room 4\, second floor
SUMMARY:Quarterly review
STATUS:CONFIRMED
SEQUENCE:2
BEGIN:VALARM
ACTION:DISPLAY
DESCRIPTION:This is an event reminder
TRIGGER:-P0DT0H10M0S
END:VALARM
END:VEVENT
END:VCALENDAR
`))
	if err != nil {
		t.Fatal(err)
	}
	if cal.Method != "REQUEST" || len(cal.Events) != 1 {
		t.Fatalf("calendar = %+v", cal)
	}
	event := cal.Events[0]
	if event.Summary != "Quarterly review" || event.Location != "Room 4, second floor" {
		t.Fatalf("text = %q / %q", event.Summary, event.Location)
	}
	if event.Description != "Agenda:\n1. Budget, plans\n2. Questions; answers" {
		t.Fatalf("description = %q", event.Description)
	}
	wantStart := time.Date(2026, 10, 12, 8, 0, 0, 0, time.UTC)
	if !event.Start.Equal(wantStart) || !event.End.Equal(wantStart.Add(90*time.Minute)) {
		t.Fatalf("bounds = %v - %v", event.Start, event.End)
	}
	if event.AllDay || event.Floating || event.TimeZone != "Europe/Berlin" {
		t.Fatalf("flags = %+v", event)
	}
	if event.Organizer != (Person{Name: "Anna Example", Email: "anna@example.com"}) {
		t.Fatalf("organizer = %+v", event.Organizer)
	}
	if len(event.Attendees) != 2 || event.Attendees[0].Name != "Bob, the Builder" || event.Attendees[1] != (Person{Email: "carol@example.com"}) {
		t.Fatalf("attendees = %+v", event.Attendees)
	}
	if event.UID != "abc123@google.com" || event.Sequence != 2 || event.Status != "CONFIRMED" {
		t.Fatalf("ids = %+v", event)
	}
}

// Outlook writes a Windows zone name and its own definition of it; a name the
// table does not know is read from that definition.
func TestParseOutlookZoneDefinition(t *testing.T) {
	data := crlf(`
BEGIN:VCALENDAR
METHOD:REQUEST
PRODID:Microsoft Exchange Server 2010
VERSION:2.0
BEGIN:VTIMEZONE
TZID:Customized Time Zone
BEGIN:STANDARD
DTSTART:16010101T030000
TZOFFSETFROM:+0200
TZOFFSETTO:+0100
RRULE:FREQ=YEARLY;INTERVAL=1;BYDAY=-1SU;BYMONTH=10
END:STANDARD
BEGIN:DAYLIGHT
DTSTART:16010101T020000
TZOFFSETFROM:+0100
TZOFFSETTO:+0200
RRULE:FREQ=YEARLY;INTERVAL=1;BYDAY=-1SU;BYMONTH=3
END:DAYLIGHT
END:VTIMEZONE
BEGIN:VEVENT
ORGANIZER;CN=Max Muster:mailto:max@example.de
SUMMARY;LANGUAGE=de-DE:Abstimmung
DTSTART;TZID=Customized Time Zone:20261124T140000
DTEND;TZID=Customized Time Zone:20261124T150000
UID:040000008200E00074C5B7101A82E00800000000
RRULE:FREQ=WEEKLY;BYDAY=TU
LOCATION;LANGUAGE=de-DE:Microsoft Teams-Besprechung
END:VEVENT
BEGIN:VEVENT
SUMMARY:Sommer
DTSTART;TZID=Customized Time Zone:20260715T090000
DURATION:PT45M
UID:second
END:VEVENT
END:VCALENDAR
`)
	cal, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(cal.Events) != 2 {
		t.Fatalf("events = %+v", cal.Events)
	}
	winter := cal.Events[0]
	if want := time.Date(2026, 11, 24, 13, 0, 0, 0, time.UTC); !winter.Start.Equal(want) || !winter.End.Equal(want.Add(time.Hour)) {
		t.Fatalf("winter = %v - %v", winter.Start, winter.End)
	}
	if !winter.Recurring || winter.Floating || winter.TimeZone != "" {
		t.Fatalf("winter flags = %+v", winter)
	}
	summer := cal.Events[1]
	if want := time.Date(2026, 7, 15, 7, 0, 0, 0, time.UTC); !summer.Start.Equal(want) || !summer.End.Equal(want.Add(45*time.Minute)) {
		t.Fatalf("summer = %v - %v", summer.Start, summer.End)
	}
}

func TestParseWindowsZoneName(t *testing.T) {
	cal, err := Parse(crlf(`
BEGIN:VCALENDAR
BEGIN:VEVENT
DTSTART;TZID="W. Europe Standard Time":20260301T090000
DTEND;TZID="W. Europe Standard Time":20260301T100000
SUMMARY:Call
END:VEVENT
END:VCALENDAR
`))
	if err != nil {
		t.Fatal(err)
	}
	event := cal.Events[0]
	if want := time.Date(2026, 3, 1, 8, 0, 0, 0, time.UTC); !event.Start.Equal(want) {
		t.Fatalf("start = %v", event.Start)
	}
	if event.TimeZone != "Europe/Berlin" {
		t.Fatalf("zone = %q", event.TimeZone)
	}
}

func TestParseAllDayAndFloating(t *testing.T) {
	cal, err := Parse([]byte("BEGIN:VCALENDAR\nMETHOD:PUBLISH\nBEGIN:VEVENT\nDTSTART;VALUE=DATE:20261224\nDTEND;VALUE=DATE:20261227\nSUMMARY:Feiertage\nEND:VEVENT\n" +
		"BEGIN:VEVENT\nDTSTART;VALUE=DATE:20261231\nSUMMARY:Silvester\nEND:VEVENT\n" +
		"BEGIN:VEVENT\nDTSTART:20261101T083000\nDURATION:PT1H\nSUMMARY:Floating\nEND:VEVENT\n" +
		"BEGIN:VEVENT\nSUMMARY:No start\nEND:VEVENT\nEND:VCALENDAR\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(cal.Events) != 3 {
		t.Fatalf("events = %+v", cal.Events)
	}
	holidays := cal.Events[0]
	if !holidays.AllDay || !holidays.Start.Equal(time.Date(2026, 12, 24, 0, 0, 0, 0, time.UTC)) || !holidays.End.Equal(time.Date(2026, 12, 27, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("holidays = %+v", holidays)
	}
	eve := cal.Events[1]
	if !eve.AllDay || !eve.End.Equal(time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("one day = %+v", eve)
	}
	floating := cal.Events[2]
	if !floating.Floating || floating.Start != time.Date(2026, 11, 1, 8, 30, 0, 0, time.UTC) || floating.End.Sub(floating.Start) != time.Hour {
		t.Fatalf("floating = %+v", floating)
	}
}

func TestParseFoldedLinesAndCancel(t *testing.T) {
	cal, err := Parse(crlf(`
BEGIN:VCALENDAR
METHOD:CANCEL
BEGIN:VEVENT
UID:x
DTSTART:20261012T100000Z
DTEND:20261012T110000Z
SUMMARY:A very long title that a producer
 folded over two lines
DESCRIPTION:Line one\nLine
	two
STATUS:CANCELLED
END:VEVENT
END:VCALENDAR
`))
	if err != nil {
		t.Fatal(err)
	}
	event := cal.Events[0]
	if cal.Method != "CANCEL" || event.Status != "CANCELLED" {
		t.Fatalf("method = %q status = %q", cal.Method, event.Status)
	}
	if event.Summary != "A very long title that a producerfolded over two lines" {
		t.Fatalf("summary = %q", event.Summary)
	}
	if event.Description != "Line one\nLinetwo" {
		t.Fatalf("description = %q", event.Description)
	}
	if !event.Start.Equal(time.Date(2026, 10, 12, 10, 0, 0, 0, time.UTC)) {
		t.Fatalf("start = %v", event.Start)
	}
}

func TestParseRejectsNonCalendar(t *testing.T) {
	if _, err := Parse([]byte("hello world")); err != ErrNoCalendar {
		t.Fatalf("err = %v", err)
	}
}

func TestParseDuration(t *testing.T) {
	for value, want := range map[string]duration{
		"P1D":        {days: 1},
		"PT1H30M":    {seconds: 5400},
		"P2W":        {days: 14},
		"P1DT2H":     {days: 1, seconds: 7200},
		"-PT15M":     {negative: true, seconds: 900},
		"P0DT0H0M0S": {},
	} {
		got, ok := parseDuration(value)
		if !ok || got != want {
			t.Errorf("parseDuration(%q) = %+v, %v; want %+v", value, got, ok, want)
		}
	}
	for _, bad := range []string{"", "1H", "PT", "P1H", "PT1D", "P1"} {
		if _, ok := parseDuration(bad); ok && bad != "PT" {
			t.Errorf("parseDuration(%q) accepted", bad)
		}
	}
}

func TestNthWeekday(t *testing.T) {
	// The last Sunday of March 2026 is the 29th, of October the 25th; the
	// second Sunday of March 2026 is the 8th.
	if got := nthWeekday(2026, 3, time.Sunday, -1); got != 29 {
		t.Fatalf("last Sunday of March = %d", got)
	}
	if got := nthWeekday(2026, 10, time.Sunday, -1); got != 25 {
		t.Fatalf("last Sunday of October = %d", got)
	}
	if got := nthWeekday(2026, 3, time.Sunday, 2); got != 8 {
		t.Fatalf("second Sunday of March = %d", got)
	}
}

func TestParseRejoinsCharacterSplitAcrossFold(t *testing.T) {
	// "Café" with the two octets of é folded onto two lines.
	data := []byte("BEGIN:VCALENDAR\r\nBEGIN:VEVENT\r\nUID:a\\,b\r\nDTSTART:20261124T130000Z\r\nSUMMARY:Caf\xc3\r\n \xa9\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n")
	cal, err := Parse(data)
	if err != nil || len(cal.Events) != 1 {
		t.Fatalf("events = %d (%v)", len(cal.Events), err)
	}
	if got := cal.Events[0].Summary; got != "Café" {
		t.Fatalf("summary = %q, want Café", got)
	}
	if got := cal.Events[0].UID; got != "a,b" {
		t.Fatalf("uid = %q, want a,b", got)
	}
}

func TestParseOffsetRejectsOutOfRangeParts(t *testing.T) {
	if got, ok := parseOffset("+0130"); !ok || got != 90*time.Minute {
		t.Fatalf("+0130 = %v, %v", got, ok)
	}
	if got, ok := parseOffset("-043000"); !ok || got != -(4*time.Hour+30*time.Minute) {
		t.Fatalf("-043000 = %v, %v", got, ok)
	}
	for _, bad := range []string{"+9999", "+2400", "+0160", "+010060", "0100", "+1"} {
		if _, ok := parseOffset(bad); ok {
			t.Errorf("parseOffset(%q) accepted", bad)
		}
	}
}
