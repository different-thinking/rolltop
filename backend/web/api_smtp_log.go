// File overview: The SMTP conversation, handed to the person whose mail did not
// leave. A failed send answers the browser with one sentence, and the reply the
// server actually gave -- the 535 that names a rejected password, the port that
// accepts a connection and then says nothing, the STARTTLS an account expects
// and the server does not offer -- was until now written only to the container
// log, which nobody running a hosted installation can read.
//
// Both routes here are user-scoped, not admin-scoped: the transcripts belong to
// the person configuring the account, and they carry that person's own envelope
// addresses. The recorder holds them per user and every read names the user
// asking, so one tenant's attempts are never in another's answer.

package web

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"rolltop/backend/smtplog"
	"rolltop/backend/store"
)

// defaultSMTPLogSessions is what the settings page asks for when it does not
// say. A misconfigured account is diagnosed from its last few attempts.
const defaultSMTPLogSessions = 10

// Bounds on the connection test. It is the one route where a signed-in user
// decides what host and port the server connects out to, and it hands back the
// first thing the peer said -- which for a peer that is not a mail server is
// its banner. Sending has always been able to reach the same address, but one
// button that answers in a second is a different tool than composing a message
// per address, so a user runs one test at a time and pauses between them.
// smtpTestHold is what a running test reserves, long enough to cover a dial
// that hangs until it times out; smtpTestMinInterval is the pause after one
// finishes.
const (
	smtpTestHold        = 90 * time.Second
	smtpTestMinInterval = 5 * time.Second
	// smtpTestReservationSweep bounds the reservation map, which is keyed by
	// user. Expired entries are dropped once there are more of them than any
	// installation is plausibly testing with at once.
	smtpTestReservationSweep = 64
)

type apiSMTPLogLine struct {
	Time      string `json:"time"`
	Direction string `json:"direction"`
	Text      string `json:"text"`
}

type apiSMTPLogSession struct {
	ID        int64            `json:"id"`
	AccountID int64            `json:"account_id"`
	Kind      string           `json:"kind"`
	Host      string           `json:"host"`
	Port      int              `json:"port"`
	Username  string           `json:"username"`
	From      string           `json:"from"`
	StartedAt string           `json:"started_at"`
	EndedAt   string           `json:"ended_at"`
	Error     string           `json:"error"`
	Truncated bool             `json:"truncated"`
	Lines     []apiSMTPLogLine `json:"lines"`
}

// apiSMTPLog answers with the caller's own recent SMTP conversations, newest
// first, optionally narrowed to one outgoing server.
func (s *Server) apiSMTPLog(w http.ResponseWriter, r *http.Request) {
	cu, ok := s.requireAPIAuth(w, r)
	if !ok {
		return
	}
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	accountID, _ := strconv.ParseInt(strings.TrimSpace(r.URL.Query().Get("account_id")), 10, 64)
	sessions := s.smtpLog.Sessions(cu.User.ID, accountID, smtpLogLimitFromRequest(r))
	out := make([]apiSMTPLogSession, 0, len(sessions))
	for _, session := range sessions {
		out = append(out, apiSMTPLogSessionFrom(session))
	}
	writeJSON(w, map[string]any{"sessions": out})
}

// smtpLogLimitFromRequest reads how many sessions to return. Like the admin log
// tail, an unusable value falls back to the default rather than failing: this
// page is read when something else is already broken.
func smtpLogLimitFromRequest(r *http.Request) int {
	raw := strings.TrimSpace(r.URL.Query().Get("limit"))
	if raw == "" {
		return defaultSMTPLogSessions
	}
	limit, err := strconv.Atoi(raw)
	if err != nil || limit <= 0 {
		return defaultSMTPLogSessions
	}
	if limit > 50 {
		return 50
	}
	return limit
}

