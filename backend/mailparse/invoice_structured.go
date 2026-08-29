// File overview: The machine-readable half of an e-invoice -- XRechnung as a
// bare XML attachment, ZUGFeRD and Factur-X as XML embedded inside the PDF --
// and the fields worth reading out of it.
//
// This exists because everything in invoice_due.go is inference and this is
// not. A sentence saying "der Betrag wird abgebucht" has to be told apart from
// a footer listing payment methods; BT-81 is a code that says which of the two
// it is, and it cannot be misread. So where a message carries one of these, it
// wins over every rule that read the prose.
//
// The reader is deliberately shape-agnostic. The same invoice is expressed in
// two mutually unintelligible schemas -- UN/CEFACT CII, which ZUGFeRD and
// XRechnung-CII use, and OASIS UBL, which XRechnung-UBL uses -- and both bury
// the four fields wanted here at the end of long, differently-named paths.
// Rather than model either schema, the decoder walks the document and picks
// elements out by local name and by the ancestry that disambiguates them. That
// is a few dozen lines instead of a few hundred, and it does not break when a
// sender uses a profile or a namespace prefix this build has never seen.

package mailparse

import (
	"bytes"
	"encoding/xml"
	"io"
	"strings"
	"time"
)

const (
	// maxStructuredInvoiceBytes bounds how much XML is parsed. A CII invoice
	// with a hundred line items is well under a megabyte; past this it is not a
	// document this needs to read to the end.
	maxStructuredInvoiceBytes = 4 * 1024 * 1024
	// maxStructuredInvoiceDepth bounds the element nesting the walker follows.
	// Both schemas are about ten deep; the bound is what stops a hand-built
	// document from turning the path stack into a memory cost.
	maxStructuredInvoiceDepth = 40
	// maxStructuredFieldBytes bounds one element's accumulated text. Every field
	// read here is a number, a date or a currency code; an element longer than
	// this is not one of them, and collecting it whole would let a hand-built
	// document spend memory through a reader that is otherwise all bounds.
	maxStructuredFieldBytes = 4096
)

// structuredInvoice is what one e-invoice XML states, in this package's own
// vocabulary rather than either schema's.
type structuredInvoice struct {
	Number   string
	DueDate  string
	Amount   string
	Currency string
	// PaymentMeans is the UNCL4461 code the document gave, unmapped. Empty when
	// it stated none, which is legal and common on an invoice that expects a
	// plain transfer.
	PaymentMeans string
	// Found records that the parse recognised the document as an e-invoice at
	// all, so a file that merely happened to be XML does not overwrite good
	// answers with empty ones.
	Found bool
}

// applyStructuredInvoice overwrites what prose suggested with what a structured
// e-invoice states. Only fields the document actually carried are taken: an
// invoice that omits the due date -- which is legal, and means "on receipt" --
// must not erase a deadline the covering mail spelled out in words.
func applyStructuredInvoice(notice *InvoiceNotice, docs []InvoiceDocument, sent time.Time) {
	for i, doc := range docs {
		if i >= maxInvoiceDocuments {
			break
		}
		xmlData := structuredPayload(doc)
		if len(xmlData) == 0 {
			continue
		}
		parsed, ok := parseStructuredInvoice(xmlData)
		if !ok {
			continue
		}
		if parsed.Number != "" {
			notice.Number = parsed.Number
		}
		if parsed.DueDate != "" {
			notice.DueDate = parsed.DueDate
		}
		if parsed.Amount != "" {
			notice.Amount = parsed.Amount
			notice.Currency = parsed.Currency
		}
		if settlement := settlementForPaymentMeans(parsed.PaymentMeans); settlement != SettlementUnknown {
			notice.Settlement = settlement
		}
		// The first e-invoice in a message is the message's invoice. A second
		// one is a copy in the other syntax, or a previous month's attached for
		// reference, and neither should overwrite what the first said.
		return
	}
	_ = sent
}

