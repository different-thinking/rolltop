package mailparse

import (
	"archive/zip"
	"bytes"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func travelSent(t *testing.T) time.Time {
	t.Helper()
	berlin, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Fatalf("zone data unavailable: %v", err)
	}
	return time.Date(2026, time.November, 2, 10, 0, 0, 0, berlin)
}

func onlyBooking(t *testing.T, bookings []TravelBooking) TravelBooking {
	t.Helper()
	if len(bookings) != 1 {
		t.Fatalf("want exactly one booking, got %d: %+v", len(bookings), bookings)
	}
	return bookings[0]
}

func ldScript(body string) string {
	return `<html><head><script type="application/ld+json">` + body + `</script></head><body>Hallo</body></html>`
}

func TestTravelSchemaFlightUsesOffsetAndAirportZone(t *testing.T) {
	html := ldScript(`{
	  "@context": "http://schema.org",
	  "@type": "FlightReservation",
	  "reservationNumber": "ABC123",
	  "reservationStatus": "http://schema.org/ReservationConfirmed",
	  "airplaneSeat": "12A",
	  "reservationFor": {
	    "@type": "Flight",
	    "flightNumber": "400",
	    "airline": {"@type": "Airline", "name": "Lufthansa", "iataCode": "LH"},
	    "departureAirport": {"@type": "Airport", "name": "Frankfurt", "iataCode": "FRA"},
	    "departureTime": "2026-11-14T10:35:00",
	    "arrivalAirport": {"@type": "Airport", "name": "New York JFK", "iataCode": "JFK"},
	    "arrivalTime": "2026-11-14T13:20:00-05:00"
	  }
	}`)
	booking := onlyBooking(t, ExtractTravelBookings(TravelInput{HTML: html, Sent: travelSent(t)}))
	if booking.Kind != TravelFlight || booking.Reference != "ABC123" || booking.Provider != "Lufthansa" {
		t.Fatalf("booking = %+v", booking)
	}
	seg := booking.Segments[0]
	if seg.Number != "LH 400" || seg.FromCode != "FRA" || seg.ToCode != "JFK" || seg.Seat != "12A" {
		t.Fatalf("segment = %+v", seg)
	}
	if seg.Start != "2026-11-14T10:35" {
		t.Errorf("start = %q", seg.Start)
	}
	// The departure named no offset; Frankfurt's zone supplies it.
	if want := time.Date(2026, 11, 14, 9, 35, 0, 0, time.UTC); !seg.StartAt.Equal(want) {
		t.Errorf("start instant = %v, want %v", seg.StartAt, want)
	}
	if want := time.Date(2026, 11, 14, 18, 20, 0, 0, time.UTC); !seg.EndAt.Equal(want) {
		t.Errorf("end instant = %v, want %v", seg.EndAt, want)
	}
}

func TestTravelSchemaTrainRoundTripIsOneBooking(t *testing.T) {
	html := ldScript(`[
	  {"@context": "http://schema.org", "@type": "TrainReservation", "reservationNumber": "Q7X9ZT",
	   "reservedTicket": {"@type": "Ticket", "ticketedSeat": {"@type": "Seat", "seatNumber": "45", "seatSection": "25"}},
	   "reservationFor": {"@type": "TrainTrip", "trainName": "ICE", "trainNumber": "597",
	     "provider": {"@type": "Organization", "name": "Deutsche Bahn"},
	     "departureStation": {"@type": "TrainStation", "name": "Berlin Hbf"}, "departureTime": "2026-11-14T08:29:00+01:00",
	     "arrivalStation": {"@type": "TrainStation", "name": "München Hbf"}, "arrivalTime": "2026-11-14T12:32:00+01:00"}},
	  {"@context": "http://schema.org", "@type": "TrainReservation", "reservationNumber": "Q7X9ZT",
	   "reservationFor": {"@type": "TrainTrip", "trainNumber": "ICE 1006",
	     "departureStation": {"@type": "TrainStation", "name": "München Hbf"}, "departureTime": "2026-11-16T17:28:00+01:00",
	     "arrivalStation": {"@type": "TrainStation", "name": "Berlin Hbf"}, "arrivalTime": "2026-11-16T21:31:00+01:00"}}
	]`)
	booking := onlyBooking(t, ExtractTravelBookings(TravelInput{HTML: html}))
	if booking.Reference != "Q7X9ZT" || len(booking.Segments) != 2 {
		t.Fatalf("booking = %+v", booking)
	}
	out, back := booking.Segments[0], booking.Segments[1]
	if out.Number != "ICE 597" || out.From != "Berlin Hbf" || out.To != "München Hbf" || out.Seat != "Coach 25, seat 45" {
		t.Errorf("outbound = %+v", out)
	}
	if back.Number != "ICE 1006" || back.Start != "2026-11-16T17:28" {
		t.Errorf("return = %+v", back)
	}
}

