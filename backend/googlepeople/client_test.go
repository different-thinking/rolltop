// File overview: How the People client classifies Google's failures. The one
// that matters is FAILED_PRECONDITION: Google returns it for a stale sync token
// and for a stale etag with nothing in the status to tell them apart, so each
// entry point has to decide which one it asked for.

package googlepeople

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func clientAgainst(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	client := NewClient()
	client.BaseURL = server.URL
	client.RetryDelay = func(int) time.Duration { return time.Millisecond }
	return client
}

// A read's only precondition is the sync token. Reporting it as anything else
// would strand the connection on a cursor it can never use again.
func TestListConnectionsReportsAPreconditionAsAnExpiredSyncToken(t *testing.T) {
	client := clientAgainst(t, func(w http.ResponseWriter, r *http.Request) {
		writeGoogleError(w, http.StatusBadRequest, "FAILED_PRECONDITION")
	})
	_, err := client.ListConnections(context.Background(), "token", ConnectionsRequest{SyncToken: "stale"})
	if !errors.Is(err, ErrSyncTokenExpired) {
		t.Fatalf("error = %v, want ErrSyncTokenExpired", err)
	}
}

// A write's only precondition is the etag, and mistaking it for an expired
// cursor would send the caller off to resync instead of resolving the conflict.
func TestUpdateContactReportsAPreconditionAsAConflict(t *testing.T) {
	client := clientAgainst(t, func(w http.ResponseWriter, r *http.Request) {
		writeGoogleError(w, http.StatusBadRequest, "FAILED_PRECONDITION")
	})
	_, err := client.UpdateContact(context.Background(), "token",
		Person{ResourceName: "people/c1", ETag: "etag-stale"})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("error = %v, want ErrConflict", err)
	}
}

// An update without an etag would overwrite whatever Google currently holds.
// Google rejects it, but catching it here keeps a bug from ever reaching a real
// address book.
func TestUpdateContactRefusesToWriteWithoutAnETag(t *testing.T) {
	client := clientAgainst(t, func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("an update without an etag reached Google")
	})
	if _, err := client.UpdateContact(context.Background(), "token", Person{ResourceName: "people/c1"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("error = %v, want the write refused", err)
	}
}

// Rate limits and server faults are worth another attempt; a rejected token is
// not, and retrying it four times would quadruple every failing sync.
func TestRateLimitsAreRetriedAndRejectedTokensAreNot(t *testing.T) {
	attempts := 0
	client := clientAgainst(t, func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts == 1 {
			writeGoogleError(w, http.StatusTooManyRequests, "RESOURCE_EXHAUSTED")
			return
		}
		_ = json.NewEncoder(w).Encode(ConnectionsPage{NextSyncToken: "token-1"})
	})
	page, err := client.ListConnections(context.Background(), "token", ConnectionsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if page.NextSyncToken != "token-1" || attempts != 2 {
		t.Fatalf("attempts = %d, page = %+v, want one retry after the rate limit", attempts, page)
	}

	attempts = 0
	unauthorized := clientAgainst(t, func(w http.ResponseWriter, r *http.Request) {
		attempts++
		writeGoogleError(w, http.StatusUnauthorized, "UNAUTHENTICATED")
	})
	if _, err := unauthorized.ListConnections(context.Background(), "token", ConnectionsRequest{}); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("error = %v, want ErrUnauthorized", err)
	}
	if attempts != 1 {
		t.Fatalf("attempts = %d, want the rejected token tried once", attempts)
	}
}

// A person Google no longer has is the end state a delete is asking for.
func TestDeleteContactTreatsAMissingPersonAsDone(t *testing.T) {
	client := clientAgainst(t, func(w http.ResponseWriter, r *http.Request) {
		writeGoogleError(w, http.StatusNotFound, "NOT_FOUND")
	})
	if err := client.DeleteContact(context.Background(), "token", "people/c1"); err != nil {
		t.Fatalf("error = %v, want a missing person treated as deleted", err)
	}
}

// A photo past the limit has to be refused, not cut off. Storing the first two
// megabytes of a larger image gives the contact a corrupt icon that looks like
// a successful import.
func TestFetchPhotoRejectsAnOversizedImageInsteadOfTruncatingIt(t *testing.T) {
	// Photos are only fetched over https, so the fake host has to serve TLS.
	photoHost := func(size int) *Client {
		server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write(make([]byte, size))
		}))
		t.Cleanup(server.Close)
		client := NewClient()
		client.HTTPClient = server.Client()
		client.BaseURL = server.URL
		return client
	}
	oversized := photoHost(maxPhotoBytes + 64)
	if _, err := oversized.FetchPhoto(context.Background(), oversized.BaseURL+"/photo.jpg"); err == nil {
		t.Fatal("an oversized photo was accepted")
	}
	within := photoHost(1024)
	data, err := within.FetchPhoto(context.Background(), within.BaseURL+"/photo.jpg")
	if err != nil || len(data) != 1024 {
		t.Fatalf("photo of 1024 bytes = %d bytes, err = %v", len(data), err)
	}
}

// The access token is the one thing that must never travel to a host that did
// not need it, and a contact photo is served from a public static host.
func TestFetchPhotoRefusesAnythingButHTTPS(t *testing.T) {
	client := NewClient()
	if _, err := client.FetchPhoto(context.Background(), "http://example.test/photo.jpg"); err == nil {
		t.Fatal("a plaintext photo URL was accepted")
	}
	if _, err := client.FetchPhoto(context.Background(), "not a url"); err == nil {
		t.Fatal("an unparseable photo URL was accepted")
	}
}
