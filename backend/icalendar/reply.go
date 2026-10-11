// File overview: The answer to an invitation as iTIP defines it (RFC 5546,
// carried by mail per RFC 6047): a METHOD:REPLY object naming the event and the
// one attendee who answers, with their participation status.

package icalendar

import (
	"errors"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Participation statuses a reply may carry.
const (
	PartStatAccepted  = "ACCEPTED"
	PartStatTentative = "TENTATIVE"
	PartStatDeclined  = "DECLINED"
)

// ErrNotAnswerable is returned for an event a reply cannot name: one without
// a UID, which is how the organizer's calendar finds the event an answer is
// about, or without an organizer to send it to.
var ErrNotAnswerable = errors.New("icalendar: the invitation cannot be answered")

// BuildReply renders the reply an attendee sends the organizer. It restates
// the event's identity -- UID, RECURRENCE-ID, SEQUENCE -- and its time and
// title, which some clients show in the answer, and names only the attendee
// answering: a reply speaks for one person and must not restate anybody
// else's answer. The times are written in UTC (or as dates) so the object
// needs no VTIMEZONE of its own.
func BuildReply(event Event, attendee Person, partStat string, now time.Time) ([]byte, error) {
	if strings.TrimSpace(event.UID) == "" || strings.TrimSpace(event.Organizer.Email) == "" || strings.TrimSpace(attendee.Email) == "" {
		return nil, ErrNotAnswerable
	}
	switch partStat {
	case PartStatAccepted, PartStatTentative, PartStatDeclined:
	default:
		return nil, errors.New("icalendar: unknown participation status " + strconv.Quote(partStat))
	}
	var b strings.Builder
	line := func(value string) {
		writeFolded(&b, value)
	}
	line("BEGIN:VCALENDAR")
	line("PRODID:-//Rolltop//Rolltop//EN")
	line("VERSION:2.0")
	line("METHOD:REPLY")
	line("BEGIN:VEVENT")
	line("UID:" + escapeText(event.UID))
	line("DTSTAMP:" + now.UTC().Format("20060102T150405Z"))
	if event.Sequence > 0 {
		line("SEQUENCE:" + strconv.Itoa(event.Sequence))
	}
	if at := event.recurrenceID; at != nil {
		line(dateTimeProperty("RECURRENCE-ID", at.at, at.date, at.floating))
	}
	if !event.Start.IsZero() {
		line(dateTimeProperty("DTSTART", event.Start, event.AllDay, event.Floating))
		if !event.End.IsZero() && event.End.After(event.Start) {
			line(dateTimeProperty("DTEND", event.End, event.AllDay, event.Floating))
		}
	}
	if event.Summary != "" {
		line("SUMMARY:" + escapeText(event.Summary))
	}
	line(personProperty("ORGANIZER", event.Organizer, ""))
	line(personProperty("ATTENDEE", attendee, partStat))
	line("END:VEVENT")
	line("END:VCALENDAR")
	return []byte(b.String()), nil
}

func dateTimeProperty(name string, at time.Time, allDay, floating bool) string {
	switch {
	case allDay:
		return name + ";VALUE=DATE:" + at.UTC().Format("20060102")
	case floating:
		return name + ":" + at.UTC().Format("20060102T150405")
	default:
		return name + ":" + at.UTC().Format("20060102T150405Z")
	}
}

func personProperty(name string, person Person, partStat string) string {
	var b strings.Builder
	b.WriteString(name)
	if partStat != "" {
		b.WriteString(";PARTSTAT=" + partStat)
	}
	if cn := strings.TrimSpace(person.Name); cn != "" {
		b.WriteString(";CN=" + quoteParam(cn))
	}
	b.WriteString(":mailto:" + strings.TrimSpace(person.Email))
	return b.String()
}

// quoteParam quotes a parameter value that holds a character a bare one may
// not; a double quote cannot be written inside one at all and is dropped.
func quoteParam(value string) string {
	value = strings.Map(func(r rune) rune {
		if r == '"' || r < 0x20 {
			return -1
		}
		return r
	}, value)
	if strings.ContainsAny(value, ";:,") {
		return `"` + value + `"`
	}
	return value
}

// escapeText escapes a TEXT value (RFC 5545 3.3.11).
func escapeText(value string) string {
	replacer := strings.NewReplacer(`\`, `\\`, ";", `\;`, ",", `\,`, "\r\n", `\n`, "\n", `\n`, "\r", `\n`)
	return replacer.Replace(value)
}

// writeFolded writes one content line, folded at 75 octets without splitting
// a character (RFC 5545 3.1).
func writeFolded(b *strings.Builder, line string) {
	const limit = 75
	first := true
	for len(line) > 0 {
		max := limit
		if !first {
			max = limit - 1
		}
		if len(line) <= max {
			if !first {
				b.WriteByte(' ')
			}
			b.WriteString(line)
			break
		}
		cut := max
		for cut > 0 && !utf8.RuneStart(line[cut]) {
			cut--
		}
		if !first {
			b.WriteByte(' ')
		}
		b.WriteString(line[:cut])
		b.WriteString("\r\n")
		line = line[cut:]
		first = false
	}
	b.WriteString("\r\n")
}