func TestTravelSchemaEventAndCancellation(t *testing.T) {
	html := ldScript(`{"@context": "http://schema.org", "@graph": [{
	  "@type": "EventReservation", "reservationNumber": "E-998877",
	  "reservationStatus": "https://schema.org/ReservationCancelled",
	  "reservedTicket": {"@type": "Ticket", "ticketedSeat": {"seatSection": "B", "seatRow": "12", "seatNumber": "7"}},
	  "reservationFor": {"@type": "MusicEvent", "name": "Rolltop Live", "startDate": "2026-12-05T20:00:00+01:00",
	    "location": {"@type": "Place", "name": "Uber Arena",
	      "address": {"@type": "PostalAddress", "streetAddress": "Uber-Platz 1", "postalCode": "10243", "addressLocality": "Berlin"}}}
	}]}`)
	booking := onlyBooking(t, ExtractTravelBookings(TravelInput{HTML: html}))
	seg := booking.Segments[0]
	if booking.Kind != TravelEvent || seg.Title != "Rolltop Live" || seg.Venue != "Uber Arena" {
		t.Fatalf("booking = %+v", booking)
	}
	if seg.Address != "Uber-Platz 1, 10243 Berlin" || seg.Seat != "Block B, row 12, seat 7" || !seg.Cancelled {
		t.Errorf("segment = %+v", seg)
	}
}

func TestTravelSchemaIgnoresAdvertisedEvents(t *testing.T) {
	html := ldScript(`{"@context": "http://schema.org", "@type": "Event", "name": "Sale", "startDate": "2026-12-01"}`)
	if got := ExtractTravelBookings(TravelInput{Subject: "Tickets jetzt sichern", HTML: html}); len(got) != 0 {
		t.Fatalf("an advertised event is not a booking: %+v", got)
	}
}

const dbTicketText = `
                                   Online-Ticket
  Hinfahrt: Berlin Hbf -> München Hbf          Flexpreis
  Auftragsnummer: 482910375512
  Gültigkeit: ab 14.11.2026

  Ihre Reiseverbindung und Reservierung Hinfahrt am 14.11.2026
  Halt                         Datum    Zeit        Gleis    Produkte     Reservierung
  Berlin Hbf                   14.11.   ab 08:29    7        ICE 597      Wg. 25, Pl. 45, 1 Fenster
  Erfurt Hbf                   14.11.   an 10:12    3
  Erfurt Hbf                   14.11.   ab 10:20    2        ICE 1001     Wg. 9, Pl. 61, 63
  München Hbf                  14.11.   an 12:32    21

  Ihre Reiseverbindung und Reservierung Rückfahrt am 16.11.2026
  Halt                         Datum    Zeit        Gleis    Produkte     Reservierung
  München Hbf                  16.11.   ab 17:28    18       ICE 1006
  Berlin Hbf                   16.11.   an 21:31    12
  DB Fernverkehr AG
`

func TestTravelDBTicketTable(t *testing.T) {
	booking, ok := travelFromDBText(dbTicketText, "DB Reisezentrum <noreply@bahn.de>", travelSent(t))
	if !ok {
		t.Fatal("the ticket table was not read")
	}
	if booking.Reference != "482910375512" || len(booking.Segments) != 3 {
		t.Fatalf("booking = %+v", booking)
	}
	first, second, back := booking.Segments[0], booking.Segments[1], booking.Segments[2]
	if first.Number != "ICE 597" || first.From != "Berlin Hbf" || first.To != "Erfurt Hbf" || first.Start != "2026-11-14T08:29" || first.End != "2026-11-14T10:12" {
		t.Errorf("first leg = %+v", first)
	}
	if first.Seat != "Coach 25, seat 45" {
		t.Errorf("first seat = %q (the 1 before Fenster is a count, not a seat)", first.Seat)
	}
	if second.Seat != "Coach 9, seats 61, 63" {
		t.Errorf("second seat = %q", second.Seat)
	}
	if back.Number != "ICE 1006" || back.Seat != "" || back.Start != "2026-11-16T17:28" {
		t.Errorf("return = %+v", back)
	}
	if want := time.Date(2026, 11, 14, 7, 29, 0, 0, time.UTC); !first.StartAt.Equal(want) {
		t.Errorf("start instant = %v, want %v", first.StartAt, want)
	}
	if first.SeatOnly {
		t.Error("a ticket is not a seat-only reservation")
	}
}

func TestTravelDBSeatReservationOnly(t *testing.T) {
	text := `
  Reservierung
  Ihre Reservierung am 20.11.2026
  Halt              Datum   Zeit       Gleis  Produkte   Reservierung
  Hamburg Hbf       20.11.  ab 06:01   13     ICE 1673   Wg. 3, Pl. 111
  Köln Hbf          20.11.  an 10:05   4
  www.bahn.de`
	booking, ok := travelFromDBText(text, "someone@example.com", travelSent(t))
	if !ok || len(booking.Segments) != 1 {
		t.Fatalf("booking = %+v ok=%v", booking, ok)
	}
	if !booking.Segments[0].SeatOnly || booking.Segments[0].Seat != "Coach 3, seat 111" {
		t.Errorf("segment = %+v", booking.Segments[0])
	}
}

