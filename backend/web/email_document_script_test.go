// File overview: Tests that message bodies reach the reader's iframe without script elements.

package web

import (
	"strings"
	"testing"
)

func TestEmailDocumentDropsScriptElements(t *testing.T) {
	body := `<p>Hello</p><script type="text/javascript">alert("x")</script><p>Bye</p>`
	doc := emailDocument(body, "", false)
	if strings.Contains(strings.ToLower(doc), "<script") {
		t.Fatalf("document kept a script element: %q", doc)
	}
	if strings.Contains(doc, `alert("x")`) {
		t.Fatalf("document kept script contents: %q", doc)
	}
	if !strings.Contains(doc, "<p>Hello</p>") || !strings.Contains(doc, "<p>Bye</p>") {
		t.Fatalf("document lost body text around the script: %q", doc)
	}
}

func TestEmailDocumentDropsUnclosedScriptElement(t *testing.T) {
	// A truncated or malformed body is exactly the case where a leftover tag
	// would still make the browser try, and log, a blocked execution. Whatever
	// follows it is the script's own source, which a browser reads to the end of
	// the file: leaving it behind would print it into the message as text and
	// reparse any markup in it as part of the body.
	doc := emailDocument(`<p>Hello</p><SCRIPT SRC="https://tracker.test/t.js">alert("x")<p>Trailing</p>`, "", true)
	if strings.Contains(strings.ToLower(doc), "<script") {
		t.Fatalf("document kept an unclosed script element: %q", doc)
	}
	if strings.Contains(doc, `alert("x")`) || strings.Contains(doc, "Trailing") {
		t.Fatalf("document kept what followed the unclosed script element: %q", doc)
	}
	if !strings.Contains(doc, "<p>Hello</p>") {
		t.Fatalf("document lost body text: %q", doc)
	}
}
