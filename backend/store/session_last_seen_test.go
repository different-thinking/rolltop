// File overview: Tests that resolving a session does not write on every request.

package store

import (
	"context"
	"path/filepath"
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
	db, err := Open(filepath.Join(t.TempDir(), "rolltop.db"))
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