func TestTravelDBNeedsDB(t *testing.T) {
	text := strings.ReplaceAll(dbTicketText, "DB Fernverkehr AG", "")
	if _, ok := travelFromDBText(text, "shop@example.com", travelSent(t)); ok {
		t.Fatal("a table nobody says is DB's is not read as DB's")
	}
}

func TestTravelMergesMarkupAndPDF(t *testing.T) {
	original := pdfTextExtractor
	defer func() { pdfTextExtractor = original }()
	pdfTextExtractor = func([]byte) (string, error) { return dbTicketText, nil }
	html := ldScript(`{"@context": "http://schema.org", "@type": "TrainReservation", "reservationNumber": "482910375512",
	   "reservationFor": {"@type": "TrainTrip", "trainNumber": "ICE 597",
	     "departureStation": {"name": "Berlin Hbf"}, "departureTime": "2026-11-14T08:29:00+01:00",
	     "arrivalStation": {"name": "Erfurt Hbf"}, "arrivalTime": "2026-11-14T10:12:00+01:00"}}`)
	in := TravelInput{
		From:  "noreply@bahn.de",
		HTML:  html,
		Files: []TravelFile{{Filename: "Ticket_482910375512.pdf", ContentType: "application/pdf", Data: []byte("%PDF")}},
		Sent:  travelSent(t),
	}
	booking := onlyBooking(t, ExtractTravelBookings(in))
	if len(booking.Segments) != 3 {
		t.Fatalf("markup and PDF should be one journey of three legs, got %+v", booking.Segments)
	}
	if booking.Segments[0].Seat != "Coach 25, seat 45" {
		t.Errorf("the seat from the PDF should join the leg from the markup: %+v", booking.Segments[0])
	}
}

