package mailparse

import (
	"strings"
	"testing"
	"time"
)

// sentAt is a Thursday, in the zone German carrier mail is written in.
func sentAt(t *testing.T) time.Time {
	t.Helper()
	berlin, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Skipf("zone data unavailable: %v", err)
	}
	return time.Date(2026, time.September, 3, 9, 12, 0, 0, berlin)
}

func onlyNotice(t *testing.T, notices []DeliveryNotice) DeliveryNotice {
	t.Helper()
	if len(notices) != 1 {
		t.Fatalf("want exactly one notice, got %d: %+v", len(notices), notices)
	}
	return notices[0]
}

func TestExtractDeliveryNoticesCarrierLink(t *testing.T) {
	content := CategoryContent{
		Subject: "Ihr Paket kommt heute",
		Text:    "Guten Tag, Ihre Sendung wird heute zwischen 10:12 und 13:12 Uhr zugestellt.",
		DeliveryLinks: []string{
			"https://nolp.dhl.de/nextt-online-public/set_identcodes.do?idc=00340434212345678901",
		},
	}
	notice := onlyNotice(t, ExtractDeliveryNotices(content, "DHL <noreply@dhl.de>", sentAt(t)))
	if notice.Carrier != "dhl" {
		t.Errorf("carrier = %q, want dhl", notice.Carrier)
	}
	if notice.TrackingNumber != "00340434212345678901" {
		t.Errorf("tracking number = %q", notice.TrackingNumber)
	}
	if notice.Status != DeliveryOutForDelivery {
		t.Errorf("status = %q, want %q", notice.Status, DeliveryOutForDelivery)
	}
	if notice.ExpectedDate != "2026-09-03" {
		t.Errorf("expected date = %q, want the message's own day", notice.ExpectedDate)
	}
	if notice.WindowStart != "10:12" || notice.WindowEnd != "13:12" {
		t.Errorf("window = %q..%q", notice.WindowStart, notice.WindowEnd)
	}
}

// A shop's dispatch mail is the earliest warning a reader gets, and it is not
// the carrier writing. The number is labelled and the carrier is named in words.
func TestExtractDeliveryNoticesShopDispatch(t *testing.T) {
	content := CategoryContent{
		Subject: "Deine Bestellung 402-99182 wurde versendet",
		Text:    "Wir haben dein Paket mit GLS versendet. Sendungsnummer: 12345678901. Die voraussichtliche Zustellung ist am Montag.",
	}
	notice := onlyNotice(t, ExtractDeliveryNotices(content, "versand@beispielshop.de", sentAt(t)))
	if notice.Carrier != "gls" {
		t.Errorf("carrier = %q, want gls from the words of the message", notice.Carrier)
	}
	if notice.TrackingNumber != "12345678901" {
		t.Errorf("tracking number = %q", notice.TrackingNumber)
	}
	if notice.Status != DeliveryAnnounced {
		t.Errorf("status = %q, want %q", notice.Status, DeliveryAnnounced)
	}
	// The message is written on a Thursday, so the named Monday is four days on.
	if notice.ExpectedDate != "2026-09-07" {
		t.Errorf("expected date = %q, want the following Monday", notice.ExpectedDate)
	}
}

func TestExtractDeliveryNoticesSelfIdentifyingNumbers(t *testing.T) {
	for _, tc := range []struct {
		name    string
		text    string
		carrier string
		number  string
	}{
		{
			name:    "ups",
			text:    "Your shipment 1Z999AA10123456784 is expected to arrive tomorrow.",
			carrier: "ups",
			number:  "1Z999AA10123456784",
		},
		{
			name:    "amazon",
			text:    "Dein Paket TBA303928172635 wird voraussichtlich am 05.09.2026 geliefert.",
			carrier: "amazon",
			number:  "TBA303928172635",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			notice := onlyNotice(t, ExtractDeliveryNotices(CategoryContent{Text: tc.text}, "hallo@unbekannt.example", sentAt(t)))
			if notice.Carrier != tc.carrier {
				t.Errorf("carrier = %q, want %q", notice.Carrier, tc.carrier)
			}
			if notice.TrackingNumber != tc.number {
				t.Errorf("tracking number = %q, want %q", notice.TrackingNumber, tc.number)
			}
		})
	}
}

