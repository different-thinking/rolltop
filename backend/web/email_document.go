// File overview: Email document rendering helpers for safe in-app message display.

package web

import (
	"html"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"rolltop/backend/store"
)

var plainTextURLRE = regexp.MustCompile(`https?://[^\s<>"']+`)

func replySubject(subject string) string {
	subject = strings.TrimSpace(subject)
	if subject == "" {
		subject = "(no subject)"
	}
	if strings.HasPrefix(strings.ToLower(subject), "re:") {
		return subject
	}
	return "Re: " + subject
}

func emailDocument(bodyHTML, bodyText string, allowRemoteImages bool) string {
	return emailDocumentWithBlocklist(bodyHTML, bodyText, allowRemoteImages, nil)
}

func emailDocumentWithBlocklist(bodyHTML, bodyText string, allowRemoteImages bool, blockedImagePatterns []string) string {
	return emailDocumentWithInlineAttachments(bodyHTML, bodyText, allowRemoteImages, blockedImagePatterns, nil)
}

func emailDocumentWithInlineAttachments(bodyHTML, bodyText string, allowRemoteImages bool, blockedImagePatterns []string, attachments []store.Attachment) string {
	plainTextDoc := false
	if strings.TrimSpace(bodyHTML) == "" {
		plainTextDoc = true
		bodyHTML = `<div class="plaintext">` + plainTextBodyHTML(bodyText) + `</div>`
	}
	bodyHTML = strings.ReplaceAll(bodyHTML, "\x00", "")
	bodyHTML = replaceInlineCIDRefs(bodyHTML, attachments)
	if allowRemoteImages {
		bodyHTML = normalizeProtocolRelativeRemoteRefs(bodyHTML)
		bodyHTML = removeBlockedRemoteImages(bodyHTML, blockedImagePatterns)
	} else {
		bodyHTML = neutralizeRemoteRefs(bodyHTML)
	}
	imgSrc := "'self' data: cid:"
	styleSrc := "'unsafe-inline'"
	fontSrc := "data:"
	if allowRemoteImages {
		imgSrc = "'self' data: cid: http: https:"
		styleSrc = "'unsafe-inline' http: https:"
		fontSrc = "data: http: https:"
	}
	documentClass := ""
	if plainTextDoc {
		documentClass = ` class="plaintext-doc"`
	}
	// Theme rules deliberately live in the frontend (lib/emailDocumentTheme.ts)
	// and are woven into this document before the iframe loads it, so the app
	// shell and the PGP plugin share one set of message-body theme colours.
	documentCSS := `html,body{margin:0;padding:0;background:#fff;color:#1f2328;font:14px/1.55 Arial,sans-serif;overflow:hidden}body{padding:18px}.plaintext{white-space:pre-wrap;overflow-wrap:anywhere;font:14px/1.55 Arial,sans-serif}.plaintext a{color:#245f80;text-decoration:none;border-bottom:1px solid #9cc5d8}pre{white-space:pre-wrap;overflow-wrap:anywhere}table{max-width:100%}img{max-width:100%;height:auto}`
	return `<!doctype html><html` + documentClass + `><head><meta charset="utf-8"><base target="_blank"><meta name="referrer" content="no-referrer"><meta http-equiv="Content-Security-Policy" content="default-src 'none'; img-src ` + imgSrc + `; style-src ` + styleSrc + `; font-src ` + fontSrc + `;"><style>` + documentCSS + `</style></head><body>` + bodyHTML + `</body></html>`
}

var cidURLRE = regexp.MustCompile(`(?i)cid:([^\s"'<>\)]+)`)

func replaceInlineCIDRefs(bodyHTML string, attachments []store.Attachment) string {
	if len(attachments) == 0 || !strings.Contains(strings.ToLower(bodyHTML), "cid:") {
		return bodyHTML
	}
	urlsByCID := inlineAttachmentURLsByCID(attachments)
	if len(urlsByCID) == 0 {
		return bodyHTML
	}
	return cidURLRE.ReplaceAllStringFunc(bodyHTML, func(match string) string {
		parts := cidURLRE.FindStringSubmatch(match)
		if len(parts) < 2 {
			return match
		}
		if replacement, ok := urlsByCID[normalizeContentID(parts[1])]; ok {
			return replacement
		}
		return match
	})
}

func inlineAttachmentURLsByCID(attachments []store.Attachment) map[string]string {
	out := make(map[string]string)
	for _, att := range attachments {
		key := normalizeContentID(att.ContentID)
		if key == "" || att.ID <= 0 {
			continue
		}
		out[key] = "/attachments/" + strconv.FormatInt(att.ID, 10) + "/inline"
	}
	return out
}

func normalizeContentID(value string) string {
	value = strings.TrimSpace(html.UnescapeString(value))
	value = strings.Trim(value, "<>'\" ")
	if strings.HasPrefix(strings.ToLower(value), "cid:") {
		value = value[4:]
	}
	if decoded, err := url.PathUnescape(value); err == nil {
		value = decoded
	}
	value = strings.TrimSpace(html.UnescapeString(value))
	value = strings.Trim(value, "<>'\" ")
	return strings.ToLower(value)
}

