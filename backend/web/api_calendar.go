// File overview: The calendar surface: which calendars exist, which of them are
// drawn, what happens in a range of time, and the writes that travel to Google.
// The sync and the write-back live in backend/googlecalendar; this file only
// decides who may ask for them and how a failure is described to the user.

package web

import (
	"context"
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"rolltop/backend/googlecalendar"
	"rolltop/backend/store"
)

// maxEventRangeDays bounds one range request. The week view asks for a week and
// the month view will ask for six; anything past a year is a client bug or an
// attempt to make the server assemble the whole mirror in one response.
const maxEventRangeDays = 400

type apiCalendar struct {
	ID int64 `json:"id"`
	// ConnectionID and ConnectionEmail name the Google account a calendar came
	// from, which is what tells two identically named calendars apart.
	ConnectionID    int64  `json:"connection_id"`
	ConnectionEmail string `json:"connection_email"`
	Name            string `json:"name"`
	Description     string `json:"description"`
	TimeZone        string `json:"time_zone"`
	Color           string `json:"color"`
	AccessRole      string `json:"access_role"`
	CanWrite        bool   `json:"can_write"`
	IsPrimary       bool   `json:"is_primary"`
	Selected        bool   `json:"selected"`
	// SyncedFrom is the oldest point the mirror covers. An empty week before it
	// means "not synced", not "nothing scheduled".
	SyncedFrom   string `json:"synced_from"`
	LastSyncAt   string `json:"last_sync_at"`
	Status       string `json:"status"`
	StatusDetail string `json:"status_detail"`
}

type apiCalendarAttendee struct {
	Email     string `json:"email"`
	Name      string `json:"name"`
	Response  string `json:"response"`
	Optional  bool   `json:"optional"`
	Organizer bool   `json:"organizer"`
	Self      bool   `json:"self"`
	Resource  bool   `json:"resource"`
}

type apiCalendarEvent struct {
	ID         int64  `json:"id"`
	CalendarID int64  `json:"calendar_id"`
	Summary    string `json:"summary"`
	// Description carries the organizer's notes and may contain HTML, which is
	// why the client renders it as text rather than as markup.
	Description string `json:"description"`
	Location    string `json:"location"`
	Status      string `json:"status"`
	// StartAt and EndAt are RFC 3339. For an all-day event they are the UTC
	// midnights of Google's plain dates and must be read in UTC.
	StartAt          string                `json:"start_at"`
	EndAt            string                `json:"end_at"`
	AllDay           bool                  `json:"all_day"`
	TimeZone         string                `json:"time_zone"`
	RecurringEventID string                `json:"recurring_event_id"`
	OrganizerEmail   string                `json:"organizer_email"`
	OrganizerName    string                `json:"organizer_name"`
	Attendees        []apiCalendarAttendee `json:"attendees"`
	MyResponse       string                `json:"my_response"`
	HTMLLink         string                `json:"html_link"`
}

// apiGoogleCalendarSync is the per-connection calendar sync state shown in
// settings. It never carries a sync token: that is a cursor Rolltop keeps for
// itself, and nothing in the UI can act on it.
type apiGoogleCalendarSync struct {
	Status        string `json:"status"`
	StatusDetail  string `json:"status_detail"`
	LastSyncAt    string `json:"last_sync_at"`
	LastSuccess   string `json:"last_success_at"`
	CalendarCount int    `json:"calendar_count"`
	// EverSynced separates "no calendars yet" from "never ran", which look the
	// same in the counter but mean very different things to a user.
	EverSynced bool `json:"ever_synced"`
}

func apiCalendarFromStore(calendar store.Calendar, email string) apiCalendar {
	return apiCalendar{
		ID:              calendar.ID,
		ConnectionID:    calendar.GoogleConnectionID,
		ConnectionEmail: email,
		Name:            calendar.Summary,
		Description:     calendar.Description,
		TimeZone:        calendar.TimeZone,
		Color:           calendar.Color,
		AccessRole:      calendar.AccessRole,
		CanWrite:        calendar.CanWrite(),
		IsPrimary:       calendar.IsPrimary,
		Selected:        calendar.Selected,
		SyncedFrom:      timeString(calendar.WindowStartAt),
		LastSyncAt:      timeString(calendar.LastSyncAt),
		Status:          calendar.Status,
		StatusDetail:    calendar.StatusDetail,
	}
}

