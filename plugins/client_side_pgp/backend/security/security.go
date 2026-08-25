package security

import (
	"bytes"
	"io"
	"mime"
	"mime/multipart"
	"net/mail"
	"net/textproto"
	"regexp"
	"strings"

	"rolltop/backend/plugins"
)

var (
	inlinePGPMessageRE = regexp.MustCompile(`(?is)-----BEGIN PGP MESSAGE-----.*?-----END PGP MESSAGE-----`)
	inlinePGPSignedRE  = regexp.MustCompile(`(?is)-----BEGIN PGP SIGNED MESSAGE-----.*?-----END PGP SIGNATURE-----`)
)

func Detect(raw []byte, body plugins.MessageBody) plugins.MessageSecurityState {
	encrypted, signed := detectPGP(raw, body.Text+"\n"+body.HTML)
	return plugins.MessageSecurityState{Encrypted: encrypted, Signed: signed}
}

func Transform(raw []byte, state plugins.MessageSecurityState, body plugins.MessageBody) plugins.MessageBodyTransform {
	if state.Encrypted {
		if body.Purpose == "display" {
			text := encryptedDisplayBody(raw)
			if strings.TrimSpace(text) == "" {
				text = body.Text
			}
			return plugins.MessageBodyTransform{
				Applied: true,
				Body:    plugins.MessageBody{Purpose: body.Purpose, Text: normalizeDisplayText(text)},
			}
		}
		return plugins.MessageBodyTransform{
			Applied:         true,
			Body:            plugins.MessageBody{Purpose: body.Purpose},
			DropAttachments: true,
		}
	}
	if state.Signed {
		if clear, ok := stripInlinePGPSignedText(body.Text); ok {
			return plugins.MessageBodyTransform{
				Applied: true,
				Body:    plugins.MessageBody{Purpose: body.Purpose, Text: clear},
			}
		}
	}
	return plugins.MessageBodyTransform{}
}

const (
	// maxPGPScanDepth bounds MIME recursion so a deeply nested message cannot turn
	// detection into a stack or time bomb. Real PGP structures are shallow.
	maxPGPScanDepth = 20
	// maxPGPScanBytes bounds the structural parse. Detection only needs the MIME
	// headers and the first armor markers, both near the top of a message, so a
	// multi-megabyte body (a large ciphertext part) does not make per-message
	// detection on the sync/index hot path parse the whole message.
	maxPGPScanBytes = 1 << 20
)

// detectPGP decides whether a message is PGP-encrypted or PGP-signed from its
// actual MIME structure rather than from substrings anywhere in the raw bytes.
// The old heuristic lowercased the whole message and matched "multipart/encrypted"
// or "application/pgp-encrypted" as substrings, so an ordinary message that
// merely quoted those type names -- a forwarded thread, a mailing-list digest, a
// reply discussing PGP -- was declared encrypted, and Transform then dropped its
// attachments. Parsing the tree restricts the type checks to real Content-Type
// headers and the inline PGP-armor checks to real text parts, not attachment
// bytes or quoted content.
func detectPGP(raw []byte, fallback string) (encrypted bool, signed bool) {
	scanRaw := limitSecurityBytes(raw, maxPGPScanBytes)
	if msg, err := mail.ReadMessage(bytes.NewReader(scanRaw)); err == nil {
		encrypted, signed = scanPGPStructure(textproto.MIMEHeader(msg.Header), msg.Body, 0)
	} else {
		// A message that will not parse as MIME is exactly where structure is not
		// available. Fall back to the previous whole-message scan rather than risk
		// missing genuinely encrypted mail; an unparseable message is rare and is
		// not the false-positive case the structural path targets.
		lower := strings.ToLower(string(limitSecurityBytes(raw, 256*1024)))
		if strings.Contains(lower, "multipart/encrypted") || strings.Contains(lower, "application/pgp-encrypted") || inlinePGPMessageRE.Match(raw) {
			encrypted = true
		}
		if strings.Contains(lower, "application/pgp-signature") || inlinePGPSignedRE.Match(raw) {
			signed = true
		}
	}
	// Inline PGP armor in the decoded, displayable body (never in attachment bytes
	// or the raw envelope) is the remaining signal, and the only one available
	// when the caller passed body text but no parseable raw.
	if inlinePGPMessageRE.MatchString(fallback) {
		encrypted = true
	}
	if inlinePGPSignedRE.MatchString(fallback) {
		signed = true
	}
	return encrypted, signed
}

