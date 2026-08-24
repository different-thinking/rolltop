// File overview: Tests for quote clipping and forwarded-message display behavior.

package web

import (
	"strings"
	"testing"

	"rolltop/backend/store"
)

func TestClipTextQuoteUsesStandardReplyMarker(t *testing.T) {
	body := "Thanks, that works for me.\n\nOn Tue, Alice <alice@example.test> wrote:\n> The earlier note\n> with quoted details"
	displayHTML, displayText, hidden := clippedEmailBody("", body, nil)
	if displayHTML != "" {
		t.Fatalf("displayHTML = %q", displayHTML)
	}
	if !hidden {
		t.Fatal("expected quoted text to be hidden")
	}
	if displayText != "Thanks, that works for me." {
		t.Fatalf("displayText = %q", displayText)
	}
}

func TestClipTextQuoteUsesPriorMessageOverlap(t *testing.T) {
	previous := strings.Join([]string{
		"The prior message has enough text to be recognized later.",
		"It spans multiple lines so the overlap is not accidental.",
		"The final line closes the repeated copied section cleanly.",
	}, "\n")
	current := "Fresh reply at the top.\n\n" + previous

	_, displayText, hidden := clippedEmailBody("", current, []string{previous})
	if !hidden {
		t.Fatal("expected repeated prior message to be hidden")
	}
	if displayText != "Fresh reply at the top." {
		t.Fatalf("displayText = %q", displayText)
	}
}

func TestClipTextQuoteSkipsLeadingQuotedPrefaceBeforeFreshReply(t *testing.T) {
	body := strings.Join([]string{
		"On 10/4/07, InjuryProneErik <injprone@gmail.com> wrote:",
		"> It reads first like a mail order bride, but turn into nigerian sneakiness with a drop of the pant...err, a hat!",
		"",
		"*cracking knuckles*",
		"You are now mesmerized by the thought of nailing an exotic European woman.",
		"",
		"On 10/4/07, InjuryProneErik <injprone@gmail.com> wrote:",
		"> It reads first like a mail order bride, but turn into nigerian sneakiness with a drop of the pant...err, a hat!",
	}, "\n")

	_, displayText, hidden := clippedEmailBody("", body, nil)
	if !hidden {
		t.Fatal("expected leading quoted preface to be hidden")
	}
	if !strings.HasPrefix(displayText, "*cracking knuckles*") {
		t.Fatalf("displayText starts with wrong fragment: %q", displayText)
	}
	if strings.HasPrefix(displayText, "On 10/4/07") {
		t.Fatalf("displayText kept leading quote attribution: %q", displayText)
	}
}

func TestClipTextQuoteKeepsQuotedOnlyReplyVisible(t *testing.T) {
	body := strings.Join([]string{
		"On Tue, Alice <alice@example.test> wrote:",
		"> The earlier note",
		"> with quoted details",
	}, "\n")

	_, displayText, hidden := clippedEmailBody("", body, nil)
	if hidden {
		t.Fatal("did not expect quoted-only body to be hidden")
	}
	if displayText != body {
		t.Fatalf("displayText = %q", displayText)
	}
}

func TestClipTextQuoteKeepsForwardedBlockWithInlineCommentVisible(t *testing.T) {
	body := strings.Join([]string{
		"Initial note before the forward.",
		"",
		"---------- Forwarded message ---------",
		"From: Jennifer Welsh <jenny@example.test>",
		"Date: Oct 4, 2007 12:54 PM",
		"Subject: Your new friend",
		"To: Erik <erik@example.test>",
		"",
		"Forwarded body that provides context.",
		"",
		"Inline comment added underneath the forwarded note.",
	}, "\n")

	_, displayText, hidden := clippedEmailBody("", body, nil)
	if hidden {
		t.Fatal("did not expect forwarded body with inline comment to be hidden")
	}
	if !strings.Contains(displayText, "Forwarded body that provides context.") || !strings.Contains(displayText, "Inline comment added underneath") {
		t.Fatalf("displayText lost forwarded or inline content: %q", displayText)
	}
}

