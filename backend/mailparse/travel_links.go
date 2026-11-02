// File overview: The "add to calendar" links a ticket shop puts in its order
// confirmation.
//
// Shops that publish no schema.org markup and attach no .ics still offer the
// event for the reader's calendar, as a Google Calendar or Outlook.com link
// whose query string is the event: title, start, end, place and notes. EVENTIM
// does this under "Im Kalender speichern". Those parameters are structured --
// the same fields the provider's own form fills in -- so they are read as such.
//
// A calendar link is also exactly what a newsletter puts under an event it is
// advertising, so on its own it proves nothing was bought. Two things have to
// stand beside it: the message speaks of tickets (ticketEvidence, over the
// subject and the body), and it names an order or booking number
// (orderReferenceRE). An advertisement carries the first and never the second.
//
// Links that would have to be fetched -- the .ics behind EVENTIM's "Apple
// Kalender" button -- are never followed: nothing in the mail is loaded from
// the network to read it.

package mailparse

import (
	"html"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

var (
	travelHrefRE = regexp.MustCompile(`(?i)href\s*=\s*["']([^"']+)["']`)
	// orderReferenceRE is an order or booking number stated as one.
	orderReferenceRE = regexp.MustCompile(`(?i)\b(?:Bestellnummer|Bestell-Nr\.?|Ordernummer|Auftragsnummer|Buchungsnummer|Buchungscode|Order number|Order no\.?|Booking reference|Booking number|Confirmation number)\s*:?\s*([A-Z0-9][A-Z0-9-]{4,24})\b`)
	// writtenLocalTimeRE is a day and a clock time as a German or English
	// confirmation writes them: "03.11.2026, 20:00", "03.11.2026 um 20:00".
	writtenLocalTimeRE = regexp.MustCompile(`\b(\d{1,2})\.(\d{1,2})\.(\d{4}),?\s*(?:um\s+)?(\d{1,2}):(\d{2})\b`)
	// shopTitleSuffixRE is the sales label a shop appends to the event's name
	// in its calendar link: "… - TICKETS IN DRESDEN".
	shopTitleSuffixRE = regexp.MustCompile(`(?i)\s+-\s+tickets\s+in\s+.+$`)
)

const (
	// maxTravelLinks bounds how many calendar links one message is read for.
	// A confirmation offers each event in two or three calendars.
	maxTravelLinks = 12
	// maxTravelLinkText bounds the body text read for the evidence around them.
	maxTravelLinkText = 64 * 1024
)

// travelFromCalendarLinks reads the add-to-calendar links in a message's HTML.
func travelFromCalendarLinks(markup, subject, from string) []TravelBooking {
	if !containsFold(markup, "calendar.google.com") && !containsFold(markup, "outlook.live.com") &&
		!containsFold(markup, "outlook.office.com") {
		return nil
	}
	text := stripHTML(markup)
	if len(text) > maxTravelLinkText {
		cut := maxTravelLinkText
		for cut > 0 && !utf8.RuneStart(text[cut]) {
			cut--
		}
		text = text[:cut]
	}
	if !ticketEvidence(subject, nil) && !ticketEvidence(text, nil) {
		return nil
	}
	reference := orderReference(subject)
	if reference == "" {
		reference = orderReference(text)
	}
	if reference == "" {
		return nil
	}
	provider := registrableDomain(addressOnly(from))
	var out []TravelBooking
	seen := 0
	for _, match := range travelHrefRE.FindAllStringSubmatch(markup, -1) {
		if seen >= maxTravelLinks {
			break
		}
		link, err := url.Parse(html.UnescapeString(strings.TrimSpace(match[1])))
		if err != nil {
			continue
		}
		seg, ok := segmentFromCalendarLink(link)
		if !ok {
			continue
		}
		seen++
		seg.Start = writtenWallClock(text, seg.Start, seg.StartAt)
		if seg.End != "" {
			seg.End = writtenEndWallClock(seg)
		}
		out = append(out, TravelBooking{Kind: TravelEvent, Reference: reference, Provider: provider, Segments: []TravelSegment{seg}})
	}
	return withoutFloatingDuplicates(out)
}

// withoutFloatingDuplicates drops a link that names an event another link of
// the same message names as an instant. Shops write the same event into every
// calendar's link, and not always the same way: EVENTIM's Outlook.com link
// carries the UTC digits of its Google link without the "Z", which read as a
// floating time is an hour off. The instant is the precise statement.
func withoutFloatingDuplicates(bookings []TravelBooking) []TravelBooking {
	exact := map[string]bool{}
	for _, booking := range bookings {
		if seg := booking.Segments[0]; !seg.StartAt.IsZero() {
			exact[compactTravelKey(seg.Title)] = true
		}
	}
	kept := bookings[:0]
	for _, booking := range bookings {
		seg := booking.Segments[0]
		if seg.StartAt.IsZero() && exact[compactTravelKey(seg.Title)] {
			continue
		}
		kept = append(kept, booking)
	}
	return kept
}

// orderReference is the first order or booking number the text states. The
// labels are matched in any case, so the value must carry a digit: "Deine
// Bestellnummer findest du im Kundenkonto" names no number, and taking
// "findest" for one would let the advertisement through the very gate this
// exists to be.
func orderReference(text string) string {
	for _, match := range orderReferenceRE.FindAllStringSubmatch(text, 8) {
		if strings.ContainsAny(match[1], "0123456789") {
			return match[1]
		}
	}
	return ""
}

// segmentFromCalendarLink reads one Google Calendar or Outlook.com link.
func segmentFromCalendarLink(link *url.URL) (TravelSegment, bool) {
	host := strings.ToLower(link.Hostname())
	q := link.Query()
	seg := TravelSegment{Kind: TravelEvent, Source: TravelSourceLink}
	var location, notes string
	switch {
	case host == "calendar.google.com" || host == "www.google.com" && strings.HasPrefix(link.Path, "/calendar"):
		// /calendar/render?action=TEMPLATE, /calendar/event?action=TEMPLATE
		// and /calendar/r/eventedit all take the same parameters.
		seg.Title = q.Get("text")
		location, notes = q.Get("location"), q.Get("details")
		start, end, _ := strings.Cut(q.Get("dates"), "/")
		zone := (*time.Location)(nil)
		if ctz := q.Get("ctz"); ctz != "" {
			if loc, err := time.LoadLocation(ctz); err == nil {
				zone = loc
			}
		}
		seg.Start, seg.StartAt = compactCalendarTime(start, zone)
		seg.End, seg.EndAt = compactCalendarTime(end, zone)
	case strings.HasPrefix(host, "outlook.") && strings.Contains(strings.ToLower(link.Path+link.RawQuery), "calendar"):
		seg.Title = q.Get("subject")
		location, notes = q.Get("location"), q.Get("body")
		seg.Start, seg.StartAt = compactCalendarTime(q.Get("startdt"), nil)
		seg.End, seg.EndAt = compactCalendarTime(q.Get("enddt"), nil)
		if strings.EqualFold(q.Get("allday"), "true") && len(seg.Start) >= len(travelDateLayout) {
			seg.Start, seg.StartAt, seg.End, seg.EndAt = seg.Start[:len(travelDateLayout)], time.Time{}, "", time.Time{}
		}
	default:
		return TravelSegment{}, false
	}
	seg.Title = strings.TrimSpace(shopTitleSuffixRE.ReplaceAllString(seg.Title, ""))
	if seg.Title == "" || seg.Start == "" {
		return TravelSegment{}, false
	}
	// A calendar link's whole-day end is exclusive, as in iCalendar; a leg
	// names its last day.
	if len(seg.Start) == len(travelDateLayout) && len(seg.End) == len(travelDateLayout) {
		exclusive, err := time.Parse(travelDateLayout, seg.End)
		seg.End = ""
		if last := exclusive.AddDate(0, 0, -1).Format(travelDateLayout); err == nil && last > seg.Start {
			seg.End = last
		}
	}
	// "Filmtheater Schauburg, Königsbrücker Straße 55, 01099 DRESDEN": the
	// venue is the first part and the address the rest.
	if venue, address, found := strings.Cut(location, ","); found {
		seg.Venue, seg.Address = strings.TrimSpace(venue), strings.TrimSpace(address)
	} else {
		seg.Venue = strings.TrimSpace(location)
	}
	seg.Seat = seatFromCalendarNotes(notes)
	return seg, true
}

// compactCalendarTime reads the times calendar links carry: "20261103T190000Z"
// (an instant), "20261103T190000" (floating, or in zone when the link names
// one), "20261103" (a whole day), and the ISO spellings Outlook also accepts.
func compactCalendarTime(raw string, zone *time.Location) (string, time.Time) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return "", time.Time{}
	}
	if t, err := time.Parse("20060102T150405Z", value); err == nil {
		return t.Format(travelLocalLayout), t
	}
	if t, err := time.Parse("20060102T150405", value); err == nil {
		if zone != nil {
			in := time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), 0, 0, zone)
			return t.Format(travelLocalLayout), in
		}
		return t.Format(travelLocalLayout), time.Time{}
	}
	if t, err := time.Parse("20060102", value); err == nil {
		return t.Format(travelDateLayout), time.Time{}
	}
	local, at, ok := parseTravelTime(value, zone)
	if !ok {
		return "", time.Time{}
	}
	return local, at
}