// structuredPayload is the XML this document carries: the one a caller already
// found, the file itself when it is an XML e-invoice, or the one hidden inside
// a PDF.
func structuredPayload(doc InvoiceDocument) []byte {
	if len(doc.Structured) > 0 {
		return doc.Structured
	}
	name := strings.ToLower(doc.Filename)
	mediaType := strings.ToLower(doc.ContentType)
	if strings.HasSuffix(name, ".xml") || strings.Contains(mediaType, "xml") {
		// The text of an XML attachment is the XML, because the extractor
		// treats it as indexable text rather than a binary.
		return []byte(doc.Text)
	}
	if strings.HasSuffix(name, ".pdf") || strings.Contains(mediaType, "pdf") {
		return embeddedInvoiceXML(doc.Raw)
	}
	return nil
}

// settlementForPaymentMeans maps UNCL4461, the code list both schemas carry
// their payment method in, onto the only question this app asks: does the
// reader have to move the money themselves.
//
// The codes not listed are the ones that do not answer it -- 1 is "instrument
// not defined", 97 is a clearing arrangement -- and they leave the prose's
// answer standing rather than replacing it with a worse one.
func settlementForPaymentMeans(code string) string {
	switch strings.TrimSpace(code) {
	case "30", "31", "42", "58":
		// Credit transfer, in its national and SEPA spellings. The reader pays.
		return SettlementTransfer
	case "49", "59":
		// Direct debit, national and SEPA. The biller collects.
		return SettlementDirectDebit
	case "48", "54", "55":
		// Bank card, credit card, debit card.
		return SettlementCard
	case "68":
		// "Online payment service" -- PayPal and its kind.
		return SettlementWallet
	case "10", "20":
		// Cash and cheque. Neither is a transfer the reader makes from a
		// banking app, but both are money they still owe.
		return SettlementTransfer
	default:
		return SettlementUnknown
	}
}

// The element names each field is carried under, by local name. The two schemas
// are merged into one table on purpose: no name here means the same thing in
// both, so a document can be read without first deciding which it is.
var (
	structuredNumberNames   = map[string]bool{"id": true}
	structuredDueDateNames  = map[string]bool{"duedatedatetime": true, "duedate": true}
	structuredAmountNames   = map[string]bool{"duepayableamount": true, "payableamount": true}
	structuredCurrencyNames = map[string]bool{"invoicecurrencycode": true, "documentcurrencycode": true}
	structuredMeansNames    = map[string]bool{"typecode": true, "paymentmeanscode": true}
)

// The ancestors that make an otherwise ambiguous name mean what is wanted.
// "ID" appears on every party, every line and every attachment in CII; the one
// that is the invoice number is the one directly under the document header.
var (
	structuredNumberParents = map[string]bool{"exchangeddocument": true, "invoice": true, "crossindustryinvoice": true}
	structuredMeansParents  = map[string]bool{
		"specifiedtradesettlementpaymentmeans": true,
		"paymentmeans":                         true,
	}
)

