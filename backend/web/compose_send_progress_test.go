package web

import (
	"strings"
	"testing"
)

// The log line for a send the proxy cut off has to name the step it was stuck
// in; that is the whole point of tracking it.
func TestComposeSendProgressNamesTheStepItEndedIn(t *testing.T) {
	progress := newComposeSendProgress()
	progress.enter("prepare")
	progress.enter("wait_for_background_work")
	progress.enter("smtp")
	summary := progress.summary()
	if !strings.HasPrefix(summary, "step=smtp ") {
		t.Fatalf("summary = %q, want it to start with the current step", summary)
	}
	for _, step := range []string{"prepare:", "wait_for_background_work:", "smtp:"} {
		if !strings.Contains(summary, step) {
			t.Fatalf("summary = %q, missing %q", summary, step)
		}
	}

	progress.enter("done")
	if summary := progress.summary(); !strings.HasPrefix(summary, "step=done ") || strings.Contains(summary, "done:") {
		t.Fatalf("finished summary = %q", summary)
	}

	var untracked *composeSendProgress
	untracked.enter("smtp")
	if untracked.summary() != "" || untracked.elapsed() != 0 {
		t.Fatal("a nil progress must track nothing")
	}
}