func normalizeProtocolRelativeRemoteRefs(value string) string {
	replacements := []struct {
		old string
		new string
	}{
		{`src="//`, `src="https://`},
		{`src='//`, `src='https://`},
		{`srcset="//`, `srcset="https://`},
		{`srcset='//`, `srcset='https://`},
		{`href="//`, `href="https://`},
		{`href='//`, `href='https://`},
		{`url(//`, `url(https://`},
	}
	for _, repl := range replacements {
		value = strings.ReplaceAll(value, repl.old, repl.new)
	}
	return value
}

var (
	emailImageTagRE = regexp.MustCompile(`(?is)<img\b[^>]*>`)
	imageURLAttrRE  = regexp.MustCompile(`(?is)\b(?:src|srcset)\s*=\s*("([^"]*)"|'([^']*)'|([^\s>]+))`)
)

// A transparent 1x1 GIF keeps blocked images in the layout without a request.
const blockedRemoteImagePixel = "data:image/gif;base64,R0lGODlhAQABAIAAAAAAAP///yH5BAEAAAAALAAAAAABAAEAAAIBRAA7"

var (
	namedImageURLAttrRE = regexp.MustCompile(`(?is)\b(src|srcset)\s*=\s*("([^"]*)"|'([^']*)'|([^\s>]+))`)
	backgroundURLAttrRE = regexp.MustCompile(`(?is)\bbackground\s*=\s*("([^"]*)"|'([^']*)'|([^\s>]+))`)
	stylesheetLinkTagRE = regexp.MustCompile(`(?is)<link\b[^>]*>`)
	linkHrefAttrRE      = regexp.MustCompile(`(?is)\bhref\s*=\s*("([^"]*)"|'([^']*)'|([^\s>]+))`)
	cssRemoteURLTokenRE = regexp.MustCompile(`(?is)url\(\s*("([^"]*)"|'([^']*)'|([^'")\s]+))\s*\)`)
	remoteRefSchemes    = []string{"http://", "https://", "//"}
)

// neutralizeRemoteRefs rewrites remote references so a blocked message body
// asks the network for nothing. The document CSP already refuses these loads,
// but a document that keeps the live URLs still makes the browser start (and
// log) one blocked request per reference, which buries real console output
// under dozens of CSP violations for a single newsletter.
func neutralizeRemoteRefs(bodyHTML string) string {
	if !containsRemoteRef(bodyHTML) {
		return bodyHTML
	}
	bodyHTML = emailImageTagRE.ReplaceAllStringFunc(bodyHTML, neutralizeImageTagRefs)
	bodyHTML = stylesheetLinkTagRE.ReplaceAllStringFunc(bodyHTML, func(tag string) string {
		if isRemoteRef(attrValue(linkHrefAttrRE, tag)) {
			return ""
		}
		return tag
	})
	bodyHTML = backgroundURLAttrRE.ReplaceAllStringFunc(bodyHTML, func(attr string) string {
		value := attrValueFromMatch(backgroundURLAttrRE.FindStringSubmatch(attr), 2)
		if !isRemoteRef(value) {
			return attr
		}
		return `data-rolltop-blocked-background="` + escapeBlockedRef(value) + `"`
	})
	return cssRemoteURLTokenRE.ReplaceAllStringFunc(bodyHTML, func(token string) string {
		value := attrValueFromMatch(cssRemoteURLTokenRE.FindStringSubmatch(token), 2)
		if !isRemoteRef(value) {
			return token
		}
		// "none" is the one replacement that stays inert in every context a
		// url() token can appear in: it is a valid background or list image and
		// it makes an @font-face src declaration invalid, so the rule is
		// dropped instead of fetched.
		return "none"
	})
}

func neutralizeImageTagRefs(tag string) string {
	return namedImageURLAttrRE.ReplaceAllStringFunc(tag, func(attr string) string {
		match := namedImageURLAttrRE.FindStringSubmatch(attr)
		value := attrValueFromMatch(match, 3)
		if strings.EqualFold(match[1], "srcset") {
			if !hasRemoteSrcsetCandidate(value) {
				return attr
			}
			return `data-rolltop-blocked-srcset="` + escapeBlockedRef(value) + `"`
		}
		if !isRemoteRef(value) {
			return attr
		}
		return `src="` + blockedRemoteImagePixel + `" data-rolltop-blocked-src="` + escapeBlockedRef(value) + `"`
	})
}

func hasRemoteSrcsetCandidate(value string) bool {
	for _, candidate := range srcsetURLCandidates(value) {
		if isRemoteRef(candidate) {
			return true
		}
	}
	return false
}

func containsRemoteRef(value string) bool {
	lower := strings.ToLower(value)
	return strings.Contains(lower, "http://") || strings.Contains(lower, "https://") || strings.Contains(lower, "//")
}

