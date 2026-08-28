// File overview: The part of a message that category classification is allowed
// to read beyond the headers, and the bounded scan that recovers it from a
// stored message.
//
// Headers cannot tell an invoice from any other robot mail: both are
// auto-submitted, both come from a no-reply address, and the difference is the
// subject line, the amount in the body, and the file the message carries. What
// is collected here is the smallest part of a message that answers that
// question -- names and text, never attachment bytes -- so classification stays
// something a background worker can run over a whole mailbox.

package mailparse

import (
	"html"
	"io"
	"mime"
	"mime/multipart"
	"net/mail"
	"net/textproto"
	"regexp"
	"strings"
	"unicode/utf8"
)

// CategoryContent is one message as content classification sees it.
type CategoryContent struct {
	Subject string
	Text    string
	Files   []CategoryFile
	// DeliveryLinks are the carrier tracking links the message carries, and
	// only those: a link to a carrier's own tracking page says both which
	// carrier and which parcel, which is more than any wording in the body can.
	// Everything else a message links to is dropped where it is found, so a
	// marketing mail's hundred links never reach a caller or the heap.
	DeliveryLinks []string
}

// CategoryFile is one attachment as content classification sees it: what it
// calls itself and what it claims to be, never its bytes. A structured
// e-invoice is recognizable from its filename alone -- the standards mandate
// the name -- which is the whole reason the scan keeps names and throws the
// data away.
type CategoryFile struct {
	Filename    string
	ContentType string
}

const (
	// maxCategoryContentText bounds the body text one decision reads. What
	// names a document names it near the top: a subject line, a greeting, an
	// invoice number and an amount. Reading a whole marketing mail after that
	// buys nothing and would put an arbitrary amount of body text on the heap
	// of a worker that is holding a tenant's turn.
	maxCategoryContentText = 16 * 1024
	// maxCategoryContentFiles bounds how many attachment names are kept. A
	// message carrying more files than this is not describing itself with the
	// ones past the limit.
	maxCategoryContentFiles = 32
	// maxCategoryScanBytes bounds how much of a stored message the backfill
	// reads. MIME is a stream, so a part cannot be skipped without passing over
	// it, and without a bound the pass would read every attachment of every
	// message in the mailbox from disk to learn a filename. A message whose
	// evidence sits past the bound keeps the category it already has -- a miss,
	// not a wrong answer, and the sender can still be filed by hand. That is a
	// property of the scan and of what the caller does with a truncated one:
	// scanCategoryContent says which it produced, and a truncated answer may
	// fill an empty category but never replace one.
	//
	// New mail does not pay this: it is classified while the parser has the
	// whole decoded message in hand.
	maxCategoryScanBytes = 1024 * 1024
	// maxDeliveryLinks bounds how many tracking links one message contributes.
	// A dispatch mail for a large order links one per parcel; past this it is
	// listing something else.
	maxDeliveryLinks = 16
	// maxDeliveryLinkScanBytes bounds how much markup is searched for them on
	// the fetch path, where the whole decoded message is in hand. The scan path
	// is already bounded by the text budget it shares with classification.
	maxDeliveryLinkScanBytes = 256 * 1024
	// maxDeliveryLinkCandidates bounds how many URLs are examined, as against
	// how many are kept. A marketing mail carries hundreds and none of them is a
	// carrier; without this the scan pays for every one of them on every message
	// fetched. A carrier's own link is not the five hundredth in a message.
	maxDeliveryLinkCandidates = 200
	// maxInvoiceScanBytes is the invoice reader's own budget, and it is eight
	// times the classification one because it is spending it on something else.
	// Classification reads a stored message to learn a filename and stops; the
	// invoice reader has to reach the bytes of the PDF, because the sentence
	// that says a bill was already settled is routinely only in there.
	//
	// It is affordable because of what selects the messages it runs on: only
	// mail the category has already called paperwork, which is a small fraction
	// of a mailbox, in batches on a background worker.
	maxInvoiceScanBytes = 8 * 1024 * 1024
	// maxInvoiceDocumentBytes bounds one attachment the invoice reader decodes.
	// An invoice PDF is a few hundred kilobytes; something larger than this is
	// a scan of a bundle, whose text extraction would cost more than the answer
	// is worth.
	maxInvoiceDocumentBytes = 4 * 1024 * 1024
)

// urlRE matches an absolute http(s) URL. Markup is searched with it directly
// rather than parsed: an href's value is a URL wherever it sits, and a real
// parse of a marketing mail's DOM would cost more than every rule that reads
// the result.
var urlRE = regexp.MustCompile(`https?://[^\s"'<>)\]]+`)