func TestClipTextQuoteKeepsInlineCommentAfterQuotedOverlapVisible(t *testing.T) {
	previous := strings.Join([]string{
		"The prior message has enough text to be recognized later.",
		"It spans multiple lines so the overlap is not accidental.",
		"The final line closes the repeated copied section cleanly.",
	}, "\n")
	body := "Fresh reply at the top.\n\n" + previous + "\n\nInline comment after the quoted prior message."

	_, displayText, hidden := clippedEmailBody("", body, []string{previous})
	if hidden {
		t.Fatal("did not expect inline comment after quoted overlap to be hidden")
	}
	if !strings.Contains(displayText, "Inline comment after the quoted prior message") {
		t.Fatalf("displayText lost inline comment: %q", displayText)
	}
}

func TestClipTextQuoteKeepsInlineCommentAfterReplyQuoteVisible(t *testing.T) {
	body := strings.Join([]string{
		"Fresh reply at the top.",
		"",
		"On Tue, Alice <alice@example.test> wrote:",
		"> The earlier note",
		"> with quoted details",
		"",
		"Inline comment below the quoted note.",
	}, "\n")

	_, displayText, hidden := clippedEmailBody("", body, nil)
	if hidden {
		t.Fatal("did not expect inline comment after reply quote to be hidden")
	}
	if !strings.Contains(displayText, "Inline comment below the quoted note") {
		t.Fatalf("displayText lost inline comment: %q", displayText)
	}
}

func TestClipHTMLQuoteKeepsRichPrefix(t *testing.T) {
	body := `<div><p>Fresh answer with enough text to keep as rich HTML.</p><blockquote type="cite"><p>Older copied text</p></blockquote></div>`
	displayHTML, _, hidden := clippedEmailBody(body, "Fresh answer with enough text to keep as rich HTML.\n\nOlder copied text", nil)
	if !hidden {
		t.Fatal("expected HTML quote to be hidden")
	}
	if strings.Contains(displayHTML, "Older copied text") {
		t.Fatalf("displayHTML still contains quoted content: %s", displayHTML)
	}
	if !strings.Contains(displayHTML, "Fresh answer") {
		t.Fatalf("displayHTML lost fresh content: %s", displayHTML)
	}
}

func TestClipHTMLQuoteFallsBackToTextForAttributionOnlyInlineReply(t *testing.T) {
	bodyHTML := `<div>On 10/4/07, InjuryProneErik &lt;<a href="mailto:injprone@gmail.com">injprone@gmail.com</a>&gt; wrote:</div><blockquote><div>Earlier quoted text.</div></blockquote><div>*cracking knuckles*</div><div>Inline reply text below the quote.</div>`
	bodyText := strings.Join([]string{
		"On 10/4/07, InjuryProneErik <injprone@gmail.com> wrote:",
		"> Earlier quoted text.",
		"",
		"*cracking knuckles*",
		"Inline reply text below the quote.",
	}, "\n")

	displayHTML, displayText, hidden := clippedEmailBody(bodyHTML, bodyText, nil)
	if !hidden {
		t.Fatal("expected quoted text to remain hidden")
	}
	if displayHTML != "" {
		t.Fatalf("expected text fallback, got displayHTML = %q", displayHTML)
	}
	if !strings.HasPrefix(displayText, "*cracking knuckles*") || !strings.Contains(displayText, "Inline reply text") {
		t.Fatalf("displayText = %q", displayText)
	}
}

func TestClipHTMLQuoteDoesNotHideForwardedHTMLMessage(t *testing.T) {
	bodyHTML := `<div>Fresh intro.</div><div>---------- Forwarded message ---------</div><span class="gmail_quote">From: Sender</span><blockquote class="gmail_quote"><div>Forwarded body.</div></blockquote>`
	bodyText := "Fresh intro.\n\n---------- Forwarded message ---------\nFrom: Sender\n\nForwarded body."
	displayHTML, _, hidden := clippedEmailBody(bodyHTML, bodyText, nil)
	if hidden {
		t.Fatal("did not expect forwarded HTML message to be hidden")
	}
	if displayHTML != bodyHTML {
		t.Fatalf("displayHTML = %q", displayHTML)
	}
}