// writtenWallClock replaces a start read as UTC with the local time the same
// message writes for it, when it writes one. A shop's link says 19:00Z and its
// text says "03.11.2026, 20:00"; both are the same moment, and the second is
// the clock the venue and the reader go by. Only a written time that is the
// instant moved by a whole quarter hour of at most fourteen hours counts -- the
// shapes a real UTC offset takes -- so a date elsewhere in the mail (the order
// date, an invoice date) cannot be mistaken for it.
func writtenWallClock(text, start string, at time.Time) string {
	if at.IsZero() || len(start) != len(travelLocalLayout) {
		return start
	}
	utc := at.UTC()
	base := time.Date(utc.Year(), utc.Month(), utc.Day(), utc.Hour(), utc.Minute(), 0, 0, time.UTC)
	best, bestOffset := "", time.Duration(-1)
	for _, match := range writtenLocalTimeRE.FindAllStringSubmatch(text, 32) {
		day, _ := strconv.Atoi(match[1])
		month, _ := strconv.Atoi(match[2])
		year, _ := strconv.Atoi(match[3])
		hour, _ := strconv.Atoi(match[4])
		minute, _ := strconv.Atoi(match[5])
		if month < 1 || month > 12 || day < 1 || day > 31 || hour > 23 || minute > 59 {
			continue
		}
		written := time.Date(year, time.Month(month), day, hour, minute, 0, 0, time.UTC)
		offset := written.Sub(base)
		if offset%(15*time.Minute) != 0 || offset > 14*time.Hour || offset < -14*time.Hour {
			continue
		}
		// With several events in one order, every written time is within
		// fourteen hours of every instant; the nearest one is this event's,
		// since the others sit a show's distance further away.
		if offset < 0 {
			offset = -offset
		}
		if bestOffset < 0 || offset < bestOffset {
			best, bestOffset = written.Format(travelLocalLayout), offset
		}
	}
	if best == "" {
		return start
	}
	return best
}

