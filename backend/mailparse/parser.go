// File overview: MIME parsing for message headers, bodies, attachments, and inline parts.

package mailparse

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/xml"
	"errors"
	"html"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"net/textproto"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/encoding"
	"golang.org/x/text/encoding/ianaindex"
	"golang.org/x/text/transform"
)

var htmlTextNoiseBlockRE = regexp.MustCompile(`(?is)<script\b[^>]*>.*?</script>|<style\b[^>]*>.*?</style>|<head\b[^>]*>.*?</head>|<title\b[^>]*>.*?</title>|<!--.*?-->`)

const (
	maxAttachmentSearchBytes = 512 * 1024
	maxPDFAttachmentBytes    = 32 * 1024 * 1024
	maxPDFSearchTextBytes    = 1024 * 1024
	maxOfficeAttachmentBytes = 32 * 1024 * 1024
	maxOfficeSearchTextBytes = 1024 * 1024
	pdfExtractionTimeout     = 10 * time.Second
	officeExtractionTimeout  = 10 * time.Second
)

var (
	pdfTextExtractor = extractPDFTextWithPdftotext
	docTextExtractor = extractDOCTextWithExternalTool
)

// Attachment is a decoded MIME part that may be indexed, displayed, or downloaded.
type Attachment struct {
	Filename    string
	ContentType string
	ContentID   string
	IsInline    bool
	Data        []byte
	// extracted memoizes SearchableText across the readers that want it. Two of
	// them now do -- the search index and the invoice reader -- and for a PDF
	// the answer costs a temporary file and an external process with a
	// ten-second budget, which is not a thing to pay for twice per message.
	//
	// It is a pointer because the callers range over parsed.Files by value, so
	// each of them holds a copy of the struct; a plain field would memoize into
	// the copy and be thrown away with it.
	extracted *extractedText
}

// extractedText is one attachment's text, computed at most once however many
// copies of the Attachment ask for it.
type extractedText struct {
	once sync.Once
	text string
}

// SearchableText extracts bounded text from attachments that are safe and useful
// to index. Binary files return an empty string so attachment bodies are not kept
// as separate blobs just for search.
//
// The answer is memoized where the parser prepared somewhere to keep it. An
// Attachment built anywhere else -- a test, a plugin -- has nowhere to memoize
// into and simply extracts each time, which is what it did before there was a
// second reader.
func (a Attachment) SearchableText() string {
	if a.extracted == nil {
		return a.searchableText()
	}
	a.extracted.once.Do(func() { a.extracted.text = a.searchableText() })
	return a.extracted.text
}

func (a Attachment) searchableText() string {
	mediaType, _, err := mime.ParseMediaType(a.ContentType)
	if err != nil {
		mediaType = strings.ToLower(strings.TrimSpace(a.ContentType))
	}
	mediaType = strings.ToLower(mediaType)
	ext := strings.ToLower(filepath.Ext(a.Filename))
	if mediaType == "text/html" || ext == ".html" || ext == ".htm" {
		return normalizeText(stripHTML(string(limitBytes(a.Data, maxAttachmentSearchBytes))))
	}
	if isSearchablePDFType(mediaType, ext) {
		text, err := pdfTextExtractor(a.Data)
		if err != nil {
			return ""
		}
		return normalizeText(string(limitBytes([]byte(text), maxPDFSearchTextBytes)))
	}
	if isSearchableDOCXType(mediaType, ext) {
		text, err := extractDOCXText(a.Data)
		if err != nil {
			return ""
		}
		return normalizeText(string(limitBytes([]byte(text), maxOfficeSearchTextBytes)))
	}
	if isSearchableODSType(mediaType, ext) {
		text, err := extractODSText(a.Data)
		if err != nil {
			return ""
		}
		return normalizeText(string(limitBytes([]byte(text), maxOfficeSearchTextBytes)))
	}
	if isSearchableDOCType(mediaType, ext) {
		text, err := docTextExtractor(a.Data)
		if err != nil {
			return ""
		}
		return normalizeText(string(limitBytes([]byte(text), maxOfficeSearchTextBytes)))
	}
	if isSearchableTextType(mediaType, ext) {
		return normalizeText(string(limitBytes(a.Data, maxAttachmentSearchBytes)))
	}
	return ""
}

