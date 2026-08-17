package store

import (
	"strings"
	"testing"
)

func TestSummarizeProblemCollapsesAMultiLineReport(t *testing.T) {
	report := "*** in database main ***\nPage 13811: never used\nTree 12 page 13733: btreeInitPage() returns error code 11"
	got := summarizeProblem(report)
	if !strings.HasPrefix(got, "*** in database main ***") {
		t.Fatalf("summary lost the leading line: %q", got)
	}
	if strings.Contains(got, "\n") {
		t.Fatalf("summary is still multi-line: %q", got)
	}
	if !strings.Contains(got, "+2 more lines") {
		t.Fatalf("summary does not say how much it dropped: %q", got)
	}
	// A single-line problem must pass through untouched, or every ordinary
	// quick_check row would grow a pointless suffix.
	if got := summarizeProblem("wrong # of entries in index idx_messages_user_starred"); got != "wrong # of entries in index idx_messages_user_starred" {
		t.Fatalf("single-line problem was rewritten: %q", got)
	}
}
