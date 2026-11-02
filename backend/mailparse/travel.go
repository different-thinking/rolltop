// File overview: Tickets the mail carries -- flights, train journeys, coach
// trips and event admissions -- reduced to bookings with legs a calendar can
// hold.
//
// The rule is the parcel rule: a ticket is only taken when something in the
// message says what it is. Four things qualify, and nothing else does:
//
//   - schema.org reservations embedded in the HTML (travel_schema.go), which
//     airlines, ticket shops and rail operators publish precisely so that mail
//     clients can draw a trip card;
//   - an Apple Wallet pass (travel_pkpass.go), whose pass.json names its own
//     kind, its date and, on newer passes, every field semantically;
//   - a Deutsche Bahn ticket or seat reservation PDF (travel_db.go), whose
//     itinerary table is a fixed layout;
//   - a published calendar object attached to a message that calls itself a
//     ticket or booking (travel_ics.go).
//
// A subject line saying "Ihre Reise" is not one of them. Words are how a
// newsletter advertises a trip; a booking states a departure.
//
// Like parcels, a booking is not an answer on the message. One journey is talked
// about by several messages -- the booking, the ticket, the seat reservation,
// the delay notice -- and one message can carry the way there and the way back,
// so the stored shape is a trip with legs, and messages hang off the trip.

package mailparse

import (
	"sort"
	"strings"
	"time"
	"unicode"
	// The departure airport names the zone a flight's times are written in, and
	// a server image without zoneinfo would otherwise silently fall back to
	// floating times for every flight.
	_ "time/tzdata"
)

// Travel kinds. The value is stored and is part of a trip's identity, so it is
// not renamed without a migration.
const (
	TravelFlight = "flight"
	TravelTrain  = "train"
	TravelBus    = "bus"
	TravelEvent  = "event"
)

// Where a leg was read from. It is shown nowhere; it is kept so a wrong answer
// can be traced to the reader that produced it.
const (
	TravelSourceSchema = "schema"
	TravelSourcePass   = "pkpass"
	TravelSourcePDF    = "pdf"
	TravelSourceICS    = "ics"
)

const (
	// maxTravelBookingsPerMessage bounds what one message contributes. A
	// booking for a family is one booking with several seats, not several
	// bookings; a message that seems to name more than this is a listing.
	maxTravelBookingsPerMessage = 8
	// maxTravelSegmentsPerBooking bounds the legs of one booking. A journey
	// there and back with two changes each way is six.
	maxTravelSegmentsPerBooking = 12
	// maxTravelFieldRunes bounds every text field, because every one of them
	// came from a stranger.
	maxTravelFieldRunes = 160
	// travelLocalLayout is a wall-clock time; travelDateLayout a whole day.
	travelLocalLayout = "2006-01-02T15:04"
	travelDateLayout  = "2006-01-02"
)

// TravelBooking is one booking as one message describes it: the reference a
// provider would ask for, and the legs.
type TravelBooking struct {
	Kind string
	// Reference is the booking code, order number or confirmation number the
	// provider issued, or empty when the message names none.
	Reference string
	// Provider is who sells or runs it: an airline, a rail operator, a venue's
	// ticket shop.
	Provider string
	Segments []TravelSegment
	// keyHint replaces the reference in the identity when the reference is not
	// something to show a reader -- a calendar object's UID.
	keyHint string
}

// TravelSegment is one leg: one flight, one train, one coach, or one event.
type TravelSegment struct {
	Kind string
	// Number is the train or flight number as a timetable prints it, "ICE 597"
	// or "LH 400"; empty for an event.
	Number string
	// Title is the leg's own name: the number for a vehicle, the event's name
	// for an admission.
	Title    string
	Provider string
	From     string
	FromCode string
	To       string
	ToCode   string
	// Venue and Address are where an event is.
	Venue   string
	Address string
	// Start and End are wall-clock times, "2006-01-02T15:04", or a plain day,
	// "2006-01-02", for an event that names no time. End may be empty.
	Start string
	End   string
	// StartAt and EndAt are the same moments as instants, or zero when the
	// message did not say which zone it meant. A zero instant is a floating
	// time: 08:29 wherever the reader is, which for a train in the reader's
	// own country is what was meant.
	StartAt time.Time
	EndAt   time.Time
	// Seat is the seat as the ticket prints it: "Coach 7, seat 45".
	Seat string
	// SeatOnly marks a seat reservation without a ticket. It is a leg of its
	// own until a ticket for the same train turns up, which it then joins.
	SeatOnly  bool
	Cancelled bool
	Source    string
}