// ParsedMessage is the normalized output from parsing a raw RFC822 message.
type ParsedMessage struct {
	MessageID   string
	InReplyTo   string
	References  string
	Subject     string
	From        string
	To          string
	CC          string
	Date        time.Time
	Text        string
	HTML        string
	Files       []Attachment
	IsEncrypted bool
	IsSigned    bool
	// HasAutocryptHeader records whether the top-level message carried an
	// Autocrypt header. The thread view gates its per-message key-import probe on
	// this so it no longer downloads every message's full source just to look.
	HasAutocryptHeader bool
	// Category is decided while the message is open: the headers name what kind
	// of traffic it is, and the body and attachment names decide whether it is
	// paperwork. Reading it back later means re-opening the raw message, which
	// is what the backfill has to do for mail stored before categories.
	Category string
	// Deliveries are the parcels the message talks about, extracted while it is
	// open for the same reason the category is: the alternative is re-opening
	// the raw message, and blob retention means most of a mailbox no longer has
	// one to open.
	Deliveries []DeliveryNotice
	// Invoice is the bill the message is about, or nil for the overwhelming
	// majority of mail that is about nothing of the sort. It is read here and
	// not later for the same retention reason as the two above, and for one
	// more that is its own: it is the only reading that opens attachment
	// *bodies*, and the bodies exist only while the message is being parsed.
	Invoice *InvoiceNotice
}

// Parse is the indexing/parser entrypoint. It decodes headers, walks MIME parts,
// collects text/html bodies and attachment metadata/data, and normalizes indexed
// text so CSS-heavy marketing mail does not poison snippets or search.
func Parse(raw []byte) (ParsedMessage, error) {
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return ParsedMessage{}, err
	}
	decoder := wordDecoder()
	subject, _ := decoder.DecodeHeader(msg.Header.Get("Subject"))
	parsed := ParsedMessage{
		MessageID:  strings.TrimSpace(msg.Header.Get("Message-ID")),
		InReplyTo:  strings.TrimSpace(msg.Header.Get("In-Reply-To")),
		References: strings.TrimSpace(msg.Header.Get("References")),
		Subject:    strings.TrimSpace(subject),
		From:       addressHeader(msg.Header.Get("From")),
		To:         addressHeader(msg.Header.Get("To")),
		CC:         addressHeader(msg.Header.Get("Cc")),
	}
	// The header decision is made before the body walk so that every exit below
	// -- including the truncated-message one -- leaves a categorized message.
	// What the walk collects can only refine it, in categorize below.
	parsed.Category = Categorize(msg.Header, parsed.From)
	parsed.HasAutocryptHeader = strings.TrimSpace(msg.Header.Get("Autocrypt")) != ""
	// The message's own date is kept in the zone it was written in as well as
	// in UTC. "Arrives tomorrow" is relative to the sender's day, and a message
	// written just after midnight in another zone resolves to the wrong day when
	// that offset has already been folded away.
	sent := time.Time{}
	if d, err := mail.ParseDate(msg.Header.Get("Date")); err == nil {
		parsed.Date = d.UTC()
		sent = d
	}
	if err := parsePart(textproto.MIMEHeader(msg.Header), msg.Body, &parsed, 0); err != nil {
		if isTolerableEOF(err) {
			parsed.Text = cleanIndexedText(parsed.Text)
			parsed.Sanitize()
			parsed.classify(sent)
			return parsed, nil
		}
		return ParsedMessage{}, err
	}
	parsed.Text = cleanIndexedText(parsed.Text)
	parsed.Sanitize()
	parsed.classify(sent)
	return parsed, nil
}

// classify settles everything the body decides once the body and the attachment
// names are known: the category, and the parcels the message announces. It runs
// after Sanitize so the text it reads is the text the rest of the app stores,
// and both readings are the same ones the backfill makes from a stored message
// -- only from a whole message rather than a bounded scan of one.
//
// The content is reduced once and read twice. Building it is the copy the whole
// design of CategoryContent exists to keep small, and doing it per reader would
// double it for every message fetched.
func (p *ParsedMessage) classify(sent time.Time) {
	content := p.CategoryContent()
	p.Category = applyInvoiceEvidence(p.Category, content)
	p.Deliveries = ExtractDeliveryNotices(content, p.From, sent)
	// The invoice reading is gated on the category, which the line above has
	// just settled. That gate is what makes it affordable to read attachment
	// bodies at all: paperwork is a small fraction of a mailbox, and every
	// other message pays nothing here beyond the comparison.
	if p.Category == CategoryInvoices {
		p.Invoice = ExtractInvoiceNotice(content, p.InvoiceDocuments(), p.From, sent)
	}
}

