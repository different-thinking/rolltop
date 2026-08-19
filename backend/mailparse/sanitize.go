// File overview: Repairs decoded mail text into byte sequences PostgreSQL accepts.

package mailparse

import (
	"strings"
	"unicode/utf8"

	"golang.org/x/text/encoding/charmap"
)

// SanitizeText returns value with everything PostgreSQL refuses to store in a
// TEXT column removed: invalid UTF-8 byte sequences and NUL bytes, both of
// which raise SQLSTATE 22021 on insert.
//
// SQLite kept whatever bytes it was handed, so this was free before the move to
// PostgreSQL. Mail is hostile input and the bad bytes are not rare: a client
// that writes raw ISO-8859-1 into a header without an encoded word sends
// "Änderung" as 0xe4 0x6e 0x64, which no charset in the message declares. That
// message must still be mirrored, so the bytes are repaired rather than the
// message rejected.
//
// Bytes that are not part of a valid UTF-8 sequence are read as Windows-1252 -
// what mail clients assume for undeclared 8-bit text, and what turns that
// header back into "Änderung". The repair is per byte, so valid UTF-8 around a
// bad byte keeps its own decoding instead of the whole string being re-read as
// Windows-1252 and coming out as mojibake.
func SanitizeText(value string) string {
	if value == "" {
		return value
	}
	if utf8.ValidString(value) && !strings.ContainsRune(value, 0) {
		return value
	}
	var b strings.Builder
	b.Grow(len(value))
	for i := 0; i < len(value); {
		r, size := utf8.DecodeRuneInString(value[i:])
		// DecodeRuneInString reports (RuneError, 1) for a byte that starts no
		// valid sequence, and (RuneError, 3) for a genuine U+FFFD the sender
		// encoded, which is valid UTF-8 and stays as it is.
		if r == utf8.RuneError && size <= 1 {
			b.WriteRune(charmap.Windows1252.DecodeByte(value[i]))
			i++
			continue
		}
		if r != 0 {
			b.WriteString(value[i : i+size])
		}
		i += size
	}
	return b.String()
}

// Sanitize repairs every text field a parsed message carries, so nothing
// derived from the raw bytes of a message can reach a TEXT column unrepaired.
// Parse and ParseDisplayBody already do this for what they produce; callers
// that rebuild fields afterwards - from a parse error, or from a plugin that
// decrypted the body - repeat it for their own values.
func (p *ParsedMessage) Sanitize() {
	if p == nil {
		return
	}
	p.MessageID = SanitizeText(p.MessageID)
	p.InReplyTo = SanitizeText(p.InReplyTo)
	p.References = SanitizeText(p.References)
	p.Subject = SanitizeText(p.Subject)
	p.From = SanitizeText(p.From)
	p.To = SanitizeText(p.To)
	p.CC = SanitizeText(p.CC)
	p.Text = SanitizeText(p.Text)
	p.HTML = SanitizeText(p.HTML)
	for i := range p.Files {
		p.Files[i].Filename = SanitizeText(p.Files[i].Filename)
		p.Files[i].ContentType = SanitizeText(p.Files[i].ContentType)
		p.Files[i].ContentID = SanitizeText(p.Files[i].ContentID)
	}
}
