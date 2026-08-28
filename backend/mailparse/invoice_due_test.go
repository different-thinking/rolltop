package mailparse

import (
	"strings"
	"testing"
	"time"
)

// invoiceSentAt is a Thursday, in the zone German billing mail is written in.
func invoiceSentAt(t *testing.T) time.Time {
	t.Helper()
	berlin, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Skipf("zone data unavailable: %v", err)
	}
	return time.Date(2026, time.September, 3, 9, 12, 0, 0, berlin)
}

func mustNotice(t *testing.T, notice *InvoiceNotice) InvoiceNotice {
	t.Helper()
	if notice == nil {
		t.Fatal("want an invoice notice, got none")
	}
	return *notice
}

func TestExtractInvoiceNoticeOpenTransfer(t *testing.T) {
	content := CategoryContent{
		Subject: "Ihre Rechnung Nr. 2026-4711",
		Text: "Sehr geehrter Kunde, anbei Ihre Rechnung. Gesamtbetrag: 149,90 EUR. " +
			"Bitte überweisen Sie den Betrag bis zum 17.09.2026 auf das Konto " +
			"DE02120300000000202051, Verwendungszweck 2026-4711.",
	}
	notice := mustNotice(t, ExtractInvoiceNotice(content, nil, "Rechnung <billing@shop.example.de>", invoiceSentAt(t)))
	if notice.Issuer != "example.de" {
		t.Errorf("issuer = %q, want example.de -- one biller, whatever subdomain it sends from", notice.Issuer)
	}
	if notice.Number != "2026-4711" {
		t.Errorf("number = %q, want 2026-4711", notice.Number)
	}
	if notice.DueDate != "2026-09-17" {
		t.Errorf("due date = %q, want 2026-09-17", notice.DueDate)
	}
	if notice.Amount != "149.90" || notice.Currency != "EUR" {
		t.Errorf("amount = %q %q, want 149.90 EUR", notice.Amount, notice.Currency)
	}
	if notice.Status != InvoiceOpen {
		t.Errorf("status = %q, want open", notice.Status)
	}
	if notice.Settlement != SettlementTransfer {
		t.Errorf("settlement = %q, want transfer", notice.Settlement)
	}
	if notice.Reference != "2026-4711" {
		t.Errorf("reference = %q", notice.Reference)
	}
}

// The case the whole feature was asked for: it was paid through PayPal and the
// invoice turns up anyway.
func TestExtractInvoiceNoticePaidWithWallet(t *testing.T) {
	content := CategoryContent{
		Subject: "Ihre Rechnung 5567 zu Bestellung 99182",
		Text: "Vielen Dank für Ihren Einkauf. Rechnungsbetrag: 42,00 EUR. " +
			"Zahlungsart: PayPal. Wir akzeptieren außerdem Kreditkarte, Lastschrift und Überweisung.",
	}
	notice := mustNotice(t, ExtractInvoiceNotice(content, nil, "shop@haendler.example.com", invoiceSentAt(t)))
	if notice.Settlement != SettlementWallet {
		t.Errorf("settlement = %q, want wallet", notice.Settlement)
	}
	if notice.Status != InvoicePaid {
		t.Errorf("status = %q, want paid -- nothing is left to transfer", notice.Status)
	}
}

// A footer listing what a shop accepts must never settle an invoice. Without the
// anchor rule this is the message that would silence every real reminder.
func TestExtractInvoiceNoticePaymentMethodFooterDoesNotSettle(t *testing.T) {
	content := CategoryContent{
		Subject: "Rechnung 7788",
		Text: "Rechnungsbetrag: 80,00 EUR, zahlbar bis 20.09.2026. " +
			"Bei uns können Sie bequem mit PayPal, Kreditkarte oder auf Rechnung bezahlen. " +
			"Bankverbindung: DE02120300000000202051.",
	}
	notice := mustNotice(t, ExtractInvoiceNotice(content, nil, "info@shop.example.de", invoiceSentAt(t)))
	if notice.Status != InvoiceOpen {
		t.Errorf("status = %q, want open", notice.Status)
	}
	if notice.DueDate != "2026-09-20" {
		t.Errorf("due date = %q, want 2026-09-20", notice.DueDate)
	}
}

