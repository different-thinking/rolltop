// File overview: Pins the security invariants email_document.go's sanitizer is
// coupled to — the per-document CSP and the removal of things that could only
// run without it — so a change that quietly weakens either fails CI. See the
// "Security invariant" comment on emailDocumentWithInlineAttachments.

package web

import (
	"strings"
	"testing"
)

// The document must always carry a CSP that forbids script and defaults every
// other resource to none, in both remote-image modes. This is the layer the
// regex sanitizer leans on; if it is ever loosened to permit a script source
// the sanitizer alone is not a sufficient XSS defense.
func TestEmailDocumentAlwaysCarriesRestrictiveCSP(t *testing.T) {
	for _, allowRemoteImages := range []bool{false, true} {
		doc := emailDocument(`<p>Hi</p>`, "", allowRemoteImages)
		lower := strings.ToLower(doc)
		if !strings.Contains(lower, `http-equiv="content-security-policy"`) {
			t.Fatalf("allowRemoteImages=%v: document is missing its CSP meta: %q", allowRemoteImages, doc)
		}
		if !strings.Contains(lower, "default-src 'none'") {
			t.Fatalf("allowRemoteImages=%v: CSP no longer defaults to none: %q", allowRemoteImages, doc)
		}
		// No script source of any kind may appear: default-src 'none' forbids
		// script, and nothing here should reintroduce one.
		if strings.Contains(lower, "script-src") {
			t.Fatalf("allowRemoteImages=%v: CSP names a script-src, which must never happen: %q", allowRemoteImages, doc)
		}
		if strings.Contains(lower, "unsafe-eval") {
			t.Fatalf("allowRemoteImages=%v: CSP allows eval: %q", allowRemoteImages, doc)
		}
	}
}

// An inline event handler cannot run in the no-allow-scripts sandbox, but it is
// stripped anyway so a blocked one does not log a sandbox violation per firing.
func TestEmailDocumentDropsInlineEventHandlers(t *testing.T) {
	doc := emailDocument(`<p>Hi</p><img src="x" onerror="alert(1)"><a onclick="steal()">c</a>`, "", true)
	lower := strings.ToLower(doc)
	if strings.Contains(lower, "onerror") || strings.Contains(lower, "onclick") {
		t.Fatalf("document kept an inline event handler: %q", doc)
	}
	if strings.Contains(doc, "alert(1)") || strings.Contains(doc, "steal()") {
		t.Fatalf("document kept handler code: %q", doc)
	}
}

// A javascript: URL is neutralized the same way, including when it is obfuscated
// with leading whitespace or an HTML entity, the way a browser would still run.
func TestEmailDocumentDropsJavascriptURLs(t *testing.T) {
	doc := emailDocument(`<a href="javascript:alert(1)">a</a><a href="  java&#115;cript:steal()">b</a>`, "", false)
	lower := strings.ToLower(doc)
	if strings.Contains(lower, "javascript:") || strings.Contains(lower, "java&#115;cript") {
		t.Fatalf("document kept a javascript: URL: %q", doc)
	}
	if strings.Contains(doc, "alert(1)") || strings.Contains(doc, "steal()") {
		t.Fatalf("document kept script-URL code: %q", doc)
	}
}