func apiCalendarEventFromStore(event store.CalendarEvent) apiCalendarEvent {
	attendees := make([]apiCalendarAttendee, 0, len(event.Attendees))
	for _, attendee := range event.Attendees {
		attendees = append(attendees, apiCalendarAttendee{
			Email:     attendee.Email,
			Name:      attendee.Name,
			Response:  attendee.Response,
			Optional:  attendee.Optional,
			Organizer: attendee.Organizer,
			Self:      attendee.Self,
			Resource:  attendee.Resource,
		})
	}
	return apiCalendarEvent{
		ID:               event.ID,
		CalendarID:       event.CalendarID,
		Summary:          event.Summary,
		Description:      event.Description,
		Location:         event.Location,
		Status:           event.Status,
		StartAt:          timeString(event.StartAt),
		EndAt:            timeString(event.EndAt),
		AllDay:           event.AllDay,
		TimeZone:         event.TimeZone,
		RecurringEventID: event.RecurringEventID,
		OrganizerEmail:   event.OrganizerEmail,
		OrganizerName:    event.OrganizerName,
		Attendees:        attendees,
		MyResponse:       event.MyResponse,
		HTMLLink:         event.HTMLLink,
	}
}

// apiCalendarPath routes everything under /api/calendar/.
func (s *Server) apiCalendarPath(w http.ResponseWriter, r *http.Request, rest string) {
	cu, ok := s.requireAPIAuth(w, r)
	if !ok {
		return
	}
	head, tail, _ := strings.Cut(rest, "/")
	switch head {
	case "calendars":
		if tail == "" {
			s.apiCalendars(w, r, cu.User.ID)
			return
		}
		s.apiCalendarByID(w, r, cu.User.ID, tail)
	case "events":
		if tail == "" {
			s.apiCalendarEvents(w, r, cu.User.ID)
			return
		}
		s.apiCalendarEventByID(w, r, cu.User.ID, tail)
	default:
		http.NotFound(w, r)
	}
}

// apiCalendars lists every subscribed calendar of every connected account.
func (s *Server) apiCalendars(w http.ResponseWriter, r *http.Request, userID int64) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	calendars, err := s.store.ListCalendars(r.Context(), userID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	writeJSON(w, map[string]any{"calendars": s.presentCalendars(r.Context(), userID, calendars)})
}

// presentCalendars attaches the Google address each calendar came from. A
// failure to read the connections costs the labels, not the list: the week view
// still has everything it needs to draw.
func (s *Server) presentCalendars(ctx context.Context, userID int64, calendars []store.Calendar) []apiCalendar {
	emails := map[int64]string{}
	if s.googleAuth != nil {
		connections, err := s.googleAuth.List(ctx, userID)
		if err != nil {
			log.Printf("calendar list user_id=%d read connections: %v", userID, err)
		}
		for _, connection := range connections {
			emails[connection.ID] = connection.GoogleEmail
		}
	}
	out := make([]apiCalendar, 0, len(calendars))
	for _, calendar := range calendars {
		out = append(out, apiCalendarFromStore(calendar, emails[calendar.GoogleConnectionID]))
	}
	return out
}