func TestExtractInvoiceNoticeDirectDebitIsNotOwed(t *testing.T) {
	content := CategoryContent{
		Subject: "Ihre Rechnung für August 2026",
		Text: "Rechnungsnummer: R-2026-08-113. Gesamtbetrag 59,99 EUR. " +
			"Der Betrag wird am 15.09.2026 von Ihrem Konto abgebucht. Sie müssen nichts weiter tun.",
	}
	notice := mustNotice(t, ExtractInvoiceNotice(content, nil, "rechnung@provider.example.de", invoiceSentAt(t)))
	if notice.Settlement != SettlementDirectDebit {
		t.Errorf("settlement = %q, want direct_debit", notice.Settlement)
	}
	if notice.Status != InvoicePaid {
		t.Errorf("status = %q, want paid", notice.Status)
	}
}

func TestExtractInvoiceNoticeDunningLevels(t *testing.T) {
	cases := []struct {
		subject string
		want    int
	}{
		{"Zahlungserinnerung zu Rechnung 4711", InvoiceDunningReminder},
		{"Mahnung zu Rechnung 4711", InvoiceDunningNotice},
		{"2. Mahnung - Rechnung 4711", InvoiceDunningFinal},
		{"Letzte Mahnung vor Inkasso, Rechnung 4711", InvoiceDunningFinal},
		{"Payment reminder for invoice 4711", InvoiceDunningReminder},
		{"Ihre Rechnung 4711", InvoiceDunningNone},
	}
	for _, tc := range cases {
		content := CategoryContent{
			Subject: tc.subject,
			Text:    "Offener Betrag: 100,00 EUR. Bankverbindung: DE02120300000000202051.",
		}
		notice := mustNotice(t, ExtractInvoiceNotice(content, nil, "buchhaltung@firma.example.de", invoiceSentAt(t)))
		if notice.DunningLevel != tc.want {
			t.Errorf("%q: dunning level = %d, want %d", tc.subject, notice.DunningLevel, tc.want)
		}
	}
}

// A dunning letter is its own evidence that money is owed: no bank details, no
// deadline, and it still has to produce an open row.
func TestExtractInvoiceNoticeDunningIsAlwaysOpen(t *testing.T) {
	content := CategoryContent{
		Subject: "Mahnung",
		Text:    "Leider konnten wir bis heute keinen Zahlungseingang feststellen.",
	}
	notice := mustNotice(t, ExtractInvoiceNotice(content, nil, "mahnwesen@firma.example.de", invoiceSentAt(t)))
	if notice.Status != InvoiceOpen {
		t.Errorf("status = %q, want open", notice.Status)
	}
	if notice.Reference != "mahnung:2026-09-03" {
		t.Errorf("reference = %q, want the message's own day as the fallback", notice.Reference)
	}
}

// The escape clause at the bottom of every dunning letter says "bereits
// bezahlt". Reading it as a receipt would silence the one message that matters.
func TestExtractInvoiceNoticeConditionalPaidPhraseIsNotAReceipt(t *testing.T) {
	content := CategoryContent{
		Subject: "Zahlungserinnerung Rechnung 900",
		Text: "Offener Betrag 30,00 EUR, zahlbar bis 10.09.2026. " +
			"Sollten Sie den Betrag bereits bezahlt haben, betrachten Sie dieses Schreiben als gegenstandslos.",
	}
	notice := mustNotice(t, ExtractInvoiceNotice(content, nil, "info@firma.example.de", invoiceSentAt(t)))
	if notice.Status != InvoiceOpen {
		t.Errorf("status = %q, want open", notice.Status)
	}
}

