package main

import (
	"strings"
	"testing"
	"time"

	"rolltop/backend/mailparse"
)

func TestTargetMatchesContentTypeReadsPrefixesAndIgnoresParameters(t *testing.T) {
	family := target{ContentTypes: "audio/"}
	for _, value := range []string{"audio/mpeg", "AUDIO/MP4", `audio/ogg; codecs="opus"`, "audio/x-m4a"} {
		if !family.matchesContentType(value) {
			t.Errorf("audio/ did not take %q", value)
		}
	}
	for _, value := range []string{"application/pdf", "image/png", "text/plain"} {
		if family.matchesContentType(value) {
			t.Errorf("audio/ took %q", value)
		}
	}

	exact := target{ContentTypes: "audio/mpeg, application/pdf"}
	if !exact.matchesContentType("application/pdf") || !exact.matchesContentType("audio/mpeg") {
		t.Fatal("a comma-separated list did not take both of its entries")
	}
	if exact.matchesContentType("audio/mp4") {
		t.Fatal("an exact type took a sibling format")
	}

	// A blank filter is "everything", which is what an empty field asks for.
	if !(target{}).matchesContentType("application/octet-stream") {
		t.Fatal("an empty filter refused an attachment")
	}
}

func TestRenderRemotePathFillsTheTemplateAndStaysInside(t *testing.T) {
	item := upload{
		MessageID:   42,
		Filename:    "Voice 001.m4a",
		Subject:     "Notes / Monday",
		FromAddr:    `Me <me@example.test>`,
		MessageDate: time.Date(2026, 5, 17, 9, 30, 0, 0, time.UTC),
	}
	if got := renderRemotePath("{yyyy}/{mm}/{filename}", item); got != "2026/05/Voice 001.m4a" {
		t.Fatalf("path = %q", got)
	}
	if got := renderRemotePath("{date}/{from}/{basename}.{ext}", item); got != "2026-05-17/me@example.test/Voice 001.m4a" {
		t.Fatalf("path = %q", got)
	}
	// A slash inside a substituted value is not allowed to invent a folder.
	if got := renderRemotePath("{subject}/{filename}", item); got != "Notes - Monday/Voice 001.m4a" {
		t.Fatalf("path = %q, want the subject reduced to one segment", got)
	}
}

func TestRenderRemotePathCannotEscapeTheStore(t *testing.T) {
	item := upload{MessageID: 7, Filename: "../../etc/passwd", MessageDate: time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)}
	got := renderRemotePath("{yyyy}/{filename}", item)
	if strings.Contains(got, "..") || strings.HasPrefix(got, "/") {
		t.Fatalf("path = %q, want a relative, traversal-free path", got)
	}
	// The whole filename has to remain one segment: the separators in it were
	// replaced rather than honoured, so it cannot have added a level.
	if strings.Count(got, "/") != 1 || !strings.HasPrefix(got, "2026/") {
		t.Fatalf("path = %q, want exactly the one level the template asked for", got)
	}
}

func TestRenderRemotePathBacksOffToTheFilename(t *testing.T) {
	item := upload{MessageID: 9, AttachmentID: 3, Filename: "memo.m4a"}
	// A template whose every placeholder resolved to nothing would name a
	// folder rather than a file.
	if got := renderRemotePath("{subject}/", item); got != "memo.m4a" {
		t.Fatalf("path = %q, want the filename when the template resolved to a folder", got)
	}
	// An attachment with no filename still needs a name to be written under.
	nameless := upload{MessageID: 9, AttachmentID: 3}
	if got := renderRemotePath("{filename}", nameless); got != "attachment-3" {
		t.Fatalf("path = %q", got)
	}
}

