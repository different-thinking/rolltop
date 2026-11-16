// File overview: Order confirmations from booking systems that put the ticket
// in a fixed text layout and nowhere else.
//
// Some shops publish no schema.org markup, attach no pass or calendar object
// and offer no add-to-calendar link the mail itself carries: the ticket is a
// PDF behind a download link (Reservix's print@home), or the calendar entry is
// an .ics behind a button (atavolo's "Kalendereintrag"), and neither is ever
// fetched. What these mails do carry is the same machine-written layout every
// time, from one booking system. That layout is what is read here, and only
// from mail the system itself is known to have sent -- the DB rule
// (travel_db.go), not a guess from words:
//
//   - Reservix (reservix.de, sent from its own domain): an order number, then
//     the event as "<Name>, 15. November 2026, 18:00 Uhr" followed by the
//     ticket lines priced in EUR. An event line counts only with a ticket line
//     under it, so a date in the letter text or an advertised event in the
//     footer is never taken for the order.
//   - atavolo (the reservation system behind many venues' "Reservierungs-
//     bestätigung", which signs every mail "powered by atavolo.com" and links
//     its own calendar entry): labelled lines "Datum: 14.11.2026" and
//     "Uhrzeit: 18:00 Uhr - 19:00 Uhr". The mail is sent from the venue's own
//     address, so the system is recognised by its signature, not the sender.
//
// Both layouts are read off the plain-text part, line by line. Neither names
// the zone its times are in, so they stay floating: the venue's clock, which a
// reader booking a court or a show in their own town reads the same way.

package mailparse

import (
	"net/mail"
	"regexp"
	"strconv"
	"strings"
)

var (
	// reservixEventLineRE is Reservix's event line. The time is optional: a
	// day ticket names the day alone.
	reservixEventLineRE = regexp.MustCompile(`^\s*(\S.*?),\s*(\d{1,2})\.\s*(Januar|Februar|März|Maerz|April|Mai|Juni|Juli|August|September|Oktober|November|Dezember)\s+(20\d{2})(?:,\s*(\d{1,2}):(\d{2})\s*Uhr)?\s*$`)
	// reservixTicketLineRE is one priced line of the order: "1 x 1. Kategorie
	// - Normalpreis      23,50 EUR".
	reservixTicketLineRE = regexp.MustCompile(`^\s*(\d{1,3})\s*x\s+(\S.*?)\s+\d{1,6}(?:\.\d{3})*,\d{2}\s*(?:EUR|€)\s*$`)

	atavoloDateRE   = regexp.MustCompile(`^\s*Datum:\s*(\d{1,2})\.(\d{1,2})\.(20\d{2})\s*$`)
	atavoloTimeRE   = regexp.MustCompile(`^\s*Uhrzeit:\s*(\d{1,2})[:.](\d{2})(?:\s*Uhr)?(?:\s*[-–]\s*(\d{1,2})[:.](\d{2})(?:\s*Uhr)?)?\s*$`)
	atavoloPeopleRE = regexp.MustCompile(`^\s*Personen:\s*(\d{1,3})\s*$`)
	atavoloPlaceRE  = regexp.MustCompile(`^\s*(?:Feld|Court|Platz|Bahn|Tisch|Bereich|Raum):\s*(\S.*?)\s*$`)
	postalCityRE    = regexp.MustCompile(`^\s*\d{4,5}\s+\S`)
)

const (
	// maxTravelShopText bounds the plain text a layout reader walks. An order
	// confirmation is a few kilobytes; anything far beyond that is a digest.
	maxTravelShopText = 64 * 1024
	// maxShopSeatLines bounds how many ticket lines are named on a leg.
	maxShopSeatLines = 4
)

var germanMonths = map[string]int{
	"januar": 1, "februar": 2, "märz": 3, "maerz": 3, "april": 4, "mai": 5, "juni": 6,
	"juli": 7, "august": 8, "september": 9, "oktober": 10, "november": 11, "dezember": 12,
}

