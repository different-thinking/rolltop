package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"rolltop/backend/smtpclient"
	"rolltop/backend/smtplog"
	"rolltop/backend/store"
)

// verifyStub stands in for the sender so the handler can be exercised without a
// mail server. It records what it was asked to log in as, which is how the test
// proves the stored account rather than a request field decides that.
type verifyStub struct {
	sessionID int64
	err       error
	calls     int
	account   store.MailAccount
}

func (v *verifyStub) Send(context.Context, store.MailAccount, smtpclient.Message) ([]byte, error) {
	return nil, errors.New("the connection test must not send")
}

func (v *verifyStub) Verify(_ context.Context, account store.MailAccount) (int64, error) {
	v.calls++
	v.account = account
	return v.sessionID, v.err
}

func TestSMTPLogAnswersOnlyTheCallersOwnConversations(t *testing.T) {
	server, owner, _ := newDatabaseAdminServer(t)
	stranger, err := server.store.CreateUser(context.Background(), "stranger@example.test", "Stranger", "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	recordSession(server, owner.ID, 7, "mine.example.test")
	recordSession(server, stranger.ID, 9, "theirs.example.test")

	body := smtpLogBody(t, server, owner, "/api/smtp-log")
	if !strings.Contains(body, "mine.example.test") {
		t.Fatalf("caller's own conversation is missing: %s", body)
	}
	if strings.Contains(body, "theirs.example.test") {
		t.Fatalf("another user's conversation leaked: %s", body)
	}
}

func TestSMTPLogNarrowsToOneOutgoingServer(t *testing.T) {
	server, owner, _ := newDatabaseAdminServer(t)
	recordSession(server, owner.ID, 7, "seven.example.test")
	recordSession(server, owner.ID, 8, "eight.example.test")

	body := smtpLogBody(t, server, owner, "/api/smtp-log?account_id=8")
	if !strings.Contains(body, "eight.example.test") {
		t.Fatalf("filtered answer dropped the requested server: %s", body)
	}
	if strings.Contains(body, "seven.example.test") {
		t.Fatalf("filtered answer carried another server: %s", body)
	}
}

// A refused login is what this endpoint exists to report, so it answers 200
// with the reason and the transcript rather than an error status the page would
// have to translate back.
func TestSMTPAccountTestReportsARefusedLoginWithItsTranscript(t *testing.T) {
	server, owner, _ := newDatabaseAdminServer(t)
	account, err := server.store.CreateSMTPAccount(context.Background(), store.SMTPAccount{
		UserID: owner.ID, Label: "Outgoing", Host: "smtp.example.test", Port: 587,
		Username: "user@example.test", UseTLS: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	recording := server.smtpLog.Start(smtplog.Session{UserID: owner.ID, AccountID: account.ID, Kind: smtplog.KindTest, Host: "smtp.example.test", Port: 587})
	recording.Server(535, "5.7.8 Username and Password not accepted")
	stub := &verifyStub{sessionID: recording.Ref(), err: errors.New("authenticate to SMTP server: 535 5.7.8 Username and Password not accepted")}
	recording.Finish(stub.err)
	server.sender = stub

	recorder := httptest.NewRecorder()
	server.apiTestSMTPAccount(recorder, smtpTestRequest(t, server, owner, account.ID), account.ID)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", recorder.Code, recorder.Body.String())
	}
	var payload struct {
		OK      bool              `json:"ok"`
		Error   string            `json:"error"`
		Session apiSMTPLogSession `json:"session"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.OK {
		t.Fatal("a refused login was reported as a working server")
	}
	if !strings.Contains(payload.Error, "535") {
		t.Fatalf("error = %q, want the server's own refusal", payload.Error)
	}
	if len(payload.Session.Lines) == 0 {
		t.Fatal("the test answered without the conversation it produced")
	}
	if stub.account.SMTPHost != "smtp.example.test" || stub.account.SMTPAccountID != account.ID {
		t.Fatalf("the test signed in as %#v, want the stored account", stub.account)
	}
}

// The account is read under the caller's own user id, so a request naming
// somebody else's outgoing server is a request for a server that does not
// exist.
func TestSMTPAccountTestRefusesAnotherUsersServer(t *testing.T) {
	server, owner, _ := newDatabaseAdminServer(t)
	stranger, err := server.store.CreateUser(context.Background(), "stranger@example.test", "Stranger", "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	account, err := server.store.CreateSMTPAccount(context.Background(), store.SMTPAccount{
		UserID: owner.ID, Label: "Outgoing", Host: "smtp.example.test", Port: 587,
	})
	if err != nil {
		t.Fatal(err)
	}
	stub := &verifyStub{}
	server.sender = stub

	recorder := httptest.NewRecorder()
	server.apiTestSMTPAccount(recorder, smtpTestRequest(t, server, stranger, account.ID), account.ID)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", recorder.Code)
	}
	if stub.calls != 0 {
		t.Fatalf("the sender was asked to sign in %d times for a server the caller does not own", stub.calls)
	}
}

// An attempt that has not finished carries no end time, and the page reads
// that field to tell a running conversation from a finished one. It has to
// reach the browser as an empty string: any timestamp there, including a
// formatted zero time, is truthy in JavaScript and would report a send that is
// still dialling as one that succeeded.
func TestSMTPLogLeavesARunningAttemptWithoutAnEndTime(t *testing.T) {
	server, owner, _ := newDatabaseAdminServer(t)
	running := server.smtpLog.Start(smtplog.Session{UserID: owner.ID, AccountID: 3, Kind: smtplog.KindSend, Host: "smtp.example.test", Port: 587})
	running.Client("EHLO localhost")

	body := smtpLogBody(t, server, owner, "/api/smtp-log")
	var payload struct {
		Sessions []apiSMTPLogSession `json:"sessions"`
	}
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Sessions) != 1 {
		t.Fatalf("sessions = %d, want 1: %s", len(payload.Sessions), body)
	}
	if payload.Sessions[0].EndedAt != "" {
		t.Fatalf("running attempt reported ended_at = %q, want it empty", payload.Sessions[0].EndedAt)
	}
	if payload.Sessions[0].StartedAt == "" {
		t.Fatal("running attempt reported no start time")
	}
}

// The test button is the one route where a signed-in user decides what address
// the server dials, and it answers with what the peer said. One test at a time
// per user, with a pause after it, is what keeps that from being a convenient
// way to sweep a network.
func TestSMTPAccountTestRefusesASecondTestWhileOneIsHeld(t *testing.T) {
	server, owner, _ := newDatabaseAdminServer(t)
	account, err := server.store.CreateSMTPAccount(context.Background(), store.SMTPAccount{
		UserID: owner.ID, Label: "Outgoing", Host: "smtp.example.test", Port: 587,
	})
	if err != nil {
		t.Fatal(err)
	}
	stub := &verifyStub{}
	server.sender = stub

	first := httptest.NewRecorder()
	server.apiTestSMTPAccount(first, smtpTestRequest(t, server, owner, account.ID), account.ID)
	if first.Code != http.StatusOK {
		t.Fatalf("first test status = %d body = %s", first.Code, first.Body.String())
	}
	second := httptest.NewRecorder()
	server.apiTestSMTPAccount(second, smtpTestRequest(t, server, owner, account.ID), account.ID)
	if second.Code != http.StatusTooManyRequests {
		t.Fatalf("second test status = %d, want 429", second.Code)
	}
	if stub.calls != 1 {
		t.Fatalf("the sender was asked to sign in %d times, want 1", stub.calls)
	}

	// The pause is per user: somebody else's test is not blocked by it.
	stranger, err := server.store.CreateUser(context.Background(), "stranger@example.test", "Stranger", "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	theirs, err := server.store.CreateSMTPAccount(context.Background(), store.SMTPAccount{
		UserID: stranger.ID, Label: "Theirs", Host: "smtp.example.test", Port: 587,
	})
	if err != nil {
		t.Fatal(err)
	}
	third := httptest.NewRecorder()
	server.apiTestSMTPAccount(third, smtpTestRequest(t, server, stranger, theirs.ID), theirs.ID)
	if third.Code != http.StatusOK {
		t.Fatalf("another user's test status = %d body = %s", third.Code, third.Body.String())
	}
}

func recordSession(server *Server, userID, accountID int64, host string) {
	recording := server.smtpLog.Start(smtplog.Session{UserID: userID, AccountID: accountID, Kind: smtplog.KindSend, Host: host, Port: 587})
	recording.Client("EHLO localhost")
	recording.Finish(nil)
}

func smtpLogBody(t *testing.T, server *Server, user store.User, target string) string {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, target, nil)
	request = request.WithContext(context.WithValue(request.Context(), userContextKey, currentUser{User: user}))
	recorder := httptest.NewRecorder()
	server.apiSMTPLog(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET %s status = %d body = %s", target, recorder.Code, recorder.Body.String())
	}
	return recorder.Body.String()
}

func smtpTestRequest(t *testing.T, server *Server, user store.User, accountID int64) *http.Request {
	t.Helper()
	target := "/api/account/smtp/" + strconv.FormatInt(accountID, 10) + "/test"
	request := httptest.NewRequest(http.MethodPost, target, bytes.NewReader(nil))
	request.Header.Set("Content-Type", "application/json")
	request = request.WithContext(context.WithValue(request.Context(), userContextKey, currentUser{User: user}))
	const csrfBase = "smtp-log-csrf"
	request.AddCookie(&http.Cookie{Name: csrfCookie, Value: csrfBase})
	request.Header.Set("X-CSRF-Token", server.csrfForBase(csrfBase))
	return request
}
