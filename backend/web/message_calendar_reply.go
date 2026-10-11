// File overview: Answering a mailed invitation by mail. An invitation whose
// event no connected calendar holds -- one that reached an IMAP-only mailbox,
// or came from an organizer outside the reader's own provider -- can only be
// answered the way the organizer's client expects to hear back: an iTIP REPLY
// (RFC 5546) mailed to the organizer (RFC 6047), sent from the account the
// invitation arrived through.
//
// An event a connected calendar does hold is answered through that calendar's
// provider instead (/api/calendar/events/:id/respond), which tells the
// organizer itself and keeps the reader's calendar in step; the card chooses.

package web

import (
	"encoding/base64"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"net/mail"
	"strings"
	"time"

	"rolltop/backend/icalendar"
	"rolltop/backend/logging"
	"rolltop/backend/smtpclient"
	"rolltop/backend/store"
)

// calendarReplyStatuses maps the answers the calendar dialog already speaks
// to the participation statuses a reply carries, with the words the mail says
// them in.
var calendarReplyStatuses = map[string]struct {
	partStat string
	subject  string
	sentence string
}{
	"accepted":  {icalendar.PartStatAccepted, "Accepted", "has accepted"},
	"tentative": {icalendar.PartStatTentative, "Tentative", "has tentatively accepted"},
	"declined":  {icalendar.PartStatDeclined, "Declined", "has declined"},
}

// invitationAnswerable reports whether an invitation can be answered by mail
// at all: it asks for answers (METHOD:REQUEST), names the event and somebody
// to tell, and the reader is not the one who sent it.
func invitationAnswerable(method string, event icalendar.Event, own map[string]bool) bool {
	if method != "REQUEST" || event.UID == "" || event.Organizer.Email == "" || event.Status == "CANCELLED" {
		return false
	}
	return !own[store.SenderIdentity(event.Organizer.Email)]
}

