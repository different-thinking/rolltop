package web

import (
	"testing"
	"time"
)

func TestBackoffGateAllowsBurstThenBacksOff(t *testing.T) {
	base := time.UnixMilli(0)
	g := newBackoffGate(3, time.Second, time.Minute, time.Hour)

	// The whole burst passes without any wait.
	for i := 0; i < 3; i++ {
		if ok, _ := g.allow("k", base); !ok {
			t.Fatalf("attempt %d in the burst was blocked", i)
		}
		g.recordFailure("k", base)
	}

	// The next failure arms the first backoff window.
	if ok, _ := g.allow("k", base); !ok {
		t.Fatal("the attempt that spends the last burst slot was blocked")
	}
	g.recordFailure("k", base)
	ok, wait := g.allow("k", base)
	if ok {
		t.Fatal("the attempt past the burst was not blocked")
	}
	if wait != time.Second {
		t.Fatalf("first backoff = %v, want 1s", wait)
	}

	// Waiting it out lets exactly one attempt through, which then doubles the
	// window when it also fails.
	after := base.Add(time.Second)
	if ok, _ := g.allow("k", after); !ok {
		t.Fatal("attempt after the backoff window was still blocked")
	}
	g.recordFailure("k", after)
	if _, wait := g.allow("k", after); wait != 2*time.Second {
		t.Fatalf("second backoff = %v, want 2s", wait)
	}
}

func TestBackoffGateSuccessClearsKey(t *testing.T) {
	base := time.UnixMilli(0)
	g := newBackoffGate(1, time.Second, time.Minute, time.Hour)
	g.recordFailure("k", base)
	g.recordFailure("k", base) // past burst -> blocked
	if ok, _ := g.allow("k", base); ok {
		t.Fatal("key should be blocked before success")
	}
	g.recordSuccess("k")
	if ok, wait := g.allow("k", base); !ok || wait != 0 {
		t.Fatalf("success did not clear the key: ok=%v wait=%v", ok, wait)
	}
}

func TestBackoffGateCapsAtMax(t *testing.T) {
	base := time.UnixMilli(0)
	g := newBackoffGate(0, time.Second, 4*time.Second, time.Hour)
	for i := 0; i < 20; i++ {
		g.recordFailure("k", base)
	}
	if _, wait := g.allow("k", base); wait != 4*time.Second {
		t.Fatalf("backoff = %v, want the 4s cap", wait)
	}
}

func TestBackoffGateKeysAreIndependent(t *testing.T) {
	base := time.UnixMilli(0)
	g := newBackoffGate(0, time.Second, time.Minute, time.Hour)
	g.recordFailure("a", base)
	if ok, _ := g.allow("b", base); !ok {
		t.Fatal("failures on one key must not block a different key")
	}
}
