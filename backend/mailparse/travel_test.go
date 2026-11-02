package mailparse

import (
	"archive/zip"
	"bytes"
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
