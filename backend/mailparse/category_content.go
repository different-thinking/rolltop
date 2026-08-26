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
	"io"
	"mime"
	"mime/multipart"
	"net/mail"
	"net/textproto"
	"strings"
	"unicode/utf8"
)

// CategoryContent is one message as content classification sees it.
type CategoryContent struct {
	Subject string
	Text    string
	Files   []CategoryFile
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
	// evidence sits past the bound keeps the category its headers earned it --
	// a miss, not a wrong answer, and the sender can still be filed by hand.
	//
	// New mail does not pay this: it is classified while the parser has the
	// whole decoded message in hand.
	maxCategoryScanBytes = 1024 * 1024
)

// CategoryContent reduces an already-parsed message to what classification
// reads. Parsing has decoded all of it anyway, so the fetch path pays nothing
// for content classification beyond this copy.
func (p ParsedMessage) CategoryContent() CategoryContent {
	content := CategoryContent{Subject: p.Subject, Text: limitCategoryText(p.Text)}
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
func scanCategoryContent(r io.Reader) (mail.Header, CategoryContent, error) {
	msg, err := mail.ReadMessage(io.LimitReader(r, maxCategoryScanBytes))
	if err != nil {
		return nil, CategoryContent{}, err
	}
	subject, _ := wordDecoder().DecodeHeader(msg.Header.Get("Subject"))
	scan := &categoryScan{subject: strings.TrimSpace(subject)}
	scan.walk(textproto.MIMEHeader(msg.Header), msg.Body, 0)
	return msg.Header, scan.content(), nil
}

// categoryScan collects the scan's findings while it walks the MIME tree.
type categoryScan struct {
	subject string
	text    strings.Builder
	files   []CategoryFile
}

func (s *categoryScan) content() CategoryContent {
	return CategoryContent{Subject: s.subject, Text: s.text.String(), Files: s.files}
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
		// The bytes are deliberately left unread. NextPart drains what is left
		// of this part on its way to the next boundary, which is the cheapest
		// skip MIME allows, and the scan budget bounds even that.
		s.addFile(decodedHeader(filename), mediaType)
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
		return
	}
	s.files = append(s.files, CategoryFile{Filename: filename, ContentType: mediaType})
}