func buildPass(t *testing.T, passJSON string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create("pass.json")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte(passJSON)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestTravelPassBoardingPassWithSemantics(t *testing.T) {
	data := buildPass(t, `{
	  "formatVersion": 1, "organizationName": "Eurowings", "description": "Boarding pass",
	  "relevantDate": "2026-11-20T06:10+01:00",
	  "boardingPass": {"transitType": "PKTransitTypeAir",
	    "primaryFields": [{"key": "origin", "label": "Köln", "value": "CGN"}, {"key": "destination", "label": "Palma", "value": "PMI"}]},
	  "semantics": {"airlineCode": "EW", "flightNumber": 582, "departureAirportCode": "CGN", "destinationAirportCode": "PMI",
	    "originalDepartureDate": "2026-11-20T06:40+01:00", "originalArrivalDate": "2026-11-20T09:05+01:00",
	    "confirmationNumber": "K8L2MN", "seats": [{"seatRow": "14", "seatNumber": "C"}]}
	}`)
	booking := onlyBooking(t, ExtractTravelBookings(TravelInput{
		Files: []TravelFile{{Filename: "boardingpass.pkpass", ContentType: "application/vnd.apple.pkpass", Data: data}},
	}))
	seg := booking.Segments[0]
	if booking.Kind != TravelFlight || booking.Reference != "K8L2MN" || seg.Number != "EW 582" {
		t.Fatalf("booking = %+v", booking)
	}
	if seg.FromCode != "CGN" || seg.ToCode != "PMI" || seg.Start != "2026-11-20T06:40" || seg.Seat != "14C" {
		t.Errorf("segment = %+v", seg)
	}
}

func TestTravelPassEventTicketWithoutSemantics(t *testing.T) {
	data := buildPass(t, `{
	  "organizationName": "Eventim", "description": "Konzert: Rolltop Live",
	  "relevantDate": "2026-12-05T19:00:00+01:00",
	  "eventTicket": {"secondaryFields": [{"key": "venue", "label": "Venue", "value": "Uber Arena"}]}
	}`)
	booking := onlyBooking(t, ExtractTravelBookings(TravelInput{
		Files: []TravelFile{{Filename: "ticket.pkpass", ContentType: "application/octet-stream", Data: data}},
	}))
	seg := booking.Segments[0]
	if booking.Kind != TravelEvent || seg.Title != "Konzert: Rolltop Live" || seg.Venue != "Uber Arena" || seg.Start != "2026-12-05T19:00" {
		t.Fatalf("booking = %+v", booking)
	}
}

func TestTravelPassCouponIsNotTravel(t *testing.T) {
	data := buildPass(t, `{"organizationName": "Shop", "relevantDate": "2026-12-05T19:00:00+01:00", "coupon": {}}`)
	if got := ExtractTravelBookings(TravelInput{Files: []TravelFile{{Filename: "c.pkpass", Data: data}}}); len(got) != 0 {
		t.Fatalf("a coupon is not travel: %+v", got)
	}
}

const ticketICS = "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nMETHOD:PUBLISH\r\nBEGIN:VEVENT\r\nUID:evt-42@tickets.example\r\n" +
	"DTSTART:20261205T190000Z\r\nDTEND:20261205T220000Z\r\nSUMMARY:Rolltop Live\r\nLOCATION:Uber Arena\\, Berlin\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"

func TestTravelICSNeedsTicketEvidence(t *testing.T) {
	file := TravelFile{Filename: "event.ics", ContentType: "text/calendar", Data: []byte(ticketICS)}
	if got := ExtractTravelBookings(TravelInput{Subject: "Termin", Files: []TravelFile{file}}); len(got) != 0 {
		t.Fatalf("a calendar object in a mail that is no ticket is not a ticket: %+v", got)
	}
	booking := onlyBooking(t, ExtractTravelBookings(TravelInput{Subject: "Ihre Tickets für Rolltop Live", Files: []TravelFile{file}}))
	seg := booking.Segments[0]
	if seg.Title != "Rolltop Live" || seg.Venue != "Uber Arena, Berlin" || seg.StartAt.IsZero() {
		t.Fatalf("segment = %+v", seg)
	}
	if booking.Key() != "event|#evt-42@tickets.example" {
		t.Errorf("key = %q", booking.Key())
	}
}

func TestTravelScanReadsStoredMessage(t *testing.T) {
	html := ldScript(`{"@context": "http://schema.org", "@type": "EventReservation", "reservationNumber": "E1",
	  "reservationFor": {"@type": "Event", "name": "Theater", "startDate": "2026-12-01T19:30:00+01:00", "location": "Stadttheater"}}`)
	raw := "From: Tickets <tickets@example.com>\r\nSubject: Ihre Tickets\r\nDate: Mon, 02 Nov 2026 10:00:00 +0100\r\n" +
		"MIME-Version: 1.0\r\nContent-Type: multipart/alternative; boundary=b\r\n\r\n" +
		"--b\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nHallo\r\n" +
		"--b\r\nContent-Type: text/html; charset=utf-8\r\n\r\n" + html + "\r\n--b--\r\n"
	bookings, err := TravelBookingsReaderScan(strings.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	booking := onlyBooking(t, bookings)
	if booking.Segments[0].Title != "Theater" || booking.Segments[0].Venue != "Stadttheater" {
		t.Fatalf("booking = %+v", booking)
	}
	parsed, err := Parse([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if len(parsed.Travel) != 1 || parsed.Travel[0].Key() != booking.Key() {
		t.Fatalf("fetch path and backfill disagree: %+v vs %+v", parsed.Travel, bookings)
	}
}

// The booking date can fall in the year before the journey; the table's days
// take the year the ticket writes beside the same day, not the first year on
// the page.
func TestTravelDBYearComesFromTheJourneyDay(t *testing.T) {
	text := `
  Buchungsdatum: 28.12.2025
  Hinfahrt am 03.01.2026
  Halt              Datum   Zeit       Gleis  Produkte   Reservierung
  Berlin Hbf        03.01.  ab 08:29   7      ICE 597
  München Hbf       03.01.  an 12:32   21
  Online-Ticket DB Fernverkehr AG`
	booking, ok := travelFromDBText(text, "noreply@bahn.de", time.Date(2025, 12, 28, 10, 0, 0, 0, time.UTC))
	if !ok || booking.Segments[0].Start != "2026-01-03T08:29" {
		t.Fatalf("booking = %+v ok=%v", booking, ok)
	}
	// With no full date at all, the journey is the next such day after the mail.
	bare := strings.ReplaceAll(strings.ReplaceAll(text, "Buchungsdatum: 28.12.2025", ""), "Hinfahrt am 03.01.2026", "")
	booking, ok = travelFromDBText(bare, "noreply@bahn.de", time.Date(2025, 12, 28, 10, 0, 0, 0, time.UTC))
	if !ok || booking.Segments[0].Start != "2026-01-03T08:29" {
		t.Fatalf("fallback booking = %+v ok=%v", booking, ok)
	}
}

func dbBookingICS(method string) string {
	return "BEGIN:VCALENDAR\r\nVERSION:2.0\r\n" + method +
		"BEGIN:VEVENT\r\nUID:out-482910375512@bahn.de\r\nDTSTART;TZID=Europe/Berlin:20261114T082900\r\n" +
		"DTEND;TZID=Europe/Berlin:20261114T123200\r\nSUMMARY:Berlin Hbf -> München Hbf\r\nLOCATION:Berlin Hbf\r\n" +
		"DESCRIPTION:Auftragsnummer: 482910375512\\nICE 597 Wagen 25\\, Platz 45\r\n" +
		"ORGANIZER:mailto:noreply@bahn.de\r\nATTENDEE:mailto:reader@example.com\r\nEND:VEVENT\r\n" +
		"BEGIN:VEVENT\r\nUID:back-482910375512@bahn.de\r\nDTSTART;TZID=Europe/Berlin:20261116T172800\r\n" +
		"DTEND;TZID=Europe/Berlin:20261116T213100\r\nSUMMARY:München Hbf -> Berlin Hbf\r\nLOCATION:München Hbf\r\n" +
		"DESCRIPTION:ICE 1006\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
}

// DB attaches the booking's journeys as a calendar object: both are train legs
// of the booking, read whatever the object's method, and the outbound one is
// the same leg as the PDF's first train.
func TestTravelDBBookingICSIsTrainLegs(t *testing.T) {
	file := TravelFile{Filename: "Buchung.ics", ContentType: "text/calendar", Data: []byte(dbBookingICS("METHOD:REQUEST\r\n"))}
	booking := onlyBooking(t, ExtractTravelBookings(TravelInput{
		Subject: "Ihre Buchungsbestätigung", From: "DB Reisezentrum <noreply@bahn.de>", Files: []TravelFile{file},
	}))
	if booking.Kind != TravelTrain || booking.Reference != "482910375512" || len(booking.Segments) != 2 {
		t.Fatalf("booking = %+v", booking)
	}
	out, back := booking.Segments[0], booking.Segments[1]
	if out.Number != "ICE 597" || out.From != "Berlin Hbf" || out.To != "München Hbf" || out.Seat != "Coach 25, seat 45" {
		t.Errorf("outbound = %+v", out)
	}
	if out.ICalUID != "out-482910375512@bahn.de" || out.Start != "2026-11-14T08:29" || out.SeatOnly {
		t.Errorf("outbound = %+v", out)
	}
	if back.Number != "ICE 1006" || back.From != "München Hbf" {
		t.Errorf("return = %+v", back)
	}

	original := pdfTextExtractor
	defer func() { pdfTextExtractor = original }()
	pdfTextExtractor = func([]byte) (string, error) { return dbTicketText, nil }
	pdf := TravelFile{Filename: "Ticket.pdf", ContentType: "application/pdf", Data: []byte("%PDF")}
	booking = onlyBooking(t, ExtractTravelBookings(TravelInput{
		Subject: "Ihre Buchungsbestätigung", From: "noreply@bahn.de", Files: []TravelFile{file, pdf}, Sent: travelSent(t),
	}))
	if len(booking.Segments) != 3 {
		t.Fatalf("the .ics and the PDF should be one journey of three legs, got %+v", booking.Segments)
	}
	if booking.Segments[0].End != "2026-11-14T10:12" || booking.Segments[0].ICalUID == "" {
		t.Errorf("the PDF decides the first train's end, the .ics adds its UID: %+v", booking.Segments[0])
	}
}

// A seat reservation's calendar object names the train by its route only; the
// subject says it is a reservation, and the minute joins it to the PDF's leg.
func TestTravelDBReservationICS(t *testing.T) {
	ics := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nBEGIN:VEVENT\r\nUID:res-1@bahn.de\r\n" +
		"DTSTART:20261120T050100Z\r\nDTEND:20261120T090500Z\r\nSUMMARY:Hamburg Hbf – Köln Hbf\r\n" +
		"DESCRIPTION:Wagen 3\\, Platz 111\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
	file := TravelFile{Filename: "Reservierung.ics", ContentType: "text/calendar", Data: []byte(ics)}
	booking := onlyBooking(t, ExtractTravelBookings(TravelInput{
		Subject: "Ihre Reservierung", From: "noreply@bahn.de", Files: []TravelFile{file},
	}))
	seg := booking.Segments[0]
	if !seg.SeatOnly || seg.Seat != "Coach 3, seat 111" || seg.Start != "2026-11-20T06:01" || seg.From != "Hamburg Hbf" {
		t.Fatalf("segment = %+v", seg)
	}

	original := pdfTextExtractor
	defer func() { pdfTextExtractor = original }()
	pdfTextExtractor = func([]byte) (string, error) {
		return "Reservierung\nHalt  Datum  Zeit  Gleis  Produkte  Reservierung\nHamburg Hbf  20.11.  ab 06:01  13  ICE 1673  Wg. 3, Pl. 111\nKöln Hbf  20.11.  an 10:05  4\nwww.bahn.de", nil
	}
	pdf := TravelFile{Filename: "Reservierung.pdf", ContentType: "application/pdf", Data: []byte("%PDF")}
	booking = onlyBooking(t, ExtractTravelBookings(TravelInput{
		Subject: "Ihre Reservierung", From: "noreply@bahn.de", Files: []TravelFile{file, pdf}, Sent: travelSent(t),
	}))
	if len(booking.Segments) != 1 || booking.Segments[0].Number != "ICE 1673" || booking.Segments[0].ICalUID != "res-1@bahn.de" {
		t.Fatalf("the .ics and the PDF should be one leg: %+v", booking.Segments)
	}
}

// The subject of a DB booking overrides the PDF's fare-word reading.
func TestTravelDBSubjectDecidesSeatOnly(t *testing.T) {
	original := pdfTextExtractor
	defer func() { pdfTextExtractor = original }()
	pdfTextExtractor = func([]byte) (string, error) {
		return "Ihre Reiseverbindung und Reservierung\nHalt  Datum  Zeit  Gleis  Produkte  Reservierung\nBerlin Hbf  14.11.2026  ab 08:29  7  ICE 597  Wg. 25, Pl. 45\nMünchen Hbf  14.11.2026  an 12:32  21\nDB Fernverkehr AG", nil
	}
	pdf := TravelFile{Filename: "Ticket.pdf", ContentType: "application/pdf", Data: []byte("%PDF")}
	booking := onlyBooking(t, ExtractTravelBookings(TravelInput{
		Subject: "Ihre Buchungsbestätigung (Auftrag 482910375512)", From: "noreply@bahn.de", Files: []TravelFile{pdf}, Sent: travelSent(t),
	}))
	if booking.Segments[0].SeatOnly {
		t.Fatalf("a booking confirmation is a ticket: %+v", booking.Segments[0])
	}
}

func readTestICS(t *testing.T, name string) TravelFile {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return TravelFile{Filename: name, ContentType: "text/calendar", Data: data}
}

// The calendar export DB attaches to a booking (one journey there with a change,
// one back with a change and a seat), exactly as DB writes it -- the check-in
// token redacted. Each train is a leg; the first journey's times carry no zone
// and are Berlin's.
func TestTravelDBRealBookingICS(t *testing.T) {
	booking := onlyBooking(t, ExtractTravelBookings(TravelInput{
		Subject: "Buchungsbestätigung Deutsche Bahn (Auftrag: 916319593981)", From: "DB Fernverkehr AG <buchungsbestaetigung@bahn.de>",
		Files: []TravelFile{readTestICS(t, "db_hinrueckfahrt.ics")},
	}))
	if booking.Reference != "916319593981" {
		t.Errorf("the order number in the subject is the booking's reference: %q", booking.Reference)
	}
	type want struct{ number, from, to, start, end, seat string }
	wants := []want{
		{"IC 1914", "Dresden-Neustadt", "Leipzig Hbf", "2026-09-25T09:29", "2026-09-25T10:31", "1st class"},
		{"RE 15 (74824)", "Leipzig Hbf", "Jena Paradies", "2026-09-25T11:02", "2026-09-25T12:12", "1st class"},
		{"RE 3 (3964)", "Jena West", "Erfurt Hbf", "2026-09-26T19:08", "2026-09-26T19:40", "1st class"},
		{"ICE 1655", "Erfurt Hbf", "Dresden-Neustadt", "2026-09-26T19:49", "2026-09-26T21:38", "Coach 12, seat 51 (1st class)"},
	}
	if len(booking.Segments) != len(wants) {
		t.Fatalf("legs = %+v", booking.Segments)
	}
	for i, w := range wants {
		got := booking.Segments[i]
		if got.Number != w.number || got.From != w.from || got.To != w.to || got.Start != w.start || got.End != w.end || got.Seat != w.seat {
			t.Errorf("leg %d = %+v, want %+v", i, got, w)
		}
		if got.SeatOnly {
			t.Errorf("leg %d of a booking is not seat-only", i)
		}
	}
	if want := time.Date(2026, 9, 25, 7, 29, 0, 0, time.UTC); !booking.Segments[0].StartAt.Equal(want) {
		t.Errorf("a zoneless DB time is Berlin's: %v, want %v", booking.Segments[0].StartAt, want)
	}
	if booking.Segments[0].ICalUID != "736b025d-df6a-4772-917d-d0d81cd9704d@bahn.de" || booking.Segments[2].ICalUID != "2469c843-bece-4e7a-b135-58ce359a9fdf@bahn.de" {
		t.Errorf("each journey's first train carries its event's UID: %q / %q", booking.Segments[0].ICalUID, booking.Segments[2].ICalUID)
	}
}

// The calendar export DB attaches to a seat reservation: one train, its seat,
// and a subject that says it is a reservation.
func TestTravelDBRealReservationICS(t *testing.T) {
	booking := onlyBooking(t, ExtractTravelBookings(TravelInput{
		Subject: "Reservierungsbestätigung Deutsche Bahn (Auftrag: 194938114535)", From: "noreply@bahn.de",
		Files: []TravelFile{readTestICS(t, "db_hinfahrt_reservierung.ics")},
	}))
	if booking.Reference != "194938114535" {
		t.Errorf("reference = %q", booking.Reference)
	}
	if len(booking.Segments) != 1 {
		t.Fatalf("legs = %+v", booking.Segments)
	}
	leg := booking.Segments[0]
	if leg.Number != "ICE 1652" || leg.From != "Dresden-Neustadt" || leg.To != "Leipzig Hbf" ||
		leg.Start != "2026-11-02T08:17" || leg.End != "2026-11-02T09:20" || leg.Seat != "Coach 12, seat 62 (1st class)" || !leg.SeatOnly {
		t.Fatalf("leg = %+v", leg)
	}
	if want := time.Date(2026, 11, 2, 7, 17, 0, 0, time.UTC); !leg.StartAt.Equal(want) {
		t.Errorf("start = %v, want %v", leg.StartAt, want)
	}
}

// The whole fetch path: a DB mail with the calendar export attached, parsed the
// way the sync parses it, and read back by the backfill's scan alike.
func TestTravelDBRealICSThroughParse(t *testing.T) {
	file := readTestICS(t, "db_hinrueckfahrt.ics")
	encoded := base64.StdEncoding.EncodeToString(file.Data)
	raw := "From: DB Fernverkehr AG <buchungsbestaetigung@bahn.de>\r\nSubject: Ihre Buchungsbestätigung\r\n" +
		"Date: Thu, 24 Sep 2026 18:22:00 +0200\r\nMIME-Version: 1.0\r\nContent-Type: multipart/mixed; boundary=b\r\n\r\n" +
		"--b\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nGute Reise\r\n" +
		"--b\r\nContent-Type: text/calendar; charset=utf-8; name=\"BAHN_Hinrueckfahrt.ics\"\r\n" +
		"Content-Disposition: attachment; filename=\"BAHN_Hinrueckfahrt.ics\"\r\nContent-Transfer-Encoding: base64\r\n\r\n" +
		encoded + "\r\n--b--\r\n"
	parsed, err := Parse([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if len(parsed.Travel) != 1 || len(parsed.Travel[0].Segments) != 4 {
		t.Fatalf("parsed travel = %+v", parsed.Travel)
	}
	scanned, err := TravelBookingsReaderScan(strings.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if len(scanned) != 1 || scanned[0].Key() != parsed.Travel[0].Key() || len(scanned[0].Segments) != 4 {
		t.Fatalf("backfill and fetch path disagree: %+v vs %+v", scanned, parsed.Travel)
	}
}

// eventimConfirmation rebuilds the parts of a real EVENTIM order confirmation
// the reader looks at -- the order number, the "Datum" block and the "Im
// Kalender speichern" links exactly as EVENTIM writes them -- without the
// buyer's address and without the ticket download links, which grant access
// to the tickets themselves.
func eventimConfirmation(orderNumber string) string {
	google := "https://calendar.google.com/calendar/r/eventedit?text=Maria+Clara+Groppler+-+Ehefrau+-+TICKETS+IN+DRESDEN" +
		"&amp;dates=20261103T190000Z/20261103T190000Z&amp;location=Filmtheater+Schauburg%2C+K%C3%B6nigsbr%C3%BCcker+Stra%C3%9Fe+55%2C+01099+DRESDEN" +
		"&amp;details=Platzinformationen%3A%0AEingang+Platzwahl+frei%2C+Block+Sitzplatz%0A%0AEingang+Platzwahl+frei%2C+Block+Sitzplatz%0A%0A%0AEVENTIM+w%C3%BCnscht+viel+Spa%C3%9F"
	outlook := "https://outlook.live.com/owa/?path=/calendar/action/compose&amp;rru=addevent&amp;subject=Maria+Clara+Groppler+-+Ehefrau+-+TICKETS+IN+DRESDEN" +
		"&amp;startdt=20261103T190000&amp;enddt=20261103T190000&amp;location=Filmtheater+Schauburg%2C+K%C3%B6nigsbr%C3%BCcker+Stra%C3%9Fe+55%2C+01099+DRESDEN"
	order := ""
	if orderNumber != "" {
		order = `<p>Deine Bestellung mit der Ordernummer ` + orderNumber + ` haben wir erhalten.</p>`
	}
	return `<html><body><h1>Bestellbestätigung</h1>` + order +
		`<h2>Deine Tickets herunterladen</h2><p>Deine Tickets stehen nun zur Verfügung.</p>` +
		`<table><tr><td>Maria Clara Groppler - Ehefrau</td></tr><tr><td>Datum:</td><td>Di., 03.11.2026, 20:00</td></tr>` +
		`<tr><td>Ort:</td><td>Filmtheater Schauburg, Königsbrücker Straße 55, 01099 DRESDEN</td></tr></table>` +
		`<h2>Im Kalender speichern</h2><a href="` + google + `">Google Kalender</a> <a href="` + google + `">Google</a>` +
		`<a href="https://www.eventim.de/imageservlet?fun=calendar&amp;affiliate=EGG&amp;t=1">Outlook</a>` +
		`<a href="` + outlook + `">Outlook.com</a></body></html>`
}

// EVENTIM publishes no markup and attaches no .ics; the event is in its
// add-to-calendar links. The Google link's instant and the local time the mail
// writes give 20:00, the Outlook.com link's zoneless copy of the UTC digits is
// not read as a second event, and the .ics behind "Outlook" is never fetched.
func TestTravelCalendarLinksEventimOrder(t *testing.T) {
	booking := onlyBooking(t, ExtractTravelBookings(TravelInput{
		Subject: "Deine EVENTIM-Bestellung: Maria Clara Groppler - Ehefrau - Bestellnummer 810156291",
		From:    "EVENTIM Kundenservice <bestellstatus@eventim.de>",
		HTML:    eventimConfirmation("810156291"),
	}))
	if booking.Kind != TravelEvent || booking.Reference != "810156291" || booking.Provider != "eventim.de" || len(booking.Segments) != 1 {
		t.Fatalf("booking = %+v", booking)
	}
	seg := booking.Segments[0]
	if seg.Title != "Maria Clara Groppler - Ehefrau" || seg.Venue != "Filmtheater Schauburg" || seg.Address != "Königsbrücker Straße 55, 01099 DRESDEN" {
		t.Errorf("segment = %+v", seg)
	}
	if seg.Start != "2026-11-03T20:00" || !seg.StartAt.Equal(time.Date(2026, 11, 3, 19, 0, 0, 0, time.UTC)) {
		t.Errorf("start = %q / %v", seg.Start, seg.StartAt)
	}
	if seg.Seat != "2× Eingang Platzwahl frei, Block Sitzplatz" {
		t.Errorf("seat = %q", seg.Seat)
	}
}

// A calendar link under an event a newsletter advertises is not a ticket: it
// names no order.
func TestTravelCalendarLinksNeedAnOrder(t *testing.T) {
	got := ExtractTravelBookings(TravelInput{
		Subject: "Tickets jetzt sichern!",
		From:    "news@eventim.de",
		HTML:    eventimConfirmation(""),
	})
	if len(got) != 0 {
		t.Fatalf("an advertised event is not a booking: %+v", got)
	}
}

// A zoneless link that names its zone is that zone's wall clock.
func TestTravelCalendarLinkWithZone(t *testing.T) {
	link := `<a href="https://calendar.google.com/calendar/render?action=TEMPLATE&amp;text=Theater&amp;dates=20261201T193000/20261201T220000&amp;ctz=Europe/Vienna&amp;location=Burgtheater">x</a>`
	booking := onlyBooking(t, ExtractTravelBookings(TravelInput{
		Subject: "Ihre Tickets – Buchungsnummer AB12345", HTML: `<p>Ihre Tickets</p>` + link,
	}))
	seg := booking.Segments[0]
	if seg.Start != "2026-12-01T19:30" || seg.End != "2026-12-01T22:00" || !seg.StartAt.Equal(time.Date(2026, 12, 1, 18, 30, 0, 0, time.UTC)) {
		t.Fatalf("segment = %+v", seg)
	}
}

// "Deine Bestellnummer findest du im Kundenkonto" names no order: a
// reference has to carry a digit, or the advertisement gate is a formality.
func TestTravelCalendarLinksOrderNeedsADigit(t *testing.T) {
	html := strings.Replace(eventimConfirmation(""), "<h2>Deine Tickets",
		"<p>Deine Bestellnummer findest du im Kundenkonto.</p><h2>Deine Tickets", 1)
	if got := ExtractTravelBookings(TravelInput{Subject: "Neue Tickets", HTML: html}); len(got) != 0 {
		t.Fatalf("a word is not an order number: %+v", got)
	}
}

// Two shows in one order: each link takes the written time nearest its own
// instant, not the first one in the mail.
func TestTravelCalendarLinksTwoShowsTwoTimes(t *testing.T) {
	link := func(text, dates string) string {
		return `<a href="https://calendar.google.com/calendar/render?action=TEMPLATE&amp;text=` + text + `&amp;dates=` + dates + `">x</a>`
	}
	html := `<p>Deine Tickets, Bestellnummer 4711123</p><p>Früh: 03.11.2026, 18:00</p><p>Spät: 03.11.2026, 21:00</p>` +
		link("Fruehe+Show", "20261103T170000Z/20261103T190000Z") + link("Spaete+Show", "20261103T200000Z/20261103T220000Z")
	booking := onlyBooking(t, ExtractTravelBookings(TravelInput{Subject: "Bestellung", HTML: html}))
	if len(booking.Segments) != 2 || booking.Segments[0].Start != "2026-11-03T18:00" || booking.Segments[1].Start != "2026-11-03T21:00" {
		t.Fatalf("segments = %+v", booking.Segments)
	}
	if booking.Segments[1].End != "2026-11-03T23:00" {
		t.Errorf("the end follows the start's offset: %q", booking.Segments[1].End)
	}
}

// A whole-day link's end is exclusive; a one-day festival is one day.
func TestTravelCalendarLinkAllDayEndIsExclusive(t *testing.T) {
	html := `<p>Deine Tickets, Buchungsnummer FEST2026</p>` +
		`<a href="https://calendar.google.com/calendar/render?action=TEMPLATE&amp;text=Festival&amp;dates=20261205/20261206">x</a>` +
		`<a href="https://calendar.google.com/calendar/render?action=TEMPLATE&amp;text=Wochenende&amp;dates=20261212/20261214">y</a>`
	booking := onlyBooking(t, ExtractTravelBookings(TravelInput{Subject: "Tickets", HTML: html}))
	if len(booking.Segments) != 2 {
		t.Fatalf("segments = %+v", booking.Segments)
	}
	if one := booking.Segments[0]; one.Start != "2026-12-05" || one.End != "" {
		t.Errorf("one day = %+v", one)
	}
	if two := booking.Segments[1]; two.Start != "2026-12-12" || two.End != "2026-12-13" {
		t.Errorf("two days = %+v", two)
	}
}
