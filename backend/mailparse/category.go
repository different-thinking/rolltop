// File overview: Header-driven message categories. One message belongs to
// exactly one category, decided from the list and automation headers the sender
// set rather than from body text, so the answer is stable and explainable.
//
// The category set is defined once, in categoryDefinitions below. Adding a
// category there gives it a name, a sidebar label, an icon, validation, and a
// route; there is deliberately no second list anywhere that could fall behind.

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

// Category is one entry of the category registry: the stored name plus the
// display text the sidebar renders it with.
type Category struct {
	Name  string
	Label string
	Icon  string
}

// categoryDefinitions is the single definition of the category set, in the
// order the sidebar shows them. Everything else in this package derives from
// it, so a category cannot exist without a label or be validated inconsistently.
var categoryDefinitions = []Category{
	{Name: CategoryRelevant, Label: "Relevant", Icon: "person"},
	{Name: CategoryNewsletters, Label: "Newsletters", Icon: "newspaper"},
	{Name: CategoryForums, Label: "Forums", Icon: "forum"},
	{Name: CategoryNotifications, Label: "Notifications", Icon: "notifications"},
}

// maxCategoryHeaderBytes bounds how much of a stored message the backfill reads
// looking for the header block. A message whose headers do not end inside this
// window is malformed for classification purposes and falls back to the address
// rules rather than pulling an entire large body into memory.
const maxCategoryHeaderBytes = 256 * 1024

// CategoryRegistry lists every category with its display text, in sidebar order.
func CategoryRegistry() []Category {
	out := make([]Category, len(categoryDefinitions))
	copy(out, categoryDefinitions)
	return out
}

// Categories lists every category name, in the order the sidebar shows them.
func Categories() []string {
	out := make([]string, 0, len(categoryDefinitions))
	for _, definition := range categoryDefinitions {
		out = append(out, definition.Name)
	}
	return out
}

// ValidCategory reports whether a name is one this build classifies into.
// Stored rows and request input are both checked through here so an unknown
// name can never reach a query.
func ValidCategory(name string) bool {
	for _, definition := range categoryDefinitions {
		if definition.Name == name {
			return true
		}
	}
	return false
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
	local := localPart(BareAddress(from))
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

// BareAddress reduces a From header value to the address inside it, lowercased.
// Classification and the per-sender corrections both key on this, so they have
// to agree character for character: a sender the classifier can read but the
// correction cannot is a sender the user is unable to file.
func BareAddress(from string) string {
	value := strings.TrimSpace(from)
	if value == "" {
		return ""
	}
	// A From header naming several addresses is degenerate but legal. The first
	// one is taken so the key stays stable, rather than depending on where the
	// scan happened to stop.
	if parsed, err := mail.ParseAddressList(value); err == nil && len(parsed) > 0 {
		value = parsed[0].Address
	} else if start := strings.Index(value, "<"); start >= 0 {
		// An unterminated angle bracket still names an address: taking the rest
		// of the value keeps a malformed header correctable rather than leaving
		// its sender permanently unfilable.
		value = value[start+1:]
		if end := strings.Index(value, ">"); end >= 0 {
			value = value[:end]
		}
	}
	value = strings.ToLower(strings.TrimSpace(value))
	if strings.ContainsAny(value, " \t") || !strings.Contains(value, "@") {
		return ""
	}
	return value
}

// localPart splits the mailbox out of an already-bare address.
func localPart(address string) string {
	at := strings.LastIndex(address, "@")
	if at <= 0 {
		return ""
	}
	return address[:at]
}

// hasListPost reports a list that accepts replies. RFC 2369 lets a list say it
// is read-only with "NO", and permits a trailing comment after it, so the
// comment has to come off before the value can be compared.
func hasListPost(header mail.Header) bool {
	value := strings.ToLower(headerValueWithoutComment(header.Get("List-Post")))
	return value != "" && value != "no" && value != "<no>"
}

// headerValueWithoutComment drops an RFC 2369 trailing comment. Only a comment
// at the end is removed, because that is where the grammar puts it and a
// parenthesis inside an address is part of the address.
func headerValueWithoutComment(value string) string {
	value = strings.TrimSpace(value)
	if open := strings.LastIndex(value, "("); open >= 0 && strings.HasSuffix(value, ")") {
		value = value[:open]
	}
	return strings.TrimSpace(value)
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
