// File overview: Digging the e-invoice XML out of a ZUGFeRD or Factur-X PDF.
//
// A hybrid e-invoice is one file that is two documents: a PDF/A a human reads,
// and an XML a machine reads, attached to the PDF as an embedded file. The
// second is the interesting one -- it states the due date and the payment
// method as fields rather than as prose -- and it is precisely the half that
// pdftotext never returns, because an embedded file is not page content.
//
// What is here is not a PDF parser and does not want to be. Reaching the
// embedded file properly means walking the trailer to the catalogue, the
// catalogue to /Names, /Names to /EmbeddedFiles, resolving indirect references
// and possibly an xref stream on the way -- several hundred lines of object
// model to reach one stream. Instead this scans for the streams directly and
// asks each one whether it is an invoice, which is a question the content
// answers unambiguously: the XML names its own root element. A stream that is
// not one costs an inflate that fails or a few bytes that do not match.
//
// Everything here is bounded, because the input is an attachment from a
// stranger: how many streams are examined, how large one may be, and how much
// it may inflate to.

package mailparse

import (
	"bytes"
	"compress/zlib"
	"io"
	"strings"
)

const (
	// maxPDFScanBytes bounds how much of a PDF is searched for streams. The
	// embedded file of a ZUGFeRD invoice is written near the front by every
	// producer in use; a PDF larger than this is a scanned bundle whose
	// interesting half, if it had one, is not what makes it large.
	maxPDFScanBytes = 8 * 1024 * 1024
	// maxPDFStreamsScanned bounds how many streams are examined. A one-page
	// invoice has a handful -- content, fonts, colour profile, the embedded
	// file; past this the file is not the shape this is looking for.
	maxPDFStreamsScanned = 64
	// maxPDFStreamBytes bounds one compressed stream, and maxPDFInflatedBytes
	// what it may become. The second is the one that matters: a few kilobytes
	// of zeros inflate to a great deal more, and an attachment is not a
	// trusted document.
	maxPDFStreamBytes    = 4 * 1024 * 1024
	maxPDFInflatedBytes  = 8 * 1024 * 1024
	minEmbeddedXMLLength = 64
)

// embeddedInvoiceXML returns the e-invoice XML embedded in a PDF, or nil when
// there is none -- which is the answer for every ordinary PDF invoice, and so
// is the case this is written to reach cheaply.
func embeddedInvoiceXML(pdf []byte) []byte {
	if len(pdf) < minEmbeddedXMLLength || !bytes.HasPrefix(bytes.TrimLeft(pdf[:min(len(pdf), 16)], "\x00 \r\n\t"), []byte("%PDF")) {
		return nil
	}
	data := pdf
	if len(data) > maxPDFScanBytes {
		data = data[:maxPDFScanBytes]
	}
	// A file name is not required to find the XML, but where the PDF does carry
	// one it is a strong hint that there is something to find at all. Its
	// absence is not a reason to stop: the marker sits in an object that may be
	// past the scan bound even when the stream is not.
	offset := 0
	for scanned := 0; scanned < maxPDFStreamsScanned; scanned++ {
		start := bytes.Index(data[offset:], []byte("stream"))
		if start < 0 {
			return nil
		}
		start += offset + len("stream")
		// The keyword is followed by CRLF or LF, and by nothing else that is
		// legal. Skipping it exactly is what keeps the first inflate byte right.
		if start < len(data) && data[start] == '\r' {
			start++
		}
		if start < len(data) && data[start] == '\n' {
			start++
		}
		end := bytes.Index(data[start:], []byte("endstream"))
		if end < 0 {
			return nil
		}
		body := data[start : start+end]
		offset = start + end + len("endstream")
		if len(body) == 0 || len(body) > maxPDFStreamBytes {
			continue
		}
		if xmlData := invoiceXMLFromStream(body); xmlData != nil {
			return xmlData
		}
	}
	return nil
}

// invoiceXMLFromStream reads one stream as an e-invoice, compressed or not.
func invoiceXMLFromStream(body []byte) []byte {
	if looksLikeInvoiceXML(body) {
		return body
	}
	inflated := inflateBounded(body)
	if inflated != nil && looksLikeInvoiceXML(inflated) {
		return inflated
	}
	return nil
}

// inflateBounded undoes the /FlateDecode every producer writes the embedded
// file with, refusing anything that expands past the bound rather than reading
// to the end and checking afterwards.
func inflateBounded(body []byte) []byte {
	reader, err := zlib.NewReader(bytes.NewReader(body))
	if err != nil {
		return nil
	}
	defer reader.Close()
	// One byte past the bound is read on purpose: it is what tells "exactly the
	// limit" from "the limit and more to come", and the latter is refused.
	out, err := io.ReadAll(io.LimitReader(reader, maxPDFInflatedBytes+1))
	if err != nil && len(out) == 0 {
		return nil
	}
	if len(out) > maxPDFInflatedBytes {
		return nil
	}
	return out
}

// looksLikeInvoiceXML is the question each stream is asked. It is answered from
// the root element rather than from the file name, because the name lives in a
// different object and the root is in the bytes already in hand.
func looksLikeInvoiceXML(data []byte) bool {
	if len(data) < minEmbeddedXMLLength {
		return false
	}
	// Only the head is searched: an XML document names its root in the first
	// few hundred bytes, after at most a declaration and a comment.
	head := data
	if len(head) > 2048 {
		head = head[:2048]
	}
	if !bytes.Contains(head, []byte("<")) {
		return false
	}
	text := string(head)
	for _, root := range embeddedInvoiceRoots {
		if strings.Contains(text, root) {
			return true
		}
	}
	return false
}

// embeddedInvoiceRoots are the document elements of the syntaxes a hybrid
// invoice embeds, with their usual prefixes left off so a producer's choice of
// namespace prefix does not matter.
var embeddedInvoiceRoots = []string{
	"CrossIndustryInvoice",
	"CrossIndustryDocument",
	":Invoice",
	"<Invoice",
	":CreditNote",
	"<CreditNote",
}