// apiMessageCalendarReply answers POST /api/messages/:id/calendar/reply.
func (s *Server) apiMessageCalendarReply(w http.ResponseWriter, r *http.Request, id int64) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	if !s.verifyCSRF(w, r) {
		return
	}
	cu, ok := s.requireAPIAuth(w, r)
	if !ok {
		return
	}
	var in struct {
		EventIndex int    `json:"event_index"`
		UID        string `json:"uid"`
		Response   string `json:"response"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	status, known := calendarReplyStatuses[strings.TrimSpace(in.Response)]
	if !known {
		writeAPIError(w, http.StatusBadRequest, "Answer with yes, maybe or no.")
		return
	}
	if s.sender == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "Sending mail is not configured on this server.")
		return
	}
	userID := cu.User.ID
	msg, err := s.store.GetMessageForUser(r.Context(), userID, id)
	if store.IsNotFound(err) {
		writeMessageGone(w)
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	parts, readable, _ := s.messageCalendarParts(r.Context(), userID, msg)
	if !readable {
		writeAPIError(w, http.StatusGone, "The message could not be read from the mail server.")
		return
	}
	var method string
	events := calendarEventsFromParts(parts, &method)
	// The index and the UID together: the index says which event of the
	// message, the UID proves the client still means the one it was shown.
	if in.EventIndex < 0 || in.EventIndex >= len(events) || events[in.EventIndex].UID != strings.TrimSpace(in.UID) {
		writeAPIError(w, http.StatusConflict, "This invitation is not the one the page showed. Reload the message and answer again.")
		return
	}
	event := events[in.EventIndex]
	own := s.ownAddresses(r.Context(), cu.User)
	if !invitationAnswerable(method, event, own) {
		writeAPIError(w, http.StatusUnprocessableEntity, "This message does not ask for an answer.")
		return
	}
	identityID := s.replyFromIdentityID(r.Context(), cu, msg, nil)
	identity, err := s.selectedComposeIdentity(r.Context(), cu, identityID)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "There is no address to answer from. Add a sending identity under Settings first.")
		return
	}
	attendee := invitationAttendee(event, identity, own)
	reply, err := icalendar.BuildReply(event, attendee, status.partStat, time.Now())
	if err != nil {
		writeAPIError(w, http.StatusUnprocessableEntity, "This invitation cannot be answered by mail.")
		return
	}
	who := attendee.Name
	if who == "" {
		who = attendee.Email
	}
	title := event.Summary
	if title == "" {
		title = "(no title)"
	}
	form := composeForm{
		To:             (&mail.Address{Name: event.Organizer.Name, Address: event.Organizer.Email}).String(),
		Subject:        status.subject + ": " + title,
		Body:           fmt.Sprintf("%s %s this invitation.\n", who, status.sentence),
		FromIdentityID: identity.ID,
		InReplyToID:    msg.ID,
		calendarReply:  reply,
	}
	_, err = s.sendCompose(r.Context(), cu, form)
	var incomplete *composeSentIncompleteError
	if err != nil && !errors.As(err, &incomplete) {
		logging.Errorf("send invitation reply failed user_id=%d message_id=%d: %v", userID, msg.ID, err)
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.notifyUserChanged(userID)
	out := map[string]any{"ok": true, "response": in.Response, "sent_from": identity.Email, "sent_to": event.Organizer.Email}
	if incomplete != nil {
		// The organizer has the answer; only the copy in Sent is missing, and
		// answering this as a failure would have the reader send it twice.
		out["warning"] = incomplete.Error()
	}
	writeJSON(w, out)
}

// invitationAttendee is the attendee the reply speaks for: the one the
// invitation names at the address it is answered from, then any of the
// reader's addresses it names -- the invitation went to an alias -- and
// otherwise the sending address itself, which the organizer's client records
// as a guest who answered.
func invitationAttendee(event icalendar.Event, identity composeIdentity, own map[string]bool) icalendar.Person {
	from := store.SenderIdentity(identity.Email)
	name := ""
	if parsed, err := mail.ParseAddress(identity.Header); err == nil {
		name = parsed.Name
	}
	var alias *icalendar.Person
	for i, attendee := range event.Attendees {
		key := store.SenderIdentity(attendee.Email)
		if key == "" {
			continue
		}
		if key == from {
			return withName(event.Attendees[i], name)
		}
		if alias == nil && own[key] {
			alias = &event.Attendees[i]
		}
	}
	if alias != nil {
		return withName(*alias, name)
	}
	return icalendar.Person{Name: name, Email: identity.Email}
}

func withName(person icalendar.Person, fallback string) icalendar.Person {
	if strings.TrimSpace(person.Name) == "" {
		person.Name = fallback
	}
	return icalendar.Person{Name: person.Name, Email: person.Email}
}

// calendarReplyMIMEBody is the body of an answer to an invitation: the
// sentence for a reader, and the REPLY for the organizer's calendar as its
// text/calendar alternative, which is where Outlook, Google and Apple look.
func calendarReplyMIMEBody(messageID, text string, reply []byte) *smtpclient.MIMEBodyOverride {
	boundary := smtpclient.MIMEBoundary(messageID, "calendar")
	var b strings.Builder
	fmt.Fprintf(&b, "--%s\r\n", boundary)
	b.WriteString("Content-Type: text/plain; charset=\"utf-8\"\r\n")
	b.WriteString("Content-Transfer-Encoding: 8bit\r\n\r\n")
	b.WriteString(strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\n", "\r\n"))
	if !strings.HasSuffix(text, "\n") {
		b.WriteString("\r\n")
	}
	fmt.Fprintf(&b, "--%s\r\n", boundary)
	b.WriteString("Content-Type: " + mime.FormatMediaType("text/calendar", map[string]string{"charset": "utf-8", "method": "REPLY"}) + "\r\n")
	b.WriteString("Content-Transfer-Encoding: base64\r\n\r\n")
	encoded := base64.StdEncoding.EncodeToString(reply)
	for len(encoded) > 76 {
		b.WriteString(encoded[:76] + "\r\n")
		encoded = encoded[76:]
	}
	b.WriteString(encoded + "\r\n")
	fmt.Fprintf(&b, "--%s--\r\n", boundary)
	return &smtpclient.MIMEBodyOverride{
		ContentType: mime.FormatMediaType("multipart/alternative", map[string]string{"boundary": boundary}),
		Body:        b.String(),
	}
}