// scanPGPStructure walks the MIME tree and reports PGP encryption or signing
// from the Content-Type of each part: multipart/encrypted (RFC 3156, PGP
// protocol) and a application/pgp-encrypted part mean encrypted; multipart/signed
// with the PGP signature protocol and a application/pgp-signature part mean
// signed. Only text parts are scanned for inline ASCII-armor markers, so a PGP
// block quoted inside a normal reply's text is still honored while the same bytes
// sitting in an attachment are not.
func scanPGPStructure(header textproto.MIMEHeader, body io.Reader, depth int) (encrypted bool, signed bool) {
	if depth > maxPGPScanDepth {
		return false, false
	}
	mediaType, params, err := mime.ParseMediaType(header.Get("Content-Type"))
	if err != nil || mediaType == "" {
		mediaType = "text/plain"
	}
	mediaType = strings.ToLower(mediaType)
	switch mediaType {
	case "application/pgp-encrypted":
		return true, false
	case "application/pgp-signature":
		return false, true
	}
	if strings.HasPrefix(mediaType, "multipart/") {
		protocol := strings.ToLower(strings.TrimSpace(params["protocol"]))
		if mediaType == "multipart/encrypted" && (protocol == "" || protocol == "application/pgp-encrypted") {
			encrypted = true
		}
		if mediaType == "multipart/signed" && protocol == "application/pgp-signature" {
			signed = true
		}
		mr := multipart.NewReader(body, params["boundary"])
		for {
			part, err := mr.NextPart()
			if err != nil {
				break
			}
			enc, sig := scanPGPStructure(part.Header, part, depth+1)
			encrypted = encrypted || enc
			signed = signed || sig
		}
		return encrypted, signed
	}
	if strings.HasPrefix(mediaType, "text/") && !partIsAttachment(header, params) {
		decoded, _ := io.ReadAll(io.LimitReader(body, 256*1024))
		if inlinePGPMessageRE.Match(decoded) {
			encrypted = true
		}
		if inlinePGPSignedRE.Match(decoded) {
			signed = true
		}
	}
	return encrypted, signed
}

// partIsAttachment reports whether a MIME part is carried as a file rather than
// shown as the message body. A text attachment that merely contains a PGP block
// -- a forwarded .txt or .asc, a quoted transcript -- is not an encrypted
// message, so its armor must not trigger detection; only inline body text may.
func partIsAttachment(header textproto.MIMEHeader, contentTypeParams map[string]string) bool {
	disposition, dispParams, _ := mime.ParseMediaType(header.Get("Content-Disposition"))
	if strings.EqualFold(strings.TrimSpace(disposition), "attachment") {
		return true
	}
	if dispParams != nil && strings.TrimSpace(dispParams["filename"]) != "" {
		return true
	}
	if contentTypeParams != nil && strings.TrimSpace(contentTypeParams["name"]) != "" {
		return true
	}
	return false
}

func encryptedDisplayBody(raw []byte) string {
	if match := inlinePGPMessageRE.Find(raw); len(match) > 0 {
		return normalizeDisplayBytes(match)
	}
	if text := encryptedMIMEPayloadDisplay(raw); text != "" {
		return text
	}
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return ""
	}
	body, err := io.ReadAll(msg.Body)
	if err != nil {
		return ""
	}
	return normalizeDisplayBytes(body)
}

func encryptedMIMEPayloadDisplay(raw []byte) string {
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return ""
	}
	return encryptedMIMEPartDisplay(textproto.MIMEHeader(msg.Header), msg.Body)
}

