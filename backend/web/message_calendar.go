// File overview: The calendar objects a message carries -- an invitation's
// .ics, or the text/calendar alternative Outlook sends in its place -- read for
// the thread view so the reader sees what and when without downloading the
// file, and can add the event to one of their calendars.
//
// Nothing here writes to a calendar. Adding goes through the event dialog and
// the ordinary create route, so the event is entered the way any event is:
// at the provider first, into a calendar the reader lists, with no guests --
// the invitation's guest list belongs to the organizer's event, and copying it
// into a new one would invite all of them a second time.

package web

import (
	"bytes"
	"context"
	"net/http"
	"sort"
	"strings"
	"time"

	"rolltop/backend/icalendar"
	"rolltop/backend/mailparse"
	"rolltop/backend/store"
)

// maxCalendarScanBytes bounds the message the thread view reads looking for an
// inline calendar part. An invitation without an .ics attachment row is an
// Outlook alternative part, which arrives in a message of a few dozen
// kilobytes; a larger message carrying one as an attachment is found through
// its attachment row instead.
const maxCalendarScanBytes = 4 << 20

// messageCarriesCalendar reports whether the thread view should offer the
// calendar card for a message. An attachment row answers for free. Without
// one, only the message being read is looked into, and only from the local
// copy: an Outlook invitation sends its calendar as a body alternative, which
// never becomes a row, and asking IMAP for every message drawn would cost the
// thread view a fetch per message just to find out.
func (s *Server) messageCarriesCalendar(ctx context.Context, userID int64, msg store.MessageRecord, attachments []store.Attachment, expanded bool) bool {
	for _, att := range attachments {
		if mailparse.IsCalendarPart(att.ContentType, att.Filename) {
			return true
		}
	}
	if !expanded || s.blobs == nil || strings.TrimSpace(msg.BlobPath) == "" {
		return false
	}
	if msg.Size > maxCalendarScanBytes {
		return false
	}
	f, err := s.blobs.OpenUserBlob(userID, msg.BlobPath)
	if err != nil {
		return false
	}
	defer f.Close()
	parts, _ := mailparse.CalendarParts(f)
	return len(parts) > 0
}

type apiMessageCalendarPerson struct {
	Name  string `json:"name,omitempty"`
	Email string `json:"email,omitempty"`
	// Status is the attendee's answer as the object states it: accepted,
	// tentative, declined or needsAction -- the words the calendar uses.
	Status string `json:"status,omitempty"`
}

// calendarPartStatResponses maps iCalendar participation statuses to the
// answers the calendar API already speaks.
var calendarPartStatResponses = map[string]string{
	icalendar.PartStatAccepted:  store.CalendarResponseAccepted,
	icalendar.PartStatTentative: store.CalendarResponseTentative,
	icalendar.PartStatDeclined:  store.CalendarResponseDeclined,
	"NEEDS-ACTION":              store.CalendarResponseNeedsAction,
}

// apiMessageCalendarMatch is a copy of the invited event a calendar of the
// reader's already holds.
type apiMessageCalendarMatch struct {
	EventID      int64  `json:"event_id"`
	CalendarID   int64  `json:"calendar_id"`
	CalendarName string `json:"calendar_name"`
	StartAt      string `json:"start_at"`
	MyResponse   string `json:"my_response,omitempty"`
}

type apiMessageCalendarEvent struct {
	UID         string `json:"uid,omitempty"`
	Summary     string `json:"summary"`
	Description string `json:"description,omitempty"`
	Location    string `json:"location,omitempty"`
	URL         string `json:"url,omitempty"`
	// StartAt and EndAt are RFC 3339 instants. For an all-day event they are
	// the UTC midnights the calendar stores one with, the end exclusive; for a
	// floating event they carry no offset and are read in the viewer's zone.
	StartAt     string                     `json:"start_at"`
	EndAt       string                     `json:"end_at"`
	AllDay      bool                       `json:"all_day"`
	Floating    bool                       `json:"floating,omitempty"`
	TimeZone    string                     `json:"time_zone,omitempty"`
	Status      string                     `json:"status,omitempty"`
	Recurring   bool                       `json:"recurring,omitempty"`
	Organizer   *apiMessageCalendarPerson  `json:"organizer,omitempty"`
	Attendees   []apiMessageCalendarPerson `json:"attendees"`
	InCalendars []apiMessageCalendarMatch  `json:"in_calendars"`
	// CanReply says the invitation asks for an answer the reader can give by
	// mail (POST /api/messages/:id/calendar/reply). An event a calendar
	// already holds is answered there instead, through its provider.
	CanReply bool `json:"can_reply,omitempty"`
}

type apiMessageCalendar struct {
	// Method is the iTIP method: REQUEST for an invitation, CANCEL for a
	// cancellation, PUBLISH (or empty) for an event that was only shared.
	Method string                    `json:"method,omitempty"`
	Events []apiMessageCalendarEvent `json:"events"`
	// CanAdd says whether the reader has a calendar a new event could go
	// into; without one the card shows the event and offers nothing.
	CanAdd bool `json:"can_add"`
}

