// File overview: Header-driven message categories. One message belongs to
// exactly one category, decided from the list and automation headers the sender
// set rather than from body text, so the answer is stable and explainable.

package mailparse

import (
	"io"
	"net/mail"
	"strings"
)

// The category names are stored in SQLite and appear in URLs, so they are part
// of the wire format and must not be renamed without a migration.
const (
	// CategoryRelevant is mail that carries no list or automation markers:
	// what is left once the machine-generated traffic has been named.
	CategoryRelevant = "relevant"
	// CategoryNewsletters is broadcast mail: a list you receive but cannot post
	// to, identified by List-Id or an unsubscribe route.
	CategoryNewsletters = "newsletters"
	// CategoryForums is discussion mail: a list that invites replies, which
	// List-Post is the sender's own statement of.
	CategoryForums = "forums"
	// CategoryNotifications is transactional automation: receipts, alerts, and
	// no-reply robots that are neither a list nor a person writing.
	CategoryNotifications = "notifications"
)

// maxCategoryHeaderBytes bounds how much of a stored message the backfill reads
// looking for the header block. A message whose headers do not end inside this
// window is malformed for classification purposes and falls back to the address
// rules rather than pulling an entire large body into memory.
const maxCategoryHeaderBytes = 256 * 1024

// Categories lists every category in the order the sidebar shows them.
func Categories() []string {
	return []string{CategoryRelevant, CategoryNewsletters, CategoryForums, CategoryNotifications}
}

// ValidCategory reports whether a name is one this build classifies into.
// Stored rows and request input are both checked through here so an unknown
// name can never reach a query.
func ValidCategory(name string) bool {
	switch name {
	case CategoryRelevant, CategoryNewsletters, CategoryForums, CategoryNotifications:
		return true
	default:
		return false
	}
}

// Categorize decides one message's category from its headers, falling back to
// the sender address when the headers say nothing. The order matters: a
// discussion list also carries unsubscribe headers, and a newsletter also looks
// automated, so the more specific claim is tested first.
func Categorize(header mail.Header, from string) string {
	if hasListPost(header) {
		return CategoryForums
	}
	if headerPresent(header, "List-Id") || headerPresent(header, "List-Unsubscribe") {
		return CategoryNewsletters
	}
	if automatedHeader(header) {
		return CategoryNotifications
	}
	return CategorizeAddress(from)
}

// CategorizeReader classifies a stored message that is being re-read from the
// blob store. Only the header block is consumed. A message whose headers cannot
// be parsed still gets a category from its address so the backfill always makes
// progress instead of revisiting the same row forever.
func CategorizeReader(r io.Reader, from string) string {
	if r == nil {
		return CategorizeAddress(from)
	}
	msg, err := mail.ReadMessage(io.LimitReader(r, maxCategoryHeaderBytes))
	if err != nil {
		return CategorizeAddress(from)
	}
	return Categorize(msg.Header, from)
}

// CategorizeAddress is the header-less fallback. It recognizes only the robot
// address shapes that are unambiguous; everything else is treated as a person
// writing, because wrongly hiding real mail costs more than a missed robot.
func CategorizeAddress(from string) string {
	local := strings.ToLower(localPart(from))
	if local == "" {
		return CategoryRelevant
	}
	compact := strings.NewReplacer("-", "", "_", "", ".", "").Replace(local)
	for _, marker := range []string{"noreply", "donotreply", "notifications", "notification", "mailerdaemon", "postmaster", "bounce"} {
		if strings.Contains(compact, marker) {
			return CategoryNotifications
		}
	}
	return CategoryRelevant
}

// localPart takes the address out of a From header value. The header may be a
// bare address or a display name plus angle brackets, and unparseable values
// simply yield nothing to match on.
func localPart(from string) string {
	value := strings.TrimSpace(from)
	if value == "" {
		return ""
	}
	if parsed, err := mail.ParseAddress(value); err == nil {
		value = parsed.Address
	} else if start := strings.LastIndex(value, "<"); start >= 0 {
		if end := strings.Index(value[start:], ">"); end > 0 {
			value = value[start+1 : start+end]
		}
	}
	at := strings.LastIndex(value, "@")
	if at <= 0 {
		return ""
	}
	return strings.TrimSpace(value[:at])
}

// hasListPost reports a list that accepts replies. RFC 2369 lets a list say it
// is read-only with "NO", which is a broadcast list rather than a forum.
func hasListPost(header mail.Header) bool {
	value := strings.ToLower(strings.TrimSpace(header.Get("List-Post")))
	return value != "" && value != "no" && value != "<no>"
}

func headerPresent(header mail.Header, name string) bool {
	return strings.TrimSpace(header.Get(name)) != ""
}

// automatedHeader reports the standard "this was sent by a machine" markers.
// Auto-Submitted's own "no" value is the explicit statement that a human sent
// the message, so it is the one value that does not count.
func automatedHeader(header mail.Header) bool {
	if value := strings.ToLower(strings.TrimSpace(header.Get("Auto-Submitted"))); value != "" && value != "no" {
		return true
	}
	switch strings.ToLower(strings.TrimSpace(header.Get("Precedence"))) {
	case "bulk", "auto_reply", "junk":
		return true
	}
	return headerPresent(header, "X-Auto-Response-Suppress")
}