// apiCalendarByID switches one calendar's visibility.
func (s *Server) apiCalendarByID(w http.ResponseWriter, r *http.Request, userID int64, rest string) {
	calendarID, ok := parsePositiveID(w, rest)
	if !ok {
		return
	}
	if r.Method != http.MethodPut {
		methodNotAllowed(w)
		return
	}
	if !s.verifyCSRF(w, r) {
		return
	}
	var in struct {
		Selected bool `json:"selected"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	if err := s.store.SetCalendarSelected(r.Context(), userID, calendarID, in.Selected); err != nil {
		if store.IsNotFound(err) {
			http.NotFound(w, r)
			return
		}
		s.serverError(w, r, err)
		return
	}
	calendar, err := s.store.Calendar(r.Context(), userID, calendarID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	// A calendar switched on for the first time has no events at all, and
	// waiting for the next poll would show the user an empty week they would
	// read as "nothing scheduled". A sync failure is not fatal here: the
	// visibility change itself succeeded and the poll will retry.
	if in.Selected && calendar.LastSuccessAt.IsZero() && s.googleCalendar != nil {
		if err := s.googleCalendar.SyncCalendar(r.Context(), userID, calendarID); err != nil {
			log.Printf("calendar first sync user_id=%d calendar_id=%d: %v", userID, calendarID, err)
		}
		if refreshed, err := s.store.Calendar(r.Context(), userID, calendarID); err == nil {
			calendar = refreshed
		}
	}
	writeJSON(w, map[string]any{
		"calendar": apiCalendarFromStore(calendar, s.connectionEmail(r.Context(), userID, calendar.GoogleConnectionID)),
	})
}

// apiCalendarEvents answers a range query and creates events.
func (s *Server) apiCalendarEvents(w http.ResponseWriter, r *http.Request, userID int64) {
	switch r.Method {
	case http.MethodGet:
		s.calendarEventRange(w, r, userID)
	case http.MethodPost:
		s.calendarEventCreate(w, r, userID)
	default:
		methodNotAllowed(w)
	}
}

// calendarEventRange returns the events of the visible calendars that overlap
// the requested range.
func (s *Server) calendarEventRange(w http.ResponseWriter, r *http.Request, userID int64) {
	query := r.URL.Query()
	from, okFrom := parseRangeBound(query.Get("from"))
	to, okTo := parseRangeBound(query.Get("to"))
	if !okFrom || !okTo || !to.After(from) {
		writeAPIError(w, http.StatusBadRequest, "A calendar range needs a from and a to.")
		return
	}
	if to.Sub(from) > maxEventRangeDays*24*time.Hour {
		writeAPIError(w, http.StatusBadRequest, "That range is too long.")
		return
	}
	calendars, err := s.store.ListCalendars(r.Context(), userID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	// The server decides which calendars are visible rather than trusting a
	// list of ids from the query: the switches are stored state, and letting a
	// request name calendars would be a second, disagreeing source of truth.
	visible := make([]int64, 0, len(calendars))
	for _, calendar := range calendars {
		if calendar.Selected {
			visible = append(visible, calendar.ID)
		}
	}
	events, err := s.store.ListCalendarEventsInRange(r.Context(), userID, visible, from, to)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	out := make([]apiCalendarEvent, 0, len(events))
	for _, event := range events {
		out = append(out, apiCalendarEventFromStore(event))
	}
	writeJSON(w, map[string]any{"events": out})
}

// calendarEventInput is what the event dialog submits.
type calendarEventInput struct {
	CalendarID  int64  `json:"calendar_id"`
	Summary     string `json:"summary"`
	Description string `json:"description"`
	Location    string `json:"location"`
	StartAt     string `json:"start_at"`
	EndAt       string `json:"end_at"`
	AllDay      bool   `json:"all_day"`
	TimeZone    string `json:"time_zone"`
	Attendees   []struct {
		Email    string `json:"email"`
		Name     string `json:"name"`
		Optional bool   `json:"optional"`
	} `json:"attendees"`
}

// toStoreEvent validates the submitted event and renders it as a stored row.
func (in calendarEventInput) toStoreEvent() (store.CalendarEvent, string) {
	start, okStart := parseRangeBound(in.StartAt)
	end, okEnd := parseRangeBound(in.EndAt)
	if !okStart || !okEnd {
		return store.CalendarEvent{}, "An event needs a start and an end."
	}
	if !end.After(start) {
		return store.CalendarEvent{}, "An event has to end after it starts."
	}
	if strings.TrimSpace(in.Summary) == "" {
		return store.CalendarEvent{}, "An event needs a title."
	}
	event := store.CalendarEvent{
		Summary:     strings.TrimSpace(in.Summary),
		Description: in.Description,
		Location:    strings.TrimSpace(in.Location),
		StartAt:     start,
		EndAt:       end,
		AllDay:      in.AllDay,
		TimeZone:    strings.TrimSpace(in.TimeZone),
	}
	for _, attendee := range in.Attendees {
		email := strings.TrimSpace(attendee.Email)
		if email == "" {
			continue
		}
		event.Attendees = append(event.Attendees, store.CalendarAttendee{
			Email:    email,
			Name:     strings.TrimSpace(attendee.Name),
			Optional: attendee.Optional,
		})
	}
	return event, ""
}

func (s *Server) calendarEventCreate(w http.ResponseWriter, r *http.Request, userID int64) {
	if !s.verifyCSRF(w, r) {
		return
	}
	if s.googleCalendar == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "Calendar sync is not available on this server.")
		return
	}
	var in calendarEventInput
	if !decodeJSON(w, r, &in) {
		return
	}
	event, problem := in.toStoreEvent()
	if problem != "" {
		writeAPIError(w, http.StatusBadRequest, problem)
		return
	}
	if in.CalendarID <= 0 {
		writeAPIError(w, http.StatusBadRequest, "An event needs a calendar.")
		return
	}
	created, err := s.googleCalendar.CreateRemoteEvent(r.Context(), userID, in.CalendarID, event)
	if err != nil {
		s.writeCalendarError(w, r, err)
		return
	}
	writeJSON(w, map[string]any{"event": apiCalendarEventFromStore(created)})
}

// apiCalendarEventByID handles the per-event operations.
func (s *Server) apiCalendarEventByID(w http.ResponseWriter, r *http.Request, userID int64, rest string) {
	idPart, action, _ := strings.Cut(rest, "/")
	eventID, ok := parsePositiveID(w, idPart)
	if !ok {
		return
	}
	switch {
	case action == "respond" && r.Method == http.MethodPost:
		s.calendarEventRespond(w, r, userID, eventID)
	case action == "" && r.Method == http.MethodPut:
		s.calendarEventUpdate(w, r, userID, eventID)
	case action == "" && r.Method == http.MethodDelete:
		s.calendarEventDelete(w, r, userID, eventID)
	default:
		methodNotAllowed(w)
	}
}

func (s *Server) calendarEventUpdate(w http.ResponseWriter, r *http.Request, userID, eventID int64) {
	if !s.verifyCSRF(w, r) {
		return
	}
	if s.googleCalendar == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "Calendar sync is not available on this server.")
		return
	}
	var in calendarEventInput
	if !decodeJSON(w, r, &in) {
		return
	}
	edited, problem := in.toStoreEvent()
	if problem != "" {
		writeAPIError(w, http.StatusBadRequest, problem)
		return
	}
	existing, err := s.store.CalendarEvent(r.Context(), userID, eventID)
	if err != nil {
		if store.IsNotFound(err) {
			http.NotFound(w, r)
			return
		}
		s.serverError(w, r, err)
		return
	}
	// Moving an event between calendars is a Google operation of its own and
	// the dialog does not offer it, so the stored calendar wins over anything
	// the request claims.
	edited.CalendarID = existing.CalendarID
	edited.ExternalID = existing.ExternalID
	updated, err := s.googleCalendar.UpdateRemoteEvent(r.Context(), userID, existing, edited)
	if errors.Is(err, googlecalendar.ErrRemoteChanged) {
		// The user's edit lost, and the version that won is what they need to
		// see. 409 with the winning event is more useful than a bare error.
		writeJSONStatus(w, http.StatusConflict, map[string]any{
			"error": "This event was changed in Google while you were editing it. The Google version is now shown.",
			"event": apiCalendarEventFromStore(updated),
		})
		return
	}
	if err != nil {
		s.writeCalendarError(w, r, err)
		return
	}
	writeJSON(w, map[string]any{"event": apiCalendarEventFromStore(updated)})
}

func (s *Server) calendarEventDelete(w http.ResponseWriter, r *http.Request, userID, eventID int64) {
	if !s.verifyCSRF(w, r) {
		return
	}
	if s.googleCalendar == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "Calendar sync is not available on this server.")
		return
	}
	event, err := s.store.CalendarEvent(r.Context(), userID, eventID)
	if err != nil {
		if store.IsNotFound(err) {
			http.NotFound(w, r)
			return
		}
		s.serverError(w, r, err)
		return
	}
	if err := s.googleCalendar.DeleteRemoteEvent(r.Context(), userID, event); err != nil {
		if errors.Is(err, googlecalendar.ErrRemoteDeleted) {
			// Already gone at Google is the end state the user asked for.
			writeJSON(w, map[string]any{"ok": true})
			return
		}
		s.writeCalendarError(w, r, err)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) calendarEventRespond(w http.ResponseWriter, r *http.Request, userID, eventID int64) {
	if !s.verifyCSRF(w, r) {
		return
	}
	if s.googleCalendar == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "Calendar sync is not available on this server.")
		return
	}
	var in struct {
		Response string `json:"response"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	event, err := s.store.CalendarEvent(r.Context(), userID, eventID)
	if err != nil {
		if store.IsNotFound(err) {
			http.NotFound(w, r)
			return
		}
		s.serverError(w, r, err)
		return
	}
	answered, err := s.googleCalendar.RespondToRemoteEvent(r.Context(), userID, event, strings.TrimSpace(in.Response))
	if errors.Is(err, googlecalendar.ErrRemoteChanged) {
		writeJSONStatus(w, http.StatusConflict, map[string]any{
			"error": "This event was changed in Google while you were answering it. The Google version is now shown.",
			"event": apiCalendarEventFromStore(answered),
		})
		return
	}
	if err != nil {
		s.writeCalendarError(w, r, err)
		return
	}
	writeJSON(w, map[string]any{"event": apiCalendarEventFromStore(answered)})
}

// googleCalendarSyncState assembles the calendar sync state of one connection.
// A store failure is logged and reported as no state rather than failing the
// whole connections list, which the settings page needs to render either way.
func (s *Server) googleCalendarSyncState(ctx context.Context, userID, connectionID int64) *apiGoogleCalendarSync {
	if s.store == nil {
		return nil
	}
	state, err := s.store.GetGoogleCalendarSync(ctx, userID, connectionID)
	if err != nil {
		log.Printf("google calendar sync state user_id=%d connection_id=%d: %v", userID, connectionID, err)
		return nil
	}
	count, err := s.store.CountCalendarsForConnection(ctx, userID, connectionID)
	if err != nil {
		log.Printf("google calendar count user_id=%d connection_id=%d: %v", userID, connectionID, err)
	}
	return &apiGoogleCalendarSync{
		Status:        state.Status,
		StatusDetail:  state.StatusDetail,
		LastSyncAt:    timeString(state.LastSyncAt),
		LastSuccess:   timeString(state.LastSuccessAt),
		CalendarCount: count,
		EverSynced:    !state.LastSuccessAt.IsZero(),
	}
}

// googleCalendarSyncNow runs a sync for one connection and answers with the
// state the settings page should now show.
//
// It runs inline rather than in the background: the user pressed a button and
// is waiting for the answer, and the sync already bounds itself.
func (s *Server) googleCalendarSyncNow(w http.ResponseWriter, r *http.Request, userID, connectionID int64) {
	if !s.verifyCSRF(w, r) {
		return
	}
	if s.googleCalendar == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "Calendar sync is not available on this server.")
		return
	}
	result, err := s.googleCalendar.SyncConnection(r.Context(), userID, connectionID)
	if store.IsNotFound(err) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.writeCalendarError(w, r, err)
		return
	}
	writeJSON(w, map[string]any{
		"ok":         true,
		"calendars":  result.Calendars,
		"created":    result.Created,
		"updated":    result.Updated,
		"deleted":    result.Deleted,
		"full_sync":  result.FullSync,
		"sync_state": s.googleCalendarSyncState(r.Context(), userID, connectionID),
	})
}