// apiMessageCalendar answers GET /api/messages/:id/calendar.
func (s *Server) apiMessageCalendar(w http.ResponseWriter, r *http.Request, id int64) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	cu, ok := s.requireAPIAuth(w, r)
	if !ok {
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
	parts, readable, err := s.messageCalendarParts(r.Context(), userID, msg)
	if !readable {
		writeAPIError(w, http.StatusGone, "The message could not be read from the mail server.")
		return
	}
	if err != nil && len(parts) == 0 {
		writeAPIError(w, http.StatusUnprocessableEntity, "The calendar attached to this message could not be read.")
		return
	}
	out := apiMessageCalendar{Events: []apiMessageCalendarEvent{}}
	events := calendarEventsFromParts(parts, &out.Method)
	if len(events) == 0 {
		writeJSON(w, out)
		return
	}
	calendars, err := s.store.ListCalendars(r.Context(), userID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	names := map[int64]string{}
	for _, calendar := range calendars {
		names[calendar.ID] = calendar.Summary
		if calendar.Listed && calendar.CanWrite() {
			out.CanAdd = true
		}
	}
	out.CanAdd = out.CanAdd && s.calendarRouter().Available()
	uids := make([]string, 0, len(events))
	for _, event := range events {
		uids = append(uids, event.UID)
	}
	held, err := s.store.ListCalendarEventsByICalUID(r.Context(), userID, uids)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	own := s.ownAddresses(r.Context(), cu.User)
	for _, event := range events {
		presented := presentMessageCalendarEvent(event, held, names)
		presented.CanReply = s.sender != nil && invitationAnswerable(out.Method, event, own)
		out.Events = append(out.Events, presented)
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, out)
}

// messageCalendarParts finds the calendar objects of a message. The local copy
// is streamed through the MIME walk rather than read whole, so a large
// attachment beside the invitation is scanned and never held in memory; only a
// message with no local copy is fetched, through rawMessageBytes. readable is
// false when the message could not be read at all.
func (s *Server) messageCalendarParts(ctx context.Context, userID int64, msg store.MessageRecord) (parts []mailparse.CalendarPart, readable bool, err error) {
	if s.blobs != nil && strings.TrimSpace(msg.BlobPath) != "" {
		if f, openErr := s.blobs.OpenUserBlob(userID, msg.BlobPath); openErr == nil {
			defer f.Close()
			parts, err = mailparse.CalendarParts(f)
			return parts, true, err
		}
	}
	raw, rawErr := s.rawMessageBytes(ctx, userID, msg)
	if rawErr != nil {
		return nil, false, rawErr
	}
	parts, err = mailparse.CalendarParts(bytes.NewReader(raw))
	return parts, true, err
}

// calendarEventsFromParts reads every part and drops the repeats: Google sends
// an invitation twice, as the body alternative and as invite.ics, and the
// reader should see one event. The method is the first one any part names.
func calendarEventsFromParts(parts []mailparse.CalendarPart, method *string) []icalendar.Event {
	var events []icalendar.Event
	seen := map[string]bool{}
	for _, part := range parts {
		cal, err := icalendar.Parse(part.Data)
		if err != nil {
			continue
		}
		if *method == "" {
			*method = cal.Method
		}
		for _, event := range cal.Events {
			key := event.UID + "\x00" + event.Start.Format(time.RFC3339) + "\x00" + event.Summary
			if seen[key] {
				continue
			}
			seen[key] = true
			events = append(events, event)
		}
	}
	return events
}

func presentMessageCalendarEvent(event icalendar.Event, held []store.CalendarEvent, names map[int64]string) apiMessageCalendarEvent {
	out := apiMessageCalendarEvent{
		UID:         event.UID,
		Summary:     event.Summary,
		Description: event.Description,
		Location:    event.Location,
		URL:         event.URL,
		StartAt:     calendarCardTime(event.Start, event.Floating),
		EndAt:       calendarCardTime(event.End, event.Floating),
		AllDay:      event.AllDay,
		Floating:    event.Floating,
		TimeZone:    event.TimeZone,
		Status:      event.Status,
		Recurring:   event.Recurring,
		Attendees:   []apiMessageCalendarPerson{},
		InCalendars: []apiMessageCalendarMatch{},
	}
	if event.Organizer.Email != "" || event.Organizer.Name != "" {
		out.Organizer = &apiMessageCalendarPerson{Name: event.Organizer.Name, Email: event.Organizer.Email}
	}
	for _, attendee := range event.Attendees {
		out.Attendees = append(out.Attendees, apiMessageCalendarPerson{
			Name:   attendee.Name,
			Email:  attendee.Email,
			Status: calendarPartStatResponses[attendee.PartStat],
		})
	}
	if event.UID == "" {
		return out
	}
	// One entry per calendar. A series is one row per occurrence, so the
	// occurrence this message is about is preferred, then the next one to
	// come, then the first the calendar holds.
	best := map[int64]store.CalendarEvent{}
	now := time.Now()
	rank := func(candidate store.CalendarEvent) int {
		switch {
		case !event.Floating && candidate.StartAt.Equal(event.Start):
			return 0
		case !candidate.EndAt.Before(now):
			return 1
		default:
			return 2
		}
	}
	for _, candidate := range held {
		if candidate.ICalUID != event.UID || candidate.Status == store.CalendarEventStatusCancelled {
			continue
		}
		current, ok := best[candidate.CalendarID]
		if !ok || rank(candidate) < rank(current) {
			best[candidate.CalendarID] = candidate
		}
	}
	for calendarID, match := range best {
		out.InCalendars = append(out.InCalendars, apiMessageCalendarMatch{
			EventID:      match.ID,
			CalendarID:   calendarID,
			CalendarName: names[calendarID],
			StartAt:      match.StartAt.UTC().Format(time.RFC3339),
			MyResponse:   match.MyResponse,
		})
	}
	sort.Slice(out.InCalendars, func(i, j int) bool {
		return out.InCalendars[i].CalendarID < out.InCalendars[j].CalendarID
	})
	return out
}

// calendarCardTime writes an instant for the card. A floating time has no
// instant, so it is written without an offset, which a browser reads as its
// own wall clock -- the meaning a floating time has.
func calendarCardTime(at time.Time, floating bool) string {
	if at.IsZero() {
		return ""
	}
	if floating {
		return at.UTC().Format("2006-01-02T15:04:05")
	}
	return at.UTC().Format(time.RFC3339)
}