// Boilerplate about dunning fees is on the back of half the invoices ever
// printed, and it is in the body, which is why the grades are read from the
// subject.
func TestExtractInvoiceNoticeDunningBoilerplateDoesNotChase(t *testing.T) {
	content := CategoryContent{
		Subject: "Rechnung 12345",
		Text: "Zahlbar bis 30.09.2026 auf unser Konto DE02120300000000202051. " +
			"Bei Zahlungsverzug werden Mahngebühren in Höhe von 5,00 EUR fällig.",
	}
	notice := mustNotice(t, ExtractInvoiceNotice(content, nil, "rechnung@firma.example.de", invoiceSentAt(t)))
	if notice.DunningLevel != InvoiceDunningNone {
		t.Errorf("dunning level = %d, want none", notice.DunningLevel)
	}
}

func TestExtractInvoiceNoticePaymentConfirmation(t *testing.T) {
	content := CategoryContent{
		Subject: "Zahlungsbestätigung zu Rechnung 2026-4711",
		Text:    "Ihre Zahlung ist bei uns eingegangen. Vielen Dank.",
	}
	notice := mustNotice(t, ExtractInvoiceNotice(content, nil, "billing@shop.example.de", invoiceSentAt(t)))
	if notice.Status != InvoicePaid {
		t.Errorf("status = %q, want paid", notice.Status)
	}
	if notice.Reference != "2026-4711" {
		t.Errorf("reference = %q, want the same reference the invoice was filed under", notice.Reference)
	}
}

func TestExtractInvoiceNoticeRelativeDeadline(t *testing.T) {
	content := CategoryContent{
		Subject: "Rechnung 88012",
		Text:    "Zahlbar innerhalb von 14 Tagen. Bankverbindung: DE02120300000000202051.",
	}
	notice := mustNotice(t, ExtractInvoiceNotice(content, nil, "a@firma.example.de", invoiceSentAt(t)))
	if notice.DueDate != "2026-09-17" {
		t.Errorf("due date = %q, want 2026-09-17", notice.DueDate)
	}
}

// A contract is in the same category and is not money owed.
func TestExtractInvoiceNoticeIgnoresContracts(t *testing.T) {
	content := CategoryContent{
		Subject: "Ihr Vertrag wurde verlängert",
		Text:    "Ihre Vertragsnummer lautet V-99182. Die Laufzeit endet am 31.12.2027.",
	}
	if notice := ExtractInvoiceNotice(content, nil, "service@provider.example.de", invoiceSentAt(t)); notice != nil {
		t.Errorf("want no notice for a contract, got %+v", notice)
	}
}

// An order confirmation names a number and a total and there is nothing to do
// about it.
func TestExtractInvoiceNoticeIgnoresOrderConfirmation(t *testing.T) {
	content := CategoryContent{
		Subject: "Bestellbestätigung 99182",
		Text:    "Vielen Dank für Ihre Bestellung über 42,00 EUR. Die Rechnung erhalten Sie separat.",
	}
	if notice := ExtractInvoiceNotice(content, nil, "shop@haendler.example.de", invoiceSentAt(t)); notice != nil {
		t.Errorf("want no notice for an order confirmation, got %+v", notice)
	}
}

func TestExtractInvoiceNoticeReadsAttachmentText(t *testing.T) {
	content := CategoryContent{
		Subject: "Ihre Rechnung",
		Text:    "Die Rechnung finden Sie im Anhang.",
		Files:   []CategoryFile{{Filename: "Rechnung_4711.pdf", ContentType: "application/pdf"}},
	}
	docs := []InvoiceDocument{{
		Filename:    "Rechnung_4711.pdf",
		ContentType: "application/pdf",
		Text:        "Rechnungsnummer: 4711\nGesamtbetrag 250,00 EUR\nZahlbar bis 24.09.2026\nIBAN DE02120300000000202051",
	}}
	notice := mustNotice(t, ExtractInvoiceNotice(content, docs, "buchhaltung@firma.example.de", invoiceSentAt(t)))
	if notice.Number != "4711" {
		t.Errorf("number = %q, want 4711", notice.Number)
	}
	if notice.DueDate != "2026-09-24" {
		t.Errorf("due date = %q, want 2026-09-24", notice.DueDate)
	}
	if notice.Amount != "250.00" {
		t.Errorf("amount = %q, want 250.00", notice.Amount)
	}
}

