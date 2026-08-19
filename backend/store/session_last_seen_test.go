// File overview: Tests that resolving a session does not write on every request.

package store

import (
	"context"
	"sync"
	"testing"
	"time"
)

func sessionLastSeen(t *testing.T, db *Store, tokenHash string) int64 {
	t.Helper()
	var lastSeen int64
	if err := db.db.QueryRow(`SELECT last_seen_at FROM sessions WHERE token_hash = ?`, tokenHash).Scan(&lastSeen); err != nil {
		t.Fatal(err)
	}
	return lastSeen
}

func TestGetSessionUserWritesLastSeenOnlyWhenStale(t *testing.T) {
	ctx := context.Background()
	db, err := openTestStore(t)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	user, err := db.CreateUser(ctx, "seen@example.test", "Seen", "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateSession(ctx, user.ID, "seen-token", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	// A fresh session was just stamped, so the requests that follow it - which
	// is every authenticated request the browser makes - must not each put a
	// write on the system database.
	stale := nowUnix() - int64(sessionLastSeenInterval/time.Second) - 1
	if _, err := db.db.ExecContext(ctx, `UPDATE sessions SET last_seen_at = ? WHERE token_hash = ?`, stale, "seen-token"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.GetSessionUser(ctx, "seen-token"); err != nil {
		t.Fatal(err)
	}
	refreshed := sessionLastSeen(t, db, "seen-token")
	if refreshed == stale {
		t.Fatal("stale last_seen_at was not refreshed")
	}

	if _, _, err := db.GetSessionUser(ctx, "seen-token"); err != nil {
		t.Fatal(err)
	}
	if again := sessionLastSeen(t, db, "seen-token"); again != refreshed {
		t.Fatalf("fresh last_seen_at was rewritten: before=%d after=%d", refreshed, again)
	}
}

// A burst of requests all read the same stale timestamp, so the decision to
// write cannot rest on that read alone: without the predicate in the statement
// every one of them would take the writer's turn in sequence, which is the
// contention this throttle exists to remove. The writes cannot be counted from
// outside - nowUnix has second granularity, so a burst's writes all carry the
// same value - so what is asserted here is the invariant the predicate gives:
// the row advances to exactly one timestamp, no lookup regresses it, and the
// throttle still holds once the burst is over.
func TestConcurrentSessionLookupsKeepLastSeenConsistent(t *testing.T) {
	ctx := context.Background()
	db, err := openTestStore(t)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	user, err := db.CreateUser(ctx, "burst@example.test", "Burst", "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateSession(ctx, user.ID, "burst-token", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	stale := nowUnix() - int64(sessionLastSeenInterval/time.Second) - 1
	if _, err := db.db.ExecContext(ctx, `UPDATE sessions SET last_seen_at = ? WHERE token_hash = ?`, stale, "burst-token"); err != nil {
		t.Fatal(err)
	}

	const callers = 8
	var wg sync.WaitGroup
	seen := make(chan int64, callers)
	errs := make(chan error, callers)
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			session, _, err := db.GetSessionUser(ctx, "burst-token")
			if err != nil {
				errs <- err
				return
			}
			seen <- session.LastSeenAt.Unix()
		}()
	}
	wg.Wait()
	close(errs)
	close(seen)
	for err := range errs {
		t.Fatal(err)
	}

	refreshed := sessionLastSeen(t, db, "burst-token")
	if refreshed <= stale {
		t.Fatalf("concurrent lookups left last_seen_at stale: %d", refreshed)
	}
	for observed := range seen {
		// Every caller either read the row before the refresh or after it. A
		// third value would mean one of them wrote over a newer timestamp.
		if observed != stale && observed != refreshed {
			t.Fatalf("lookup reported last_seen_at %d, want %d or %d", observed, stale, refreshed)
		}
	}

	// The burst has just refreshed the row, so the lookups that follow it write
	// nothing at all.
	if _, _, err := db.GetSessionUser(ctx, "burst-token"); err != nil {
		t.Fatal(err)
	}
	if after := sessionLastSeen(t, db, "burst-token"); after != refreshed {
		t.Fatalf("lookup after the burst rewrote last_seen_at: before=%d after=%d", refreshed, after)
	}
}
