package main

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestParseTargetAddressPicksTheTransportFromTheScheme(t *testing.T) {
	for _, tc := range []struct {
		raw       string
		transport string
		host      string
		port      int
		share     string
		root      string
	}{
		{"https://cloud.example.org/remote.php/dav/files/me/Recordings/", transportWebDAV, "cloud.example.org", 0, "", "remote.php/dav/files/me/Recordings"},
		{"http://cloud.example.org/dav", transportWebDAV, "cloud.example.org", 0, "", "dav"},
		// The two shapes ALL-INKL.COM's own instructions hand out.
		{"sftp://w0123456.kasserver.com/recordings", transportSFTP, "w0123456.kasserver.com", 22, "", "recordings"},
		{"smb://s0123456.kasserver.com/s0123456/recordings", transportSMB, "s0123456.kasserver.com", 445, "s0123456", "recordings"},
		// A share with nothing under it is the whole share.
		{"smb://s0123456.kasserver.com/s0123456", transportSMB, "s0123456.kasserver.com", 445, "s0123456", ""},
		{"sftp://host.example:2222/deep/path/", transportSFTP, "host.example", 2222, "", "deep/path"},
		// ssh:// and cifs:// are what people type; they mean the same two.
		{"ssh://host.example/x", transportSFTP, "host.example", 22, "", "x"},
		{"cifs://host.example/share", transportSMB, "host.example", 445, "share", ""},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			address, err := parseTargetAddress(tc.raw)
			if err != nil {
				t.Fatalf("parseTargetAddress(%q): %v", tc.raw, err)
			}
			if address.Transport != tc.transport || address.Host != tc.host ||
				address.Port != tc.port || address.Share != tc.share || address.Root != tc.root {
				t.Fatalf("address = %+v, want transport=%s host=%s port=%d share=%q root=%q",
					address, tc.transport, tc.host, tc.port, tc.share, tc.root)
			}
		})
	}
}

func TestParseTargetAddressRefusesWhatCannotBeArchivedTo(t *testing.T) {
	for _, tc := range []struct{ name, raw string }{
		{"empty", "   "},
		{"no scheme", "cloud.example.org/dav/"},
		{"file scheme", "file:///etc/passwd"},
		{"ftp is not one of the three", "ftp://host.example/x"},
		{"no host", "https:///dav/"},
		{"credentials in the address", "https://me:secret@cloud.example.org/dav/"},
		{"smb without a share", "smb://s0123456.kasserver.com/"},
		{"smb with no path at all", "smb://s0123456.kasserver.com"},
		{"port that is not a number", "sftp://host.example:ssh/x"},
		{"port out of range", "sftp://host.example:99999/x"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if address, err := parseTargetAddress(tc.raw); err == nil {
				t.Fatalf("parseTargetAddress(%q) accepted an address it should refuse: %+v", tc.raw, address)
			}
		})
	}
}

func TestParseTargetAddressMakesTheBaseACollection(t *testing.T) {
	address, err := parseTargetAddress("https://cloud.example.org/dav/files/me?v=1#top")
	if err != nil {
		t.Fatal(err)
	}
	if address.URL.Path != "/dav/files/me/" {
		t.Fatalf("path = %q, want a trailing slash so relative paths resolve under it", address.URL.Path)
	}
	if address.URL.RawQuery != "" || address.URL.Fragment != "" {
		t.Fatalf("query/fragment survived: %q %q", address.URL.RawQuery, address.URL.Fragment)
	}
}

// Whatever a caller asks for, it lands under the target's own root.
func TestJoinRootConfinesEveryPath(t *testing.T) {
	for _, tc := range []struct{ root, relative, want string }{
		{"", "a/b.m4a", "a/b.m4a"},
		{"recordings", "a/b.m4a", "recordings/a/b.m4a"},
		{"recordings", "", "recordings/"},
		{"/recordings/", "2026/05/x.m4a", "recordings/2026/05/x.m4a"},
		{"recordings", "../../etc/passwd", "recordings/etc/passwd"},
		{"recordings", "/../secrets", "recordings/secrets"},
	} {
		if got := joinRoot(tc.root, tc.relative); got != tc.want {
			t.Errorf("joinRoot(%q, %q) = %q, want %q", tc.root, tc.relative, got, tc.want)
		}
		if strings.Contains(joinRoot(tc.root, tc.relative), "..") {
			t.Errorf("joinRoot(%q, %q) kept a traversal", tc.root, tc.relative)
		}
	}
}

// The transports that store only bytes and a name need the type guessed, or a
// recording downloads as an opaque blob instead of playing in place.
func TestGuessContentTypeNamesTheFormatsThisArchiveHolds(t *testing.T) {
	// These are answered from the plugin's own table, so they are the same on
	// every machine rather than whatever /etc/mime.types happens to say.
	for name, want := range map[string]string{
		"memo.m4a":       "audio/mp4",
		"memo.opus":      "audio/opus",
		"memo.aac":       "audio/aac",
		"memo.amr":       "audio/amr",
		"memo.mp3":       "audio/mpeg",
		"MEMO.M4A":       "audio/mp4",
		"2026/05/x.flac": "audio/flac",
	} {
		if got := guessContentType(name); got != want {
			t.Errorf("guessContentType(%q) = %q, want %q", name, got, want)
		}
	}
	// Everything the browser will render in place has to be named, or the file
	// downloads as an opaque blob instead of playing.
	for name := range audioContentTypes {
		if !renderableInline(guessContentType("x" + name)) {
			t.Errorf("%s is not renderable inline, so a recording would not play", name)
		}
	}
	if got := guessContentType("no-extension"); got != "" {
		t.Errorf("guessContentType(no extension) = %q", got)
	}
}

func TestTargetTransportReportsAnUnreadableAddressAsUnknown(t *testing.T) {
	if got := targetTransport(target{BaseURL: "smb://host/share"}); got != transportSMB {
		t.Fatalf("transport = %q", got)
	}
	// An address stored by hand, or by an older build, must not be guessed at.
	if got := targetTransport(target{BaseURL: "not an address"}); got != "unknown" {
		t.Fatalf("transport = %q, want unknown", got)
	}
}

// The limit is a backstop for the case nothing checked up front: a WebDAV
// server answering chunked declares no length. Ending the stream quietly there
// would hand the reader a truncated recording under a 200 -- which looks
// exactly like a complete one.
func TestLimitedReadCloserFailsInsteadOfTruncating(t *testing.T) {
	body := io.NopCloser(bytes.NewReader(bytes.Repeat([]byte("x"), 40)))
	reader := newLimitedReadCloser(body, 16)
	got, err := io.ReadAll(reader)
	if !errors.Is(err, errTooLarge) {
		t.Fatalf("ReadAll error = %v, want errTooLarge", err)
	}
	// Not one byte past the limit reaches the caller.
	if len(got) != 16 {
		t.Fatalf("bytes handed on = %d, want 16", len(got))
	}
}

// A file that is exactly the limit is not too large, and reading it must end
// the ordinary way.
func TestLimitedReadCloserPassesAFileOfExactlyTheLimit(t *testing.T) {
	payload := bytes.Repeat([]byte("y"), 16)
	reader := newLimitedReadCloser(io.NopCloser(bytes.NewReader(payload)), 16)
	got, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("ReadAll = %v, want the file", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("bytes = %d, want %d", len(got), len(payload))
	}
}
