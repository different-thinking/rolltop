package httpfile

import (
	"mime"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDispositionKeepsAnASCIINamePlain(t *testing.T) {
	if got := Disposition(false, "memo.m4a", "attachment"); got != `attachment; filename="memo.m4a"` {
		t.Fatalf("disposition = %q", got)
	}
	if got := Disposition(true, "photo.png", "attachment"); got != `inline; filename="photo.png"` {
		t.Fatalf("disposition = %q", got)
	}
}

// The bug this package exists for: the plain parameter is ISO-8859-1, so a
// UTF-8 name written into it reaches the browser as mojibake.
func TestDispositionCarriesANonASCIINameInTheExtendedParameter(t *testing.T) {
	for _, name := range []string{
		"Sprachmemo Ü.m4a",
		"メモ.m4a",
		"réunion 12:00.m4a",
		"Отчёт.pdf",
		"emoji 🎧.m4a",
	} {
		header := Disposition(false, name, "attachment")
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

// The header has to survive being written and read back by net/http, which is
// the thing an invalid one actually breaks.
func TestDispositionSurvivesTheHTTPRoundTrip(t *testing.T) {
	const name = "Sprachmemo Ü.m4a"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Disposition", Disposition(false, name, "attachment"))
		_, _ = w.Write([]byte("bytes"))
	}))
	defer server.Close()

	res, err := http.Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	_, params, err := mime.ParseMediaType(res.Header.Get("Content-Disposition"))
	if err != nil {
		t.Fatalf("the served header did not parse: %v", err)
	}
	if params["filename"] != name {
		t.Fatalf("filename = %q, want %q", params["filename"], name)
	}
}

func TestFilenameFallbackStaysInsideTheQuotedString(t *testing.T) {
	header := Disposition(false, `we"ird\name.m4a`, "attachment")
	if _, _, err := mime.ParseMediaType(header); err != nil {
		t.Fatalf("a quote in the name broke the header %q: %v", header, err)
	}
	fallback := header[strings.Index(header, `filename="`)+len(`filename="`):]
	fallback = fallback[:strings.IndexByte(fallback, '"')]
	if strings.ContainsAny(fallback, `"\`) {
		t.Fatalf("fallback = %q, want the quote and backslash replaced", fallback)
	}
}

func TestFilenameUsesTheFallbackWhenThereIsNoUsableName(t *testing.T) {
	for _, name := range []string{"", "   ", "/", ".", ".."} {
		if got := Filename(name, "attachment"); got != `filename="attachment"` {
			t.Errorf("Filename(%q) = %q, want the caller's fallback", name, got)
		}
	}
	// A name that is entirely non-ASCII reduces to underscores in the plain
	// parameter, which says nothing -- the fallback says more, and the real
	// name still travels in filename*.
	header := Filename("メモ", "attachment")
	if !strings.HasPrefix(header, `filename="attachment"`) {
		t.Fatalf("header = %q, want the fallback in the plain parameter", header)
	}
	_, params, err := mime.ParseMediaType("attachment; " + header)
	if err != nil || params["filename"] != "メモ" {
		t.Fatalf("params = %v, err = %v; the real name must still be carried", params, err)
	}
	if got := Filename("", ""); got != `filename="download"` {
		t.Fatalf("Filename with no fallback = %q", got)
	}
}

// A filename parameter names a file, never a path: a name arriving with
// directories in it is reduced to its last segment.
func TestFilenameKeepsOnlyTheBaseName(t *testing.T) {
	if got := Filename("../../etc/passwd", "attachment"); got != `filename="passwd"` {
		t.Fatalf("Filename = %q", got)
	}
	if got := Filename("2026/05/memo.m4a", "attachment"); got != `filename="memo.m4a"` {
		t.Fatalf("Filename = %q", got)
	}
	// A trailing slash still names its last segment, which is a usable name.
	if got := Filename("some/dir/", "attachment"); got != `filename="dir"` {
		t.Fatalf("Filename = %q", got)
	}
}

// Control characters would make the header invalid and net/http refuse to write
// it, taking the whole response with it.
func TestFilenameStripsControlCharacters(t *testing.T) {
	header := Disposition(false, "memo\r\nX-Evil: 1.m4a", "attachment")
	if strings.ContainsAny(header, "\r\n") {
		t.Fatalf("header = %q, want no control characters", header)
	}
	if _, _, err := mime.ParseMediaType(header); err != nil {
		t.Fatalf("header did not parse: %v", err)
	}
}