func encryptedMIMEPartDisplay(header textproto.MIMEHeader, body io.Reader) string {
	mediaType, params, err := mime.ParseMediaType(header.Get("Content-Type"))
	if err != nil || mediaType == "" {
		mediaType = "text/plain"
	}
	mediaType = strings.ToLower(mediaType)
	if strings.HasPrefix(mediaType, "multipart/") {
		mr := multipart.NewReader(body, params["boundary"])
		for {
			part, err := mr.NextPart()
			if err == io.EOF {
				return ""
			}
			if err != nil {
				return ""
			}
			if text := encryptedMIMEPartDisplay(part.Header, part); text != "" {
				return text
			}
		}
	}
	if mediaType == "application/pgp-encrypted" {
		_, _ = io.Copy(io.Discard, body)
		return ""
	}
	decoded, err := io.ReadAll(body)
	if err != nil {
		return ""
	}
	if match := inlinePGPMessageRE.Find(decoded); len(match) > 0 {
		return normalizeDisplayBytes(match)
	}
	disposition, dispParams, _ := mime.ParseMediaType(header.Get("Content-Disposition"))
	filename := ""
	if dispParams != nil {
		filename = dispParams["filename"]
	}
	if filename == "" && params != nil {
		filename = params["name"]
	}
	name := strings.ToLower(strings.TrimSpace(filename))
	if mediaType == "application/octet-stream" && strings.Contains(name, "encrypted") {
		return normalizeDisplayBytes(decoded)
	}
	if strings.EqualFold(disposition, "inline") && strings.HasSuffix(name, ".asc") {
		return normalizeDisplayBytes(decoded)
	}
	return ""
}

func normalizeDisplayBytes(value []byte) string {
	value = bytes.TrimSpace(value)
	if len(value) == 0 {
		return ""
	}
	return normalizeDisplayText(string(value))
}

func stripInlinePGPSignedText(value string) (string, bool) {
	value = strings.ReplaceAll(value, "\r\n", "\n")
	value = strings.ReplaceAll(value, "\r", "\n")
	begin := strings.Index(value, "-----BEGIN PGP SIGNED MESSAGE-----")
	if begin < 0 {
		return "", false
	}
	sigBeginRel := strings.Index(value[begin:], "-----BEGIN PGP SIGNATURE-----")
	if sigBeginRel < 0 {
		return "", false
	}
	sigBegin := begin + sigBeginRel
	block := value[begin:sigBegin]
	bodyOffset := clearSignedBodyOffset(block)
	if bodyOffset < 0 {
		return "", false
	}
	prefix := strings.TrimSpace(value[:begin])
	replacement := unescapeClearSignedBody(block[bodyOffset:])
	suffix := ""
	if sigEndRel := strings.Index(value[sigBegin:], "-----END PGP SIGNATURE-----"); sigEndRel >= 0 {
		suffixStart := sigBegin + sigEndRel + len("-----END PGP SIGNATURE-----")
		suffix = strings.TrimSpace(value[suffixStart:])
	}
	parts := make([]string, 0, 3)
	if prefix != "" {
		parts = append(parts, prefix)
	}
	if replacement != "" {
		parts = append(parts, replacement)
	}
	if suffix != "" {
		parts = append(parts, suffix)
	}
	return normalizeDisplayText(strings.Join(parts, "\n\n")), true
}

func clearSignedBodyOffset(block string) int {
	lineEnd := strings.IndexByte(block, '\n')
	if lineEnd < 0 {
		return -1
	}
	pos := lineEnd + 1
	for pos <= len(block) {
		next := strings.IndexByte(block[pos:], '\n')
		lineEnd = len(block)
		lineNext := len(block)
		if next >= 0 {
			lineEnd = pos + next
			lineNext = lineEnd + 1
		}
		if strings.TrimSpace(block[pos:lineEnd]) == "" {
			return lineNext
		}
		if next < 0 {
			return -1
		}
		pos = lineNext
	}
	return -1
}

func unescapeClearSignedBody(value string) string {
	value = strings.Trim(value, "\n")
	lines := strings.Split(value, "\n")
	for i, line := range lines {
		if strings.HasPrefix(line, "- ") {
			lines[i] = strings.TrimPrefix(line, "- ")
		}
	}
	return strings.Join(lines, "\n")
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

func limitSecurityBytes(value []byte, n int) []byte {
	if len(value) <= n {
		return value
	}
	return value[:n]
}
