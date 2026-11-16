package mailparse

import (
	"strings"
	"testing"
)

// reservixOrderMail is a Reservix print@home order confirmation as it arrives:
// plain text only, quoted-printable, the tickets behind a download link.
const reservixOrderMail = "To: Erika Muster <erika@example.org>\r\n" +
	"From: =?utf-8?B?cmVzZXJ2aXguZGU=?= <noreply@reservix.de>\r\n" +
	"Subject: Reservix | Reservix GmbH - Ihre Ticketbestellung 91123876\r\n" +
	"Date: Sat, 14 Nov 2026 07:52:16 +0000\r\n" +
	"MIME-Version: 1.0\r\n" +
	"Content-Type: text/plain; charset=utf-8\r\n" +
	"Content-Transfer-Encoding: quoted-printable\r\n\r\n" +
	"Hallo Erika Muster,\r\n\r\n" +
	"wir bedanken uns f=C3=BCr folgende Bestellung:\r\n=\r\n\r\n" +
	"Bestellnummer: 91123876\r\n\r\n" +
	"Silent Rocco \"Untold Stories\" - 41. Interna=\r\n" +
	"tionales Pantomime Theater Festival, 15. November 2026, 18:00 Uhr\r\n\r\n" +
	"1 x =\r\n" +
	"1. Kategorie - Normalpreis                                                 =\r\n" +
	"                        23,50 EUR\r\n" +
	"1 x 1. Kategorie - Erm=C3=A4=C3=9Figt  =\r\n" +
	"                                                                          1=\r\n" +
	"8,00 EUR\r\n\r\n" +
	"1 x Servicegeb=C3=BChr                                       =\r\n" +
	"                                                2,00 EUR\r\n" +
	"Gesamtpreis inkl. aller Geb=C3=BChren und USt      =\r\n" +
	"                                                         43,50 EUR\r\n\r\n" +
	"Lieferart: print@home f=C3=BCr Tickets\r\n\r\n" +
	"Tickets selbst ausdrucken (PDF):\r\n" +
	"https://download.reservix.com/ticket/0000.pdf\r\n\r\n" +
	"Freundliche Gr=C3=BC=C3=9Fe\r\nIhr Reservix-Team\r\n"

// atavoloReservationMail is a venue's reservation confirmation written by
// atavolo: sent from the venue, signed by atavolo, calendar entry behind a link.
const atavoloReservationMail = "Date: Sat, 14 Nov 2026 11:50:13 +0100\r\n" +
	"To: erika@example.org\r\n" +
	"From: BallsportARENA Dresden | Squash <info@ballsportarena-dresden.de>\r\n" +
	"Subject: =?UTF-8?Q?Reservierungsbest=C3=A4tigung_-_Ihre_Reservierung_am_14.11.2026?=\r\n" +
	"Message-ID: <abc@login.atavolo.com>\r\n" +
	"MIME-Version: 1.0\r\n" +
	"Content-Type: multipart/alternative; boundary=\"b1\"\r\n\r\n" +
	"--b1\r\nContent-Type: text/plain; charset=UTF-8\r\nContent-Transfer-Encoding: 8bit\r\n\r\n" +
	"Sehr geehrte Frau Muster,\r\n" +
	"vielen Dank für Ihre Reservierung in unserem Haus, die wir Ihnen hiermit bestätigen.\r\n\r\n" +
	"Name: Muster, Erika\r\n" +
	"Datum: 14.11.2026\r\n" +
	"Uhrzeit: 18:00 Uhr - 19:00 Uhr\r\n" +
	"Personen: 2\r\n" +
	"Feld: iSquash | 38€ pro Stunde\r\n" +
	"Bitte beachten Sie, dass wir Ihnen auch ein anderes Feld zuweisen können.\r\n\r\n" +
	"Wir freuen uns auf Ihren Besuch.\r\n" +
	"BallsportARENA Dresden | Squash\r\n" +
	"Weißeritzstraße 4\r\n" +
	"01067 Dresden\r\n" +
	"Telefon: +49 351 0000000\r\n\r\n" +
	"powered by atavolo.com\r\n\r\n" +
	"--b1\r\nContent-Type: text/html; charset=UTF-8\r\nContent-Transfer-Encoding: 8bit\r\n\r\n" +
	"<html><body><a href=\"https://login.atavolo.com/sport/termin.php?l=x&amp;r=5\" title=\"Kalendereintrag\">Kalender</a></body></html>\r\n" +
	"--b1--\r\n"

