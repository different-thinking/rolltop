// File overview: Semantics of the per-turn account session cache.

package syncer

import (
	"context"
	"testing"
)

type scopeTestSession struct {
	closed int
}

func TestAccountSessionScopeCachesAndReusesOneSessionPerAccount(t *testing.T) {
	ctx, endSessions := WithAccountSessionScope(context.Background())
	defer endSessions()
	scope := AccountSessionScopeFrom(ctx)
	if scope == nil {
		t.Fatal("context does not carry the session scope")
	}

	session, cached, slot := scope.Acquire(1)
	if session != nil || cached || !slot {
		t.Fatalf("first acquire = (%v, %t, %t), want empty claimed slot", session, cached, slot)
	}
	first := &scopeTestSession{}
	scope.Release(1, first, func() { first.closed++ })

	session, cached, slot = scope.Acquire(1)
	if session != first || !cached || !slot {
		t.Fatalf("second acquire = (%v, %t, %t), want the cached session", session, cached, slot)
	}
	// While account 1's slot is busy, a nested acquire must be refused so the
	// caller falls back to a connection of its own.
	if _, _, nested := scope.Acquire(1); nested {
		t.Fatal("nested acquire claimed a busy slot")
	}
	// Another account's slot is independent.
	if _, cached, slot := scope.Acquire(2); cached || !slot {
		t.Fatalf("account 2 acquire = (cached=%t, slot=%t), want empty claimed slot", cached, slot)
	}
	scope.Discard(2)
	scope.Release(1, first, func() { first.closed++ })
	if first.closed != 0 {
		t.Fatalf("session closed %d times while cached", first.closed)
	}
}

func TestAccountSessionScopeCloseTerminatesIdleSessions(t *testing.T) {
	ctx, endSessions := WithAccountSessionScope(context.Background())
	scope := AccountSessionScopeFrom(ctx)
	idle := &scopeTestSession{}
	if _, _, slot := scope.Acquire(7); !slot {
		t.Fatal("could not claim slot")
	}
	scope.Release(7, idle, func() { idle.closed++ })

	// A slot still busy at close time must terminate on release instead of
	// re-caching into a closed scope.
	late := &scopeTestSession{}
	if _, _, slot := scope.Acquire(8); !slot {
		t.Fatal("could not claim second slot")
	}

	endSessions()
	if idle.closed != 1 {
		t.Fatalf("idle session closed %d times, want 1", idle.closed)
	}
	scope.Release(8, late, func() { late.closed++ })
	if late.closed != 1 {
		t.Fatalf("late-released session closed %d times, want 1", late.closed)
	}
	// A closed scope refuses new slots entirely.
	if _, _, slot := scope.Acquire(9); slot {
		t.Fatal("closed scope handed out a slot")
	}
}

func TestWithAccountSessionScopeReusesAnOuterScope(t *testing.T) {
	ctx, endOuter := WithAccountSessionScope(context.Background())
	defer endOuter()
	outer := AccountSessionScopeFrom(ctx)
	nestedCtx, endNested := WithAccountSessionScope(ctx)
	defer endNested()
	if AccountSessionScopeFrom(nestedCtx) != outer {
		t.Fatal("nested scope shadowed the outer one")
	}
	// The nested close must not close the outer scope's sessions.
	idle := &scopeTestSession{}
	if _, _, slot := outer.Acquire(1); !slot {
		t.Fatal("could not claim slot")
	}
	outer.Release(1, idle, func() { idle.closed++ })
	endNested()
	if idle.closed != 0 {
		t.Fatalf("nested close terminated the outer scope's session %d times", idle.closed)
	}
}
