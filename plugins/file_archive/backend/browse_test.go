package main

import (
	"mime"
	"strings"
	"testing"
)

func TestContentDispositionKeepsAnASCIINamePlain(t *testing.T) {
	got := contentDisposition(false, "memo.m4a", "audio/mp4")
	if got != `attachment; filename="memo.m4a"` {
		t.Fatalf("disposition = %q", got)
	}
}

// The names this archive is full of are not ASCII, and the plain parameter is
// ISO-8859-1 -- so the real name has to travel in filename*.
func TestContentDispositionCarriesANonASCIINameInTheExtendedParameter(t *testing.T) {
	for _, name := range []string{"Sprachmemo Ü.m4a", "メモ.m4a", "réunion 12:00.m4a"} {
		header := contentDisposition(false, name, "audio/mp4")
		disposition, params, err := mime.ParseMediaType(header)
		if err != nil {
			t.Fatalf("ParseMediaType(%q): %v", header, err)
		}
		if disposition != "attachment" {
			t.Errorf("disposition = %q", disposition)
		}
		// mime.ParseMediaType decodes filename* into "filename", which is the
		// same reading a browser does.
		if params["filename"] != name {
			t.Errorf("filename for %q = %q, want the name back unchanged", name, params["filename"])
		}
	}
}

func TestContentDispositionFallbackStaysInsideTheQuotedString(t *testing.T) {
	header := contentDisposition(false, `we"ird\name.m4a`, "audio/mp4")
	if _, _, err := mime.ParseMediaType(header); err != nil {
		t.Fatalf("a quote in the name broke the header %q: %v", header, err)
	}
	fallback := header[strings.Index(header, `filename="`)+len(`filename="`):]
	fallback = fallback[:strings.IndexByte(fallback, '"')]
	if strings.ContainsAny(fallback, `"\`) {
		t.Fatalf("fallback = %q, want the quote and backslash replaced", fallback)
	}
}

func TestContentDispositionOnlyRendersMediaInline(t *testing.T) {
	for _, contentType := range []string{"audio/mp4", "video/mp4", "image/png"} {
		if got := contentDisposition(true, "x", contentType); !strings.HasPrefix(got, "inline;") {
			t.Errorf("%s = %q, want inline", contentType, got)
		}
	}
	// A document from a server this Rolltop does not control must not render
	// on this origin, however it is asked for.
	for _, contentType := range []string{"text/html", "image/svg+xml", "application/pdf", ""} {
		if got := contentDisposition(true, "x", contentType); !strings.HasPrefix(got, "attachment;") {
			t.Errorf("%s = %q, want attachment", contentType, got)
		}
	}
}

func TestParentPathWalksUpAndStopsAtTheRoot(t *testing.T) {
	for raw, want := range map[string]string{
		"":              "",
		"2026/":         "",
		"2026/05/":      "2026/",
		"2026/05/x.m4a": "2026/05/",
		"/2026/05/":     "2026/",
		"../../etc/x":   "etc/",
	} {
		if got := parentPath(raw); got != want {
			t.Errorf("parentPath(%q) = %q, want %q", raw, got, want)
		}
	}
}