func TestSafeSegmentStripsSeparatorsAndControls(t *testing.T) {
	if got := safeSegment("a/b\\c:d?e*f\"g<h>i|j"); strings.ContainsAny(got, `/\:?*"<>|`) {
		t.Fatalf("segment = %q, still holds a separator", got)
	}
	if got := safeSegment("with\ttab\nand\rreturn"); strings.ContainsAny(got, "\t\n\r") {
		t.Fatalf("segment = %q, still holds a control character", got)
	}
	if got := safeSegment("..."); got != "" {
		t.Fatalf("segment = %q, want dots-only reduced to nothing", got)
	}
	if got := safeSegment(strings.Repeat("x", 400)); len(got) > 120 {
		t.Fatalf("segment length = %d, want it bounded", len(got))
	}
}

func TestAppendPathSuffixKeepsTheExtension(t *testing.T) {
	if got := appendPathSuffix("2026/05/memo.m4a", "1f4a2b3c"); got != "2026/05/memo-1f4a2b3c.m4a" {
		t.Fatalf("path = %q", got)
	}
	if got := appendPathSuffix("memo", "abc"); got != "memo-abc" {
		t.Fatalf("path = %q", got)
	}
}

func TestMatchAttachmentPrefersNameAndSizeTogether(t *testing.T) {
	files := []mailparse.Attachment{
		{Filename: "memo.m4a", ContentType: "audio/mp4", Data: []byte("aaaa")},
		{Filename: "memo.m4a", ContentType: "audio/mp4", Data: []byte("bb")},
	}
	item := upload{Filename: "memo.m4a", ContentType: "audio/mp4", Size: 2}
	file, ok := matchAttachment(item, files)
	if !ok || len(file.Data) != 2 {
		t.Fatalf("matched %d bytes, want the part whose size the row records", len(file.Data))
	}

	// With no size recorded, the name alone is enough.
	byName, ok := matchAttachment(upload{Filename: "memo.m4a"}, files)
	if !ok || byName.Filename != "memo.m4a" {
		t.Fatal("a row with no size failed to match by name")
	}

	// A part with no filename at all is still reachable by type and size.
	unnamed := []mailparse.Attachment{{ContentType: "audio/mpeg", Data: []byte("xyz")}}
	byType, ok := matchAttachment(upload{ContentType: `audio/mpeg; name="x"`, Size: 3}, unnamed)
	if !ok || len(byType.Data) != 3 {
		t.Fatal("an unnamed part was not matched by type and size")
	}

	if _, ok := matchAttachment(upload{Filename: "gone.m4a", Size: 99}, files); ok {
		t.Fatal("a part that is not in the message was matched anyway")
	}
}

func TestRetryDelayClimbsToAnHourAndStops(t *testing.T) {
	if got := retryDelay(1); got != time.Minute {
		t.Fatalf("first retry = %v, want a minute", got)
	}
	if got := retryDelay(2); got != 2*time.Minute {
		t.Fatalf("second retry = %v", got)
	}
	if got := retryDelay(20); got != time.Hour {
		t.Fatalf("late retry = %v, want the ladder capped at an hour", got)
	}
	// The ladder must never go backwards, or a failing upload would speed up.
	previous := time.Duration(0)
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		delay := retryDelay(attempt)
		if delay < previous {
			t.Fatalf("retryDelay(%d) = %v, less than the previous %v", attempt, delay, previous)
		}
		previous = delay
	}
}

func TestSenderAddressReducesAHeaderToItsAddress(t *testing.T) {
	if got := senderAddress("Robert <me@example.test>"); got != "me@example.test" {
		t.Fatalf("address = %q", got)
	}
	if got := senderAddress("plain@example.test"); got != "plain@example.test" {
		t.Fatalf("address = %q", got)
	}
}

func TestTruncateErrorBoundsWhatIsStored(t *testing.T) {
	if got := truncateError(strings.Repeat("e", 900)); len(got) > 510 {
		t.Fatalf("stored error length = %d", len(got))
	}
	if got := truncateError("  short  "); got != "short" {
		t.Fatalf("error = %q", got)
	}
}
