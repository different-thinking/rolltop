package googleauth

import (
	"context"
	"sync"
	"testing"
	"time"

	"rolltop/backend/store"
)

// countingConnectionStore records how often a connection row is read, which is
// the cost the cache exists to remove.
type countingConnectionStore struct {
	ConnectionStore
	mu    sync.Mutex
	reads int
}

func (s *countingConnectionStore) GoogleConnection(ctx context.Context, userID, connectionID int64) (store.GoogleConnection, error) {
	s.mu.Lock()
	s.reads++
	s.mu.Unlock()
	return s.ConnectionStore.GoogleConnection(ctx, userID, connectionID)
}

func (s *countingConnectionStore) readCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reads
}

// countingEnv rebuilds the manager over a counting store while keeping the
// fake Google endpoints and the controllable clock of the shared harness.
func countingEnv(t *testing.T) (*testEnv, *countingConnectionStore, store.GoogleConnection) {
	t.Helper()
	env := newTestEnv(t)
	connection := env.connect(t, env.userID)
	counting := &countingConnectionStore{ConnectionStore: env.db}
	env.manager = NewManager(env.google.config(), counting, testMasterKey)
	env.manager.SetNow(func() time.Time { return env.now })
	env.manager.Client().RetryDelay = func(int) time.Duration { return time.Millisecond }
	return env, counting, connection
}

// A sync pass opens several IMAP connections per mailbox and each one asks for
// a token. Reading and decrypting the same unchanged row every time adds
// hundreds of queries to the database that already reports lock contention.
func TestAccessTokenServesRepeatCallsFromMemory(t *testing.T) {
	env, counting, connection := countingEnv(t)
	ctx := context.Background()
	if _, err := env.manager.AccessToken(ctx, env.userID, connection.ID); err != nil {
		t.Fatalf("first token: %v", err)
	}
	if counting.readCount() != 1 {
		t.Fatalf("first call performed %d reads, want 1", counting.readCount())
	}
	for i := 0; i < 5; i++ {
		if _, err := env.manager.AccessToken(ctx, env.userID, connection.ID); err != nil {
			t.Fatalf("token %d: %v", i, err)
		}
	}
	if got := counting.readCount(); got != 1 {
		t.Fatalf("connection reads = %d, want the row read once and then cached", got)
	}
}

// The cache must never outlive the point at which the stored copy would have
// been refreshed, or a sync would authenticate with a dead credential.
func TestCachedTokenIsAbandonedInsideTheRefreshWindow(t *testing.T) {
	env, _, connection := countingEnv(t)
	ctx := context.Background()
	first, err := env.manager.AccessToken(ctx, env.userID, connection.ID)
	if err != nil {
		t.Fatalf("first token: %v", err)
	}
	env.now = env.now.Add(time.Hour - refreshSkew)
	second, err := env.manager.AccessToken(ctx, env.userID, connection.ID)
	if err != nil {
		t.Fatalf("token inside the refresh window: %v", err)
	}
	if first == second {
		t.Fatal("a token inside the refresh window was served from cache")
	}
}

// ForceRefresh exists because a server rejected the token the caller held. The
// cached copy is exactly that value, so serving it would turn the retry into a
// no-op and leave the account failing.
func TestForceRefreshBypassesAndReplacesTheCache(t *testing.T) {
	env, _, connection := countingEnv(t)
	ctx := context.Background()
	cached, err := env.manager.AccessToken(ctx, env.userID, connection.ID)
	if err != nil {
		t.Fatalf("first token: %v", err)
	}
	refreshed, err := env.manager.ForceRefresh(ctx, env.userID, connection.ID)
	if err != nil {
		t.Fatalf("force refresh: %v", err)
	}
	if refreshed == cached {
		t.Fatal("force refresh returned the token the caller had already been rejected for")
	}
	next, err := env.manager.AccessToken(ctx, env.userID, connection.ID)
	if err != nil {
		t.Fatalf("token after refresh: %v", err)
	}
	if next != refreshed {
		t.Fatalf("cache serves %q after a refresh produced %q", next, refreshed)
	}
}

// A disconnected account must stop working immediately. A token left in memory
// would keep IMAP and SMTP authenticating until it expired on its own.
func TestDisconnectDropsTheCachedToken(t *testing.T) {
	env, _, connection := countingEnv(t)
	ctx := context.Background()
	if _, err := env.manager.AccessToken(ctx, env.userID, connection.ID); err != nil {
		t.Fatalf("first token: %v", err)
	}
	if _, err := env.manager.Disconnect(ctx, env.userID, connection.ID); err != nil {
		t.Fatalf("disconnect: %v", err)
	}
	if _, err := env.manager.AccessToken(ctx, env.userID, connection.ID); err == nil {
		t.Fatal("a disconnected connection still produced an access token")
	}
}