// InvoiceDocuments reduces the attachments to the ones a bill is actually
// carried in, with their text pulled out. It is exported because the on-demand
// path builds the same list from a message it re-parsed.
//
// The text comes from SearchableText, which is the point: the search index asks
// the same attachments the same question, and the answer is memoized, so
// reading an invoice out of a PDF costs a comparison on the fetch path rather
// than a second run of pdftotext.
func (p ParsedMessage) InvoiceDocuments() []InvoiceDocument {
	docs := make([]InvoiceDocument, 0, maxInvoiceDocuments)
	for _, file := range p.Files {
		if len(docs) >= maxInvoiceDocuments {
			break
		}
		if !invoiceDocumentCandidate(file.Filename, file.ContentType) {
			continue
		}
		docs = append(docs, InvoiceDocument{
			Filename:    file.Filename,
			ContentType: file.ContentType,
			Text:        file.SearchableText(),
			Raw:         file.Data,
		})
	}
	return docs
}

// invoiceDocumentCandidate reports whether an attachment is the kind of file an
// invoice arrives as. It is the same list categorization grades filenames by,
// read here to decide what is worth extracting text from rather than what a
// message is: an image or an archive is neither, whatever it is named.
func invoiceDocumentCandidate(filename, contentType string) bool {
	name := strings.ToLower(strings.TrimSpace(filename))
	mediaType := strings.ToLower(strings.TrimSpace(contentType))
	if hasAnySuffix(name, invoiceDocumentExtensions) {
		return true
	}
	switch {
	case strings.Contains(mediaType, "pdf"),
		strings.Contains(mediaType, "xml"),
		strings.Contains(mediaType, "officedocument"),
		strings.Contains(mediaType, "opendocument"),
		strings.Contains(mediaType, "msword"):
		return true
	default:
		return false
	}
}

// ParseDisplayBody is the lighter display path used when a raw message is loaded
// on demand. It skips attachment bodies and returns body text/html only.
func ParseDisplayBody(r io.Reader) (string, string, error) {
	msg, err := mail.ReadMessage(r)
	if err != nil {
		return "", "", err
	}
	var parsed ParsedMessage
	if err := parseDisplayPart(textproto.MIMEHeader(msg.Header), msg.Body, &parsed, 0); err != nil {
		if isTolerableEOF(err) {
			parsed.Sanitize()
			return normalizeDisplayText(parsed.Text), parsed.HTML, nil
		}
		return "", "", err
	}
	parsed.Sanitize()
	return normalizeDisplayText(parsed.Text), parsed.HTML, nil
}

// maxMIMEDepth bounds how deep the multipart recursion in parsePart and
// parseDisplayPart descends. Each level holds a live multipart.Reader (with its
// own buffer) for the whole recursion, so a message of a few hundred thousand
// nested multipart parts — well under the 16 MB fetch cap in raw bytes — can
// otherwise inflate to well over a gigabyte of live heap and OOM-kill the sync
// mid-turn, then reparse and crash again on restart. Legitimate mail nests only
// a handful of levels; a part below the limit is left unwalked rather than opened.
const maxMIMEDepth = 50