func isRemoteRef(value string) bool {
	value = strings.ToLower(strings.Trim(strings.TrimSpace(html.UnescapeString(value)), `"' `))
	for _, prefix := range remoteRefSchemes {
		if strings.HasPrefix(value, prefix) {
			return true
		}
	}
	return false
}

func attrValue(re *regexp.Regexp, tag string) string {
	return attrValueFromMatch(re.FindStringSubmatch(tag), 2)
}

// attrValueFromMatch reads the first non-empty alternative of a quoted or
// unquoted attribute capture group starting at start.
func attrValueFromMatch(match []string, start int) string {
	if len(match) <= start {
		return ""
	}
	for _, candidate := range match[start:] {
		if strings.TrimSpace(candidate) != "" {
			return strings.TrimSpace(candidate)
		}
	}
	return ""
}

func escapeBlockedRef(value string) string {
	return html.EscapeString(html.UnescapeString(value))
}

func removeBlockedRemoteImages(bodyHTML string, patterns []string) string {
	blockers := compileRemoteImageBlockPatterns(patterns)
	if len(blockers) == 0 {
		return bodyHTML
	}
	return emailImageTagRE.ReplaceAllStringFunc(bodyHTML, func(tag string) string {
		for _, candidate := range imageURLCandidatesFromTag(tag) {
			for _, blocker := range blockers {
				if blocker.MatchString(candidate) {
					return ""
				}
			}
		}
		return tag
	})
}

func compileRemoteImageBlockPatterns(patterns []string) []*regexp.Regexp {
	out := make([]*regexp.Regexp, 0, len(patterns))
	for _, pattern := range patterns {
		pattern = strings.TrimSpace(pattern)
		if pattern == "" {
			continue
		}
		re, err := regexp.Compile(pattern)
		if err == nil {
			out = append(out, re)
		}
	}
	return out
}

func imageURLCandidatesFromTag(tag string) []string {
	var out []string
	for _, match := range imageURLAttrRE.FindAllStringSubmatch(tag, -1) {
		value := ""
		for _, candidate := range match[2:] {
			if candidate != "" {
				value = candidate
				break
			}
		}
		for _, candidate := range srcsetURLCandidates(value) {
			if candidate != "" {
				out = append(out, candidate)
			}
		}
	}
	return out
}

func srcsetURLCandidates(value string) []string {
	var out []string
	for _, part := range strings.Split(value, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		fields := strings.Fields(part)
		if len(fields) == 0 {
			continue
		}
		out = append(out, strings.TrimSpace(fields[0]))
	}
	return out
}

func plainTextBodyHTML(bodyText string) string {
	bodyText = strings.ReplaceAll(bodyText, "\x00", "")
	matches := plainTextURLRE.FindAllStringIndex(bodyText, -1)
	if len(matches) == 0 {
		return html.EscapeString(bodyText)
	}
	var b strings.Builder
	last := 0
	for _, match := range matches {
		if match[0] < last {
			continue
		}
		b.WriteString(html.EscapeString(bodyText[last:match[0]]))
		rawMatch := bodyText[match[0]:match[1]]
		rawURL, trailing := splitTrailingURLPunctuation(rawMatch)
		if rawURL == "" {
			b.WriteString(html.EscapeString(rawMatch))
		} else {
			escapedURL := html.EscapeString(rawURL)
			b.WriteString(`<a href="` + escapedURL + `" target="_blank" rel="noreferrer noopener">` + html.EscapeString(shortURLLabel(rawURL)) + `</a>`)
			b.WriteString(html.EscapeString(trailing))
		}
		last = match[1]
	}
	b.WriteString(html.EscapeString(bodyText[last:]))
	return b.String()
}

func splitTrailingURLPunctuation(value string) (string, string) {
	cut := len(value)
	for cut > 0 {
		r := rune(value[cut-1])
		size := 1
		if r >= 0x80 {
			r, size = lastRune(value[:cut])
		}
		if !strings.ContainsRune(".,;:!?)\"]}", r) {
			break
		}
		cut -= size
	}
	return value[:cut], value[cut:]
}

func lastRune(value string) (rune, int) {
	runes := []rune(value)
	if len(runes) == 0 {
		return 0, 0
	}
	r := runes[len(runes)-1]
	return r, len(string(r))
}

func shortURLLabel(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return truncateMiddle(rawURL, 76)
	}
	label := u.Host
	if u.EscapedPath() != "" && u.EscapedPath() != "/" {
		label += u.EscapedPath()
	}
	if u.RawQuery != "" || u.Fragment != "" {
		label += "?..."
	}
	return truncateMiddle(label, 76)
}

func truncateMiddle(value string, maxRunes int) string {
	runes := []rune(value)
	if maxRunes <= 0 || len(runes) <= maxRunes {
		return value
	}
	if maxRunes <= 8 {
		return string(runes[:maxRunes])
	}
	head := (maxRunes - 3) / 2
	tail := maxRunes - 3 - head
	return string(runes[:head]) + "..." + string(runes[len(runes)-tail:])
}