// apiTestSMTPAccount runs a login against one of the caller's outgoing servers
// and answers with the conversation it produced. It offers no message, so
// pressing the button cannot deliver mail to anybody.
func (s *Server) apiTestSMTPAccount(w http.ResponseWriter, r *http.Request, accountID int64) {
	cu, ok := s.requireAPIAuth(w, r)
	if !ok {
		return
	}
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	if !s.verifyCSRF(w, r) {
		return
	}
	account, err := s.store.GetSMTPAccountForUser(r.Context(), cu.User.ID, accountID)
	if err != nil {
		if store.IsNotFound(err) {
			http.NotFound(w, r)
			return
		}
		s.serverError(w, r, err)
		return
	}
	if s.sender == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "sending is not configured on this server")
		return
	}
	if !s.reserveSMTPTest(cu.User.ID) {
		writeAPIError(w, http.StatusTooManyRequests, "a connection test is already running or just finished; try again in a few seconds")
		return
	}
	defer s.releaseSMTPTest(cu.User.ID)
	sessionID, testErr := s.sender.Verify(r.Context(), smtpTestEnvelope(cu, account))
	// A refused login is the expected answer here, not a server fault: the
	// request succeeded, and what it found out is the payload. The transcript
	// goes back either way, because a test that only says "failed" is the
	// situation this endpoint exists to end.
	response := map[string]any{"ok": testErr == nil}
	if testErr != nil {
		response["error"] = testErr.Error()
	}
	if session, found := s.smtpLog.Session(cu.User.ID, sessionID); found {
		response["session"] = apiSMTPLogSessionFrom(session)
	}
	writeJSON(w, response)
}

// reserveSMTPTest claims this user's single connection-test slot, or reports
// that they already have one. The reservation is a deadline rather than a flag
// so a test whose handler dies without releasing it expires on its own.
func (s *Server) reserveSMTPTest(userID int64) bool {
	now := time.Now()
	s.smtpTestMu.Lock()
	defer s.smtpTestMu.Unlock()
	if until, ok := s.smtpTestUntil[userID]; ok && now.Before(until) {
		return false
	}
	if s.smtpTestUntil == nil {
		s.smtpTestUntil = map[int64]time.Time{}
	}
	if len(s.smtpTestUntil) > smtpTestReservationSweep {
		for id, until := range s.smtpTestUntil {
			if !now.Before(until) {
				delete(s.smtpTestUntil, id)
			}
		}
	}
	s.smtpTestUntil[userID] = now.Add(smtpTestHold)
	return true
}

// releaseSMTPTest shortens the reservation to the pause between tests.
func (s *Server) releaseSMTPTest(userID int64) {
	s.smtpTestMu.Lock()
	defer s.smtpTestMu.Unlock()
	if s.smtpTestUntil == nil {
		return
	}
	s.smtpTestUntil[userID] = time.Now().Add(smtpTestMinInterval)
}

// smtpTestEnvelope describes the account to the sender. The address only labels
// the recorded session -- a test stops before MAIL FROM -- so the connected
// mailbox is preferred and the signed-in address stands in when there is none.
func smtpTestEnvelope(cu currentUser, account store.SMTPAccount) store.MailAccount {
	from := strings.TrimSpace(account.Username)
	if from == "" {
		from = cu.User.Email
	}
	return store.MailAccount{
		UserID:                account.UserID,
		Email:                 from,
		SMTPAccountID:         account.ID,
		SMTPHost:              account.Host,
		SMTPPort:              account.Port,
		SMTPUsername:          account.Username,
		EncryptedSMTPPassword: account.EncryptedPassword,
		SMTPUseTLS:            account.UseTLS,
		AuthType:              account.AuthType,
		GoogleConnectionID:    account.GoogleConnectionID,
	}
}

func apiSMTPLogSessionFrom(session smtplog.Session) apiSMTPLogSession {
	lines := make([]apiSMTPLogLine, 0, len(session.Lines))
	for _, line := range session.Lines {
		lines = append(lines, apiSMTPLogLine{
			Time:      timeString(line.At),
			Direction: line.Direction,
			Text:      line.Text,
		})
	}
	return apiSMTPLogSession{
		ID:        session.ID,
		AccountID: session.AccountID,
		Kind:      session.Kind,
		Host:      session.Host,
		Port:      session.Port,
		Username:  session.Username,
		From:      session.From,
		StartedAt: timeString(session.StartedAt),
		EndedAt:   timeString(session.EndedAt),
		Error:     session.Err,
		Truncated: session.Truncated,
		Lines:     lines,
	}
}