func parsedAndScanned(t *testing.T, raw string) []TravelBooking {
	t.Helper()
	parsed, err := Parse([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	scanned, err := TravelBookingsReaderScan(strings.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if len(parsed.Travel) != len(scanned) {
		t.Fatalf("fetch path and backfill disagree: %+v vs %+v", parsed.Travel, scanned)
	}
	for i := range scanned {
		if parsed.Travel[i].Key() != scanned[i].Key() || len(parsed.Travel[i].Segments) != len(scanned[i].Segments) {
			t.Fatalf("fetch path and backfill disagree: %+v vs %+v", parsed.Travel, scanned)
		}
	}
	return parsed.Travel
}

func TestTravelReservixOrderConfirmation(t *testing.T) {
	booking := onlyBooking(t, parsedAndScanned(t, reservixOrderMail))
	if booking.Kind != TravelEvent || booking.Reference != "91123876" || booking.Provider != "Reservix" {
		t.Fatalf("booking = %+v", booking)
	}
	if len(booking.Segments) != 1 {
		t.Fatalf("segments = %+v", booking.Segments)
	}
	seg := booking.Segments[0]
	if seg.Title != `Silent Rocco "Untold Stories" - 41. Internationales Pantomime Theater Festival` {
		t.Fatalf("title = %q", seg.Title)
	}
	if seg.Start != "2026-11-15T18:00" || !seg.StartAt.IsZero() {
		t.Fatalf("start = %q / %v, want a floating 18:00", seg.Start, seg.StartAt)
	}
	if seg.Seat != "1. Kategorie - Normalpreis; 1. Kategorie - Ermäßigt" {
		t.Fatalf("seat = %q", seg.Seat)
	}
}

// The layout is Reservix's; the same lines from anybody else are not read.
func TestTravelReservixLayoutNeedsReservix(t *testing.T) {
	raw := strings.Replace(reservixOrderMail, "noreply@reservix.de", "noreply@example.com", 1)
	if got := parsedAndScanned(t, raw); len(got) != 0 {
		t.Fatalf("travel = %+v", got)
	}
}

// An event line counts only with an admission under it: a newsletter from the
// same domain lists events and dates, never priced tickets.
func TestTravelReservixEventNeedsATicketLine(t *testing.T) {
	text := "Bestellnummer: 91123876\n\nKonzert in der Halle, 3. Dezember 2026, 20:00 Uhr\n\n" +
		"1 x Servicegebühr    2,00 EUR\n\nNächste Woche: Lesung, 10. Dezember 2026, 19:30 Uhr\n"
	got := ExtractTravelBookings(TravelInput{From: "noreply@reservix.de", Subject: "Ihre Bestellung", Text: text})
	if len(got) != 0 {
		t.Fatalf("travel = %+v", got)
	}
}

func TestTravelReservixDayTicketAndTwoEvents(t *testing.T) {
	text := "Bestellnummer: 5550001\n\n" +
		"Stadtfest Tagesticket, 3. Dezember 2026\n2 x Stehplatz    10,00 EUR\n\n" +
		"Konzert, 4. Dezember 2026, 20:00 Uhr\n1 x Sitzplatz Reihe 3    30,00 EUR\n"
	booking := onlyBooking(t, ExtractTravelBookings(TravelInput{From: "Reservix <noreply@reservix.de>", Subject: "Ihre Ticketbestellung", Text: text}))
	if len(booking.Segments) != 2 {
		t.Fatalf("segments = %+v", booking.Segments)
	}
	if day := booking.Segments[0]; day.Start != "2026-12-03" || day.Seat != "2× Stehplatz" {
		t.Fatalf("day ticket = %+v", day)
	}
	if show := booking.Segments[1]; show.Start != "2026-12-04T20:00" || show.Title != "Konzert" {
		t.Fatalf("show = %+v", show)
	}
}

func TestTravelAtavoloReservation(t *testing.T) {
	booking := onlyBooking(t, parsedAndScanned(t, atavoloReservationMail))
	if booking.Kind != TravelEvent || booking.Provider != "BallsportARENA Dresden" {
		t.Fatalf("booking = %+v", booking)
	}
	seg := booking.Segments[0]
	if seg.Title != "BallsportARENA Dresden | Squash" || seg.Venue != "BallsportARENA Dresden" {
		t.Fatalf("segment = %+v", seg)
	}
	if seg.Start != "2026-11-14T18:00" || seg.End != "2026-11-14T19:00" || !seg.StartAt.IsZero() {
		t.Fatalf("times = %q-%q / %v", seg.Start, seg.End, seg.StartAt)
	}
	if seg.Address != "Weißeritzstraße 4, 01067 Dresden" || seg.Seat != "iSquash, 2 Personen" {
		t.Fatalf("address = %q, seat = %q", seg.Address, seg.Seat)
	}
}

// The labelled lines are atavolo's layout; a mail atavolo did not write is not
// read for them, however much it looks like a reservation.
func TestTravelAtavoloNeedsAtavolo(t *testing.T) {
	raw := strings.Replace(atavoloReservationMail, "powered by atavolo.com", "", 1)
	raw = strings.Replace(raw, "login.atavolo.com/", "example.com/", 1)
	if got := parsedAndScanned(t, raw); len(got) != 0 {
		t.Fatalf("travel = %+v", got)
	}
}

// A booking past midnight ends the next morning rather than losing its end.
func TestTravelAtavoloEndPastMidnightAndOnePerson(t *testing.T) {
	text := "Datum: 31.12.2026\nUhrzeit: 22:00 Uhr - 01:00 Uhr\nPersonen: 1\n\npowered by atavolo.com\n"
	booking := onlyBooking(t, ExtractTravelBookings(TravelInput{
		From: "Bowling Center <info@example.com>", Subject: "Reservierungsbestätigung", Text: text,
	}))
	seg := booking.Segments[0]
	if seg.Start != "2026-12-31T22:00" || seg.End != "2027-01-01T01:00" || seg.Seat != "1 Person" {
		t.Fatalf("segment = %+v", seg)
	}
}

// A reservation a friend forwarded is still atavolo's text, but the friend is
// not the venue.
func TestTravelAtavoloForwardedKeepsNoForwarderName(t *testing.T) {
	text := "Datum: 14.11.2026\nUhrzeit: 18:00 Uhr\n\npowered by atavolo.com\n"
	booking := onlyBooking(t, ExtractTravelBookings(TravelInput{
		From: "Max Freund <max@example.com>", Subject: "WG: Reservierungsbestätigung", Text: text,
	}))
	seg := booking.Segments[0]
	if seg.Title != "Reservierung" || seg.Venue != "" || booking.Provider != "" {
		t.Fatalf("booking = %+v", booking)
	}
}

func TestTravelReservixAustrianMonth(t *testing.T) {
	text := "Bestellnummer: 5550002\n\nNeujahrskonzert, 1. Jänner 2027, 11:00 Uhr\n1 x Kategorie A    80,00 EUR\n"
	booking := onlyBooking(t, ExtractTravelBookings(TravelInput{From: "noreply@reservix.at", Subject: "Ihre Bestellung", Text: text}))
	if booking.Segments[0].Start != "2027-01-01T11:00" {
		t.Fatalf("segment = %+v", booking.Segments[0])
	}
}
