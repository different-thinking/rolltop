package mailparse

import (
	"bytes"
	"compress/zlib"
	"strings"
	"testing"
)

const testInvoiceXML = `<?xml version="1.0" encoding="UTF-8"?>
<rsm:CrossIndustryInvoice xmlns:rsm="urn:un:unece:uncefact:data:standard:CrossIndustryInvoice:100">
  <rsm:ExchangedDocument><ram:ID>ZF-2026-99</ram:ID></rsm:ExchangedDocument>
</rsm:CrossIndustryInvoice>`

// hybridPDF builds the shape a ZUGFeRD producer writes: an ordinary PDF header,
// a stream of page content, and the invoice XML as a second, compressed stream.
// It is not a valid PDF in any other respect, which is the point -- the reader
// under test deliberately does not parse the object model.
func hybridPDF(t *testing.T, payload []byte, compress bool) []byte {
	t.Helper()
	var out bytes.Buffer
	out.WriteString("%PDF-1.7\n")
	out.WriteString("1 0 obj\n<< /Length 12 >>\nstream\nBT /F1 Tf ET\nendstream\nendobj\n")
	out.WriteString("2 0 obj\n<< /Type /EmbeddedFile /Filter /FlateDecode >>\nstream\n")
	if compress {
		var packed bytes.Buffer
		writer := zlib.NewWriter(&packed)
		if _, err := writer.Write(payload); err != nil {
			t.Fatalf("compress: %v", err)
		}
		if err := writer.Close(); err != nil {
			t.Fatalf("close: %v", err)
		}
		out.Write(packed.Bytes())
	} else {
		out.Write(payload)
	}
	out.WriteString("\nendstream\nendobj\n%%EOF\n")
	return out.Bytes()
}

func TestEmbeddedInvoiceXMLFindsACompressedStream(t *testing.T) {
	found := embeddedInvoiceXML(hybridPDF(t, []byte(testInvoiceXML), true))
	if found == nil {
		t.Fatal("want the embedded invoice XML to be found")
	}
	if !strings.Contains(string(found), "ZF-2026-99") {
		t.Errorf("found the wrong stream: %.120q", found)
	}
}

// Not every producer compresses the embedded file, and an uncompressed one is
// readable without an inflate at all.
func TestEmbeddedInvoiceXMLFindsAnUncompressedStream(t *testing.T) {
	found := embeddedInvoiceXML(hybridPDF(t, []byte(testInvoiceXML), false))
	if found == nil || !strings.Contains(string(found), "ZF-2026-99") {
		t.Fatalf("want the uncompressed XML found, got %.120q", found)
	}
}

// The overwhelming majority of PDF invoices carry no XML at all, so answering
// "none" cheaply and correctly is the case that matters most.
func TestEmbeddedInvoiceXMLIgnoresAnOrdinaryPDF(t *testing.T) {
	if found := embeddedInvoiceXML(hybridPDF(t, []byte("just some page content, no invoice here at all"), true)); found != nil {
		t.Errorf("want no XML in an ordinary PDF, got %.120q", found)
	}
}

func TestEmbeddedInvoiceXMLIgnoresWhatIsNotAPDF(t *testing.T) {
	if found := embeddedInvoiceXML([]byte(testInvoiceXML)); found != nil {
		t.Error("want a bare XML file to be refused here -- it is not a PDF, and the caller reads it directly")
	}
	if found := embeddedInvoiceXML(nil); found != nil {
		t.Error("want nil for no input")
	}
}

// A ZUGFeRD PDF has to reach the same answer a bare XRechnung attachment does.
func TestExtractInvoiceNoticeReadsTheXMLInsideAPDF(t *testing.T) {
	const cii = `<?xml version="1.0" encoding="UTF-8"?>
<rsm:CrossIndustryInvoice xmlns:rsm="urn:un:unece:uncefact:data:standard:CrossIndustryInvoice:100">
  <rsm:ExchangedDocument><ram:ID>ZF-2026-55</ram:ID></rsm:ExchangedDocument>
  <rsm:SupplyChainTradeTransaction>
    <ram:ApplicableHeaderTradeSettlement>
      <ram:SpecifiedTradeSettlementPaymentMeans><ram:TypeCode>59</ram:TypeCode></ram:SpecifiedTradeSettlementPaymentMeans>
      <ram:SpecifiedTradePaymentTerms>
        <ram:DueDateDateTime><udt:DateTimeString xmlns:udt="urn:x">20260920</udt:DateTimeString></ram:DueDateDateTime>
      </ram:SpecifiedTradePaymentTerms>
    </ram:ApplicableHeaderTradeSettlement>
  </rsm:SupplyChainTradeTransaction>
</rsm:CrossIndustryInvoice>`
	content := CategoryContent{
		Subject: "Ihre Rechnung ZF-2026-55",
		// The visible page says "please transfer", which is what the prose rules
		// would conclude. The embedded XML says the money is collected by direct
		// debit, and it is the one that cannot be misread.
		Text: "Bitte überweisen Sie den Betrag auf unser Konto. IBAN DE02120300000000202051.",
	}
	docs := []InvoiceDocument{{
		Filename:    "Rechnung.pdf",
		ContentType: "application/pdf",
		Text:        "Rechnung ZF-2026-55",
		Raw:         hybridPDF(t, []byte(cii), true),
	}}
	notice := mustNotice(t, ExtractInvoiceNotice(content, docs, "billing@firma.example.de", invoiceSentAt(t)))
	if notice.Settlement != SettlementDirectDebit {
		t.Errorf("settlement = %q, want direct_debit from the embedded XML", notice.Settlement)
	}
	if notice.Status != InvoicePaid {
		t.Errorf("status = %q, want paid -- the biller collects it", notice.Status)
	}
	if notice.DueDate != "2026-09-20" {
		t.Errorf("due date = %q, want 2026-09-20", notice.DueDate)
	}
	if notice.Number != "ZF-2026-55" {
		t.Errorf("number = %q", notice.Number)
	}
}