// appendDeliveryLinks adds the tracking links one piece of text carries. HTML
// entities are undone first: a query string is written "?a=1&amp;b=2" in
// markup, and the carrier's parcel number is routinely the parameter after it.
func appendDeliveryLinks(dst []string, text string) []string {
	if len(text) > maxDeliveryLinkScanBytes {
		text = text[:maxDeliveryLinkScanBytes]
	}
	if !strings.Contains(text, "http") {
		return dst
	}
	// The matches are walked one at a time rather than collected. A marketing
	// mail carries hundreds of links and all but a handful are dropped by the
	// filter below, so gathering them all first would put every one of them on
	// the heap of the worker holding this tenant's turn -- which is the cost
	// DeliveryLinks exists to avoid. Slicing the match keeps the substring
	// pointing into the text; only a link that is kept, or one carrying an
	// entity, allocates.
	for offset, seen := 0, 0; offset < len(text) && len(dst) < maxDeliveryLinks && seen < maxDeliveryLinkCandidates; seen++ {
		match := urlRE.FindStringIndex(text[offset:])
		if match == nil {
			break
		}
		candidate := text[offset+match[0] : offset+match[1]]
		offset += match[1]
		if !strings.Contains(candidate, "&amp;") && !DeliveryLinkCandidate(candidate) {
			// The host decides, and an entity cannot appear in one, so an
			// unescaped candidate is rejected before it is unescaped.
			continue
		}
		candidate = strings.TrimRight(html.UnescapeString(candidate), ".,;:")
		if !DeliveryLinkCandidate(candidate) || containsString(dst, candidate) {
			continue
		}
		dst = append(dst, candidate)
	}
	return dst
}

// CategoryContent reduces an already-parsed message to what classification
// reads. Parsing has decoded all of it anyway, so the fetch path pays nothing
// for content classification beyond this copy.
func (p ParsedMessage) CategoryContent() CategoryContent {
	content := CategoryContent{Subject: p.Subject, Text: limitCategoryText(p.Text)}
	// Links are read from the markup, not from the indexed text: stripping HTML
	// keeps what a reader sees and throws away every href, which is exactly
	// where a carrier's tracking link lives.
	content.DeliveryLinks = appendDeliveryLinks(appendDeliveryLinks(nil, p.Text), p.HTML)
	// A message that is only HTML still says what it is; the indexed text is
	// empty for it, so the markup is stripped here the way display does it.
	if strings.TrimSpace(content.Text) == "" && strings.TrimSpace(p.HTML) != "" {
		content.Text = limitCategoryText(stripHTML(p.HTML))
	}
	for _, file := range p.Files {
		if len(content.Files) >= maxCategoryContentFiles {
			break
		}
		content.Files = append(content.Files, CategoryFile{Filename: file.Filename, ContentType: file.ContentType})
	}
	return content
}

// limitCategoryText cuts body text to the classification budget on a rune
// boundary, so the folded text the rules match against cannot end in half a
// character.
func limitCategoryText(value string) string {
	if len(value) <= maxCategoryContentText {
		return value
	}
	cut := maxCategoryContentText
	for cut > 0 && !utf8.RuneStart(value[cut]) {
		cut--
	}
	return value[:cut]
}

// scanCategoryContent reads a stored message far enough to classify it: the
// header block, the body text, and the attachment names. It is the backfill's
// counterpart to what Parse already has in hand for newly fetched mail.
//
// Only a failure to read the headers is reported. A body that stops early --
// because the message is truncated, malformed, or simply longer than the scan
// budget -- still leaves a header set and whatever text was collected, and
// classifying from that is better than filing the message on its address alone.
//
// The second return says whether the scan reached the end of what it was given
// rather than stopping at a budget. A truncated scan may have missed the very
// attachment that names the message, so its answer is allowed to fill an empty
// category but never to replace one a whole message produced.
func scanCategoryContent(r io.Reader) (mail.Header, CategoryContent, bool, error) {
	header, content, _, complete, err := scanMessageContent(r, maxCategoryScanBytes, false)
	return header, content, complete, err
}

// scanInvoiceContent is the same walk with the attachments actually opened. It
// is a separate entry point rather than a flag on the one above because the two
// cost entirely different things, and nothing should reach for this one by
// accident: it decodes document bodies and, for a PDF, runs an external text
// extractor on each.
func scanInvoiceContent(r io.Reader) (mail.Header, CategoryContent, []InvoiceDocument, bool, error) {
	return scanMessageContent(r, maxInvoiceScanBytes, true)
}

func scanMessageContent(r io.Reader, budget int64, collectDocuments bool) (mail.Header, CategoryContent, []InvoiceDocument, bool, error) {
	counted := &countingReader{r: io.LimitReader(r, budget)}
	msg, err := mail.ReadMessage(counted)
	if err != nil {
		return nil, CategoryContent{}, nil, false, err
	}
	subject, _ := wordDecoder().DecodeHeader(msg.Header.Get("Subject"))
	scan := &categoryScan{subject: strings.TrimSpace(subject), collectDocuments: collectDocuments}
	scan.walk(textproto.MIMEHeader(msg.Header), msg.Body, 0)
	complete := counted.n < budget && !scan.droppedFile
	return msg.Header, scan.content(), scan.docs, complete, nil
}