// parsePart recursively walks the MIME tree for indexing. Attachments keep their
// decoded data long enough for search extraction; text/html parts feed the message
// body fields used for search and previews.
func parsePart(header textproto.MIMEHeader, body io.Reader, parsed *ParsedMessage, depth int) error {
	contentType := header.Get("Content-Type")
	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil || mediaType == "" {
		mediaType = "text/plain"
	}
	if strings.HasPrefix(strings.ToLower(mediaType), "multipart/") {
		if depth >= maxMIMEDepth {
			return nil
		}
		mr := multipart.NewReader(body, params["boundary"])
		for {
			part, err := mr.NextPart()
			if err == io.EOF {
				return nil
			}
			if isTolerableEOF(err) {
				return nil
			}
			if err != nil {
				return err
			}
			if err := parsePart(part.Header, part, parsed, depth+1); err != nil {
				if isTolerableEOF(err) {
					return nil
				}
				return err
			}
		}
	}

	decoded, err := io.ReadAll(decodeTransfer(header, body))
	if err != nil {
		if isTolerableEOF(err) {
			decoded = bytes.TrimSpace(decoded)
		} else {
			return err
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
	contentID := strings.Trim(header.Get("Content-ID"), "<>")
	if filename != "" || strings.EqualFold(disposition, "attachment") || isCIDReferencedPart(contentID, mediaType) {
		parsed.Files = append(parsed.Files, Attachment{
			Filename:    decodedHeader(filename),
			ContentType: mediaType,
			ContentID:   contentID,
			IsInline:    isInlineMIMEFile(disposition, mediaType, contentID),
			Data:        decoded,
			extracted:   &extractedText{},
		})
		return nil
	}

	switch strings.ToLower(mediaType) {
	case "text/plain":
		parsed.Text += "\n" + decodeTextBytes(decoded, params["charset"])
	case "text/html":
		htmlText := decodeTextBytes(decoded, params["charset"])
		if strings.TrimSpace(parsed.HTML) == "" {
			parsed.HTML = htmlText
		}
		if strings.TrimSpace(parsed.Text) == "" {
			parsed.Text += "\n" + stripHTML(htmlText)
		}
	}
	return nil
}

// isCIDReferencedPart reports a part the message body can only reach through its
// Content-ID. Senders routinely ship those images with neither a filename nor a
// disposition, and the filename test above dropped them: the part never became
// an attachment row, the cid: reference in the body found nothing to point at,
// and the reader showed a broken image for a picture the message carried all
// along. text/ parts stay out of it - multipart/related gives the HTML body
// itself a Content-ID often enough, and pulling that out of the body and into
// the attachment list would empty the message.
func isCIDReferencedPart(contentID, mediaType string) bool {
	if strings.TrimSpace(contentID) == "" {
		return false
	}
	return !strings.HasPrefix(strings.ToLower(strings.TrimSpace(mediaType)), "text/")
}

func isInlineMIMEFile(disposition, mediaType, contentID string) bool {
	disposition = strings.ToLower(strings.TrimSpace(disposition))
	mediaType = strings.ToLower(strings.TrimSpace(mediaType))
	contentID = strings.TrimSpace(contentID)
	if disposition == "attachment" {
		return false
	}
	if disposition == "inline" {
		return true
	}
	return contentID != "" && strings.HasPrefix(mediaType, "image/")
}

// parseDisplayPart mirrors parsePart but discards attachment streams immediately,
// avoiding unnecessary memory use when the caller only needs renderable body text.
func parseDisplayPart(header textproto.MIMEHeader, body io.Reader, parsed *ParsedMessage, depth int) error {
	contentType := header.Get("Content-Type")
	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil || mediaType == "" {
		mediaType = "text/plain"
	}
	lowerMediaType := strings.ToLower(mediaType)
	if strings.HasPrefix(lowerMediaType, "multipart/") {
		if depth >= maxMIMEDepth {
			return nil
		}
		mr := multipart.NewReader(body, params["boundary"])
		for {
			part, err := mr.NextPart()
			if err == io.EOF {
				return nil
			}
			if isTolerableEOF(err) {
				return nil
			}
			if err != nil {
				return err
			}
			if err := parseDisplayPart(part.Header, part, parsed, depth+1); err != nil {
				if isTolerableEOF(err) {
					return nil
				}
				return err
			}
			if lowerMediaType != "multipart/alternative" && parsedDisplayBodyFound(parsed) {
				return nil
			}
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
		_, _ = io.Copy(io.Discard, decodeTransfer(header, body))
		return nil
	}

	decoded, err := io.ReadAll(decodeTransfer(header, body))
	if err != nil {
		if isTolerableEOF(err) {
			decoded = bytes.TrimSpace(decoded)
		} else {
			return err
		}
	}
	switch lowerMediaType {
	case "text/plain":
		parsed.Text += "\n" + decodeTextBytes(decoded, params["charset"])
	case "text/html":
		htmlText := decodeTextBytes(decoded, params["charset"])
		if strings.TrimSpace(parsed.HTML) == "" {
			parsed.HTML = htmlText
		}
		if strings.TrimSpace(parsed.Text) == "" {
			parsed.Text += "\n" + stripHTML(htmlText)
		}
	}
	return nil
}

func parsedDisplayBodyFound(parsed *ParsedMessage) bool {
	return strings.TrimSpace(parsed.HTML) != "" || strings.TrimSpace(parsed.Text) != ""
}

func isTolerableEOF(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "unexpected eof") || strings.Contains(msg, "nextpart: eof")
}

func decodeTransfer(header textproto.MIMEHeader, body io.Reader) io.Reader {
	switch strings.ToLower(strings.TrimSpace(header.Get("Content-Transfer-Encoding"))) {
	case "base64":
		return base64.NewDecoder(base64.StdEncoding, body)
	case "quoted-printable":
		return quotedprintable.NewReader(body)
	default:
		return body
	}
}

func addressHeader(value string) string {
	if strings.TrimSpace(value) == "" {
		return ""
	}
	addrs, err := (&mail.AddressParser{WordDecoder: wordDecoder()}).ParseList(value)
	if err != nil {
		return SanitizeText(strings.TrimSpace(value))
	}
	out := make([]string, 0, len(addrs))
	for _, addr := range addrs {
		// Each part is repaired before it is quoted, not after: strconv.Quote
		// escapes a byte that is not valid UTF-8 into the literal text \xNN,
		// which is itself valid UTF-8 and therefore nothing SanitizeText would
		// touch afterwards. An encoded word that declares UTF-8 and carries
		// ISO-8859-1 - the common case for a display name - would be stored as
		// "M\xfcller" for good.
		address := SanitizeText(addr.Address)
		if name := SanitizeText(strings.TrimSpace(addr.Name)); name != "" {
			out = append(out, strconv.Quote(name)+" <"+address+">")
			continue
		}
		out = append(out, address)
	}
	return strings.Join(out, ", ")
}

func decodedHeader(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	out, err := wordDecoder().DecodeHeader(value)
	if err != nil {
		return SanitizeText(value)
	}
	return SanitizeText(out)
}

// DecodeTextBytes exposes MIME charset decoding for callers that need consistent text handling outside full parsing.
func DecodeTextBytes(data []byte, charset string) string {
	return decodeTextBytes(data, charset)
}

func wordDecoder() *mime.WordDecoder {
	return &mime.WordDecoder{CharsetReader: charsetReader}
}

func charsetReader(charset string, input io.Reader) (io.Reader, error) {
	enc, err := lookupTextEncoding(charset)
	if err != nil {
		return nil, err
	}
	if enc == nil {
		return input, nil
	}
	return transform.NewReader(input, enc.NewDecoder()), nil
}

// decodeTextBytes prefers the declared MIME charset, then handles common older
// Japanese escape sequences, and finally falls back to repairing the raw bytes
// so bad mail still produces some display/index text. Every path ends in
// SanitizeText: a part that declares no charset, or declares one it does not
// hold, is the usual source of the bytes PostgreSQL rejects.
func decodeTextBytes(data []byte, charset string) string {
	if strings.TrimSpace(charset) != "" {
		if text, ok := decodeBytesAsCharset(data, charset); ok {
			return SanitizeText(text)
		}
	}
	if looksLikeISO2022JP(data) {
		if text, ok := decodeBytesAsCharset(data, "iso-2022-jp"); ok {
			return SanitizeText(text)
		}
	}
	return SanitizeText(string(data))
}

func decodeBytesAsCharset(data []byte, charset string) (string, bool) {
	enc, err := lookupTextEncoding(charset)
	if err != nil {
		return "", false
	}
	if enc == nil {
		return string(data), true
	}
	out, err := io.ReadAll(transform.NewReader(bytes.NewReader(data), enc.NewDecoder()))
	if err != nil {
		return "", false
	}
	return string(out), true
}

func lookupTextEncoding(charset string) (encoding.Encoding, error) {
	charset = strings.ToLower(strings.Trim(strings.TrimSpace(charset), `"`))
	switch charset {
	case "", "utf-8", "utf8", "us-ascii", "ascii":
		return nil, nil
	default:
		return ianaindex.MIME.Encoding(charset)
	}
}

func looksLikeISO2022JP(data []byte) bool {
	return bytes.Contains(data, []byte("\x1b$B")) ||
		bytes.Contains(data, []byte("\x1b$@")) ||
		bytes.Contains(data, []byte("\x1b(J"))
}

func normalizeText(value string) string {
	fields := strings.Fields(value)
	if len(fields) == 0 {
		return ""
	}
	return strings.Join(fields, " ")
}

func normalizeDisplayText(value string) string {
	value = strings.ReplaceAll(value, "\r\n", "\n")
	value = strings.ReplaceAll(value, "\r", "\n")
	lines := strings.Split(value, "\n")
	for len(lines) > 0 && strings.TrimSpace(lines[0]) == "" {
		lines = lines[1:]
	}
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	return strings.Join(lines, "\n")
}

// cleanIndexedText removes CSS-looking debris before body text is stored/indexed;
// display-time rendering should not be responsible for fixing indexed snippets.
func cleanIndexedText(value string) string {
	value = removeIndexedCSSRules(value)
	value = trimIndexedTextJunk(value)
	return normalizeDisplayText(value)
}

func removeIndexedCSSRules(value string) string {
	// Fast path: a body with no '{' has no rules to strip, so skip the scan and
	// the match map entirely (the common case for plain text).
	if strings.IndexByte(value, '{') < 0 {
		return value
	}
	// A body with an absurd number of braces is never real CSS; skip stripping
	// rather than build an O(n) match map that would amplify a crafted input into
	// a large transient allocation. The debris then stays in the indexed text,
	// which is cosmetic — the point is to bound both time and memory.
	if strings.Count(value, "{") > maxIndexedCSSBraces {
		return value
	}
	closes := matchIndexedCSSBraces(value)
	var b strings.Builder
	for i := 0; i < len(value); {
		openRel := strings.Index(value[i:], "{")
		if openRel < 0 {
			b.WriteString(value[i:])
			break
		}
		open := i + openRel
		close, ok := closes[open]
		if !ok {
			b.WriteString(value[i:])
			break
		}
		start := indexedCSSSelectorStart(value, open)
		selector := strings.TrimSpace(value[start:open])
		body := strings.TrimSpace(value[open+1 : close])
		if looksLikeIndexedCSSRule(selector, body) {
			if start > i {
				b.WriteString(value[i:start])
			}
			b.WriteByte(' ')
			i = close + 1
			continue
		}
		b.WriteString(value[i : open+1])
		i = open + 1
	}
	return b.String()
}

// maxIndexedCSSBraces caps how many opening braces removeIndexedCSSRules will
// process. Real CSS in a mail body has at most a few thousand rules; beyond this
// the input is debris and stripping is skipped so the match map cannot grow
// without bound. The cap also bounds the matcher's stack, so nesting depth needs
// no separate limit (which previously mispaired braces past its cutoff).
const maxIndexedCSSBraces = 20000

// matchIndexedCSSBraces returns, for each '{' byte position in value, the byte
// position of its balanced '}' (or no entry when it has none), computed in a
// single linear pass. removeIndexedCSSRules used to rescan from every '{' to its
// close, which is O(n^2) on nested braces: a text/plain body of a few hundred
// thousand nested braces made a single Parse run for minutes and stalled the
// sync of the folder holding it. Callers gate this on maxIndexedCSSBraces, so
// the map and stack stay bounded. '{' and '}' are single-byte ASCII that cannot
// appear inside a multi-byte UTF-8 rune, so a byte scan is equivalent to the old
// rune-by-rune walk.
func matchIndexedCSSBraces(value string) map[int]int {
	closes := make(map[int]int)
	stack := make([]int, 0, 64)
	for i := 0; i < len(value); i++ {
		switch value[i] {
		case '{':
			stack = append(stack, i)
		case '}':
			if n := len(stack); n > 0 {
				closes[stack[n-1]] = i
				stack = stack[:n-1]
			}
		}
	}
	return closes
}

func indexedCSSSelectorStart(value string, open int) int {
	start := open
	for start > 0 {
		r, size := utf8.DecodeLastRuneInString(value[:start])
		if r == utf8.RuneError && size == 0 {
			break
		}
		if r == '{' || r == '}' || r == ';' || r == '\n' || r == '\r' {
			break
		}
		start -= size
	}
	for start < open {
		r, size := utf8.DecodeRuneInString(value[start:open])
		if r == utf8.RuneError && size == 0 {
			break
		}
		if !unicode.IsSpace(r) {
			break
		}
		start += size
	}
	return start
}

func looksLikeIndexedCSSRule(selector, body string) bool {
	if selector == "" || body == "" || len(body) > 2000 {
		return false
	}
	lowerSelector := strings.ToLower(selector)
	lowerBody := strings.ToLower(body)
	if !strings.Contains(lowerBody, ":") {
		return false
	}
	for _, token := range []string{"margin", "padding", "color", "font", "display", "width", "height", "box-sizing", "line-height", "text-decoration", "background", "border"} {
		if strings.Contains(lowerBody, token+":") || strings.Contains(lowerBody, token+"-") {
			return true
		}
	}
	return strings.ContainsAny(lowerSelector, "#.*>[],:") ||
		strings.Contains(lowerSelector, "body") ||
		strings.Contains(lowerSelector, "table") ||
		strings.Contains(lowerSelector, "div") ||
		strings.Contains(lowerSelector, "span") ||
		strings.Contains(lowerSelector, "img")
}

func trimIndexedTextJunk(value string) string {
	value = strings.TrimSpace(value)
	for {
		next := strings.TrimLeft(value, " -_.,;:|}")
		if next == value {
			return value
		}
		value = strings.TrimSpace(next)
	}
}

func limitBytes(data []byte, max int) []byte {
	if len(data) <= max {
		return data
	}
	return data[:max]
}

func isSearchablePDFType(mediaType, ext string) bool {
	return mediaType == "application/pdf" || mediaType == "application/x-pdf" || ext == ".pdf"
}

func isSearchableDOCXType(mediaType, ext string) bool {
	return mediaType == "application/vnd.openxmlformats-officedocument.wordprocessingml.document" || ext == ".docx"
}

func isSearchableDOCType(mediaType, ext string) bool {
	switch mediaType {
	case "application/msword", "application/vnd.ms-word", "application/x-msword":
		return true
	default:
		return ext == ".doc"
	}
}

func isSearchableODSType(mediaType, ext string) bool {
	return mediaType == "application/vnd.oasis.opendocument.spreadsheet" || ext == ".ods"
}

func extractPDFTextWithPdftotext(data []byte) (string, error) {
	if len(data) == 0 {
		return "", nil
	}
	if len(data) > maxPDFAttachmentBytes {
		return "", errors.New("pdf attachment too large for search extraction")
	}
	tmp, err := os.CreateTemp("", "rolltop-pdf-*.pdf")
	if err != nil {
		return "", err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), pdfExtractionTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "pdftotext", "-enc", "UTF-8", "-layout", tmpName, "-")
	cmd.Stderr = io.Discard
	out, err := cmd.Output()
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if err != nil {
		return "", err
	}
	return string(limitBytes(out, maxPDFSearchTextBytes)), nil
}

func extractDOCXText(data []byte) (string, error) {
	return extractOfficeZipText(data, isDOCXTextPart, map[string]bool{"t": true})
}

func extractODSText(data []byte) (string, error) {
	return extractOfficeZipText(data, func(name string) bool { return name == "content.xml" }, map[string]bool{"p": true, "span": true, "h": true, "a": true})
}

func extractOfficeZipText(data []byte, includePart func(string) bool, textElements map[string]bool) (string, error) {
	if len(data) == 0 {
		return "", nil
	}
	if len(data) > maxOfficeAttachmentBytes {
		return "", errors.New("office attachment too large for search extraction")
	}
	reader, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return "", err
	}
	var out strings.Builder
	for _, file := range reader.File {
		name := strings.ToLower(file.Name)
		if !includePart(name) {
			continue
		}
		rc, err := file.Open()
		if err != nil {
			return "", err
		}
		err = appendOfficeXMLText(&out, io.LimitReader(rc, maxOfficeAttachmentBytes), textElements)
		closeErr := rc.Close()
		if err != nil {
			return "", err
		}
		if closeErr != nil {
			return "", closeErr
		}
		if out.Len() >= maxOfficeSearchTextBytes {
			break
		}
	}
	return out.String(), nil
}