func TestClipHTMLQuoteKeepsForwardedHTMLWhenTextQuoteWasClipped(t *testing.T) {
	bodyHTML := `<div>On 10/4/07, Sender wrote:</div><blockquote class="gmail_quote"><div>Earlier note.</div></blockquote><div>*cracking knuckles*</div><div>---------- Forwarded message ---------</div><blockquote class="gmail_quote"><div>Forwarded body.</div></blockquote><div>Inline comment.</div>`
	bodyText := strings.Join([]string{
		"On 10/4/07, Sender wrote:",
		"> Earlier note.",
		"",
		"*cracking knuckles*",
		"",
		"---------- Forwarded message ---------",
		"> Forwarded body.",
		"",
		"Inline comment.",
	}, "\n")

	displayHTML, displayText, hidden := clippedEmailBody(bodyHTML, bodyText, nil)
	if hidden {
		t.Fatal("did not expect forwarded HTML to be hidden just because text was clipped")
	}
	if displayHTML != bodyHTML {
		t.Fatalf("displayHTML = %q", displayHTML)
	}
	if !strings.HasPrefix(displayText, "*cracking knuckles*") {
		t.Fatalf("displayText = %q", displayText)
	}
}

func TestClipHTMLQuotePreservesForwardedBlockquoteContent(t *testing.T) {
	bodyHTML := `<div>*cracking knuckles*</div><div>---------- Forwarded message ---------</div><blockquote><div>From: Jennifer Welsh &lt;<a href="mailto:jenny@example.test">jenny@example.test</a>&gt;</div><div>Date: Oct 4, 2007 12:54 PM</div><div>Subject: Your new friend</div><br><div>Forwarded body that provides context.</div></blockquote><div>Inline comment added underneath the forwarded note.</div>`
	bodyText := strings.Join([]string{
		"*cracking knuckles*",
		"",
		"---------- Forwarded message ---------",
		"From: Jennifer Welsh <jenny@example.test>",
		"Date: Oct 4, 2007 12:54 PM",
		"Subject: Your new friend",
		"",
		"Forwarded body that provides context.",
		"",
		"Inline comment added underneath the forwarded note.",
	}, "\n")

	displayHTML, displayText, hidden := clippedEmailBody(bodyHTML, bodyText, nil)
	if hidden {
		t.Fatal("did not expect forwarded blockquote content to be hidden")
	}
	if !strings.Contains(displayHTML, "Forwarded body that provides context.") || !strings.Contains(displayHTML, "Inline comment added underneath") {
		t.Fatalf("displayHTML lost forwarded or inline content: %q", displayHTML)
	}
	if displayText != bodyText {
		t.Fatalf("displayText = %q", displayText)
	}
}

func TestClipHTMLQuoteFallsBackToFullTextForInlineCommentAfterForward(t *testing.T) {
	bodyHTML := `<div>Initial note before the forward.</div><blockquote><div>---------- Forwarded message ---------</div><div>Forwarded body that provides context.</div></blockquote><div>Inline comment added underneath the forwarded note.</div>`
	bodyText := strings.Join([]string{
		"Initial note before the forward.",
		"",
		"---------- Forwarded message ---------",
		"Forwarded body that provides context.",
		"",
		"Inline comment added underneath the forwarded note.",
	}, "\n")

	displayHTML, displayText, hidden := clippedEmailBody(bodyHTML, bodyText, nil)
	if hidden {
		t.Fatal("did not expect full text fallback to be hidden")
	}
	if displayHTML != bodyHTML {
		t.Fatalf("displayHTML = %q", displayHTML)
	}
	if !strings.Contains(displayText, "Forwarded body that provides context.") || !strings.Contains(displayText, "Inline comment added underneath") {
		t.Fatalf("displayText lost forwarded or inline content: %q", displayText)
	}
}

func TestClipHTMLQuoteDoesNotRemoveLeadingBlockquoteFormatting(t *testing.T) {
	body := `<blockquote><p>This whole message uses blockquote styling.</p></blockquote>`
	displayHTML, _, hidden := clippedEmailBody(body, "This whole message uses blockquote styling.", nil)
	if hidden {
		t.Fatal("did not expect leading blockquote-only body to be hidden")
	}
	if displayHTML != body {
		t.Fatalf("displayHTML = %q", displayHTML)
	}
}

func TestEmailDocumentRendersPlainTextAsProportionalWrappedText(t *testing.T) {
	doc := emailDocument("", "Hello\n\nThis should wrap like mail.", false)
	if strings.Contains(doc, "<pre>") {
		t.Fatalf("plain text rendered as pre: %s", doc)
	}
	if !strings.Contains(doc, `class="plaintext"`) {
		t.Fatalf("missing plaintext wrapper: %s", doc)
	}
	if !strings.Contains(doc, "white-space:pre-wrap") {
		t.Fatalf("missing whitespace preservation: %s", doc)
	}
}

