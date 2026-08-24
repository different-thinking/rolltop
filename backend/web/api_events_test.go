// File overview: What the chrome event stream does with producers that report
// far faster than a reader can use.

package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"rolltop/backend/store"
	"rolltop/backend/store/storetest"
)

// readEventStream runs the SSE handler until stop is called and returns what it
// wrote. The recorder is only read once the handler has returned.
func readEventStream(t *testing.T, server *Server, user store.User, drive func()) string {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequest(http.MethodGet, "/api/events", nil)
	req = req.WithContext(context.WithValue(ctx, userContextKey, currentUser{User: user}))
	res := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		server.apiEvents(res, req)
	}()
	drive()
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("event stream did not stop after its request was cancelled")
	}
	return res.Body.String()
}

func countChromeEvents(body string) int {
	return strings.Count(body, "event: chrome\n")
}

// Every signal rebuilds the whole chrome snapshot — the folder list with its
// counts, the categories, the archive mapping — once per connected tab. A sync
// mirroring a mailbox and a move working through a folder both signal per
// message, so a stream that answered each one turned one reader's open tabs
// into a query storm against the database the operation was competing with.
func TestEventStreamCoalescesABurstOfSignals(t *testing.T) {
	ctx := context.Background()
	db, err := storetest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	tenant := newScopeTestTenant(t, ctx, db, "events-burst@example.test")
	server := &Server{store: db, masterKey: []byte("12345678901234567890123456789012"), events: newEventHub()}

	const burst = 400
	body := readEventStream(t, server, tenant.user, func() {
		// Let the stream send its opening snapshot and subscribe before the burst.
		waitForChromeEvents(t, server, tenant.user.ID, 1)
		for i := 0; i < burst; i++ {
			server.events.Notify(tenant.user.ID)
		}
		time.Sleep(3 * syncEventMinInterval)
	})

	events := countChromeEvents(body)
	if events == 0 {
		t.Fatal("the stream sent nothing at all")
	}
	// The opening snapshot plus a bounded handful for the burst. The exact count
	// depends on scheduling; what must hold is that it is nothing like one per
	// signal.
	if events > 10 {
		t.Fatalf("chrome events = %d for %d signals, want them collapsed into a handful", events, burst)
	}
}

// Collapsing a burst must not delay a lone signal: a finished move announces
// itself once, and the reader is waiting for exactly that.
func TestEventStreamSendsALoneSignalPromptly(t *testing.T) {
	ctx := context.Background()
	db, err := storetest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	tenant := newScopeTestTenant(t, ctx, db, "events-single@example.test")
	server := &Server{store: db, masterKey: []byte("12345678901234567890123456789012"), events: newEventHub()}

	started := time.Now()
	var delivered time.Duration
	body := readEventStream(t, server, tenant.user, func() {
		waitForChromeEvents(t, server, tenant.user.ID, 1)
		server.events.Notify(tenant.user.ID)
		time.Sleep(4 * syncEventMinInterval)
		delivered = time.Since(started)
	})

	if countChromeEvents(body) < 2 {
		t.Fatalf("chrome events = %d, want the opening snapshot and the signal", countChromeEvents(body))
	}
	if delivered > 5*time.Second {
		t.Fatalf("a lone signal took %s to be answered", delivered)
	}
}

// waitForChromeEvents blocks until the hub has a subscriber, which is what says
// the stream is running and will see what is published next.
func waitForChromeEvents(t *testing.T, server *Server, userID int64, subscribers int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		server.events.mu.Lock()
		count := len(server.events.subscribers[userID])
		server.events.mu.Unlock()
		if count >= subscribers {
			// The handler subscribes before it writes its opening snapshot, so give
			// that write a moment to land before the burst starts.
			time.Sleep(50 * time.Millisecond)
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("event stream did not subscribe within the deadline")
}