func isDOCXTextPart(name string) bool {
	if name == "word/document.xml" {
		return true
	}
	if !strings.HasPrefix(name, "word/") || !strings.HasSuffix(name, ".xml") {
		return false
	}
	base := filepath.Base(name)
	return strings.HasPrefix(base, "header") || strings.HasPrefix(base, "footer") || base == "footnotes.xml" || base == "endnotes.xml" || base == "comments.xml"
}

func appendOfficeXMLText(out *strings.Builder, r io.Reader, textElements map[string]bool) error {
	decoder := xml.NewDecoder(r)
	decoder.Strict = false
	textDepth := 0
	for {
		tok, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if textDepth > 0 || textElements[strings.ToLower(t.Name.Local)] {
				textDepth++
			}
		case xml.CharData:
			if textDepth > 0 {
				appendOfficeText(out, string(t))
				if out.Len() >= maxOfficeSearchTextBytes {
					return nil
				}
			}
		case xml.EndElement:
			if textDepth > 0 {
				textDepth--
			}
		}
	}
}

func appendOfficeText(out *strings.Builder, value string) {
	value = strings.TrimSpace(value)
	if value == "" || out.Len() >= maxOfficeSearchTextBytes {
		return
	}
	remaining := maxOfficeSearchTextBytes - out.Len()
	if remaining <= 0 {
		return
	}
	if out.Len() > 0 {
		out.WriteByte(' ')
		remaining--
	}
	if len(value) > remaining {
		value = string(limitBytes([]byte(value), remaining))
	}
	out.WriteString(value)
}