func TestEmailDocumentLeavesThemingToTheClient(t *testing.T) {
	doc := emailDocument("", "Hello dark mode", false)
	if !strings.Contains(doc, `class="plaintext-doc"`) {
		t.Fatalf("missing plaintext document marker: %s", doc)
	}
	// Theme rules belong to frontend/src/lib/emailDocumentTheme.ts, which is the
	// single source shared with the PGP plugin. A copy here would drift.
	if strings.Contains(doc, "data-rolltop-theme") {
		t.Fatalf("server document embeds theme rules: %s", doc)
	}

	htmlDoc := emailDocument(`<p>Hello HTML dark mode</p>`, "", false)
	if strings.Contains(htmlDoc, `class="plaintext-doc"`) {
		t.Fatalf("html document should not use plaintext marker: %s", htmlDoc)
	}
	if strings.Contains(htmlDoc, "data-rolltop-theme") {
		t.Fatalf("server document embeds theme rules: %s", htmlDoc)
	}
}

func TestEmailDocumentRewritesInlineCIDImages(t *testing.T) {
	attachments := []store.Attachment{
		{ID: 42, ContentID: "hero@example.test", IsInline: true, ContentType: "image/png"},
		{ID: 43, ContentID: "Logo.JPG", IsInline: true, ContentType: "image/jpeg"},
	}
	body := `<p>Images</p><img src="cid:hero%40example.test"><img src='cid:logo.jpg'><img src="cid:missing">`
	doc := emailDocumentWithInlineAttachments(body, "", false, nil, attachments)
	if !strings.Contains(doc, `src="/attachments/42/inline"`) {
		t.Fatalf("encoded cid was not rewritten: %s", doc)
	}
	if !strings.Contains(doc, `src='/attachments/43/inline'`) {
		t.Fatalf("case-insensitive cid was not rewritten: %s", doc)
	}
	if !strings.Contains(doc, `cid:missing`) {
		t.Fatalf("unknown cid should be left alone: %s", doc)
	}
	if !strings.Contains(doc, `img-src 'self' data: cid:`) {
		t.Fatalf("same-origin inline images not allowed by CSP: %s", doc)
	}
}

func TestEmailDocumentAllowsRemoteStylesAndFontsWithImages(t *testing.T) {
	doc := emailDocumentWithBlocklist(`<link rel="stylesheet" href="//cdn.example.test/mail.css"><style>@font-face{src:url(//cdn.example.test/mail.woff2)}</style>`, "", true, nil)
	if !strings.Contains(doc, `style-src 'unsafe-inline' http: https:`) {
		t.Fatalf("remote styles not allowed by CSP: %s", doc)
	}
	if !strings.Contains(doc, `font-src data: http: https:`) {
		t.Fatalf("remote fonts not allowed by CSP: %s", doc)
	}
	if !strings.Contains(doc, `href="https://cdn.example.test/mail.css"`) {
		t.Fatalf("protocol-relative stylesheet URL not normalized: %s", doc)
	}
	if !strings.Contains(doc, `url(https://cdn.example.test/mail.woff2)`) {
		t.Fatalf("protocol-relative font URL not normalized: %s", doc)
	}
}

