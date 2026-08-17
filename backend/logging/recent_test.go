package logging

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestRecentReturnsTheNewestLinesOldestFirst(t *testing.T) {
	var r recorder
	at := time.Date(2026, 8, 17, 10, 0, 0, 0, time.UTC)
	for i := range 5 {
		r.add(at, fmt.Sprintf("line %d\n", i))
	}
	got := messages(r.snapshot(3))
	want := []string{"line 2", "line 3", "line 4"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("snapshot = %v, want %v", got, want)
	}
	if len(r.snapshot(50)) != 5 {
		t.Fatalf("snapshot beyond the stored count = %d, want 5", len(r.snapshot(50)))
	}
}

// The ring must drop the oldest lines rather than the newest ones: an operator
// reads this page right after reproducing a failure, so the tail is the part
// that has to survive.
func TestRecentDropsTheOldestLinesWhenTheRingWraps(t *testing.T) {
	var r recorder
	at := time.Date(2026, 8, 17, 10, 0, 0, 0, time.UTC)
	for i := range recentCapacity + 10 {
		r.add(at, fmt.Sprintf("line %d", i))
	}
	got := messages(r.snapshot(recentCapacity + 100))
	if len(got) != recentCapacity {
		t.Fatalf("stored lines = %d, want %d", len(got), recentCapacity)
	}
	if got[0] != "line 10" {
		t.Fatalf("oldest kept line = %q, want %q", got[0], "line 10")
	}
	if last := got[len(got)-1]; last != fmt.Sprintf("line %d", recentCapacity+9) {
		t.Fatalf("newest line = %q", last)
	}
}

func TestRecorderWriteStripsTheStandardLoggerTimestamp(t *testing.T) {
	var r recorder
	if _, err := r.Write([]byte("2026/08/17 10:11:12 error server error GET /api/mail: boom\n")); err != nil {
		t.Fatal(err)
	}
	got := r.snapshot(1)
	if len(got) != 1 {
		t.Fatalf("records = %d", len(got))
	}
	if got[0].Message != "error server error GET /api/mail: boom" {
		t.Fatalf("message = %q", got[0].Message)
	}
	if got[0].Time.IsZero() {
		t.Fatal("record has no capture time")
	}
}

// A line that does not start with the standard prefix must keep its first
// characters: guessing a fixed-width timestamp away would silently eat content.
func TestRecorderWriteKeepsLinesWithoutAStandardPrefix(t *testing.T) {
	var r recorder
	r.add(time.Now().UTC(), "sqlite: database disk image is malformed")
	r.add(time.Now().UTC(), "   \n")
	got := messages(r.snapshot(10))
	if len(got) != 1 || got[0] != "sqlite: database disk image is malformed" {
		t.Fatalf("records = %v", got)
	}
}

func TestRecorderTruncatesAnOversizedLine(t *testing.T) {
	var r recorder
	r.add(time.Now().UTC(), strings.Repeat("x", maxRecentLineBytes+500))
	got := r.snapshot(1)
	if len(got) != 1 {
		t.Fatalf("records = %d", len(got))
	}
	if len(got[0].Message) != maxRecentLineBytes+3 {
		t.Fatalf("message length = %d, want %d", len(got[0].Message), maxRecentLineBytes+3)
	}
	if !strings.HasSuffix(got[0].Message, "...") {
		t.Fatal("truncated message does not say it was cut")
	}
}

func messages(records []Record) []string {
	out := make([]string, 0, len(records))
	for _, record := range records {
		out = append(out, record.Message)
	}
	return out
}
