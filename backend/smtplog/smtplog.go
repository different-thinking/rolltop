// File overview: In-memory record of what actually crossed the SMTP wire. A
// send that fails leaves the browser with one sentence and the operator with a
// container log nobody hosted can read, so the reply that names the reason --
// "535 authentication failed", "554 relay denied", a TLS handshake that never
// completed -- was written down nowhere the person configuring the server could
// look. This package keeps the conversation of the newest attempts per user so
// the SMTP settings page can show it.
//
// What it holds is a diagnostic tail, not an audit trail: bounded per user,
// dropped on restart, and never written to the database. Credentials and
// message bodies never enter it -- the recorder redacts the AUTH exchange and
// stores only the byte count of DATA.

package smtplog

import (
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Bounds. A failing account is diagnosed from its last few attempts, so the
// tail is short enough that a busy tenant cannot grow the process and long
// enough to cover the reproduction the reader just did.
const (
	sessionsPerUser  = 12
	linesPerSession  = 400
	maxLineBytes     = 2000
	maxTrackedUsers  = 64
	redactedSecret   = "[credentials redacted]"
	redactedBodyNote = "message body"
)

// Direction says who spoke, so the page can render a transcript rather than a
// pile of text.
const (
	// DirectionClient is a line Rolltop sent.
	DirectionClient = "client"
	// DirectionServer is a reply the mail server sent.
	DirectionServer = "server"
	// DirectionNote is Rolltop's own commentary: the dial, the TLS handshake,
	// the message body that is deliberately not transcribed, the final error.
	DirectionNote = "note"
)

// Kinds of attempt, so a reader can tell a send from a button press.
const (
	// KindSend is an attempt that carried a message.
	KindSend = "send"
	// KindTest is the settings page's connection test, which stops before
	// offering one.
	KindTest = "test"
)

// Line is one recorded utterance with the time the process saw it.
type Line struct {
	At        time.Time
	Direction string
	Text      string
}

// Session is one finished or running SMTP attempt as a reader sees it: a plain
// value, safe to hand out, snapshotted from the recording behind it.
type Session struct {
	ID        int64
	UserID    int64
	AccountID int64
	Kind      string
	Host      string
	Port      int
	Username  string
	From      string
	StartedAt time.Time
	EndedAt   time.Time
	// Err is the failure the attempt ended with, empty when it succeeded. It is
	// the same text the browser was given, kept beside the transcript because
	// the transcript alone does not say which step the caller gave up on.
	Err string
	// Truncated says the attempt hit the line bound. The page shows that rather
	// than letting a reader take a cut-off transcript for the whole story.
	Truncated bool
	Lines     []Line
}

// Recording is the writable side, held by the attempt in progress. It is kept
// apart from Session so what a reader gets is a value with no lock in it and no
// slice the sender is still appending to.
type Recording struct {
	mu      sync.Mutex
	session Session
}

// Recorder keeps the newest sessions for each user.
type Recorder struct {
	mu       sync.Mutex
	nextID   int64
	byUser   map[int64][]*Recording
	userSeen map[int64]time.Time
}

// NewRecorder returns an empty recorder. A nil *Recorder is usable and records
// nothing, so a Sender built without one -- every test that only asserts on
// delivery -- needs no wiring.
func NewRecorder() *Recorder {
	return &Recorder{byUser: map[int64][]*Recording{}, userSeen: map[int64]time.Time{}}
}

// Start opens a recording from the metadata of the attempt and files it under
// the user immediately, so an attempt that hangs until the request is cancelled
// is visible while it hangs.
func (r *Recorder) Start(meta Session) *Recording {
	if r == nil {
		return nil
	}
	meta.StartedAt = time.Now().UTC()
	meta.EndedAt = time.Time{}
	meta.Err = ""
	meta.Truncated = false
	meta.Lines = nil
	recording := &Recording{session: meta}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nextID++
	recording.session.ID = r.nextID
	sessions := append(r.byUser[meta.UserID], recording)
	if len(sessions) > sessionsPerUser {
		sessions = sessions[len(sessions)-sessionsPerUser:]
	}
	r.byUser[meta.UserID] = sessions
	r.userSeen[meta.UserID] = meta.StartedAt
	r.pruneUsersLocked()
	return recording
}

// pruneUsersLocked drops the least recently active tenants once more than
// maxTrackedUsers have sent. Their tail is the one nobody is currently
// diagnosing, and without this an installation's memory grows with its user
// count rather than with its activity.
func (r *Recorder) pruneUsersLocked() {
	for len(r.byUser) > maxTrackedUsers {
		var oldestUser int64
		var oldestAt time.Time
		for userID, seen := range r.userSeen {
			if oldestAt.IsZero() || seen.Before(oldestAt) {
				oldestUser, oldestAt = userID, seen
			}
		}
		delete(r.byUser, oldestUser)
		delete(r.userSeen, oldestUser)
	}
}

// Sessions returns the newest sessions for one user, newest first.
func (r *Recorder) Sessions(userID int64, limit int) []Session {
	if r == nil || limit <= 0 {
		return nil
	}
	r.mu.Lock()
	recordings := append([]*Recording(nil), r.byUser[userID]...)
	r.mu.Unlock()
	out := make([]Session, 0, len(recordings))
	for i := len(recordings) - 1; i >= 0 && len(out) < limit; i-- {
		out = append(out, recordings[i].Snapshot())
	}
	return out
}

// Session returns one session by id, scoped to the user that owns it. A reader
// asking for somebody else's attempt gets the same answer as one asking for an
// attempt that never existed.
func (r *Recorder) Session(userID, id int64) (Session, bool) {
	if r == nil || id <= 0 {
		return Session{}, false
	}
	r.mu.Lock()
	recordings := append([]*Recording(nil), r.byUser[userID]...)
	r.mu.Unlock()
	for _, recording := range recordings {
		// The id is read before the snapshot so a lookup does not copy the
		// transcript of every attempt it walks past.
		if recording.Ref() == id {
			return recording.Snapshot(), true
		}
	}
	return Session{}, false
}

// Snapshot copies out what has been recorded so far. A recording still being
// written to must not hand its own slice to a reader.
func (r *Recording) Snapshot() Session {
	if r == nil {
		return Session{}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.session
	out.Lines = append([]Line(nil), r.session.Lines...)
	return out
}

// Ref is the recording's id, or zero when nothing is being recorded. A caller
// that wants to hand a reader the conversation it just caused uses it rather
// than the newest session, which on a busy account belongs to somebody else.
func (r *Recording) Ref() int64 {
	if r == nil {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.session.ID
}

// Client records a command Rolltop sent. Callers pass the line as written to
// the server; redaction happens here so no call site can forget it.
func (r *Recording) Client(line string) { r.add(DirectionClient, redactCommand(line)) }

// Server records a reply. Multi-line replies arrive joined from textproto and
// are kept whole: the extension list a server advertises is most of what a
// misconfigured port is diagnosed from.
func (r *Recording) Server(code int, message string) {
	r.add(DirectionServer, formatReply(code, message))
}

// Note records something that has no wire representation: the dial, the TLS
// upgrade, the size of a body that is deliberately not transcribed.
func (r *Recording) Note(text string) { r.add(DirectionNote, text) }

// Secret records a line whose content is credentials -- the base64 blobs of an
// AUTH exchange. That a line was sent is worth showing; its content never is.
func (r *Recording) Secret() { r.add(DirectionClient, redactedSecret) }

// Body records that a message payload was sent, by size only. Raw message
// bodies must not be logged, and a transcript is no exception.
func (r *Recording) Body(bytes int) { r.Note(formatBodyNote(bytes)) }

// Finish closes the recording with the error the attempt ended with, or nil.
func (r *Recording) Finish(err error) {
	if r == nil {
		return
	}
	if err != nil {
		r.Note("failed: " + err.Error())
	} else {
		r.Note("completed")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.session.EndedAt = time.Now().UTC()
	if err != nil {
		r.session.Err = truncateLine(collapseLineBreaks(err.Error()))
	}
}

func (r *Recording) add(direction, text string) {
	if r == nil {
		return
	}
	text = truncateLine(collapseLineBreaks(text))
	if strings.TrimSpace(text) == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.session.Lines) >= linesPerSession {
		r.session.Truncated = true
		return
	}
	r.session.Lines = append(r.session.Lines, Line{At: time.Now().UTC(), Direction: direction, Text: text})
}

// collapseLineBreaks keeps one recorded utterance to one record. A server reply
// arrives with its continuation lines joined by newlines, and a reader scanning
// a transcript for the failing step should see them as that one reply.
func collapseLineBreaks(text string) string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	return strings.ReplaceAll(text, "\n", " | ")
}

func truncateLine(text string) string {
	text = strings.TrimRight(text, "\r\n")
	if len(text) <= maxLineBytes {
		return text
	}
	cut := text[:maxLineBytes]
	for len(cut) > 0 && !utf8.ValidString(cut) {
		cut = cut[:len(cut)-1]
	}
	return cut + "..."
}
