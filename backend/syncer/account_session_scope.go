// File overview: Per-turn cache of authenticated IMAP connections, one per
// account, threaded through a sync turn's context.

package syncer

import (
	"context"
	"sync"
)

// AccountSessionScope caches one authenticated IMAP connection per account for
// the duration of one sync turn. Every operation in a turn used to pay its own
// TCP handshake, TLS negotiation and LOGIN — a single small-folder turn cost
// five to six logins, and a flag-push batch up to five hundred — which is
// exactly what mail hosts throttle. The scope saves only the login: each
// operation still selects its mailbox and proves its generation on the reused
// connection, so reuse costs nothing in safety.
//
// The scope lives in the turn's context. Fetchers that support it look it up
// with AccountSessionScopeFrom; a context without one keeps the historical
// connection-per-operation behavior, which is also the fallback whenever the
// account's slot is already busy (a nested call from inside a fetch handler).
type AccountSessionScope struct {
	mu     sync.Mutex
	closed bool
	slots  map[int64]*accountSessionSlot
}

type accountSessionSlot struct {
	busy    bool
	session any
	close   func()
}

type accountSessionScopeKey struct{}

// WithAccountSessionScope returns a context carrying a fresh session scope and
// the function that closes every idle cached connection. A context that
// already carries a scope is returned unchanged with a no-op close, so nested
// turns share the outer scope instead of shadowing it.
func WithAccountSessionScope(ctx context.Context) (context.Context, func()) {
	if ctx == nil {
		ctx = context.Background()
	}
	if AccountSessionScopeFrom(ctx) != nil {
		return ctx, func() {}
	}
	scope := &AccountSessionScope{slots: map[int64]*accountSessionSlot{}}
	return context.WithValue(ctx, accountSessionScopeKey{}, scope), scope.Close
}

// AccountSessionScopeFrom returns the turn's session scope, or nil when the
// context does not carry one.
func AccountSessionScopeFrom(ctx context.Context) *AccountSessionScope {
	if ctx == nil {
		return nil
	}
	scope, _ := ctx.Value(accountSessionScopeKey{}).(*AccountSessionScope)
	return scope
}

// Acquire claims the account's connection slot. It returns the cached idle
// session when one exists, and reports whether the slot was claimed at all:
// slot=false means another operation currently holds it (or the scope is
// closed) and the caller must fall back to a connection of its own without
// calling Release or Discard.
func (s *AccountSessionScope) Acquire(accountID int64) (session any, cached bool, slot bool) {
	if s == nil {
		return nil, false, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, false, false
	}
	entry := s.slots[accountID]
	if entry == nil {
		entry = &accountSessionSlot{}
		s.slots[accountID] = entry
	}
	if entry.busy {
		return nil, false, false
	}
	entry.busy = true
	session = entry.session
	cached = session != nil
	entry.session = nil
	entry.close = nil
	return session, cached, true
}

// Release stores a healthy session back into the account's slot for the next
// operation and frees the slot. closeFn must close the session; it is invoked
// when the scope is already closed or when Close runs later.
func (s *AccountSessionScope) Release(accountID int64, session any, closeFn func()) {
	if s == nil {
		if closeFn != nil {
			closeFn()
		}
		return
	}
	s.mu.Lock()
	entry := s.slots[accountID]
	if entry != nil {
		entry.busy = false
	}
	if s.closed || entry == nil || session == nil {
		s.mu.Unlock()
		if closeFn != nil {
			closeFn()
		}
		return
	}
	entry.session = session
	entry.close = closeFn
	s.mu.Unlock()
}

// Discard frees the account's slot without caching anything. It is for a
// connection the operation already terminated or that failed.
func (s *AccountSessionScope) Discard(accountID int64) {
	if s == nil {
		return
	}
	s.mu.Lock()
	if entry := s.slots[accountID]; entry != nil {
		entry.busy = false
	}
	s.mu.Unlock()
}

// Close terminates every idle cached connection. Slots that are still busy are
// marked closed so their release terminates instead of re-caching.
func (s *AccountSessionScope) Close() {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	var closers []func()
	for _, entry := range s.slots {
		if entry.session != nil && entry.close != nil {
			closers = append(closers, entry.close)
		}
		entry.session = nil
		entry.close = nil
	}
	s.mu.Unlock()
	for _, closeFn := range closers {
		closeFn()
	}
}
