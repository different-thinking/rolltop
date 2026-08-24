// File overview: Session-scope connection reuse and fetch-stall watchdog tests.

package imapclient

import (
	"bufio"
	"context"
	"errors"
	"io"
	"log"
	"net"
	"testing"
	"time"

	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/client"

	"rolltop/backend/store"
	"rolltop/backend/syncer"
)

// newPipeIMAPClient builds a client against an in-memory server that answers
// the greeting and then swallows everything.
func newPipeIMAPClient(t *testing.T) *client.Client {
	t.Helper()
	clientConn, serverConn := net.Pipe()
	go func() {
		defer serverConn.Close()
		if _, err := io.WriteString(serverConn, "* OK [CAPABILITY IMAP4rev1] test server ready\r\n"); err != nil {
			return
		}
		_, _ = io.Copy(io.Discard, serverConn)
	}()
	c, err := client.New(clientConn)
	if err != nil {
		t.Fatal(err)
	}
	c.ErrorLog = log.New(io.Discard, "", 0)
	t.Cleanup(func() { _ = c.Terminate() })
	return c
}

func TestSessionClientReusesCachedConnectionWithoutLoggingIn(t *testing.T) {
	c := newPipeIMAPClient(t)
	c.SetState(imap.AuthenticatedState, nil)

	f := &Fetcher{Timeout: 200 * time.Millisecond}
	// A closed port so a login attempt fails immediately: the reuse assertions
	// below hold exactly because no login is ever tried.
	account := store.MailAccount{ID: 42, Host: "127.0.0.1", Port: 1, Username: "u", EncryptedPassword: "x"}

	ctx, endSessions := syncer.WithAccountSessionScope(context.Background())
	defer endSessions()
	scope := syncer.AccountSessionScopeFrom(ctx)
	if _, _, slot := scope.Acquire(account.ID); !slot {
		t.Fatal("could not seed the account slot")
	}
	scope.Release(account.ID, c, func() { _ = terminateClient(c) })

	got, release, err := f.sessionClient(ctx, account)
	if err != nil {
		t.Fatalf("sessionClient with a cached connection attempted a login: %v", err)
	}
	if got != c {
		t.Fatal("sessionClient did not hand back the cached connection")
	}
	release()

	// A healthy release re-caches: the next operation reuses it again.
	got, release, err = f.sessionClient(ctx, account)
	if err != nil || got != c {
		t.Fatalf("second acquire = (%v, err=%v), want the cached connection", got, err)
	}
	release()

	// While the slot is busy, a nested operation must fall back to its own
	// connection instead of sharing the sequential-only client.
	inUse, releaseInUse, err := f.sessionClient(ctx, account)
	if err != nil || inUse != c {
		t.Fatalf("third acquire = (%v, err=%v)", inUse, err)
	}
	if _, _, nestedErr := f.sessionClient(ctx, account); nestedErr == nil {
		t.Fatal("nested sessionClient reused a busy slot instead of dialing for itself")
	}
	releaseInUse()

	// A connection parked in the logout state is stale: the next acquire must
	// not hand it out again.
	_ = terminateClient(c)
	if _, _, err := f.sessionClient(ctx, account); err == nil {
		t.Fatal("sessionClient reused a terminated connection")
	}
}

func TestSessionClientWithoutScopeKeepsConnectionPerOperation(t *testing.T) {
	f := &Fetcher{Timeout: 200 * time.Millisecond}
	account := store.MailAccount{ID: 7, Host: "127.0.0.1", Port: 1, Username: "u", EncryptedPassword: "x"}
	if _, _, err := f.sessionClient(context.Background(), account); err == nil {
		t.Fatal("scopeless sessionClient did not dial")
	}
}

func TestGuardedUIDFetchToleratesSlowButFlowingConnections(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		defer serverConn.Close()
		if _, err := io.WriteString(serverConn, "* OK [CAPABILITY IMAP4rev1] test server ready\r\n"); err != nil {
			return
		}
		reader := bufio.NewReader(serverConn)
		if _, err := reader.ReadString('\n'); err != nil {
			return
		}
		_, _ = io.Copy(io.Discard, reader)
	}()
	c, err := client.New(clientConn)
	if err != nil {
		t.Fatal(err)
	}
	c.ErrorLog = log.New(io.Discard, "", 0)
	defer c.Terminate()
	c.SetState(imap.SelectedState, &imap.MailboxStatus{Name: "INBOX", Messages: 1, UidNext: 2, UidValidity: 1})
	c.Timeout = 400 * time.Millisecond

	// Register byte-level activity for this client and keep touching it well
	// past the idle window: the watchdog must treat flowing data as progress
	// where the old flat deadline killed the command regardless.
	activity := &connActivity{}
	connActivityRegistry.Store(c, activity)
	touchingUntil := time.Now().Add(1 * time.Second)
	go func() {
		for time.Now().Before(touchingUntil) {
			activity.touch()
			time.Sleep(50 * time.Millisecond)
		}
	}()

	seqset := new(imap.SeqSet)
	seqset.AddNum(1)
	messages := make(chan *imap.Message, 1)
	started := time.Now()
	err = guardedUIDFetch(context.Background(), c, seqset, []imap.FetchItem{imap.FetchUid, rawBodySection().FetchItem()}, messages)
	elapsed := time.Since(started)
	if !errors.Is(err, errFetchStalled) {
		t.Fatalf("guardedUIDFetch() error = %v, want the fetch-stalled verdict", err)
	}
	if elapsed < 900*time.Millisecond {
		t.Fatalf("guardedUIDFetch() gave up after %s while data was still flowing", elapsed)
	}
	if _, ok := <-messages; ok {
		t.Fatal("UID FETCH message channel remained open")
	}
	<-serverDone
}
