// File overview: Tests that a remote image lookup failure is reported as a
// server error instead of being hidden behind the 404 used for cache misses.

package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"rolltop/backend/store/storetest"
	"rolltop/internal/testlog"
)

// A store failure leaves the cache zero-valued, which also fails the "is this
// entry usable" checks. Classifying the error first is what keeps the failure
// from being answered as an ordinary cache miss.
func TestRemoteImageLookupFailureIsAServerError(t *testing.T) {
	logs := testlog.Capture(t)
	db, err := storetest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	user, err := db.CreateUser(context.Background(), "images@example.test", "Images", "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	server := &Server{store: db}

	req := httptest.NewRequest(http.MethodGet, "/remote-images/deadbeef", nil)
	req = req.WithContext(context.WithValue(req.Context(), userContextKey, currentUser{User: user}))
	rec := httptest.NewRecorder()

	server.handleRemoteImage(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d (body=%s)", rec.Code, http.StatusInternalServerError, rec.Body.String())
	}
	if logs.Len() == 0 {
		t.Fatal("remote image lookup failure was not logged")
	}
}