func TestExtractDeliveryNoticesDates(t *testing.T) {
	for _, tc := range []struct {
		name string
		text string
		want string
	}{
		{"german numeric", "Voraussichtliche Zustellung: Donnerstag, 10.09.2026", "2026-09-10"},
		{"german no year", "Die Lieferung erfolgt am 05.09.", "2026-09-05"},
		{"german month name", "Zustellung voraussichtlich am 8. September", "2026-09-08"},
		{"english month name", "Expected delivery September 9, 2026", "2026-09-09"},
		{"iso", "Delivery scheduled for 2026-09-11", "2026-09-11"},
		{"relative", "Ihr Paket wird morgen zugestellt", "2026-09-04"},
		{"date before anchor", "Am 07.09.2026 wird Ihre Sendung zugestellt.", "2026-09-07"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			content := CategoryContent{Text: tc.text + " Sendungsnummer: 00340434212345678901"}
			notice := onlyNotice(t, ExtractDeliveryNotices(content, "noreply@dhl.de", sentAt(t)))
			if notice.ExpectedDate != tc.want {
				t.Errorf("expected date = %q, want %q", notice.ExpectedDate, tc.want)
			}
		})
	}
}

// A date across the turn of the year belongs to the year that puts it next to
// the message, not to the one the message was written in.
func TestExtractDeliveryNoticesYearRollover(t *testing.T) {
	berlin, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Skipf("zone data unavailable: %v", err)
	}
	sent := time.Date(2026, time.December, 29, 14, 0, 0, 0, berlin)
	content := CategoryContent{Text: "Voraussichtliche Zustellung am 04.01. Sendungsnummer: 00340434212345678901"}
	notice := onlyNotice(t, ExtractDeliveryNotices(content, "noreply@dhl.de", sent))
	if notice.ExpectedDate != "2027-01-04" {
		t.Errorf("expected date = %q, want 2027-01-04", notice.ExpectedDate)
	}
}

func TestExtractDeliveryNoticesStatus(t *testing.T) {
	for _, tc := range []struct {
		name string
		text string
		want string
	}{
		{"announced", "Ihr Paket wurde versandt und wird zugestellt.", DeliveryAnnounced},
		{"out for delivery", "Ihre Sendung ist in Zustellung.", DeliveryOutForDelivery},
		{"delivered", "Ihre Sendung wurde zugestellt.", DeliveryDelivered},
		{"delivered english", "Your parcel has been delivered.", DeliveryDelivered},
	} {
		t.Run(tc.name, func(t *testing.T) {
			content := CategoryContent{Text: tc.text + " Sendungsnummer: 00340434212345678901"}
			notice := onlyNotice(t, ExtractDeliveryNotices(content, "noreply@dhl.de", sentAt(t)))
			if notice.Status != tc.want {
				t.Errorf("status = %q, want %q", notice.Status, tc.want)
			}
		})
	}
}

// The whole point of requiring a label or a link: numbers are everywhere in
// mail, and none of these messages is about a parcel.
func TestExtractDeliveryNoticesIgnoresUnrelatedNumbers(t *testing.T) {
	for _, tc := range []struct {
		name    string
		content CategoryContent
		from    string
	}{
		{
			name:    "invoice",
			content: CategoryContent{Subject: "Ihre Rechnung", Text: "Rechnungsnummer: 2026-0004711, Betrag 129,00 EUR, zahlbar bis 30.09.2026."},
			from:    "buchhaltung@beispiel.de",
		},
		{
			name:    "order confirmation without a tracking number",
			content: CategoryContent{Subject: "Bestellbestätigung", Text: "Bestellnummer 402-9918273-1122334. Die Lieferung erfolgt in 3-5 Werktagen."},
			from:    "shop@beispiel.de",
		},
		{
			name:    "phone number",
			content: CategoryContent{Text: "Rufen Sie uns an unter 0049 30 123456789. Die Lieferung ist unterwegs."},
			from:    "service@beispiel.de",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if notices := ExtractDeliveryNotices(tc.content, tc.from, sentAt(t)); len(notices) > 0 {
				t.Errorf("want no notices, got %+v", notices)
			}
		})
	}
}