// The PDF is where "already paid" is stated when the covering mail says nothing.
func TestExtractInvoiceNoticeAttachmentSettlesTheBill(t *testing.T) {
	content := CategoryContent{
		Subject: "Ihre Rechnung 6001",
		Text:    "Ihre Rechnung im Anhang.",
	}
	docs := []InvoiceDocument{{
		Filename:    "Rechnung_6001.pdf",
		ContentType: "application/pdf",
		Text:        "Rechnungsnummer 6001\nGesamtbetrag 19,99 EUR\nZahlungsart: PayPal\nDer Betrag wurde bereits beglichen.",
	}}
	notice := mustNotice(t, ExtractInvoiceNotice(content, docs, "billing@shop.example.de", invoiceSentAt(t)))
	if notice.Status != InvoicePaid {
		t.Errorf("status = %q, want paid", notice.Status)
	}
}

func TestNormalizeAmount(t *testing.T) {
	cases := map[string]string{
		"1.234,56": "1234.56",
		"1,234.56": "1234.56",
		"149,90":   "149.90",
		"149.90":   "149.90",
		"1 234,56": "1234.56",
		"42":       "42.00",
		"0,00":     "0.00",
	}
	for raw, want := range cases {
		got, ok := normalizeAmount(raw)
		if !ok || got != want {
			t.Errorf("normalizeAmount(%q) = %q, %v; want %q", raw, got, ok, want)
		}
	}
	if _, ok := normalizeAmount("1.2.3.4"); ok {
		t.Error("want a reference number spelled with separators to be refused")
	}
}

func TestRegistrableDomain(t *testing.T) {
	cases := map[string]string{
		"billing@mail.example.de":    "example.de",
		"a@example.co.uk":            "example.co.uk",
		"b@sub.domain.example.co.uk": "example.co.uk",
		"Rechnung <c@Example.DE>":    "example.de",
		"d@example.com":              "example.com",
	}
	for address, want := range cases {
		if got := registrableDomain(BareAddress(address)); got != want {
			t.Errorf("registrableDomain(%q) = %q, want %q", address, got, want)
		}
	}
}

// A structured e-invoice states the payment method as a code, which is the one
// source here that cannot be misread -- so it overrules the prose.
func TestExtractInvoiceNoticeStructuredOverridesProse(t *testing.T) {
	const ubl = `<?xml version="1.0" encoding="UTF-8"?>
<Invoice xmlns="urn:oasis:names:specification:ubl:schema:xsd:Invoice-2">
  <ID>RE-2026-777</ID>
  <DueDate>2026-09-30</DueDate>
  <DocumentCurrencyCode>EUR</DocumentCurrencyCode>
  <PaymentMeans><PaymentMeansCode>59</PaymentMeansCode></PaymentMeans>
  <LegalMonetaryTotal><PayableAmount currencyID="EUR">310.00</PayableAmount></LegalMonetaryTotal>
</Invoice>`
	content := CategoryContent{
		Subject: "Rechnung RE-2026-777",
		Text:    "Bitte überweisen Sie den Betrag auf unser Konto DE02120300000000202051.",
	}
	docs := []InvoiceDocument{{
		Filename:    "xrechnung.xml",
		ContentType: "application/xml",
		Text:        ubl,
	}}
	notice := mustNotice(t, ExtractInvoiceNotice(content, docs, "billing@firma.example.de", invoiceSentAt(t)))
	if notice.Settlement != SettlementDirectDebit {
		t.Errorf("settlement = %q, want direct_debit from payment means code 59", notice.Settlement)
	}
	if notice.Status != InvoicePaid {
		t.Errorf("status = %q, want paid -- the biller collects it", notice.Status)
	}
	if notice.DueDate != "2026-09-30" {
		t.Errorf("due date = %q, want 2026-09-30", notice.DueDate)
	}
	if notice.Number != "RE-2026-777" {
		t.Errorf("number = %q", notice.Number)
	}
	if notice.Amount != "310.00" {
		t.Errorf("amount = %q, want 310.00", notice.Amount)
	}
}