// writeCalendarError maps a sync or write-back failure onto a status code and a
// message the user can act on. Upstream error text is never forwarded: on a
// write it echoes the event's own title, notes and guest list back.
func (s *Server) writeCalendarError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	// A disabled API is a server-configuration fault, not something the account
	// holder can grant their way out of, so it must not be answered with the
	// reconnect instruction they have probably just followed.
	case errors.Is(err, googlecalendar.ErrServiceDisabled):
		writeAPIError(w, http.StatusConflict,
			"The Google Calendar API is switched off for the Google Cloud project this connection's OAuth client belongs to. Enable it there; reconnecting does not help.")
	case errors.Is(err, googlecalendar.ErrScopeMissing), errors.Is(err, googlecalendar.ErrForbidden):
		writeAPIError(w, http.StatusConflict,
			"This Google account has not granted access to calendars. Reconnect it in Google settings.")
	case errors.Is(err, googlecalendar.ErrReadOnlyCalendar):
		writeAPIError(w, http.StatusForbidden, "This calendar is shared read-only.")
	case errors.Is(err, googlecalendar.ErrNotAnInvitation):
		writeAPIError(w, http.StatusBadRequest, "There is no invitation to answer on this event.")
	case errors.Is(err, googlecalendar.ErrRemoteDeleted):
		writeAPIError(w, http.StatusConflict,
			"This event was deleted in Google while you were editing it, so the change was not saved.")
	case errors.Is(err, googlecalendar.ErrRemoteChanged):
		writeAPIError(w, http.StatusConflict, "This event was changed in Google while you were editing it.")
	// A bare conflict is a write Google refused on a stale etag that no caller
	// resolved into one of the two above -- a delete is the case that reaches
	// here. Without this it would fall through to the server-error branch and
	// be logged as an internal fault, which it is not: the event simply moved
	// on since the last poll.
	case errors.Is(err, googlecalendar.ErrConflict):
		writeAPIError(w, http.StatusConflict,
			"This event was changed in Google since it was last synced. Reload the week and try again.")
	case errors.Is(err, googlecalendar.ErrUnauthorized):
		writeAPIError(w, http.StatusConflict, "This Google account needs to be authorized again.")
	case errors.Is(err, googlecalendar.ErrNotFound), store.IsNotFound(err):
		http.NotFound(w, r)
	case errors.Is(err, googlecalendar.ErrUpstream), errors.Is(err, googlecalendar.ErrSyncTokenExpired):
		writeAPIError(w, http.StatusBadGateway, "Google could not be reached.")
	case errors.Is(err, context.DeadlineExceeded):
		writeAPIError(w, http.StatusGatewayTimeout, "The Google request took too long.")
	default:
		s.serverError(w, r, err)
	}
}

// connectionEmail labels one calendar with the account it came from. A lookup
// failure costs the label, not the response.
func (s *Server) connectionEmail(ctx context.Context, userID, connectionID int64) string {
	if s.googleAuth == nil || connectionID <= 0 {
		return ""
	}
	connection, err := s.googleAuth.Get(ctx, userID, connectionID)
	if err != nil {
		return ""
	}
	return connection.GoogleEmail
}

// parseRangeBound reads an RFC 3339 timestamp from the query or a request body.
func parseRangeBound(value string) (time.Time, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, false
	}
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}, false
	}
	return parsed.UTC(), true
}

func parsePositiveID(w http.ResponseWriter, raw string) (int64, bool) {
	id, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil || id <= 0 {
		writeAPIError(w, http.StatusBadRequest, "Invalid id.")
		return 0, false
	}
	return id, true
}