// One parcel named twice -- in the text and in the carrier's own link -- is one
// shipment, and the half that knows the carrier wins.
func TestExtractDeliveryNoticesDeduplicates(t *testing.T) {
	content := CategoryContent{
		Text:          "Sendungsnummer: 00340434212345678901. Verfolgen Sie Ihre Sendung online.",
		DeliveryLinks: []string{"https://nolp.dhl.de/nextt-online-public/set_identcodes.do?idc=00340434212345678901"},
	}
	notice := onlyNotice(t, ExtractDeliveryNotices(content, "versand@beispielshop.de", sentAt(t)))
	if notice.Carrier != "dhl" {
		t.Errorf("carrier = %q, want dhl", notice.Carrier)
	}
}

func TestExtractDeliveryNoticesWithoutDateHeader(t *testing.T) {
	content := CategoryContent{Text: "Ihr Paket kommt heute. Sendungsnummer: 00340434212345678901"}
	notice := onlyNotice(t, ExtractDeliveryNotices(content, "noreply@dhl.de", time.Time{}))
	if notice.ExpectedDate != "" {
		t.Errorf("expected date = %q, want empty without a Date header", notice.ExpectedDate)
	}
	if notice.Status != DeliveryOutForDelivery {
		t.Errorf("status = %q, want the status the words still give", notice.Status)
	}
}

func TestDeliveryTrackingURL(t *testing.T) {
	if got := DeliveryTrackingURL("ups", "1Z999AA10123456784"); got != "https://www.ups.com/track?tracknum=1Z999AA10123456784" {
		t.Errorf("ups url = %q", got)
	}
	if got := DeliveryTrackingURL("amazon", "TBA303928172635"); got != "" {
		t.Errorf("amazon url = %q, want empty: its tracking page needs the order", got)
	}
	if got := DeliveryCarrierLabel("gls"); got != "GLS" {
		t.Errorf("label = %q", got)
	}
	if got := DeliveryCarrierLabel(""); got != "Parcel" {
		t.Errorf("label for an unclaimed number = %q", got)
	}
}

// deliveryRawMessage is a carrier mail as one actually arrives: multipart, with
// the tracking link only in the HTML part, and its query string entity-escaped
// the way markup writes it.
const deliveryRawMessage = "From: DHL Paket <noreply@dhl.de>\r\n" +
	"To: reader@example.test\r\n" +
	"Subject: Ihre Sendung kommt heute\r\n" +
	"Date: Thu, 03 Sep 2026 07:14:00 +0200\r\n" +
	"Auto-Submitted: auto-generated\r\n" +
	"MIME-Version: 1.0\r\n" +
	"Content-Type: multipart/alternative; boundary=\"sep\"\r\n" +
	"\r\n" +
	"--sep\r\n" +
	"Content-Type: text/plain; charset=utf-8\r\n" +
	"\r\n" +
	"Guten Tag, Ihre Sendung wird heute zwischen 10:12 und 13:12 Uhr zugestellt.\r\n" +
	"\r\n" +
	"--sep\r\n" +
	"Content-Type: text/html; charset=utf-8\r\n" +
	"\r\n" +
	"<html><body><p>Ihre Sendung wird <b>heute</b> zugestellt.</p>" +
	"<a href=\"https://nolp.dhl.de/nextt-online-public/set_identcodes.do?idc=00340434212345678901&amp;lang=de\">" +
	"Sendung verfolgen</a></body></html>\r\n" +
	"--sep--\r\n"

