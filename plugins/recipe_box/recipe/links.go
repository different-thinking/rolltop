// File overview: The links a note-to-self carries, and only the ones worth
// asking a recipe of.
//
// A message the reader sent themselves is mostly one link and maybe a line of
// their own. What is collected here is that link, unwrapped from whatever
// redirect the sending app put around it, with the tracking parameters taken
// off, so the same recipe shared twice is one row and not two. Everything a
// message links to besides -- an unsubscribe route, a logo, a mailto -- is
// dropped where it is found.

package recipe

import (
	"html"
	"net"
	"net/url"
	"regexp"
	"sort"
	"strings"
)

// MaxLinksPerMessage bounds what one message contributes. A note-to-self
// carrying more links than this is a reading list, not a recipe someone kept,
// and every link is a page fetch.
const MaxLinksPerMessage = 10

var (
	textURLRE = regexp.MustCompile(`(?i)\bhttps?://[^\s<>"'\x60]+`)
	hrefRE    = regexp.MustCompile(`(?is)<a\b[^>]*?\bhref\s*=\s*("([^"]*)"|'([^']*)'|([^\s>]+))`)
)

// trackingParams are query parameters that say who shared a link, never which
// page it is. Dropping them is what lets the same recipe, shared from a
// newsletter and from a phone, land on one row.
var trackingParams = map[string]bool{
	"fbclid": true, "gclid": true, "dclid": true, "msclkid": true, "igshid": true, "igsh": true,
	"mc_cid": true, "mc_eid": true, "_hsenc": true, "_hsmi": true, "mkt_tok": true,
	"si": true, "ref": true, "ref_src": true, "share": true, "shared": true, "sharedby": true,
	"cmp": true, "cmpid": true, "wt_mc": true, "ito": true, "ncid": true,
}

// skippedHosts are hosts whose pages are never a recipe a reader could be
// reading: documents behind a login, meetings, code. Fetching them would only
// put a login page into the list of links that were not recognised.
var skippedHosts = []string{
	"docs.google.com", "drive.google.com", "meet.google.com", "calendar.google.com", "mail.google.com",
	"accounts.google.com", "dropbox.com", "onedrive.live.com", "sharepoint.com", "teams.microsoft.com",
	"zoom.us", "github.com", "gitlab.com", "maps.google.com", "goo.gl", "maps.app.goo.gl", "wa.me",
	"whatsapp.com", "t.me", "localhost",
}

// redirectParams name, per redirecting host, the query parameter holding the
// real destination. Gmail wraps links it rewrites in google.com/url, Outlook in
// its safelinks, Facebook and Instagram in their own outbound pages.
var redirectParams = []struct {
	host  string
	path  string
	param string
}{
	{"google.com", "/url", "q"},
	{"google.com", "/url", "url"},
	{"l.facebook.com", "/l.php", "u"},
	{"lm.facebook.com", "/l.php", "u"},
	{"l.instagram.com", "/", "u"},
	{"safelinks.protection.outlook.com", "/", "url"},
	{"out.reddit.com", "/", "url"},
	{"href.li", "/", ""},
}

// ExtractLinks returns the distinct links of a message's text and markup, in
// the order they first appear, normalized and capped at MaxLinksPerMessage.
func ExtractLinks(text, markup string) []string {
	var raw []string
	for _, match := range hrefRE.FindAllStringSubmatch(markup, -1) {
		raw = append(raw, html.UnescapeString(firstNonEmpty(match[2], match[3], match[4])))
	}
	raw = append(raw, textURLRE.FindAllString(text, -1)...)
	// Markup that is not inside a link still carries URLs the sender typed.
	raw = append(raw, textURLRE.FindAllString(html.UnescapeString(stripTags(markup)), -1)...)

	seen := map[string]bool{}
	var out []string
	for _, candidate := range raw {
		link, ok := NormalizeLink(candidate)
		if !ok || seen[link] {
			continue
		}
		seen[link] = true
		out = append(out, link)
		if len(out) >= MaxLinksPerMessage {
			break
		}
	}
	return out
}

