// File overview: Regression tests for the two crafted-input DoS vectors in the
// MIME parser — quadratic CSS-brace scanning and unbounded multipart recursion.

package mailparse

import (
	"strings"
	"testing"
	"time"
)

// TestRemoveIndexedCSSRulesDeepNestingTerminates guards the O(n) brace matching.
// The old rescan-per-brace made this input take minutes; it must now finish
// near-instantly. Two shapes are checked: one just under the brace cap (which
// exercises the real matcher) and one far over it (which must take the bail-out
// path and still return fast). The assertion is generous (a whole second) so it
// flags only a return to quadratic behavior, not ordinary timing noise.
func TestRemoveIndexedCSSRulesDeepNestingTerminates(t *testing.T) {
	for _, pairs := range []int{maxIndexedCSSBraces - 1000, maxIndexedCSSBraces * 10} {
		input := strings.Repeat("{", pairs) + strings.Repeat("}", pairs)
		done := make(chan string, 1)
		start := time.Now()
		go func() { done <- removeIndexedCSSRules(input) }()
		select {
		case <-done:
		case <-time.After(1 * time.Second):
			t.Fatalf("removeIndexedCSSRules did not finish within 1s for %d nested braces (quadratic regression)", pairs)
		}
		if elapsed := time.Since(start); elapsed > 1*time.Second {
			t.Fatalf("removeIndexedCSSRules took %s for %d nested braces", elapsed, pairs)
		}
	}
}

// TestRemoveIndexedCSSRulesNoBracesIsUnchanged checks the fast path returns the
// input untouched (and, implicitly, without allocating a match map).
func TestRemoveIndexedCSSRulesNoBraces(t *testing.T) {
	in := "plain body with no css rules at all, just text and punctuation!?;"
	if got := removeIndexedCSSRules(in); got != in {
		t.Fatalf("no-brace body changed: %q", got)
	}
}

// TestMatchIndexedCSSBracesBalances checks the linear matcher pairs braces the
// same way balanced matching does, including unmatched braces on either side.
func TestMatchIndexedCSSBracesBalances(t *testing.T) {
	value := "a{b{c}d}e}{f"
	closes := matchIndexedCSSBraces(value)
	// Opens at indices 1 and 3; closes at 5 and 7. The trailing '}' at 9 has no
	// open, and the final '{' at 10 has no close.
	if got, ok := closes[1]; !ok || got != 7 {
		t.Fatalf("outer brace: got (%d,%v), want (7,true)", got, ok)
	}
	if got, ok := closes[3]; !ok || got != 5 {
		t.Fatalf("inner brace: got (%d,%v), want (5,true)", got, ok)
	}
	if _, ok := closes[10]; ok {
		t.Fatalf("unmatched trailing '{' at 10 should have no close")
	}
}

// TestParseDeeplyNestedMultipartBounded feeds a message nested far past the
// depth limit and requires Parse to return promptly without descending every
// level (the guard against multipart recursion inflating heap to an OOM).
func TestParseDeeplyNestedMultipartBounded(t *testing.T) {
	const levels = 5000
	var b strings.Builder
	b.WriteString("From: a@example.com\r\n")
	b.WriteString("Subject: nested\r\n")
	b.WriteString("MIME-Version: 1.0\r\n")
	for i := 0; i < levels; i++ {
		b.WriteString("Content-Type: multipart/mixed; boundary=\"b")
		b.WriteString(boundaryLabel(i))
		b.WriteString("\"\r\n\r\n")
		b.WriteString("--b")
		b.WriteString(boundaryLabel(i))
		b.WriteString("\r\n")
	}
	b.WriteString("Content-Type: text/plain\r\n\r\nhello\r\n")

	done := make(chan struct{}, 1)
	go func() {
		// We only care that it returns; a crafted message legitimately yields a
		// tolerable-EOF path, so an error here is acceptable, a hang is not.
		_, _ = Parse([]byte(b.String()))
		done <- struct{}{}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("Parse did not finish within 5s for %d nested multipart levels", levels)
	}
}

func boundaryLabel(i int) string {
	const digits = "0123456789"
	if i == 0 {
		return "0"
	}
	var out []byte
	for i > 0 {
		out = append([]byte{digits[i%10]}, out...)
		i /= 10
	}
	return string(out)
}
