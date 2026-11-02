// File overview: The bounded scan the ticket backfill reads a stored message
// with. It collects exactly what ExtractTravelBookings reads on the fetch path
// -- the subject, the sender, the HTML and the ticket attachments -- so the two
// paths reach the same answer about the same message.

package mailparse

import (
	"io"
	"mime"
	"mime/multipart"
	"net/mail"
	"net/textproto"
	"strings"
	"time"
)

const (
	// maxTravelScanBytes bounds how much of a stored message the backfill reads.
	// A ticket mail carries its PDF or its pass, which is the reason the bound
	// is larger than the parcel scan's.
	maxTravelScanBytes = 8 << 20
	// maxTravelHTMLBytes bounds the markup kept for the structured-data read.
	maxTravelHTMLBytes = 1 << 20
)

// TravelBookingsReaderScan reads a stored message for the tickets it carries.
// Only a failure to read the headers is reported; a body cut short yields what
// was collected before the cut, because a ticket is an answer that can only be
// missing, never wrong.
func TravelBookingsReaderScan(r io.Reader) ([]TravelBooking, error) {
	msg, err := mail.ReadMessage(io.LimitReader(r, maxTravelScanBytes))
	if err != nil {
		return nil, err
	}
	subject, _ := wordDecoder().DecodeHeader(msg.Header.Get("Subject"))
	in := TravelInput{
		Subject: SanitizeText(strings.TrimSpace(subject)),
		From:    SanitizeText(addressHeader(msg.Header.Get("From"))),
	}
	if d, err := mail.ParseDate(msg.Header.Get("Date")); err == nil {
		in.Sent = d
	} else {
		in.Sent = time.Time{}
	}
	scan := &travelScan{in: &in}
	scan.walk(textproto.MIMEHeader(msg.Header), msg.Body, 0)
	in.HTML = SanitizeText(in.HTML)
	return ExtractTravelBookings(in), nil
}

type travelScan struct {
	in *TravelInput
}

func (s *travelScan) walk(header textproto.MIMEHeader, body io.Reader, depth int) {
	mediaType, params, err := mime.ParseMediaType(header.Get("Content-Type"))
	if err != nil || mediaType == "" {
		mediaType = "text/plain"
	}
	lower := strings.ToLower(mediaType)
	if strings.HasPrefix(lower, "multipart/") {
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
		if len(s.in.Files) >= maxTravelFiles || !travelDocumentCandidate(name, mediaType, s.in.From) {
			return
		}
		data, err := io.ReadAll(io.LimitReader(decodeTransfer(header, body), maxTravelFileBytes+1))
		if (err != nil && !isTolerableEOF(err)) || len(data) == 0 || len(data) > maxTravelFileBytes {
			return
		}
		s.in.Files = append(s.in.Files, TravelFile{Filename: name, ContentType: mediaType, Data: data})
		return
	}
	if lower == "text/html" && s.in.HTML == "" {
		decoded, err := io.ReadAll(io.LimitReader(decodeTransfer(header, body), maxTravelHTMLBytes))
		if err != nil && !isTolerableEOF(err) && len(decoded) == 0 {
			return
		}
		s.in.HTML = decodeTextBytes(decoded, params["charset"])
	}
}