// parseStructuredInvoice walks one e-invoice and picks out the four fields.
//
// The walk is a token stream rather than a document tree: an invoice is a
// streamable shape, and unmarshalling one into structs would mean modelling two
// schemas to reach four values. The path stack is what gives each element the
// context that tells "the invoice's ID" from "the seller's ID".
func parseStructuredInvoice(data []byte) (structuredInvoice, bool) {
	if len(data) == 0 || len(data) > maxStructuredInvoiceBytes {
		return structuredInvoice{}, false
	}
	decoder := xml.NewDecoder(bytes.NewReader(data))
	// A mail attachment is not a trusted document. Both switches turn off the
	// parts of XML that reach outside the bytes in hand or expand inside them.
	decoder.Strict = false
	decoder.Entity = map[string]string{}
	decoder.CharsetReader = func(_ string, input io.Reader) (io.Reader, error) { return input, nil }

	out := structuredInvoice{}
	path := make([]string, 0, 16)
	// depth counts every element, where path only holds the ones inside the cap.
	// The two have to be tracked apart: popping on an end tag whose start tag was
	// never pushed unwinds the path past where it should be, and from there on
	// every element is reported under the wrong parent -- which is how a line
	// item's <ID> becomes the invoice number. A document deep enough to hit the
	// cap is not one this reads correctly either way, but it must not be read
	// *wrongly*.
	depth := 0
	// An element's text is collected and read once, when its end tag arrives,
	// rather than as each chunk of character data turns up. The decoder is
	// entitled to split one element's text across several CharData tokens --
	// it does so at an entity, so "Rechnung &amp; Co" arrives in three pieces --
	// and every field here is written first-one-wins. Acting per token would
	// therefore store "Rechnung" and silently drop the rest, which for an
	// invoice number is a reference that matches nothing.
	var text strings.Builder
	for {
		token, err := decoder.Token()
		if err != nil {
			break
		}
		switch element := token.(type) {
		case xml.StartElement:
			name := strings.ToLower(element.Name.Local)
			depth++
			if depth <= maxStructuredInvoiceDepth {
				path = append(path, name)
			}
			// Whatever stood before a child element belongs to no field: these
			// documents put values in leaves, and the mixed content that would
			// make this wrong does not occur in either schema.
			text.Reset()
			if isStructuredInvoiceRoot(name) {
				out.Found = true
			}
			// A currency is carried as an attribute on the amount as often as
			// it is as an element of its own.
			if out.Currency == "" && structuredAmountNames[name] {
				out.Currency = attributeValue(element, "currencyid")
			}
		case xml.CharData:
			// Text below the cap is skipped rather than attributed to whatever
			// the truncated path happens to end in.
			if len(path) == 0 || depth > maxStructuredInvoiceDepth {
				continue
			}
			// The builder is bounded for the same reason everything else here
			// is: this is an attachment from a stranger, and one element is
			// entitled to be as long as the document.
			if text.Len() < maxStructuredFieldBytes {
				text.Write(element)
			}
		case xml.EndElement:
			if depth <= maxStructuredInvoiceDepth && len(path) > 0 {
				if value := strings.TrimSpace(text.String()); value != "" {
					applyStructuredField(&out, path, value)
				}
				path = path[:len(path)-1]
			}
			text.Reset()
			if depth > 0 {
				depth--
			}
		}
	}
	if !out.Found {
		return structuredInvoice{}, false
	}
	return out, true
}

// isStructuredInvoiceRoot recognises the document element of each syntax. It is
// what keeps an unrelated XML attachment -- a bank statement, a shipping
// manifest -- from being read as an invoice and overwriting the prose's answers
// with nothing.
func isStructuredInvoiceRoot(name string) bool {
	switch name {
	case "crossindustryinvoice", "crossindustrydocument", "invoice", "creditnote":
		return true
	default:
		return false
	}
}

// applyStructuredField records one element's text against the field it belongs
// to. First writer wins for every field: both schemas state each of these once
// in the header, and anything later in the document is a line item repeating
// the shape.
func applyStructuredField(out *structuredInvoice, path []string, value string) {
	name := path[len(path)-1]
	parent := ""
	if len(path) >= 2 {
		parent = path[len(path)-2]
	}
	switch {
	case out.Number == "" && structuredNumberNames[name] && structuredNumberParents[parent]:
		out.Number = normalizeInvoiceNumber(value)
	case out.DueDate == "" && structuredDueDateNames[name]:
		out.DueDate = structuredDate(value)
	case out.DueDate == "" && name == "datetimestring" && structuredDueDateNames[parent]:
		// CII wraps its dates one level deeper, in a DateTimeString carrying a
		// format attribute.
		out.DueDate = structuredDate(value)
	case out.Amount == "" && structuredAmountNames[name]:
		if amount, ok := normalizeAmount(value); ok {
			out.Amount = amount
		}
	case out.Currency == "" && structuredCurrencyNames[name]:
		out.Currency = normalizeCurrency(value)
	case out.PaymentMeans == "" && structuredMeansNames[name] && structuredMeansParents[parent]:
		out.PaymentMeans = value
	}
}

// structuredDate accepts the two spellings the schemas use: UBL's plain
// "2026-09-15" and CII's "20260915". Anything else is left alone rather than
// guessed at.
func structuredDate(value string) string {
	value = strings.TrimSpace(value)
	if len(value) >= 10 {
		if day, err := time.Parse("2006-01-02", value[:10]); err == nil {
			return plainDate(day)
		}
	}
	if len(value) == 8 {
		if day, err := time.Parse("20060102", value); err == nil {
			return plainDate(day)
		}
	}
	return ""
}

func attributeValue(element xml.StartElement, name string) string {
	for _, attr := range element.Attr {
		if strings.EqualFold(attr.Name.Local, name) {
			return normalizeCurrency(attr.Value)
		}
	}
	return ""
}