// NormalizeLink turns a link as found into the form it is stored and compared
// in: http(s) only, redirect wrappers removed, host lowercased, fragment and
// tracking parameters dropped. It reports false for anything that is not a
// page worth asking for a recipe.
func NormalizeLink(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	raw = strings.TrimRight(raw, ".,;:!?)]}>'\"")
	u, err := url.Parse(raw)
	if err != nil {
		return "", false
	}
	for range 3 {
		next, ok := unwrapRedirect(u)
		if !ok {
			break
		}
		u = next
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", false
	}
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	if host == "" || !strings.Contains(host, ".") || u.User != nil {
		return "", false
	}
	if hostMatches(host, skippedHosts) || looksLikeAsset(u.Path) || looksLikeMailPlumbing(host, u.Path) {
		return "", false
	}
	port := u.Port()
	u.Scheme = scheme
	u.Host = host
	if port != "" && !(scheme == "http" && port == "80") && !(scheme == "https" && port == "443") {
		u.Host = net.JoinHostPort(host, port)
	}
	u.Fragment = ""
	u.RawFragment = ""
	query := u.Query()
	for key := range query {
		lower := strings.ToLower(key)
		if strings.HasPrefix(lower, "utm_") || trackingParams[lower] {
			query.Del(key)
		}
	}
	u.RawQuery = encodeSortedQuery(query)
	if u.Path == "" {
		u.Path = "/"
	}
	return u.String(), true
}

func unwrapRedirect(u *url.URL) (*url.URL, bool) {
	host := strings.ToLower(u.Hostname())
	for _, rule := range redirectParams {
		if !hostMatches(host, []string{rule.host}) {
			continue
		}
		if rule.path != "/" && u.Path != rule.path {
			continue
		}
		var target string
		if rule.param == "" {
			target = strings.TrimPrefix(u.RawQuery, "?")
		} else {
			target = u.Query().Get(rule.param)
		}
		if target == "" {
			continue
		}
		next, err := url.Parse(target)
		if err != nil || next.Host == "" {
			continue
		}
		return next, true
	}
	return nil, false
}

func hostMatches(host string, list []string) bool {
	for _, entry := range list {
		if host == entry || strings.HasSuffix(host, "."+entry) {
			return true
		}
	}
	return false
}

func looksLikeAsset(path string) bool {
	lower := strings.ToLower(path)
	for _, ext := range []string{".png", ".jpg", ".jpeg", ".gif", ".webp", ".svg", ".ico", ".css", ".js", ".pdf", ".zip", ".mp4", ".mov", ".mp3", ".ics", ".vcf"} {
		if strings.HasSuffix(lower, ext) {
			return true
		}
	}
	return false
}

// looksLikeMailPlumbing drops the links mail clients add on their own: the
// unsubscribe and preference routes, read receipts and "sent from my phone"
// footers. None of them is something the reader shared.
func looksLikeMailPlumbing(host, path string) bool {
	lower := strings.ToLower(path)
	for _, part := range []string{"unsubscribe", "/optout", "/opt-out", "email-preferences", "/track/open", "/wf/open"} {
		if strings.Contains(lower, part) {
			return true
		}
	}
	return hostMatches(host, []string{"aka.ms", "go.microsoft.com", "support.apple.com", "apple.co"})
}

func encodeSortedQuery(query url.Values) string {
	if len(query) == 0 {
		return ""
	}
	keys := make([]string, 0, len(query))
	for key := range query {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, key := range keys {
		for _, value := range query[key] {
			if b.Len() > 0 {
				b.WriteByte('&')
			}
			b.WriteString(url.QueryEscape(key))
			if value != "" {
				b.WriteByte('=')
				b.WriteString(url.QueryEscape(value))
			}
		}
	}
	return b.String()
}

// NoteText is what the reader wrote beside the links: the message text with
// every URL removed and its whitespace collapsed, bounded. A shared recipe
// often carries one line of the reader's own ("for Sunday", "less sugar"),
// which is worth more on the card than the site's description.
func NoteText(text string, limit int) string {
	text = textURLRE.ReplaceAllString(text, " ")
	var lines []string
	for _, line := range strings.Split(text, "\n") {
		line = strings.Join(strings.Fields(line), " ")
		lower := strings.ToLower(line)
		// A quoted reply and the client's own footer are not the reader's note.
		if line == "" || strings.HasPrefix(line, ">") || strings.HasPrefix(lower, "sent from") ||
			strings.HasPrefix(lower, "gesendet von") || strings.HasPrefix(lower, "von meinem") || line == "--" {
			if line == "--" {
				break
			}
			continue
		}
		lines = append(lines, line)
	}
	note := strings.Join(lines, " ")
	return truncateRunes(note, limit)
}

func truncateRunes(value string, limit int) string {
	if limit <= 0 {
		return value
	}
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	cut := strings.TrimSpace(string(runes[:limit]))
	return cut + "…"
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