// travelFromShopText runs the layout readers that apply to the message.
func travelFromShopText(in TravelInput) []TravelBooking {
	text := in.Text
	if text == "" {
		return nil
	}
	if len(text) > maxTravelShopText {
		text = truncateUTF8(text, maxTravelShopText)
	}
	lines := strings.Split(strings.ReplaceAll(text, "\r", ""), "\n")
	var out []TravelBooking
	if reservixSender(in.From) {
		if booking, ok := travelFromReservix(lines, text, in.Subject); ok {
			out = append(out, booking)
		}
	}
	if atavoloMail(text, in.HTML) {
		if booking, ok := travelFromAtavolo(lines, in.Subject, in.From); ok {
			out = append(out, booking)
		}
	}
	return out
}

func truncateUTF8(text string, limit int) string {
	cut := limit
	for cut > 0 && cut < len(text) && (text[cut]&0xC0) == 0x80 {
		cut--
	}
	return text[:cut]
}

// reservixSender reports mail from Reservix's own domains.
func reservixSender(from string) bool {
	switch registrableDomain(addressOnly(from)) {
	case "reservix.de", "reservix.com", "reservix.at", "reservix.ch":
		return true
	default:
		return false
	}
}

// travelFromReservix reads a Reservix order confirmation.
func travelFromReservix(lines []string, text, subject string) (TravelBooking, bool) {
	reference := orderReference(subject)
	if reference == "" {
		reference = orderReference(text)
	}
	if reference == "" {
		return TravelBooking{}, false
	}
	cancelled := cancellationWords(subject)
	var segments []TravelSegment
	var current *TravelSegment
	var seats []string
	flush := func() {
		if current != nil && len(seats) > 0 {
			current.Seat = strings.Join(seats, "; ")
			segments = append(segments, *current)
		}
		current, seats = nil, nil
	}
	for _, line := range lines {
		if match := reservixEventLineRE.FindStringSubmatch(line); match != nil {
			flush()
			seg, ok := reservixSegment(match)
			if !ok {
				continue
			}
			seg.Cancelled = cancelled
			current = &seg
			continue
		}
		if current == nil {
			continue
		}
		match := reservixTicketLineRE.FindStringSubmatch(line)
		if match == nil || shopFeeLine(match[2]) {
			continue
		}
		// An event line only counts with a ticket under it; the fees are
		// priced lines too, and say nothing about admission.
		if len(seats) < maxShopSeatLines {
			label := strings.Join(strings.Fields(match[2]), " ")
			if count, _ := strconv.Atoi(match[1]); count > 1 {
				label = strconv.Itoa(count) + "× " + label
			}
			seats = append(seats, label)
		} else if len(seats) == maxShopSeatLines {
			seats = append(seats, "…")
		}
	}
	flush()
	if len(segments) == 0 {
		return TravelBooking{}, false
	}
	return TravelBooking{Kind: TravelEvent, Reference: reference, Provider: "Reservix", Segments: segments}, true
}

func reservixSegment(match []string) (TravelSegment, bool) {
	day, _ := strconv.Atoi(match[2])
	month := germanMonths[strings.ToLower(match[3])]
	year, _ := strconv.Atoi(match[4])
	if month == 0 || day < 1 || day > 31 {
		return TravelSegment{}, false
	}
	seg := TravelSegment{Kind: TravelEvent, Source: TravelSourceText, Title: strings.TrimSpace(match[1])}
	if match[5] != "" {
		hour, _ := strconv.Atoi(match[5])
		minute, _ := strconv.Atoi(match[6])
		if hour > 23 || minute > 59 {
			return TravelSegment{}, false
		}
		seg.Start = shopLocalTime(year, month, day, hour, minute)
	} else {
		seg.Start = shopDay(year, month, day)
	}
	return seg, true
}

// shopFeeLine reports a priced order line that is not an admission: the
// service fee, postage, an insurance.
func shopFeeLine(label string) bool {
	lower := strings.ToLower(label)
	return containsAnyWord(lower, "gebühr", "gebuehr", "versand", "porto", "versicherung", "shipping")
}

func cancellationWords(subject string) bool {
	lower := strings.ToLower(subject)
	return strings.Contains(lower, "storn") || strings.Contains(lower, "cancel") || strings.Contains(lower, "abgesagt")
}

// atavoloMail reports a message the atavolo reservation system wrote: it signs
// its text "powered by atavolo.com" and links its own calendar entry.
func atavoloMail(text, markup string) bool {
	return containsFold(text, "powered by atavolo.com") || containsFold(markup, "login.atavolo.com/")
}