func TestEmailDocumentNeutralizesRemoteRefsWhileImagesAreBlocked(t *testing.T) {
	body := `<link rel="stylesheet" href="https://cdn.example.test/mail.css">` +
		`<style>@import "https://cdn.example.test/import.css";body{background:url(https://cdn.example.test/hero.jpg)}</style>` +
		`<picture><source srcset="https://cdn.example.test/logo@2x.png 2x"><img src="https://cdn.example.test/logo.png" alt="Logo"></picture>` +
		`<input type="image" src="https://cdn.example.test/button.png">` +
		`<video poster="//cdn.example.test/poster.jpg"></video>` +
		`<td background="//cdn.example.test/tile.png"><div style="background-image:url('https://cdn.example.test/bg.jpg')">Hero</div></td>`
	doc := emailDocument(body, "", false)
	// The CSP still refuses remote loads, but the document must not ask for
	// them: every live reference here would cost one console violation.
	for _, live := range []string{
		` src="https://cdn.example.test/logo.png"`,
		` srcset="https://cdn.example.test/logo@2x.png 2x"`,
		` src="https://cdn.example.test/button.png"`,
		` poster="//cdn.example.test/poster.jpg"`,
		` background="//cdn.example.test/tile.png"`,
		"url(https://cdn.example.test/hero.jpg)",
		"url('https://cdn.example.test/bg.jpg')",
		"mail.css",
		"import.css",
	} {
		if strings.Contains(doc, live) {
			t.Fatalf("document kept a live remote reference %q: %s", live, doc)
		}
	}
	for _, blocked := range []string{
		`data-rolltop-blocked-src="https://cdn.example.test/logo.png"`,
		`data-rolltop-blocked-srcset="https://cdn.example.test/logo@2x.png 2x"`,
		`data-rolltop-blocked-src="https://cdn.example.test/button.png"`,
		`data-rolltop-blocked-poster="//cdn.example.test/poster.jpg"`,
		`data-rolltop-blocked-background="//cdn.example.test/tile.png"`,
	} {
		if !strings.Contains(doc, blocked) {
			t.Fatalf("blocked reference %q was not preserved: %s", blocked, doc)
		}
	}
	if !strings.Contains(doc, `alt="Logo"`) || !strings.Contains(doc, "Hero") {
		t.Fatalf("message content was lost: %s", doc)
	}
}

func TestEmailDocumentKeepsLocalRefsWhileImagesAreBlocked(t *testing.T) {
	attachments := []store.Attachment{{ID: 7, ContentID: "hero@example.test", IsInline: true, ContentType: "image/png"}}
	body := `<img src="cid:hero@example.test"><img src="data:image/png;base64,AAAA"><img src="/attachments/7/download">` +
		`<img srcset="cid:hero@example.test 1x, https://cdn.example.test/hero@2x.png 2x">` +
		`<div style="background-image:url(data:image/gif;base64,AAAA)">Local</div>` +
		`<p>Docs at url(https://example.test/guide) explain it.</p>`
	doc := emailDocumentWithInlineAttachments(body, "", false, nil, attachments)
	for _, kept := range []string{
		`src="/attachments/7/inline"`,
		`src="data:image/png;base64,AAAA"`,
		`src="/attachments/7/download"`,
		`srcset="/attachments/7/inline 1x"`,
		`url(data:image/gif;base64,AAAA)`,
		`Docs at url(https://example.test/guide) explain it.`,
	} {
		if !strings.Contains(doc, kept) {
			t.Fatalf("%q was neutralized: %s", kept, doc)
		}
	}
	if strings.Contains(doc, `srcset="https://cdn.example.test/hero@2x.png 2x"`) {
		t.Fatalf("remote srcset candidate was kept: %s", doc)
	}
	if strings.Count(doc, "data-rolltop-blocked-") != 1 {
		t.Fatalf("local references were treated as remote: %s", doc)
	}
}

func TestEmailDocumentNeutralizesSVGAndObjectRemoteRefs(t *testing.T) {
	body := `<svg><image href="https://cdn.example.test/a.svg" /><image xlink:href="https://cdn.example.test/b.svg" /><use href="https://cdn.example.test/sprite.svg#i" /></svg>` +
		`<object data="https://cdn.example.test/c.svg"></object>` +
		`<a href="https://cdn.example.test/click">Angebot</a>`
	doc := emailDocument(body, "", false)
	for _, live := range []string{
		` href="https://cdn.example.test/a.svg"`,
		` xlink:href="https://cdn.example.test/b.svg"`,
		` href="https://cdn.example.test/sprite.svg#i"`,
		` data="https://cdn.example.test/c.svg"`,
	} {
		if strings.Contains(doc, live) {
			t.Fatalf("document kept a live remote reference %q: %s", live, doc)
		}
	}
	for _, blocked := range []string{
		`data-rolltop-blocked-href="https://cdn.example.test/a.svg"`,
		`data-rolltop-blocked-xlink-href="https://cdn.example.test/b.svg"`,
		`data-rolltop-blocked-href="https://cdn.example.test/sprite.svg#i"`,
		`data-rolltop-blocked-data="https://cdn.example.test/c.svg"`,
	} {
		if !strings.Contains(doc, blocked) {
			t.Fatalf("blocked reference %q was not preserved: %s", blocked, doc)
		}
	}
	// An anchor href is a navigation the user starts, not a load the browser
	// makes, so it has to survive untouched.
	if !strings.Contains(doc, `<a href="https://cdn.example.test/click">Angebot</a>`) {
		t.Fatalf("anchor link was rewritten: %s", doc)
	}
}