// The whole fetch path: a raw message in, a parcel out, with the number coming
// from a link that only exists in markup the indexed text throws away.
func TestParseExtractsDeliveriesFromMarkupLinks(t *testing.T) {
	parsed, err := Parse([]byte(deliveryRawMessage))
	if err != nil {
		t.Fatal(err)
	}
	notice := onlyNotice(t, parsed.Deliveries)
	if notice.Carrier != "dhl" {
		t.Errorf("carrier = %q, want dhl", notice.Carrier)
	}
	if notice.TrackingNumber != "00340434212345678901" {
		t.Errorf("tracking number = %q -- the entity-escaped query may have swallowed the next parameter", notice.TrackingNumber)
	}
	// The message was written on 3 September in +02:00, and "heute" is its own
	// day and not the day the test happens to run on.
	if notice.ExpectedDate != "2026-09-03" {
		t.Errorf("expected date = %q, want 2026-09-03", notice.ExpectedDate)
	}
	if notice.Status != DeliveryOutForDelivery {
		t.Errorf("status = %q", notice.Status)
	}
	if notice.WindowStart != "10:12" || notice.WindowEnd != "13:12" {
		t.Errorf("window = %q..%q", notice.WindowStart, notice.WindowEnd)
	}
	// Reading a parcel out of a message must not move it out of the category
	// its headers put it in.
	if parsed.Category != CategoryNotifications {
		t.Errorf("category = %q, want the header answer to be untouched", parsed.Category)
	}
}

// The backfill path has to reach the same answer from the stored message.
func TestDeliveryNoticesReaderScanMatchesTheFetchPath(t *testing.T) {
	scanned, complete, err := DeliveryNoticesReaderScan(strings.NewReader(deliveryRawMessage))
	if err != nil {
		t.Fatal(err)
	}
	if !complete {
		t.Error("a message this size should scan whole")
	}
	parsed, err := Parse([]byte(deliveryRawMessage))
	if err != nil {
		t.Fatal(err)
	}
	if len(scanned) != len(parsed.Deliveries) {
		t.Fatalf("scan found %d parcels, the parse found %d", len(scanned), len(parsed.Deliveries))
	}
	for i := range scanned {
		if scanned[i] != parsed.Deliveries[i] {
			t.Errorf("parcel %d differs: scan %+v, parse %+v", i, scanned[i], parsed.Deliveries[i])
		}
	}
}

