// File overview: Tests that serverError writes the underlying error to the
// process log so operator logs are not silent on internal server errors.

package web

import (
	"bytes"
	"context"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func captureLogOutput(t *testing.T) *bytes.Buffer {
	t.Helper()
	var logs bytes.Buffer
	previousWriter := log.Writer()
	previousFlags := log.Flags()
	log.SetOutput(&logs)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(previousWriter)
		log.SetFlags(previousFlags)
	})
	return &logs
}

func TestServerErrorLogsUnderlyingError(t *testing.T) {
	logs := captureLogOutput(t)
	s := &Server{}
	rec := httptest.NewRecorder()
	s.serverError(rec, errors.New("query users: disk I/O error"))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
	}
	if !strings.Contains(logs.String(), "server error: query users: disk I/O error") {
		t.Fatalf("log output %q does not contain the underlying error", logs.String())
	}
}

func TestServerErrorDoesNotLogCanceledRequests(t *testing.T) {
	logs := captureLogOutput(t)
	s := &Server{}
	rec := httptest.NewRecorder()
	s.serverError(rec, context.Canceled)
	if rec.Code != http.StatusRequestTimeout {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusRequestTimeout)
	}
	if logs.Len() != 0 {
		t.Fatalf("expected no log output for canceled requests, got %q", logs.String())
	}
}