// AllDay reports a leg that names a day and no time.
func (s TravelSegment) AllDay() bool {
	return len(s.Start) == len(travelDateLayout)
}

// Key is what makes the same leg named by two messages -- or by the PDF and the
// markup of one message -- one leg. The station names are deliberately not part
// of it: "Berlin Hbf" and "Berlin Hbf (tief)" are one departure.
func (s TravelSegment) Key() string {
	name := compactTravelKey(s.Number)
	if name == "" {
		name = compactTravelKey(s.Title)
	}
	start := s.Start
	if len(start) > len(travelLocalLayout) {
		start = start[:len(travelLocalLayout)]
	}
	return s.Kind + "|" + name + "|" + start
}

// Key is the booking's identity. A booking with a reference is that reference
// for that kind of travel, which is what lets the booking confirmation and the
// ticket that follows it two days later be one trip. Without one, the first
// leg stands for the booking.
func (b TravelBooking) Key() string {
	if b.keyHint != "" {
		return b.Kind + "|#" + b.keyHint
	}
	if ref := compactTravelKey(b.Reference); ref != "" {
		return b.Kind + "|" + ref
	}
	if len(b.Segments) == 0 {
		return ""
	}
	return b.Kind + "|@" + b.Segments[0].Key()
}

// compactTravelKey folds a name into the shape identities compare: upper case,
// letters and digits only.
func compactTravelKey(value string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(value) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// ValidTravelKind keeps a kind this build does not know out of the database.
func ValidTravelKind(kind string) bool {
	switch kind {
	case TravelFlight, TravelTrain, TravelBus, TravelEvent:
		return true
	default:
		return false
	}
}

// TravelInput is everything the readers may look at. It is assembled from a
// parsed message on the fetch path and from a bounded scan on the backfill, so
// both reach the same answer about the same message.
type TravelInput struct {
	Subject string
	From    string
	// HTML is the markup the structured data is embedded in. It is read for
	// script blocks only.
	HTML string
	// Files are the attachments a reader can use: passes, calendar objects and
	// ticket PDFs, decoded. travelDocumentCandidate decides which.
	Files []TravelFile
	Sent  time.Time
}

// TravelFile is one attachment a ticket reader is allowed to open.
type TravelFile struct {
	Filename    string
	ContentType string
	Data        []byte
}

// travelDocumentCandidate reports the attachments worth opening for a ticket.
// Passes and calendar objects are small and say what they are. A PDF is only
// opened when the sender is a rail operator this build reads, or the file calls
// itself a ticket: a text extractor per invoice and newsletter PDF is a cost the
// fetch path has no reason to pay.
func travelDocumentCandidate(filename, contentType, from string) bool {
	if isPassFile(filename, contentType) || IsCalendarPart(contentType, filename) {
		return true
	}
	if !isPDFFile(filename, contentType) {
		return false
	}
	if dbSender(from) {
		return true
	}
	name := strings.ToLower(filename)
	for _, word := range []string{"ticket", "fahrkarte", "reservierung", "reservation"} {
		if strings.Contains(name, word) {
			return true
		}
	}
	return false
}

func isPDFFile(filename, contentType string) bool {
	return strings.Contains(strings.ToLower(contentType), "pdf") ||
		strings.HasSuffix(strings.ToLower(strings.TrimSpace(filename)), ".pdf")
}

// TravelFiles reduces a parsed message's attachments to the ones a ticket
// reader may open.
func (p ParsedMessage) TravelFiles() []TravelFile {
	var out []TravelFile
	for _, file := range p.Files {
		if len(out) >= maxTravelFiles {
			break
		}
		if len(file.Data) == 0 || len(file.Data) > maxTravelFileBytes {
			continue
		}
		if !travelDocumentCandidate(file.Filename, file.ContentType, p.From) {
			continue
		}
		out = append(out, TravelFile{Filename: file.Filename, ContentType: file.ContentType, Data: file.Data})
	}
	return out
}

const (
	// maxTravelFiles bounds how many attachments one message is read for.
	maxTravelFiles = 8
	// maxTravelFileBytes bounds one of them. A ticket PDF with a barcode is a
	// few hundred kilobytes and a pass well under one megabyte.
	maxTravelFileBytes = 4 << 20
)

// ExtractTravelBookings reads every source a message offers and merges what
// they say. Each reader answers for its own evidence; merging is what makes a
// DB mail's markup and its PDF one journey rather than two.
func ExtractTravelBookings(in TravelInput) []TravelBooking {
	var found []TravelBooking
	if in.HTML != "" {
		found = append(found, travelFromSchema(in.HTML)...)
	}
	ticketWords := ticketEvidence(in.Subject, in.Files)
	for _, file := range in.Files {
		switch {
		case isPassFile(file.Filename, file.ContentType):
			if booking, ok := travelFromPass(file.Data); ok {
				found = append(found, booking)
			}
		case IsCalendarPart(file.ContentType, file.Filename):
			if ticketWords {
				found = append(found, travelFromICS(file.Data)...)
			}
		case isPDFFile(file.Filename, file.ContentType):
			if booking, ok := travelFromDBPDF(file.Data, in.From, in.Sent); ok {
				found = append(found, booking)
			}
		}
	}
	return mergeTravelBookings(found)
}

// ticketEvidence is the one place words count, and only to decide whether a
// calendar object that came with the message is the ticket's date or somebody's
// meeting. The object itself still has to state the event.
func ticketEvidence(subject string, files []TravelFile) bool {
	words := []string{"ticket", "eintritt", "boarding", "bordkarte", "fahrkarte", "reservierung",
		"reservation", "booking", "buchung"}
	haystack := strings.ToLower(subject)
	for _, file := range files {
		haystack += " " + strings.ToLower(file.Filename)
	}
	for _, word := range words {
		if strings.Contains(haystack, word) {
			return true
		}
	}
	return false
}

// mergeTravelBookings joins bookings that are the same booking -- the same
// reference, or a shared leg -- and inside each the legs that are the same leg.
// The first source to name a field wins it, except that a seat is collected
// from whichever source has one, and a leg is only seat-only when every source
// that named it said so.
func mergeTravelBookings(found []TravelBooking) []TravelBooking {
	var out []TravelBooking
	for _, booking := range found {
		booking = cleanTravelBooking(booking)
		if len(booking.Segments) == 0 {
			continue
		}
		merged := false
		for i := range out {
			if sameTravelBooking(out[i], booking) {
				out[i] = joinTravelBookings(out[i], booking)
				merged = true
				break
			}
		}
		if !merged {
			if len(out) >= maxTravelBookingsPerMessage {
				break
			}
			out = append(out, booking)
		}
	}
	for i := range out {
		sort.SliceStable(out[i].Segments, func(a, b int) bool {
			return out[i].Segments[a].Start < out[i].Segments[b].Start
		})
	}
	return out
}

func sameTravelBooking(a, b TravelBooking) bool {
	if a.Kind != b.Kind {
		return false
	}
	if ra, rb := compactTravelKey(a.Reference), compactTravelKey(b.Reference); ra != "" && ra == rb {
		return true
	}
	for _, left := range a.Segments {
		for _, right := range b.Segments {
			if left.Key() == right.Key() {
				return true
			}
		}
	}
	return false
}

func joinTravelBookings(a, b TravelBooking) TravelBooking {
	if a.Reference == "" {
		a.Reference = b.Reference
	}
	if a.Provider == "" {
		a.Provider = b.Provider
	}
	if a.keyHint == "" && a.Reference == "" {
		a.keyHint = b.keyHint
	}
	for _, seg := range b.Segments {
		joined := false
		for i := range a.Segments {
			if a.Segments[i].Key() == seg.Key() {
				a.Segments[i] = joinTravelSegments(a.Segments[i], seg)
				joined = true
				break
			}
		}
		if !joined && len(a.Segments) < maxTravelSegmentsPerBooking {
			a.Segments = append(a.Segments, seg)
		}
	}
	return a
}

func joinTravelSegments(a, b TravelSegment) TravelSegment {
	fill := func(dst *string, src string) {
		if *dst == "" {
			*dst = src
		}
	}
	fill(&a.Number, b.Number)
	fill(&a.Title, b.Title)
	fill(&a.Provider, b.Provider)
	fill(&a.From, b.From)
	fill(&a.FromCode, b.FromCode)
	fill(&a.To, b.To)
	fill(&a.ToCode, b.ToCode)
	fill(&a.Venue, b.Venue)
	fill(&a.Address, b.Address)
	fill(&a.End, b.End)
	fill(&a.Seat, b.Seat)
	if a.StartAt.IsZero() {
		a.StartAt = b.StartAt
	}
	if a.EndAt.IsZero() {
		a.EndAt = b.EndAt
	}
	a.SeatOnly = a.SeatOnly && b.SeatOnly
	a.Cancelled = a.Cancelled || b.Cancelled
	return a
}

// cleanTravelBooking bounds and repairs everything a booking carries, and drops
// legs no calendar could hold: a leg without a start is not a leg.
func cleanTravelBooking(b TravelBooking) TravelBooking {
	b.Reference = cleanTravelField(b.Reference)
	b.Provider = cleanTravelField(b.Provider)
	b.keyHint = cleanTravelField(b.keyHint)
	if !ValidTravelKind(b.Kind) {
		b.Segments = nil
		return b
	}
	kept := make([]TravelSegment, 0, len(b.Segments))
	for _, seg := range b.Segments {
		if len(kept) >= maxTravelSegmentsPerBooking {
			break
		}
		seg.Kind = b.Kind
		seg.Number = cleanTravelField(seg.Number)
		seg.Title = cleanTravelField(seg.Title)
		seg.Provider = cleanTravelField(seg.Provider)
		seg.From = cleanTravelField(seg.From)
		seg.FromCode = strings.ToUpper(cleanTravelField(seg.FromCode))
		seg.To = cleanTravelField(seg.To)
		seg.ToCode = strings.ToUpper(cleanTravelField(seg.ToCode))
		seg.Venue = cleanTravelField(seg.Venue)
		seg.Address = cleanTravelField(seg.Address)
		seg.Seat = cleanTravelField(seg.Seat)
		if !validTravelTime(seg.Start) {
			continue
		}
		if seg.End != "" && (!validTravelTime(seg.End) || seg.End < seg.Start) {
			seg.End = ""
			seg.EndAt = time.Time{}
		}
		if seg.Title == "" {
			seg.Title = seg.Number
		}
		if seg.Title == "" && seg.Kind != TravelEvent && (seg.From != "" || seg.To != "") {
			seg.Title = strings.TrimSpace(seg.From + " → " + seg.To)
		}
		if seg.Title == "" {
			continue
		}
		if seg.Provider == "" {
			seg.Provider = b.Provider
		}
		kept = append(kept, seg)
	}
	b.Segments = kept
	return b
}

func cleanTravelField(value string) string {
	value = strings.Join(strings.Fields(SanitizeText(value)), " ")
	if runes := []rune(value); len(runes) > maxTravelFieldRunes {
		value = string(runes[:maxTravelFieldRunes])
	}
	return value
}

func validTravelTime(value string) bool {
	switch len(value) {
	case len(travelLocalLayout):
		_, err := time.Parse(travelLocalLayout, value)
		return err == nil
	case len(travelDateLayout):
		_, err := time.Parse(travelDateLayout, value)
		return err == nil
	default:
		return false
	}
}

// parseTravelTime reads a timestamp the way the structured sources write one:
// ISO 8601 with or without seconds, offset or zone. The wall clock is returned
// as written; the instant only when the text named its offset, or when a zone is
// supplied for a time that did not.
func parseTravelTime(raw string, zone *time.Location) (local string, at time.Time, ok bool) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return "", time.Time{}, false
	}
	withOffset := []string{
		time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04Z07:00", "2006-01-02T15:04:05Z0700",
		"2006-01-02T15:04Z0700", "2006-01-02 15:04:05Z07:00", "2006-01-02 15:04Z07:00",
	}
	for _, layout := range withOffset {
		if t, err := time.Parse(layout, value); err == nil {
			return t.Format(travelLocalLayout), t, true
		}
	}
	withoutOffset := []string{"2006-01-02T15:04:05", "2006-01-02T15:04", "2006-01-02 15:04:05", "2006-01-02 15:04"}
	for _, layout := range withoutOffset {
		if t, err := time.Parse(layout, value); err == nil {
			if zone != nil {
				in := time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), 0, 0, zone)
				return t.Format(travelLocalLayout), in, true
			}
			return t.Format(travelLocalLayout), time.Time{}, true
		}
	}
	if t, err := time.Parse(travelDateLayout, value); err == nil {
		return t.Format(travelDateLayout), time.Time{}, true
	}
	return "", time.Time{}, false
}

// dbSender reports mail from Deutsche Bahn's own domains. It decides whether a
// PDF is worth opening, never whether it is a ticket -- the layout decides that.
func dbSender(from string) bool {
	switch registrableDomain(addressOnly(from)) {
	case "bahn.de", "bahn.com", "db.de", "deutschebahn.com":
		return true
	default:
		return false
	}
}

// addressOnly takes the address out of a From header that may carry a name.
func addressOnly(from string) string {
	value := strings.TrimSpace(from)
	if open := strings.LastIndex(value, "<"); open >= 0 {
		if end := strings.Index(value[open:], ">"); end > 0 {
			return value[open+1 : open+end]
		}
	}
	return value
}