// Ordinary mail must come out of the same path with nothing at all, or every
// list in the app grows a chip it should not have.
func TestParseLeavesOrdinaryMailWithoutParcels(t *testing.T) {
	raw := "From: Anna <anna@example.test>\r\n" +
		"To: reader@example.test\r\n" +
		"Subject: Rechnung 2026-0004711\r\n" +
		"Date: Thu, 03 Sep 2026 09:00:00 +0200\r\n" +
		"\r\n" +
		"Hallo, anbei die Rechnung ueber 129,00 EUR. Die Lieferung der Ware erfolgt\r\n" +
		"wie besprochen. Meine Nummer ist 0049 30 123456789.\r\n"
	parsed, err := Parse([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if len(parsed.Deliveries) != 0 {
		t.Errorf("want no parcels, got %+v", parsed.Deliveries)
	}
}

// One parcel found twice -- once by the carrier's link, once by the label in
// the text that does not name a carrier -- is one parcel. Keyed on the carrier
// as well as the number it was two, which the store then wrote as two rows.
func TestExtractDeliveryNoticesMergesOnTheNumberAlone(t *testing.T) {
	content := CategoryContent{Text: "Tracking number: 1Z999AA10123456784. Vielen Dank für Ihre Bestellung."}
	notice := onlyNotice(t, ExtractDeliveryNotices(content, "shop@beispielshop.de", sentAt(t)))
	if notice.Carrier != "ups" {
		t.Errorf("carrier = %q, want the one the number's own shape names", notice.Carrier)
	}
}

// An announcement is not a report. Both of these name a day in the future, and
// filing them as delivered took the parcel off the list before it arrived.
func TestExtractDeliveryNoticesDoesNotReadAnnouncementsAsDelivered(t *testing.T) {
	for _, tc := range []struct {
		name string
		text string
	}{
		{"german", "Ihre Sendung wird voraussichtlich zugestellt am 07.09.2026."},
		{"english", "Your parcel will be delivered on September 7, 2026."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			content := CategoryContent{Text: tc.text + " Sendungsnummer: 00340434212345678901"}
			notice := onlyNotice(t, ExtractDeliveryNotices(content, "noreply@dhl.de", sentAt(t)))
			if notice.Status == DeliveryDelivered {
				t.Errorf("status = %q for a message about a day that has not come", notice.Status)
			}
			if notice.ExpectedDate != "2026-09-07" {
				t.Errorf("expected date = %q, want 2026-09-07", notice.ExpectedDate)
			}
		})
	}
}

// "Auf dem Weg zu Ihnen" is what a shop writes when it hands a parcel over. It
// says the parcel is moving, not that it arrives today.
func TestExtractDeliveryNoticesDoesNotReadDispatchAsToday(t *testing.T) {
	content := CategoryContent{Text: "Dein Paket ist auf dem Weg zu Ihnen. Sendungsnummer: 12345678901"}
	notice := onlyNotice(t, ExtractDeliveryNotices(content, "shop@beispielshop.de", sentAt(t)))
	if notice.Status == DeliveryOutForDelivery {
		t.Errorf("status = %q for a dispatch note", notice.Status)
	}
	if notice.ExpectedDate != "" {
		t.Errorf("expected date = %q, want none: the message named no day", notice.ExpectedDate)
	}
}

// A link to a carrier is usually not a link to a parcel. A shop link and a help
// anchor both end in something that reads as a number.
func TestExtractDeliveryNoticesIgnoresNonTrackingCarrierLinks(t *testing.T) {
	for _, link := range []string{
		"https://www.amazon.de/dp/3442267749",
		"https://www.dhl.de/de/privatkunden/hilfe.html#faq-1234567890",
		"https://www.ups.com/de/de/support/contact.page",
	} {
		t.Run(link, func(t *testing.T) {
			content := CategoryContent{Text: "Viele Grüße von uns.", DeliveryLinks: []string{link}}
			if notices := ExtractDeliveryNotices(content, "newsletter@beispiel.de", sentAt(t)); len(notices) > 0 {
				t.Errorf("want no parcels from %q, got %+v", link, notices)
			}
		})
	}
}

// The real tracking pages still have to work, including the two carriers whose
// number is in the path and in the fragment rather than in a named parameter.
func TestExtractDeliveryNoticesReadsTrackingPagePaths(t *testing.T) {
	for _, tc := range []struct {
		link    string
		carrier string
		number  string
	}{
		{"https://tracking.dpd.de/status/de_DE/parcel/01234567890123", "dpd", "01234567890123"},
		{"https://www.myhermes.de/empfangen/sendungsverfolgung/sendungsinformation/#11223344556677", "hermes", "11223344556677"},
	} {
		t.Run(tc.carrier, func(t *testing.T) {
			content := CategoryContent{Text: "Verfolgen Sie Ihre Sendung.", DeliveryLinks: []string{tc.link}}
			notice := onlyNotice(t, ExtractDeliveryNotices(content, "noreply@beispiel.de", sentAt(t)))
			if notice.Carrier != tc.carrier || notice.TrackingNumber != tc.number {
				t.Errorf("got %+v, want %s/%s", notice, tc.carrier, tc.number)
			}
		})
	}
}

// A window holding a reference number and a real date must yield the date. The
// reference matches the German date expression and the calendar rejects it, and
// stopping there lost the answer standing beside it.
func TestExtractDeliveryNoticesLooksPastAnImplausibleMatch(t *testing.T) {
	content := CategoryContent{
		Text: "Voraussichtliche Zustellung 12.34.56 am 07.09.2026. Sendungsnummer: 00340434212345678901",
	}
	notice := onlyNotice(t, ExtractDeliveryNotices(content, "noreply@dhl.de", sentAt(t)))
	if notice.ExpectedDate != "2026-09-07" {
		t.Errorf("expected date = %q, want 2026-09-07", notice.ExpectedDate)
	}
}

// realDHLArrivingToday is rebuilt from a DHL "kommt heute" mail as it actually
// arrives: filed under Newsletters because it carries an unsubscribe route, the
// delivery window written with a stray double space, and the parcel number
// printed in the body under "Sendungsstatus einsehen" -- which is a heading and
// not a label. The href varies: sometimes it is the tracking page, and often it
// is the click-tracking redirect this fixture's second form uses.
func realDHLArrivingToday(link string) string {
	return "From: DHL Paket <noreply@dhl.de>\r\n" +
		"To: reader@example.test\r\n" +
		"Subject: Ihre CrowdFarming Sendung kommt heute - Jetzt Live verfolgen\r\n" +
		"Date: Fri, 28 Aug 2026 09:12:00 +0200\r\n" +
		"List-Unsubscribe: <mailto:unsub@dhl.de>\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: text/html; charset=utf-8\r\n" +
		"\r\n" +
		`<html><body><p>Ihre Sendung wird zugestellt</p><p>Hallo,</p>` +
		`<p>Ihre <b>CrowdFarming</b> Sendung wird Ihnen <b>heute zwischen 12:20  - 13:50 Uhr</b> ` +
		`durch Ihre Paketzustellkraft zugestellt.</p>` +
		`<a href="` + link + `">Jetzt live verfolgen</a>` +
		`<p>Sendungsstatus einsehen</p><p>00340434652966959030</p>` +
		`<p>Sie möchten keine oder nur noch bestimmte Informationen zu Ihrer Sendung erhalten?</p>` +
		`</body></html>` + "\r\n"
}

func TestParseReadsARealArrivingTodayMail(t *testing.T) {
	for _, tc := range []struct {
		name string
		link string
	}{
		{"tracking link", "https://www.dhl.de/de/privatkunden/pakete-empfangen/verfolgen.html?piececode=00340434652966959030"},
		// The carrier's own number, behind a redirect that says nothing. The
		// body still prints it, and the sender is what makes a bare number safe
		// to read.
		{"click-tracking redirect", "https://links.dhl-news.de/r/abc123XYZ"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parsed, err := Parse([]byte(realDHLArrivingToday(tc.link)))
			if err != nil {
				t.Fatal(err)
			}
			notice := onlyNotice(t, parsed.Deliveries)
			if notice.Carrier != "dhl" || notice.TrackingNumber != "00340434652966959030" {
				t.Errorf("got %+v", notice)
			}
			if notice.ExpectedDate != "2026-08-28" {
				t.Errorf("expected date = %q, want the day the mail was written", notice.ExpectedDate)
			}
			if notice.WindowStart != "12:20" || notice.WindowEnd != "13:50" {
				t.Errorf("window = %q..%q", notice.WindowStart, notice.WindowEnd)
			}
			if notice.Status != DeliveryOutForDelivery {
				t.Errorf("status = %q", notice.Status)
			}
			// "wird zugestellt" is an announcement; only the report is a delivery.
			if notice.Status == DeliveryDelivered {
				t.Error("an announcement was read as an arrival")
			}
		})
	}
}