func TestEmailDocumentKeepsDataURLCandidatesInMixedSrcset(t *testing.T) {
	body := `<img srcset="data:image/png;base64,AAAA 1x, https://cdn.example.test/hero@2x.png 2x" src="data:image/png;base64,AAAA">`
	doc := emailDocument(body, "", false)
	if !strings.Contains(doc, `srcset="data:image/png;base64,AAAA 1x"`) {
		t.Fatalf("data URL candidate was torn apart: %s", doc)
	}
	if !strings.Contains(doc, `data-rolltop-blocked-srcset="data:image/png;base64,AAAA 1x, https://cdn.example.test/hero@2x.png 2x"`) {
		t.Fatalf("removed remote candidate was not recorded: %s", doc)
	}
	// The remote candidate may only survive inside the blocked data attribute.
	if strings.Count(doc, "cdn.example.test") != 1 {
		t.Fatalf("remote candidate stayed live: %s", doc)
	}
}

func TestEmailDocumentDoesNotRewriteAttributeLookalikeText(t *testing.T) {
	body := `<img alt="see src=https://cdn.example.test/x.png here" title='background="//cdn.example.test/y.png"' src="/attachments/1/inline">`
	doc := emailDocumentWithInlineAttachments(body, "", false, nil, nil)
	if !strings.Contains(doc, `alt="see src=https://cdn.example.test/x.png here"`) {
		t.Fatalf("alt text was rewritten: %s", doc)
	}
	if !strings.Contains(doc, `title='background="//cdn.example.test/y.png"'`) {
		t.Fatalf("title text was rewritten: %s", doc)
	}
	if strings.Contains(doc, "data-rolltop-blocked-") {
		t.Fatalf("attribute text was treated as a reference: %s", doc)
	}
}

func TestEmailDocumentRemovesBlockedRemoteImages(t *testing.T) {
	body := `<p>Brand mail</p><img src="https://track.example.test/open.php?id=1"><img src="https://cdn.example.test/logo.png">`
	doc := emailDocumentWithBlocklist(body, "", true, []string{`(?i)/open\.php`})
	if strings.Contains(doc, "open.php") {
		t.Fatalf("blocked tracker image was retained: %s", doc)
	}
	if !strings.Contains(doc, "logo.png") {
		t.Fatalf("legitimate image was removed: %s", doc)
	}
}

func TestEmailDocumentNeutralizesRefsThatResolveAgainstTheApp(t *testing.T) {
	// about:srcdoc inherits the reader's URL as its base, so each of these would
	// be fetched from Rolltop itself - refused by the document CSP here, and
	// answered with the app's own HTML or a 404 once images are allowed.
	body := `<style>@font-face{font-family:Duplicate;src:url(static/DuplicateSans-Regular-Web.woff2) format("woff2")}@import "mail.css";</style>` +
		`<img src="images/hero.png" alt="Hero">` +
		`<img srcset="images/hero.png 1x">` +
		`<link rel="stylesheet" href="/assets/mail.css">` +
		`<div style="background-image:url('../tile.png')">Tile</div>`
	for _, imagesAllowed := range []bool{false, true} {
		doc := emailDocument(body, "", imagesAllowed)
		for _, live := range []string{
			"url(static/DuplicateSans-Regular-Web.woff2)",
			` src="images/hero.png"`,
			` srcset="images/hero.png 1x"`,
			`href="/assets/mail.css"`,
			"url('../tile.png')",
			"mail.css",
		} {
			if strings.Contains(doc, live) {
				t.Fatalf("images_allowed=%t kept a reference to the app origin %q: %s", imagesAllowed, live, doc)
			}
		}
		for _, parked := range []string{
			`data-rolltop-unresolved-src="images/hero.png"`,
			`data-rolltop-unresolved-srcset="images/hero.png 1x"`,
		} {
			if !strings.Contains(doc, parked) {
				t.Fatalf("images_allowed=%t dropped %q without a record: %s", imagesAllowed, parked, doc)
			}
		}
		if !strings.Contains(doc, `alt="Hero"`) || !strings.Contains(doc, "Tile") {
			t.Fatalf("images_allowed=%t lost message content: %s", imagesAllowed, doc)
		}
	}
}