func extractDOCTextWithExternalTool(data []byte) (string, error) {
	if len(data) == 0 {
		return "", nil
	}
	if len(data) > maxOfficeAttachmentBytes {
		return "", errors.New("doc attachment too large for search extraction")
	}
	tmp, err := os.CreateTemp("", "rolltop-doc-*.doc")
	if err != nil {
		return "", err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	commands := [][]string{
		{"antiword", "-m", "UTF-8.txt", tmpName},
		{"catdoc", "-w", tmpName},
	}
	var lastErr error
	for _, args := range commands {
		ctx, cancel := context.WithTimeout(context.Background(), officeExtractionTimeout)
		cmd := exec.CommandContext(ctx, args[0], args[1:]...)
		cmd.Stderr = io.Discard
		out, err := cmd.Output()
		if ctx.Err() != nil {
			cancel()
			return "", ctx.Err()
		}
		cancel()
		if err == nil {
			return string(limitBytes(out, maxOfficeSearchTextBytes)), nil
		}
		lastErr = err
	}
	if lastErr != nil {
		return "", lastErr
	}
	return "", errors.New("doc text extractor unavailable")
}

func isSearchableTextType(mediaType, ext string) bool {
	if strings.HasPrefix(mediaType, "text/") {
		return true
	}
	switch mediaType {
	case "application/json", "application/xml", "application/xhtml+xml", "application/csv", "application/ics", "application/javascript", "application/x-javascript":
		return true
	}
	switch ext {
	case ".txt", ".text", ".md", ".markdown", ".csv", ".tsv", ".json", ".xml", ".html", ".htm", ".ics", ".vcf", ".log", ".go", ".py", ".js", ".ts", ".tsx", ".jsx", ".css", ".sql", ".yaml", ".yml", ".toml":
		return true
	default:
		return false
	}
}

func stripHTML(value string) string {
	value = html.UnescapeString(value)
	value = htmlTextNoiseBlockRE.ReplaceAllString(value, " ")
	var b strings.Builder
	inTag := false
	for _, r := range value {
		switch r {
		case '<':
			inTag = true
		case '>':
			inTag = false
			b.WriteByte(' ')
		default:
			if !inTag {
				if unicode.IsSpace(r) {
					b.WriteByte(' ')
				} else {
					b.WriteRune(r)
				}
			}
		}
	}
	return html.UnescapeString(b.String())
}