// countingReader reports how much of the message the scan actually pulled, which
// is how hitting the byte budget is told apart from reaching the end.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// categoryScan collects the scan's findings while it walks the MIME tree.
type categoryScan struct {
	subject string
	text    strings.Builder
	files   []CategoryFile
	links   []string
	// droppedFile records that the message carried more attachments than the
	// scan keeps names for, which is the other way it can miss the one file
	// that would have named the message.
	droppedFile bool
	// collectDocuments turns on decoding attachment bodies, which the ordinary
	// scan deliberately never does. Only the invoice reader sets it.
	collectDocuments bool
	docs             []InvoiceDocument
}

func (s *categoryScan) content() CategoryContent {
	return CategoryContent{Subject: s.subject, Text: s.text.String(), Files: s.files, DeliveryLinks: s.links}
}

// walk descends one MIME part. Errors are absorbed rather than propagated: a
// part that cannot be read is one part, and the parts already collected still
// describe the message.
func (s *categoryScan) walk(header textproto.MIMEHeader, body io.Reader, depth int) {
	mediaType, params, err := mime.ParseMediaType(header.Get("Content-Type"))
	if err != nil || mediaType == "" {
		mediaType = "text/plain"
	}
	lowerMediaType := strings.ToLower(mediaType)
	if strings.HasPrefix(lowerMediaType, "multipart/") {
		if depth >= maxMIMEDepth {
			return
		}
		mr := multipart.NewReader(body, params["boundary"])
		for {
			part, err := mr.NextPart()
			if err != nil {
				return
			}
			s.walk(part.Header, part, depth+1)
		}
	}

	disposition, dispParams, _ := mime.ParseMediaType(header.Get("Content-Disposition"))
	filename := ""
	if dispParams != nil {
		filename = dispParams["filename"]
	}
	if filename == "" && params != nil {
		filename = params["name"]
	}
	if filename != "" || strings.EqualFold(disposition, "attachment") {
		name := decodedHeader(filename)
		s.addFile(name, mediaType)
		// The bytes are deliberately left unread unless somebody asked for
		// them. NextPart drains what is left of this part on its way to the
		// next boundary, which is the cheapest skip MIME allows, and the scan
		// budget bounds even that.
		if s.collectDocuments && invoiceDocumentCandidate(name, mediaType) {
			s.addDocument(name, mediaType, header, body)
		}
		return
	}
	switch lowerMediaType {
	case "text/plain", "text/html":
		remaining := maxCategoryContentText - s.text.Len()
		if remaining <= 0 {
			return
		}
		decoded, err := io.ReadAll(io.LimitReader(decodeTransfer(header, body), int64(remaining)))
		if err != nil && !isTolerableEOF(err) && len(decoded) == 0 {
			return
		}
		text := decodeTextBytes(decoded, params["charset"])
		// The links are taken before the markup is stripped, for the same
		// reason the fetch path reads them out of the raw HTML.
		s.links = appendDeliveryLinks(s.links, text)
		if lowerMediaType == "text/html" {
			text = stripHTML(text)
		}
		s.addText(text)
	}
}

func (s *categoryScan) addText(value string) {
	value = normalizeText(value)
	if value == "" {
		return
	}
	if s.text.Len() > 0 {
		s.text.WriteByte(' ')
	}
	s.text.WriteString(value)
}

func (s *categoryScan) addFile(filename, mediaType string) {
	if len(s.files) >= maxCategoryContentFiles {
		s.droppedFile = true
		return
	}
	s.files = append(s.files, CategoryFile{Filename: filename, ContentType: mediaType})
}

// addDocument decodes one attachment far enough to read a bill out of it. The
// text goes through the same extractor the search index uses, so a PDF is read
// by pdftotext here exactly as it is on the fetch path and the two paths cannot
// reach different answers about the same file.
//
// A part too large for the bound is skipped rather than truncated: half a PDF
// is not a smaller PDF, and handing a truncated one to an extractor produces
// either nothing or, worse, a page of text without the total on it.
//
// Every way out of here that skips a document marks the scan as incomplete, and
// that is not bookkeeping -- it is the whole safety rule. droppedFile is what
// makes scanMessageContent report the scan as partial, and a partial scan is
// forbidden from recording an unpaid invoice (syncer.invoiceFromScan). A
// document skipped silently would leave the scan calling itself complete while
// the one page saying the bill was already collected went unread, which is the
// false reminder this feature must not produce.
func (s *categoryScan) addDocument(filename, mediaType string, header textproto.MIMEHeader, body io.Reader) {
	if len(s.docs) >= maxInvoiceDocuments {
		s.droppedFile = true
		return
	}
	// One byte past the bound distinguishes "exactly the limit" from "the limit
	// and more to come".
	data, err := io.ReadAll(io.LimitReader(decodeTransfer(header, body), maxInvoiceDocumentBytes+1))
	if err != nil && !isTolerableEOF(err) && len(data) == 0 {
		s.droppedFile = true
		return
	}
	if len(data) == 0 || len(data) > maxInvoiceDocumentBytes {
		s.droppedFile = true
		return
	}
	file := Attachment{Filename: filename, ContentType: mediaType, Data: data}
	s.docs = append(s.docs, InvoiceDocument{
		Filename:    filename,
		ContentType: mediaType,
		Text:        file.SearchableText(),
		Raw:         data,
	})
}