func TestEmailDocumentKeepsResolvableRefsWhileNeutralizingRelativeOnes(t *testing.T) {
	attachments := []store.Attachment{{ID: 7, ContentID: "hero@example.test", IsInline: true, ContentType: "image/png"}}
	body := `<img src="cid:hero@example.test"><img src="data:image/png;base64,AAAA"><img src="/attachments/7/download">` +
		`<img src="cid:missing"><a href="#top">Top</a><a href="mailto:sender@example.test">Mail</a>` +
		`<img src="https://cdn.example.test/hero.png">`
	doc := emailDocumentWithInlineAttachments(body, "", true, nil, attachments)
	for _, kept := range []string{
		`src="/attachments/7/inline"`,
		`src="data:image/png;base64,AAAA"`,
		`src="/attachments/7/download"`,
		`src="cid:missing"`,
		`href="#top"`,
		`href="mailto:sender@example.test"`,
		`src="https://cdn.example.test/hero.png"`,
	} {
		if !strings.Contains(doc, kept) {
			t.Fatalf("%q was neutralized: %s", kept, doc)
		}
	}
	if strings.Contains(doc, "data-rolltop-unresolved-") {
		t.Fatalf("a resolvable reference was treated as unresolvable: %s", doc)
	}
}

func TestEmailDocumentRemovesInlineScripting(t *testing.T) {
	// Nothing here can run: the frame is sandboxed without allow-scripts and the
	// document CSP allows no script source. Each one would still cost a
	// "Blocked script execution in 'about:srcdoc'" line when it fired.
	body := `<img src="cid:missing" onerror="alert(1)" alt="Hero">` +
		`<a href="javascript:alert(2)" onclick='alert(3)'>Angebot</a>` +
		`<body onload="alert(4)"><p ONMOUSEOVER="alert(5)">Text</p>`
	doc := emailDocument(body, "", false)
	for _, script := range []string{"alert(1)", "alert(2)", "alert(3)", "alert(4)", "alert(5)", "onerror", "onclick", "onload", "javascript:"} {
		if strings.Contains(strings.ToLower(doc), strings.ToLower(script)) {
			t.Fatalf("document kept inline scripting %q: %s", script, doc)
		}
	}
	if !strings.Contains(doc, `alt="Hero"`) || !strings.Contains(doc, "Angebot") || !strings.Contains(doc, "Text") {
		t.Fatalf("message content was lost: %s", doc)
	}
}

func TestEmailDocumentKeepsRelativeRefsUnderADeclaredBase(t *testing.T) {
	body := `<base href="https://newsletter.example.test/2026/08/"><img src="hero.png" alt="Hero">`
	allowed := emailDocumentWithBlocklist(body, "", true, nil)
	if !strings.Contains(allowed, ` src="hero.png"`) {
		t.Fatalf("relative reference under a declared base was dropped: %s", allowed)
	}
	// Without images the same reference is a remote one, which is what this
	// mode refuses - live or not, it would cost a CSP violation.
	blocked := emailDocument(body, "", false)
	if strings.Contains(blocked, ` src="hero.png"`) {
		t.Fatalf("blocked document kept a reference to the sender's server: %s", blocked)
	}
	if !strings.Contains(blocked, `alt="Hero"`) {
		t.Fatalf("message content was lost: %s", blocked)
	}
}

func TestEmailDocumentIgnoresACommentedOutBase(t *testing.T) {
	// A base inside a comment sets nothing, so the reference beside it still
	// resolves against the app and must not be kept alive by it.
	body := `<!-- <base href="https://newsletter.example.test/"> --><img src="hero.png" alt="Hero">`
	doc := emailDocumentWithBlocklist(body, "", true, nil)
	if strings.Contains(doc, ` src="hero.png"`) {
		t.Fatalf("commented-out base kept a reference to the app origin alive: %s", doc)
	}
	if !strings.Contains(doc, `data-rolltop-unresolved-src="hero.png"`) {
		t.Fatalf("dropped reference was not recorded: %s", doc)
	}
}