// writtenEndWallClock moves the end by the offset the start was found at, so a
// leg whose start was re-expressed in local time does not end in UTC.
func writtenEndWallClock(seg TravelSegment) string {
	if seg.StartAt.IsZero() || seg.EndAt.IsZero() {
		return seg.End
	}
	start, err := time.Parse(travelLocalLayout, seg.Start)
	if err != nil {
		return seg.End
	}
	return start.Add(seg.EndAt.Sub(seg.StartAt)).Format(travelLocalLayout)
}

// seatFromCalendarNotes reads the seat lines a shop writes into the event's
// notes under "Platzinformationen:" (or "Seat information:"), one per ticket,
// and says them once with their count.
func seatFromCalendarNotes(notes string) string {
	lower := strings.ToLower(notes)
	at := strings.Index(lower, "platzinformationen:")
	if at < 0 {
		at = strings.Index(lower, "seat information:")
	}
	if at < 0 {
		return ""
	}
	rest := notes[strings.Index(notes[at:], ":")+at+1:]
	counts := map[string]int{}
	var order []string
	blank := 0
	for _, line := range strings.Split(strings.ReplaceAll(rest, "\r", ""), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			blank++
			if blank >= 2 && len(order) > 0 {
				break
			}
			continue
		}
		blank = 0
		if counts[line] == 0 {
			order = append(order, line)
		}
		counts[line]++
		if len(order) >= 4 {
			break
		}
	}
	parts := make([]string, 0, len(order))
	for _, line := range order {
		if counts[line] > 1 {
			parts = append(parts, strconv.Itoa(counts[line])+"× "+line)
		} else {
			parts = append(parts, line)
		}
	}
	return strings.Join(parts, "; ")
}