func TestParseStructuredInvoiceCII(t *testing.T) {
	const cii = `<?xml version="1.0" encoding="UTF-8"?>
<rsm:CrossIndustryInvoice xmlns:rsm="urn:un:unece:uncefact:data:standard:CrossIndustryInvoice:100"
    xmlns:ram="urn:un:unece:uncefact:data:standard:ReusableAggregateBusinessInformationEntity:100">
  <rsm:ExchangedDocument><ram:ID>ZF-2026-12</ram:ID></rsm:ExchangedDocument>
  <rsm:SupplyChainTradeTransaction>
    <ram:ApplicableHeaderTradeSettlement>
      <ram:InvoiceCurrencyCode>EUR</ram:InvoiceCurrencyCode>
      <ram:SpecifiedTradeSettlementPaymentMeans><ram:TypeCode>58</ram:TypeCode></ram:SpecifiedTradeSettlementPaymentMeans>
      <ram:SpecifiedTradePaymentTerms>
        <ram:DueDateDateTime><udt:DateTimeString format="102" xmlns:udt="urn:x">20261015</udt:DateTimeString></ram:DueDateDateTime>
      </ram:SpecifiedTradePaymentTerms>
      <ram:SpecifiedTradeSettlementHeaderMonetarySummation>
        <ram:DuePayableAmount>1234.56</ram:DuePayableAmount>
      </ram:SpecifiedTradeSettlementHeaderMonetarySummation>
    </ram:ApplicableHeaderTradeSettlement>
  </rsm:SupplyChainTradeTransaction>
</rsm:CrossIndustryInvoice>`
	parsed, ok := parseStructuredInvoice([]byte(cii))
	if !ok {
		t.Fatal("want the CII document to parse")
	}
	if parsed.Number != "ZF-2026-12" {
		t.Errorf("number = %q", parsed.Number)
	}
	if parsed.DueDate != "2026-10-15" {
		t.Errorf("due date = %q, want 2026-10-15", parsed.DueDate)
	}
	if parsed.Amount != "1234.56" {
		t.Errorf("amount = %q", parsed.Amount)
	}
	if settlementForPaymentMeans(parsed.PaymentMeans) != SettlementTransfer {
		t.Errorf("payment means %q should be a transfer", parsed.PaymentMeans)
	}
}

// An XML attachment that is not an invoice must leave the prose's answers alone.
func TestParseStructuredInvoiceIgnoresUnrelatedXML(t *testing.T) {
	if _, ok := parseStructuredInvoice([]byte(`<?xml version="1.0"?><Shipment><ID>1</ID></Shipment>`)); ok {
		t.Error("want an unrelated XML document to be refused")
	}
}

func TestInvoiceNoticeReaderScanReadsAttachment(t *testing.T) {
	raw := strings.Join([]string{
		"From: Buchhaltung <billing@firma.example.de>",
		"Subject: Ihre Rechnung 30012",
		"Date: Thu, 03 Sep 2026 09:12:00 +0200",
		"MIME-Version: 1.0",
		`Content-Type: multipart/mixed; boundary="b1"`,
		"",
		"--b1",
		"Content-Type: text/plain; charset=utf-8",
		"",
		"Ihre Rechnung finden Sie im Anhang.",
		"--b1",
		`Content-Type: application/xml; name="xrechnung.xml"`,
		`Content-Disposition: attachment; filename="xrechnung.xml"`,
		"",
		`<?xml version="1.0"?><Invoice xmlns="urn:oasis:names:specification:ubl:schema:xsd:Invoice-2">` +
			`<ID>30012</ID><DueDate>2026-09-25</DueDate>` +
			`<PaymentMeans><PaymentMeansCode>58</PaymentMeansCode></PaymentMeans>` +
			`<LegalMonetaryTotal><PayableAmount currencyID="EUR">75.00</PayableAmount></LegalMonetaryTotal></Invoice>`,
		"--b1--",
		"",
	}, "\r\n")

	notice, complete, err := InvoiceNoticeReaderScan(strings.NewReader(raw))
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if !complete {
		t.Error("want the scan to have reached the end of the message")
	}
	got := mustNotice(t, notice)
	if got.Number != "30012" {
		t.Errorf("number = %q, want 30012", got.Number)
	}
	if got.DueDate != "2026-09-25" {
		t.Errorf("due date = %q, want 2026-09-25", got.DueDate)
	}
	if got.Status != InvoiceOpen {
		t.Errorf("status = %q, want open -- code 58 is the reader transferring", got.Status)
	}
	if got.Settlement != SettlementTransfer {
		t.Errorf("settlement = %q, want transfer", got.Settlement)
	}
}

