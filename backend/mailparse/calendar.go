// File overview: Finding the calendar objects a message carries -- the .ics
// attachment of an invitation, or the text/calendar alternative Outlook and
// Exchange send in its place.

package mailparse

import (
	"io"
	"mime"
	"mime/multipart"
	"net/mail"
	"net/textproto"
	"path/filepath"
	"strings"
)

const (
	// maxCalendarParts bounds how many calendar objects one message yields.
	// An invitation carries one, often twice (Google sends the inline
	// alternative and an invite.ics with the same content).
	maxCalendarParts = 4
	// maxCalendarPartBytes bounds one object. An invitation is a few
	// kilobytes; a published calendar of a whole year is a few hundred.
	maxCalendarPartBytes = 1 << 20
)

// CalendarPart is one calendar object found in a message.
type CalendarPart struct {
	Filename string
	Data     []byte
}

// IsCalendarPart reports whether a MIME part or attachment row is a calendar
// object, by its media type or, for the senders who label it
// application/octet-stream, by its file name.
func IsCalendarPart(contentType, filename string) bool {
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		mediaType = contentType
	}
	switch strings.ToLower(strings.TrimSpace(mediaType)) {
	case "text/calendar", "application/ics", "text/x-vcalendar", "application/x-ics":
		return true
	}
	switch strings.ToLower(filepath.Ext(strings.TrimSpace(filename))) {
	case ".ics", ".ical", ".icalendar", ".vcs":
		return true
	}
	return false
}

// CalendarParts walks a raw message and returns the calendar objects in it,
// decoded to UTF-8. Every other part is skipped without being decoded, so a
// message carrying a large attachment beside its invitation costs a scan of
// that attachment and not a copy of it. A truncated message yields what was
// found before the cut.
func CalendarParts(r io.Reader) ([]CalendarPart, error) {
	msg, err := mail.ReadMessage(r)
	if err != nil {
		return nil, err
	}
	var parts []CalendarPart
	err = collectCalendarParts(textproto.MIMEHeader(msg.Header), msg.Body, &parts, 0)
	if err != nil && !isTolerableEOF(err) {
		return parts, err
	}
	return parts, nil
}

func collectCalendarParts(header textproto.MIMEHeader, body io.Reader, parts *[]CalendarPart, depth int) error {
	if len(*parts) >= maxCalendarParts {
		return nil
	}
	mediaType, params, err := mime.ParseMediaType(header.Get("Content-Type"))
	if err != nil || mediaType == "" {
		mediaType = "text/plain"
	}
	if strings.HasPrefix(strings.ToLower(mediaType), "multipart/") {
		if depth >= maxMIMEDepth {
			return nil
		}
		mr := multipart.NewReader(body, params["boundary"])
		for len(*parts) < maxCalendarParts {
			part, err := mr.NextPart()
			if err == io.EOF {
				return nil
			}
			if err != nil {
				return err
			}
			if err := collectCalendarParts(part.Header, part, parts, depth+1); err != nil {
				return err
			}
		}
		return nil
	}
	_, dispParams, _ := mime.ParseMediaType(header.Get("Content-Disposition"))
	filename := dispParams["filename"]
	if filename == "" {
		filename = params["name"]
	}
	filename = decodedHeader(filename)
	if !IsCalendarPart(mediaType, filename) {
		return nil
	}
	data, err := io.ReadAll(io.LimitReader(decodeTransfer(header, body), maxCalendarPartBytes+1))
	if err != nil && !isTolerableEOF(err) {
		return err
	}
	if len(data) > maxCalendarPartBytes {
		return nil
	}
	// Unfold before decoding: RFC 5545 lets a fold fall inside a multi-octet
	// character, and decoding first would read each half as a bad byte and
	// repair it into mojibake that unfolding afterwards cannot undo.
	text := DecodeTextBytes(unfoldCalendarBytes(data), params["charset"])
	*parts = append(*parts, CalendarPart{Filename: filename, Data: []byte(text)})
	return nil
}

// unfoldCalendarBytes joins folded content lines (RFC 5545 3.1) on the raw
// octets: a line break followed by a space or a tab is removed together with
// that one character.
func unfoldCalendarBytes(data []byte) []byte {
	out := make([]byte, 0, len(data))
	for i := 0; i < len(data); i++ {
		if data[i] == '\r' || data[i] == '\n' {
			next := i + 1
			if data[i] == '\r' && next < len(data) && data[next] == '\n' {
				next++
			}
			if next < len(data) && (data[next] == ' ' || data[next] == '\t') {
				i = next
				continue
			}
		}
		out = append(out, data[i])
	}
	return out
}
