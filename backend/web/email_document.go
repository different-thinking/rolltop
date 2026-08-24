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
	bodyHTML = removeScriptElements(bodyHTML)
	bodyHTML = removeInlineScripting(bodyHTML)
	bodyHTML = replaceInlineCIDRefs(bodyHTML, attachments)
	if allowRemoteImages {
		bodyHTML = normalizeProtocolRelativeRemoteRefs(bodyHTML)
		bodyHTML = removeBlockedRemoteImages(bodyHTML, blockedImagePatterns)
		// A message that carries its own <base href> answers the question the
		// pass below exists for: its relative references resolve against the
		// sender's server, which the reader has just agreed to load from.
		if !declaresRemoteBase(bodyHTML) {
			bodyHTML = neutralizeUnresolvableRefs(bodyHTML)
		}
	} else {
		bodyHTML = neutralizeRemoteRefs(bodyHTML)
		// A declared base does not save a relative reference here: it makes it
		// remote, and remote is what this mode refuses.
		bodyHTML = neutralizeUnresolvableRefs(bodyHTML)
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

// A script in a message body never runs: the document below allows no script
// source at all, and the iframe around it is sandboxed without allow-scripts.
// It is removed anyway, because a blocked one is not silent - the browser logs
// "Blocked script execution in 'about:srcdoc'" for every message the reader
// opens, which buries the console errors that do mean something.
var (
	scriptBlockRE = regexp.MustCompile(`(?is)<script\b[^>]*>.*?</script\s*>`)
	scriptOpenRE  = regexp.MustCompile(`(?is)<script\b[^>]*>`)
	scriptCloseRE = regexp.MustCompile(`(?is)</script\s*>`)
)

func removeScriptElements(bodyHTML string) string {
	if !strings.Contains(strings.ToLower(bodyHTML), "<script") {
		return bodyHTML
	}
	bodyHTML = scriptBlockRE.ReplaceAllString(bodyHTML, "")
	// An opening tag with no closing tag left after that swallows the rest of
	// the document in a browser too - everything behind it is script text until
	// the end of the file - so dropping the tag alone would not match what the
	// reader would have seen: it would spill the script's source into the body
	// as visible text, and reparse whatever markup follows as document HTML.
	if opening := scriptOpenRE.FindStringIndex(bodyHTML); opening != nil {
		return bodyHTML[:opening[0]]
	}
	return scriptCloseRE.ReplaceAllString(bodyHTML, "")
}

// A blocked script is not silent either. The frame around a message body is
// sandboxed without allow-scripts, so an inline event handler - typically the
// onerror on an image that did not load - costs one "Blocked script execution
// in 'about:srcdoc'" line per firing, and a reader opening one newsletter fills
// the console with them. The handlers and javascript: URLs are removed for the
// same reason <script> is: they can never run here, and what cannot run should
// not be logged.
var eventHandlerNameRE = regexp.MustCompile(`(?i)^on[a-z]+$`)

// There is deliberately no cheap pre-test for whether a body is worth walking.
// The obvious ones read the markup the way this code hopes it is written -
// whitespace before a handler, javascript: spelled in letters - and a body that
// spells either differently, as `<img src="x"onerror="…">` does and as an
// entity-encoded scheme does, would skip the pass that was written for it. The
// walk is one pass over the tags of a message body, which the neutralizing pass
// below spends unconditionally already.
func removeInlineScripting(bodyHTML string) string {
	return htmlTagRE.ReplaceAllStringFunc(bodyHTML, func(tag string) string {
		return rewriteTagAttrs(tag, func(attrName, rawValue string) (string, bool) {
			if eventHandlerNameRE.MatchString(strings.TrimSpace(attrName)) {
				return "", true
			}
			value, _ := splitAttrValue(rawValue)
			if isScriptURL(value) {
				return "", true
			}
			return "", false
		})
	})
}

// isScriptURL reads a URL the way a browser does before it decides to run one:
// leading whitespace and HTML entities are decoded away first, so an obfuscated
// javascript: URL is recognised as the same thing as a plain one.
func isScriptURL(value string) bool {
	value = strings.ToLower(html.UnescapeString(value))
	value = strings.Map(func(r rune) rune {
		if r <= ' ' {
			return -1
		}
		return r
	}, value)
	return strings.HasPrefix(value, "javascript:") || strings.HasPrefix(value, "vbscript:")
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

// hasUnresolvedCIDRefs reports a body that points at a part of its own message
// which no attachment row answers, so replaceInlineCIDRefs left the cid: URL
// standing and the reader will show a broken image for it.
func hasUnresolvedCIDRefs(bodyHTML string, attachments []store.Attachment) bool {
	if !strings.Contains(strings.ToLower(bodyHTML), "cid:") {
		return false
	}
	urlsByCID := inlineAttachmentURLsByCID(attachments)
	for _, match := range cidURLRE.FindAllStringSubmatch(bodyHTML, -1) {
		if len(match) < 2 {
			continue
		}
		if _, ok := urlsByCID[normalizeContentID(match[1])]; !ok {
			return true
		}
	}
	return false
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

var (
	htmlTagRE    = regexp.MustCompile(`(?is)<[a-zA-Z][^>"']*(?:(?:"[^"]*"|'[^']*')[^>"']*)*>`)
	styleBlockRE = regexp.MustCompile(`(?is)<style\b[^>]*>.*?</style>`)
	tagNameRE    = regexp.MustCompile(`(?is)^<\s*([a-zA-Z][^\s/>]*)`)
	// An attribute does not need whitespace in front of it: a browser starts a
	// new one at whatever follows a quoted value, and a solidus separates them
	// too, so `<img src="x"onerror="…">` and `<img src="x"/onerror="…">` both
	// carry a handler. Requiring the space here let those through untouched -
	// the handler was never seen, and neither was a remote src written the same
	// way. Only an unquoted value keeps the solidus, which the value pattern
	// below already does, because that is where the browser keeps it too.
	tagAttrRE        = regexp.MustCompile(`(?is)^[\s/]*([^\s/>=]+)(\s*=\s*("[^"]*"|'[^']*'|[^\s>]*))?`)
	cssImportRuleRE  = regexp.MustCompile(`(?is)@import\s+(?:url\(\s*("[^"]*"|'[^']*'|[^'")\s]*)\s*\)|("[^"]*"|'[^']*'))[^;]*;?`)
	cssURLTokenRE    = regexp.MustCompile(`(?is)url\(\s*("([^"]*)"|'([^']*)'|([^'")\s]+))\s*\)`)
	remoteRefSchemes = []string{"http://", "https://", "//"}
	// Attributes the browser fetches on its own, whatever the element is.
	remoteFetchAttrs = map[string]bool{"src": true, "srcset": true, "lowsrc": true, "dynsrc": true, "poster": true, "background": true}
	// Attributes that only load a resource on particular elements. href is not
	// in the shared set because on an anchor it is a navigation, but SVG
	// <image>, <use> and <feImage> fetch it (xlink:href on legacy documents),
	// and <object> fetches data.
	remoteFetchAttrsByTag = map[string]map[string]bool{
		"image":   {"href": true, "xlink:href": true},
		"use":     {"href": true, "xlink:href": true},
		"feimage": {"href": true, "xlink:href": true},
		"object":  {"data": true},
	}
)

// refFilter is what one neutralizing pass removes: a test for the references it
// takes out, and the data-* infix the removed value is parked under, so a
// reference the reader could still choose to load stays distinguishable from
// one that could never have loaded at all.
type refFilter struct {
	blocks func(value string) bool
	marker string
}

var (
	remoteRefFilter       = refFilter{blocks: isRemoteRef, marker: "blocked"}
	unresolvableRefFilter = refFilter{blocks: isUnresolvableRef, marker: "unresolved"}
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
	return neutralizeRefs(bodyHTML, remoteRefFilter)
}

// neutralizeUnresolvableRefs rewrites references that point nowhere a message
// body can reach. A body is rendered in an about:srcdoc iframe, and such a
// frame inherits the reader's own URL as its base, so every relative reference
// in the mail resolves against the app instead of against the sender: a mail
// asking for static/DuplicateSans-Regular-Web.woff2 makes the browser ask
// Rolltop for /messages/static/DuplicateSans-Regular-Web.woff2 - refused by the
// document CSP while remote content is blocked, and answered with the reader's
// own HTML or a 404 once it is allowed. Neither outcome shows the picture the
// sender meant, and the reference cannot be repaired because the origin it was
// written for is nowhere in the message, so it is dropped like a blocked remote
// one. This runs whether or not remote content is allowed: allowing images
// grants the sender's server, not Rolltop's own routes.
func neutralizeUnresolvableRefs(bodyHTML string) string {
	return neutralizeRefs(bodyHTML, unresolvableRefFilter)
}

// declaresRemoteBase reports a message that sets its own document base to an
// absolute address. The browser resolves the message's relative references
// against that rather than against the reader's URL, so they name the sender's
// server after all.
func declaresRemoteBase(bodyHTML string) bool {
	if !strings.Contains(strings.ToLower(bodyHTML), "<base") {
		return false
	}
	// A base inside a comment sets nothing - the browser never reads it as an
	// element - so reading one here would hand a message the exception on the
	// strength of markup that does not apply, and leave its relative references
	// live for the app to answer.
	bodyHTML = htmlCommentRE.ReplaceAllString(bodyHTML, "")
	for _, tag := range htmlTagRE.FindAllString(bodyHTML, -1) {
		if strings.EqualFold(tagName(tag), "base") && isRemoteRef(tagAttrValue(tag, "href")) {
			return true
		}
	}
	return false
}

func neutralizeRefs(bodyHTML string, filter refFilter) string {
	// url() is only rewritten where a browser reads it as CSS. Running it over
	// the whole document would also rewrite url(...) inside ordinary sentences.
	bodyHTML = styleBlockRE.ReplaceAllStringFunc(bodyHTML, func(css string) string {
		return neutralizeFilteredCSS(css, filter)
	})
	return htmlTagRE.ReplaceAllStringFunc(bodyHTML, func(tag string) string {
		return neutralizeTagRefs(tag, filter)
	})
}

func neutralizeTagRefs(tag string, filter refFilter) string {
	name := strings.ToLower(tagName(tag))
	if name == "link" && filter.blocks(tagAttrValue(tag, "href")) {
		return ""
	}
	elementAttrs := remoteFetchAttrsByTag[name]
	return rewriteTagAttrs(tag, func(attrName, rawValue string) (string, bool) {
		lower := strings.ToLower(attrName)
		value, quote := splitAttrValue(rawValue)
		switch {
		case lower == "style":
			neutralized := neutralizeFilteredCSS(value, filter)
			if neutralized == value {
				return "", false
			}
			return "style=" + quoteAttrValue(neutralized, quote), true
		case lower == "srcset":
			kept, removed := withoutFilteredSrcsetCandidates(value, filter)
			if !removed {
				return "", false
			}
			blocked := `data-rolltop-` + filter.marker + `-srcset="` + escapeBlockedRef(value) + `"`
			if kept == "" {
				return blocked, true
			}
			return "srcset=" + quoteAttrValue(kept, quote) + " " + blocked, true
		case (remoteFetchAttrs[lower] || elementAttrs[lower]) && filter.blocks(value):
			// The reference is dropped rather than replaced with a placeholder
			// image: an element without src makes no request and still shows
			// its alt text at its own size.
			return `data-rolltop-` + filter.marker + `-` + blockedAttrName(lower) + `="` + escapeBlockedRef(value) + `"`, true
		}
		return "", false
	})
}

func neutralizeFilteredCSS(css string, filter refFilter) string {
	css = cssImportRuleRE.ReplaceAllStringFunc(css, func(rule string) string {
		if filter.blocks(attrValueFromMatch(cssImportRuleRE.FindStringSubmatch(rule), 1)) {
			return ""
		}
		return rule
	})
	return cssURLTokenRE.ReplaceAllStringFunc(css, func(token string) string {
		if !filter.blocks(attrValueFromMatch(cssURLTokenRE.FindStringSubmatch(token), 2)) {
			return token
		}
		// "none" is the one replacement that stays inert in every context a
		// url() token can appear in: it is a valid background or list image and
		// it makes an @font-face src declaration invalid, so the rule is
		// dropped instead of fetched.
		return "none"
	})
}

// rewriteTagAttrs walks a tag's attributes in order and lets rewrite replace
// whole `name=value` tokens. Matching attribute patterns against the raw tag
// instead would also match text that merely looks like an attribute inside
// another attribute's quoted value, and rewriting that text would break the
// quoting of the attribute it lives in.
func rewriteTagAttrs(tag string, rewrite func(name, rawValue string) (string, bool)) string {
	open := tagNameRE.FindString(tag)
	if open == "" {
		return tag
	}
	var b strings.Builder
	b.WriteString(open)
	rest := tag[len(open):]
	for {
		match := tagAttrRE.FindStringSubmatchIndex(rest)
		if match == nil {
			break
		}
		name := rest[match[2]:match[3]]
		rawValue := ""
		if match[6] >= 0 {
			rawValue = rest[match[6]:match[7]]
		}
		if replacement, ok := rewrite(name, rawValue); ok {
			b.WriteString(" " + replacement)
		} else {
			b.WriteString(rest[match[0]:match[1]])
		}
		rest = rest[match[1]:]
	}
	b.WriteString(rest)
	return b.String()
}

func tagName(tag string) string {
	match := tagNameRE.FindStringSubmatch(tag)
	if len(match) < 2 {
		return ""
	}
	return match[1]
}

func tagAttrValue(tag, name string) string {
	found := ""
	rewriteTagAttrs(tag, func(attrName, rawValue string) (string, bool) {
		if strings.EqualFold(attrName, name) && found == "" {
			found, _ = splitAttrValue(rawValue)
		}
		return "", false
	})
	return found
}

// splitAttrValue returns an attribute value without its quotes, plus the quote
// character the source used so a rewritten value can keep it.
func splitAttrValue(rawValue string) (string, string) {
	value := strings.TrimSpace(rawValue)
	value = strings.TrimSpace(strings.TrimPrefix(value, "="))
	if len(value) >= 2 && (value[0] == '"' || value[0] == '\'') && value[len(value)-1] == value[0] {
		return value[1 : len(value)-1], string(value[0])
	}
	return value, ""
}

func quoteAttrValue(value, quote string) string {
	if quote == "'" && !strings.Contains(value, "'") {
		return "'" + value + "'"
	}
	return `"` + strings.ReplaceAll(value, `"`, "&quot;") + `"`
}

// blockedAttrName keeps a namespaced attribute such as xlink:href usable as the
// suffix of the data-* attribute the blocked reference is parked in.
func blockedAttrName(name string) string {
	return strings.ReplaceAll(name, ":", "-")
}

// withoutFilteredSrcsetCandidates drops only the candidates the filter removes,
// so a srcset that also lists an inline attachment keeps rendering that
// attachment.
func withoutFilteredSrcsetCandidates(value string, filter refFilter) (string, bool) {
	candidates := parseSrcset(value)
	kept := make([]string, 0, len(candidates))
	removed := false
	for _, candidate := range candidates {
		if filter.blocks(candidate.url) {
			removed = true
			continue
		}
		kept = append(kept, strings.TrimSpace(candidate.url+" "+candidate.descriptor))
	}
	return strings.Join(kept, ", "), removed
}

type srcsetCandidate struct {
	url        string
	descriptor string
}

// parseSrcset splits a srcset the way an HTML parser does: a candidate URL runs
// to the next whitespace, and only a comma that ends it separates candidates.
// Splitting on every comma instead would tear a local data: URL, whose payload
// contains one, into invalid candidates.
func parseSrcset(value string) []srcsetCandidate {
	out := make([]srcsetCandidate, 0, 4)
	for i := 0; i < len(value); {
		for i < len(value) && (isSrcsetSpace(value[i]) || value[i] == ',') {
			i++
		}
		if i >= len(value) {
			break
		}
		start := i
		for i < len(value) && !isSrcsetSpace(value[i]) {
			i++
		}
		rawURL := value[start:i]
		if trimmed := strings.TrimRight(rawURL, ","); len(trimmed) < len(rawURL) {
			out = append(out, srcsetCandidate{url: trimmed})
			continue
		}
		for i < len(value) && isSrcsetSpace(value[i]) {
			i++
		}
		descriptorStart := i
		for i < len(value) && value[i] != ',' {
			i++
		}
		descriptor := strings.TrimSpace(value[descriptorStart:i])
		if i < len(value) {
			i++
		}
		out = append(out, srcsetCandidate{url: rawURL, descriptor: descriptor})
	}
	return out
}

func isSrcsetSpace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r' || b == '\f'
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

// isUnresolvableRef reports a reference the browser would resolve against
// Rolltop itself. Everything a message body may legitimately load names where
// it comes from: a data: payload carries it, a cid: part names an attachment of
// this very message, and an /attachments/ path is one this renderer wrote for
// exactly that part. A cid: that found no attachment stays: it is a valid
// scheme the CSP allows and a missing part is worth seeing as a broken image
// rather than silently dropping. What is left is relative or root-relative -
// written for the sender's web server and meaningful only there.
func isUnresolvableRef(value string) bool {
	value = strings.ToLower(strings.Trim(strings.TrimSpace(html.UnescapeString(value)), `"' `))
	if value == "" || strings.HasPrefix(value, "#") {
		return false
	}
	for _, prefix := range []string{"http://", "https://", "//", "data:", "cid:", "mailto:", "tel:", "/attachments/"} {
		if strings.HasPrefix(value, prefix) {
			return false
		}
	}
	return true
}

// attrValueFromMatch reads the first non-empty alternative of a quoted or
// unquoted capture group starting at start.
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
	for _, candidate := range parseSrcset(value) {
		if candidate.url != "" {
			out = append(out, candidate.url)
		}
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