// pdftotext wraps a PDF's lines wherever the page layout did, so the sentence
// announcing a direct debit routinely straddles a newline. Reading it only on
// one line was a reminder for money the biller collects itself.
func TestExtractInvoiceNoticeReadsADebitNoticeAcrossLines(t *testing.T) {
	content := CategoryContent{Subject: "Ihre Rechnung 6001", Text: "Ihre Rechnung im Anhang."}
	wrapped := []InvoiceDocument{{
		Filename:    "Rechnung_6001.pdf",
		ContentType: "application/pdf",
		Text:        "Rechnungsnummer 6001\nGesamtbetrag 19,99 EUR\nDer Betrag wird am 15.09.2026\nvon Ihrem Konto abgebucht.",
	}}
	oneLine := []InvoiceDocument{{
		Filename:    "Rechnung_6001.pdf",
		ContentType: "application/pdf",
		Text:        "Rechnungsnummer 6001 Gesamtbetrag 19,99 EUR Der Betrag wird am 15.09.2026 von Ihrem Konto abgebucht.",
	}}
	for name, docs := range map[string][]InvoiceDocument{"wrapped": wrapped, "one line": oneLine} {
		notice := mustNotice(t, ExtractInvoiceNotice(content, docs, "billing@shop.example.de", invoiceSentAt(t)))
		if notice.Settlement != SettlementDirectDebit {
			t.Errorf("%s: settlement = %q, want direct_debit", name, notice.Settlement)
		}
		if notice.Status != InvoicePaid {
			t.Errorf("%s: status = %q, want paid", name, notice.Status)
		}
	}
}

// A deadline written without a year, in a message sent in December, means the
// January weeks ahead -- not the one eleven months back, which the invoice
// window is wide enough to accept and which reads as a year of being overdue.
func TestExtractInvoiceNoticeResolvesAYearlessDeadlineAcrossNewYear(t *testing.T) {
	berlin, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Skipf("zone data unavailable: %v", err)
	}
	content := CategoryContent{
		Subject: "Ihre Rechnung 12345",
		Text:    "Zahlbar bis 15.01. auf unser Konto. Bankverbindung: DE02120300000000202051.",
	}
	sent := time.Date(2026, time.December, 20, 9, 0, 0, 0, berlin)
	notice := mustNotice(t, ExtractInvoiceNotice(content, nil, "billing@firma.example.de", sent))
	if notice.DueDate != "2027-01-15" {
		t.Errorf("due date = %q, want 2027-01-15", notice.DueDate)
	}
}

// The mirror image: a deadline in early January refers back to December, and
// must not jump a year forward.
func TestExtractInvoiceNoticeResolvesAYearlessDeadlineBackwards(t *testing.T) {
	berlin, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Skipf("zone data unavailable: %v", err)
	}
	content := CategoryContent{
		Subject: "Zahlungserinnerung Rechnung 900",
		Text:    "Zahlbar war der Betrag bis 28.12. Bankverbindung: DE02120300000000202051.",
	}
	sent := time.Date(2027, time.January, 8, 9, 0, 0, 0, berlin)
	notice := mustNotice(t, ExtractInvoiceNotice(content, nil, "billing@firma.example.de", sent))
	if notice.DueDate != "2026-12-28" {
		t.Errorf("due date = %q, want 2026-12-28", notice.DueDate)
	}
}
