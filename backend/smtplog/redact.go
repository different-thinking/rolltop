// File overview: What a transcript is allowed to say. The rule the rest of
// Rolltop follows -- never log passwords, tokens, or raw message bodies --
// does not stop applying because the text came off the wire, and an SMTP
// conversation carries exactly those two things in a fixed, recognizable
// place: the AUTH exchange and everything between DATA and its terminating dot.

package smtplog

import (
	"fmt"
	"strings"
)

// redactCommand keeps the verb and drops any argument that could be a
// credential. AUTH is the only command that carries one: its initial response
// is the base64 of the password or the OAuth token, so the mechanism name is
// kept -- it is what tells a reader whether PLAIN, LOGIN or XOAUTH2 was
// attempted -- and the blob after it is not.
func redactCommand(line string) string {
	trimmed := strings.TrimRight(line, "\r\n")
	fields := strings.Fields(trimmed)
	if len(fields) == 0 || !strings.EqualFold(fields[0], "AUTH") {
		return trimmed
	}
	if len(fields) == 1 {
		return "AUTH"
	}
	if len(fields) == 2 {
		// "AUTH LOGIN" with no initial response sends nothing secret yet, so
		// the command is shown as it went out.
		return "AUTH " + fields[1]
	}
	return "AUTH " + fields[1] + " " + redactedSecret
}

// formatReply renders a server reply the way the server said it, because the
// code and the text after it are the whole diagnosis.
func formatReply(code int, message string) string {
	message = strings.TrimRight(message, "\r\n")
	if code <= 0 {
		return message
	}
	if strings.TrimSpace(message) == "" {
		return fmt.Sprintf("%d", code)
	}
	return fmt.Sprintf("%d %s", code, message)
}

// formatBodyNote stands in for the payload. The size is the part of it that
// diagnoses a failure -- a server refusing at the end of DATA is usually
// refusing the size -- and the content is what must never be recorded.
func formatBodyNote(bytes int) string {
	return fmt.Sprintf("<%s: %d bytes, not recorded>", redactedBodyNote, bytes)
}