// travelFromAtavolo reads an atavolo reservation confirmation.
func travelFromAtavolo(lines []string, subject, from string) (TravelBooking, bool) {
	lowerSubject := strings.ToLower(subject)
	if !containsAnyWord(lowerSubject, "reservierung", "buchung", "reservation", "booking", "termin") {
		return TravelBooking{}, false
	}
	var day, month, year, hour, minute, endHour, endMinute = 0, 0, 0, -1, 0, -1, 0
	people, place := "", ""
	for _, line := range lines {
		if match := atavoloDateRE.FindStringSubmatch(line); match != nil && year == 0 {
			day, _ = strconv.Atoi(match[1])
			month, _ = strconv.Atoi(match[2])
			year, _ = strconv.Atoi(match[3])
			continue
		}
		if match := atavoloTimeRE.FindStringSubmatch(line); match != nil && hour < 0 {
			hour, _ = strconv.Atoi(match[1])
			minute, _ = strconv.Atoi(match[2])
			if match[3] != "" {
				endHour, _ = strconv.Atoi(match[3])
				endMinute, _ = strconv.Atoi(match[4])
			}
			continue
		}
		if match := atavoloPeopleRE.FindStringSubmatch(line); match != nil && people == "" {
			people = match[1]
			continue
		}
		if match := atavoloPlaceRE.FindStringSubmatch(line); match != nil && place == "" {
			// "iSquash | 38€ pro Stunde": the price is not where one plays.
			place, _, _ = strings.Cut(match[1], "|")
			place = strings.TrimSpace(place)
		}
	}
	if year == 0 || month < 1 || month > 12 || day < 1 || day > 31 {
		return TravelBooking{}, false
	}
	name := senderDisplayName(from)
	venue := strings.TrimSpace(strings.SplitN(name, "|", 2)[0])
	if venue == "" {
		venue = registrableDomain(addressOnly(from))
	}
	title := name
	if title == "" {
		title = "Reservierung " + venue
	}
	seg := TravelSegment{
		Kind:      TravelEvent,
		Source:    TravelSourceText,
		Title:     title,
		Venue:     venue,
		Address:   signatureAddress(lines, name),
		Cancelled: cancellationWords(subject),
	}
	if hour >= 0 && hour <= 23 && minute <= 59 {
		seg.Start = shopLocalTime(year, month, day, hour, minute)
		if endHour >= 0 && endHour <= 23 && endMinute <= 59 {
			seg.End = shopLocalTime(year, month, day, endHour, endMinute)
		}
	} else {
		seg.Start = shopDay(year, month, day)
	}
	var seat []string
	if place != "" {
		seat = append(seat, place)
	}
	if people != "" {
		seat = append(seat, people+" Personen")
	}
	seg.Seat = strings.Join(seat, ", ")
	return TravelBooking{Kind: TravelEvent, Provider: venue, Segments: []TravelSegment{seg}}, true
}

// senderDisplayName is the name a From header carries, which for atavolo is
// the venue and the area booked: "BallsportARENA Dresden | Squash".
func senderDisplayName(from string) string {
	if addr, err := mail.ParseAddress(strings.TrimSpace(from)); err == nil {
		return strings.TrimSpace(addr.Name)
	}
	return ""
}

// signatureAddress reads the venue's address from the signature atavolo
// closes with: the sender's name on a line of its own, the street, and the
// postal code with the town. Anything else is no address.
func signatureAddress(lines []string, name string) string {
	if name == "" {
		return ""
	}
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.TrimSpace(lines[i]) != name || i+2 >= len(lines) {
			continue
		}
		street, town := strings.TrimSpace(lines[i+1]), strings.TrimSpace(lines[i+2])
		if street == "" || strings.Contains(street, ":") || !strings.ContainsAny(street, "0123456789") || !postalCityRE.MatchString(town) {
			return ""
		}
		return street + ", " + town
	}
	return ""
}

func shopLocalTime(year, month, day, hour, minute int) string {
	return strconv.Itoa(year) + "-" + twoDigits(month) + "-" + twoDigits(day) + "T" + twoDigits(hour) + ":" + twoDigits(minute)
}

func shopDay(year, month, day int) string {
	return strconv.Itoa(year) + "-" + twoDigits(month) + "-" + twoDigits(day)
}

func twoDigits(n int) string {
	if n < 10 {
		return "0" + strconv.Itoa(n)
	}
	return strconv.Itoa(n)
}