// The bare-number rule is gated on the sender. The same body from a shop, with
// no label and no tracking link, must still yield nothing.
func TestExtractDeliveryNoticesKeepsBareNumbersToTheCarriersOwnMail(t *testing.T) {
	content := CategoryContent{Text: "Vielen Dank. Ihre Kundennummer lautet 00340434652966959030."}
	if notices := ExtractDeliveryNotices(content, "shop@beispielshop.de", sentAt(t)); len(notices) > 0 {
		t.Errorf("want no parcels from a shop's bare number, got %+v", notices)
	}
}

// A carrier is a company that also sends invoices, newsletters and service
// mail. The bare-number rule must not turn every correctly sized digit run in
// them into a parcel.
func TestExtractDeliveryNoticesGatesTheBareNumberRule(t *testing.T) {
	for _, tc := range []struct {
		name    string
		subject string
		text    string
		from    string
	}{
		{
			name:    "invoice from the postal arm",
			subject: "Ihre Rechnung",
			text:    "Rechnung 100234567890 über 24,90 EUR.",
			from:    "service@deutschepost.de",
		},
		{
			name:    "service number in a newsletter",
			subject: "Neuigkeiten für Sie",
			text:    "Erreichen Sie uns unter 022843331120.",
			from:    "newsletter@dhl.de",
		},
		{
			name:    "contract number",
			subject: "Ihre Vertragsunterlagen",
			text:    "Ihre Vertragsnummer 12345678901 finden Sie oben rechts.",
			from:    "info@gls-group.eu",
		},
		{
			// The subject says parcel, so the first gate lets it through; the
			// label beside the number is what stops it.
			name:    "customer number in a parcel mail",
			subject: "Ihre Sendung ist unterwegs",
			text:    "Ihre Kundennummer lautet 100234567890.",
			from:    "noreply@dhl.de",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			content := CategoryContent{Subject: tc.subject, Text: tc.text}
			if notices := ExtractDeliveryNotices(content, tc.from, sentAt(t)); len(notices) > 0 {
				t.Errorf("want no parcels, got %+v", notices)
			}
		})
	}
}

