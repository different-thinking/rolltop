package mailparse

import (
	"fmt"
	"strings"
	"testing"
)

// invoiceMessageWithAttachment builds a multipart message carrying one named
// attachment of the given decoded size.
func invoiceMessageWithAttachment(filename string, size int) string {
	return strings.Join([]string{
		"From: Buchhaltung <billing@firma.example.de>",
		"Subject: Ihre Rechnung 4711",
		"Date: Thu, 03 Sep 2026 09:12:00 +0200",
		"MIME-Version: 1.0",
		`Content-Type: multipart/mixed; boundary="b1"`,
		"",
		"--b1",
		"Content-Type: text/plain; charset=utf-8",
		"",
		"Zahlbar bis 17.09.2026. IBAN DE02120300000000202051.",
		"--b1",
		fmt.Sprintf(`Content-Type: application/pdf; name=%q`, filename),
		fmt.Sprintf(`Content-Disposition: attachment; filename=%q`, filename),
		"",
		strings.Repeat("A", size),
		"--b1--",
		"",
	}, "\r\n")
}

// A document the scan could not take is the very place "already paid" tends to
// be written, so skipping one has to make the scan call itself incomplete --
// that flag is what stops the backfill recording an unpaid bill it never read.
func TestScanInvoiceContentReportsASkippedAttachment(t *testing.T) {
	small := invoiceMessageWithAttachment("Rechnung.pdf", 64)
	_, _, docs, complete, err := scanInvoiceContent(strings.NewReader(small))
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if !complete {
		t.Error("want a message whose attachment was read to count as complete")
	}
	if len(docs) != 1 {
		t.Fatalf("want the attachment collected, got %d", len(docs))
	}

	oversized := invoiceMessageWithAttachment("Rechnung.pdf", maxInvoiceDocumentBytes+16)
	_, _, docs, complete, err = scanInvoiceContent(strings.NewReader(oversized))
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(docs) != 0 {
		t.Errorf("want an oversized attachment skipped, got %d", len(docs))
	}
	if complete {
		t.Error("want a skipped attachment to mark the scan incomplete -- an open bill must not be recorded on evidence nothing read")
	}
}

// The same holds once the per-message document cap is reached.
func TestScanInvoiceContentReportsDroppedDocumentsPastTheCap(t *testing.T) {
	parts := []string{
		"From: Buchhaltung <billing@firma.example.de>",
		"Subject: Ihre Rechnung 4711",
		"Date: Thu, 03 Sep 2026 09:12:00 +0200",
		"MIME-Version: 1.0",
		`Content-Type: multipart/mixed; boundary="b1"`,
		"",
	}
	for i := 0; i < maxInvoiceDocuments+2; i++ {
		name := fmt.Sprintf("Rechnung_%d.pdf", i)
		parts = append(parts,
			"--b1",
			fmt.Sprintf(`Content-Type: application/pdf; name=%q`, name),
			fmt.Sprintf(`Content-Disposition: attachment; filename=%q`, name),
			"",
			"tiny",
		)
	}
	parts = append(parts, "--b1--", "")
	_, _, docs, complete, err := scanInvoiceContent(strings.NewReader(strings.Join(parts, "\r\n")))
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(docs) != maxInvoiceDocuments {
		t.Errorf("want the cap honoured, got %d documents", len(docs))
	}
	if complete {
		t.Error("want documents past the cap to mark the scan incomplete")
	}
}

// Past the depth cap the path stack must not unwind further than it was filled,
// or every element after it is attributed to the wrong parent -- which is how a
// line item's <ID> becomes the invoice number.
func TestParseStructuredInvoiceSurvivesDeepNesting(t *testing.T) {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0"?><Invoice xmlns="urn:oasis:names:specification:ubl:schema:xsd:Invoice-2">`)
	// A deeply nested branch, closed again, standing before the fields that
	// matter. If the pops outrun the pushes the path is wrong from here on.
	depth := maxStructuredInvoiceDepth + 10
	for i := 0; i < depth; i++ {
		b.WriteString("<Wrap>")
	}
	b.WriteString("<ID>NOT-THE-INVOICE-NUMBER</ID>")
	for i := 0; i < depth; i++ {
		b.WriteString("</Wrap>")
	}
	b.WriteString(`<ID>RE-2026-777</ID><DueDate>2026-09-30</DueDate>`)
	b.WriteString(`<PaymentMeans><PaymentMeansCode>58</PaymentMeansCode></PaymentMeans>`)
	b.WriteString(`<LegalMonetaryTotal><PayableAmount currencyID="EUR">10.00</PayableAmount></LegalMonetaryTotal>`)
	b.WriteString(`</Invoice>`)

	parsed, ok := parseStructuredInvoice([]byte(b.String()))
	if !ok {
		t.Fatal("want the document to parse")
	}
	if parsed.Number != "RE-2026-777" {
		t.Errorf("number = %q, want RE-2026-777 -- the deep branch must not have shifted the path", parsed.Number)
	}
	if parsed.DueDate != "2026-09-30" {
		t.Errorf("due date = %q, want 2026-09-30", parsed.DueDate)
	}
	if parsed.PaymentMeans != "58" {
		t.Errorf("payment means = %q, want 58", parsed.PaymentMeans)
	}
}
