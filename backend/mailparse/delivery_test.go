package mailparse

import (
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
	if got := DeliveryCarrierLabel(""); got != "Paket" {
		t.Errorf("label for an unclaimed number = %q", got)
	}
}