// The eight-notice cap is filled in order, so the number the message actually
// labelled must be taken before any bare one.
func TestExtractDeliveryNoticesPrefersALabelledNumberOverBareOnes(t *testing.T) {
	var items strings.Builder
	for i := 0; i < 9; i++ {
		items.WriteString("Artikel 90000000000")
		items.WriteByte(byte('0' + i))
		items.WriteString(". ")
	}
	content := CategoryContent{
		Subject: "Ihre Sendung kommt heute",
		Text:    items.String() + "Sendungsnummer: 00340434652966959030",
	}
	notices := ExtractDeliveryNotices(content, "noreply@dhl.de", sentAt(t))
	found := false
	for _, notice := range notices {
		if notice.TrackingNumber == "00340434652966959030" {
			found = true
		}
	}
	if !found {
		t.Errorf("the labelled number was crowded out by bare ones: %+v", notices)
	}
}

// "Sendungsstatus" heads a section rather than labelling a number, and what
// follows it is as often a date as anything else.
func TestExtractDeliveryNoticesDoesNotReadADateAsATrackingNumber(t *testing.T) {
	for _, text := range []string{
		"Sendungsstatus vom 2026-08-28: in Bearbeitung.",
		"Sendungsstatus aktualisiert: 2026-08-28",
		"Sendungsstatus: offen seit 2026-08-25",
	} {
		t.Run(text, func(t *testing.T) {
			content := CategoryContent{Subject: "Statusmeldung", Text: text}
			for _, notice := range ExtractDeliveryNotices(content, "info@beispiel.de", sentAt(t)) {
				if notice.TrackingNumber == "20260828" || notice.TrackingNumber == "20260825" {
					t.Errorf("a date became tracking number %q", notice.TrackingNumber)
				}
			}
		})
	}
}

func TestNormalizeTrackingNumberRefusesACompactDate(t *testing.T) {
	if got := normalizeTrackingNumber("2026-08-28"); got != "" {
		t.Errorf("normalized a date to %q", got)
	}
	// A real number of the same length is not a date and must survive.
	if got := normalizeTrackingNumber("99887766"); got != "99887766" {
		t.Errorf("normalized a plain number to %q", got)
	}
}
