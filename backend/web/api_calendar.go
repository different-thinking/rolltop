// File overview: The calendar surface: which calendars exist, which of them are
// drawn, what happens in a range of time, and the writes that travel to the
// provider a calendar belongs to -- Google or Microsoft 365. The sync and the
// write-back live in backend/googlecalendar and backend/m365calendar, and
// calendarlink routes between them; this file only decides who may ask for
// them and how a failure is described to the user.

package web

import (
	"context"
	"errors"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"rolltop/backend/calendarlink"
	"rolltop/backend/googlecalendar"
	"rolltop/backend/m365calendar"
	"rolltop/backend/store"
)

// maxEventRangeDays bounds one range request. The week view asks for a week and
// the month view will ask for six; anything past a year is a client bug or an
// attempt to make the server assemble the whole mirror in one response.
const maxEventRangeDays = 400

type apiCalendar struct {
	ID int64 `json:"id"`
	// Provider is "google" or "microsoft". ConnectionID is an id within that
	// provider's connections, so the two together name the account.
	Provider string `json:"provider"`
	// ConnectionID and ConnectionEmail name the account a calendar came from,
	// which is what tells two identically named calendars apart.
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
	// CopyTarget marks the reader's second calendar, the one a new event can
	// be copied into as well.
	CopyTarget bool `json:"copy_target"`
	// BusyTarget marks the reader's busy calendar, the one a new event can be
	// copied into as a placeholder; BusyLabel is the placeholder's title as
	// the reader set it, empty for the default (BusyTitle).
	BusyTarget bool   `json:"busy_target"`
	BusyLabel  string `json:"busy_label"`
	BusyTitle  string `json:"busy_title"`
	// Listed says whether the calendar view lists the calendar at all. One
	// that is not listed is never selected and never the second calendar.
	Listed bool `json:"listed"`
	// OnlineMeetingProviders lists the online meeting kinds an event here can
	// be made -- a Microsoft calendar's Teams. Empty means none.
	OnlineMeetingProviders []string `json:"online_meeting_providers"`
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
	// LinkPrimary marks the copy of a linked pair that was entered, and the
	// one that carries its guest list.
	LinkPrimary bool `json:"link_primary"`
	// LinkMasked marks a placeholder in the busy calendar: the time of an
	// event entered elsewhere under a stand-in title.
	LinkMasked bool `json:"link_masked"`
	// AlsoIn lists the other calendars holding a copy of this event. The week
	// view draws a linked pair once, and this is how the entry it draws says
	// where else the appointment lives -- including a calendar that is
	// switched off.
	AlsoIn []apiCalendarEventCopy `json:"also_in"`
	// OnlineMeeting marks an event held online; OnlineMeetingURL is the link
	// that joins it (Teams, or Google Meet).
	OnlineMeeting         bool   `json:"online_meeting"`
	OnlineMeetingProvider string `json:"online_meeting_provider"`
	OnlineMeetingURL      string `json:"online_meeting_url"`
}

// apiCalendarEventCopy is one other copy of a linked event.
type apiCalendarEventCopy struct {
	CalendarID int64 `json:"calendar_id"`
	EventID    int64 `json:"event_id"`
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
	providers := calendar.OnlineMeetingProviders
	if providers == nil {
		providers = []string{}
	}
	provider := calendar.Provider
	if provider == "" {
		provider = store.CalendarProviderGoogle
	}
	return apiCalendar{
		ID:                     calendar.ID,
		Provider:               provider,
		OnlineMeetingProviders: providers,
		ConnectionID:           calendar.ConnectionID(),
		ConnectionEmail:        email,
		Name:                   calendar.Summary,
		Description:            calendar.Description,
		TimeZone:               calendar.TimeZone,
		Color:                  calendar.Color,
		AccessRole:             calendar.AccessRole,
		CanWrite:               calendar.CanWrite(),
		IsPrimary:              calendar.IsPrimary,
		Selected:               calendar.Selected,
		CopyTarget:             calendar.CopyTarget,
		BusyTarget:             calendar.BusyTarget,
		BusyLabel:              calendar.BusyLabel,
		BusyTitle:              calendar.PlaceholderTitle(),
		Listed:                 calendar.Listed,
		SyncedFrom:             timeString(calendar.WindowStartAt),
		LastSyncAt:             timeString(calendar.LastSyncAt),
		Status:                 calendar.Status,
		StatusDetail:           calendar.StatusDetail,
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
		LinkPrimary:      event.Linked() && event.LinkPrimary,
		LinkMasked:       event.Linked() && event.LinkMasked,
		AlsoIn:           []apiCalendarEventCopy{},

		OnlineMeeting:         event.OnlineMeeting,
		OnlineMeetingProvider: event.OnlineMeetingProvider,
		OnlineMeetingURL:      event.OnlineMeetingURL,
	}
}

// presentCalendarEvent renders one event a write answered with, naming its
// copies. Every single-event answer has to: the dialog reads also_in to decide
// whether the "also add to" box starts ticked, and an answer without it would
// have the next save delete a copy nobody asked to remove. A failed lookup
// costs the names, which the next week reload restores.
func (s *Server) presentCalendarEvent(ctx context.Context, userID int64, event store.CalendarEvent) apiCalendarEvent {
	copies, err := s.store.ListCalendarEventCopies(ctx, userID, event)
	if err != nil {
		log.Printf("calendar event copies user_id=%d event_id=%d: %v", userID, event.ID, err)
	}
	return apiCalendarEventWithCopies(event, copies)
}

// apiCalendarEventWithCopies renders an event together with the copies it
// stands for.
func apiCalendarEventWithCopies(event store.CalendarEvent, copies []store.CalendarEvent) apiCalendarEvent {
	out := apiCalendarEventFromStore(event)
	for _, copied := range copies {
		out.AlsoIn = append(out.AlsoIn, apiCalendarEventCopy{CalendarID: copied.CalendarID, EventID: copied.ID})
	}
	return out
}

// calendarRouter hands every calendar write to the provider of the calendar it
// concerns, which is what lets a week -- and one linked pair -- mix Google and
// Microsoft 365 calendars. It is assembled per call from the server's current
// backends; only a backend that exists is installed, because a nil pointer in
// the interface would read as configured and fail at the first write.
func (s *Server) calendarRouter() *calendarlink.Router {
	router := &calendarlink.Router{}
	if s.store != nil {
		router.Calendars = s.store
	}
	if s.googleCalendar != nil {
		router.Google = s.googleCalendar
	}
	if s.microsoftCalendar != nil {
		router.Microsoft = s.microsoftCalendar
	}
	return router
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

// presentCalendars attaches the account address each calendar came from. A
// failure to read the connections costs the labels, not the list: the week view
// still has everything it needs to draw.
func (s *Server) presentCalendars(ctx context.Context, userID int64, calendars []store.Calendar) []apiCalendar {
	googleEmails := map[int64]string{}
	if s.googleAuth != nil {
		connections, err := s.googleAuth.List(ctx, userID)
		if err != nil {
			log.Printf("calendar list user_id=%d read google connections: %v", userID, err)
		}
		for _, connection := range connections {
			googleEmails[connection.ID] = connection.GoogleEmail
		}
	}
	microsoftEmails := map[int64]string{}
	if s.microsoftAuth != nil {
		connections, err := s.microsoftAuth.List(ctx, userID)
		if err != nil {
			log.Printf("calendar list user_id=%d read microsoft connections: %v", userID, err)
		}
		for _, connection := range connections {
			microsoftEmails[connection.ID] = connection.Email
		}
	}
	out := make([]apiCalendar, 0, len(calendars))
	for _, calendar := range calendars {
		email := googleEmails[calendar.GoogleConnectionID]
		if calendar.IsMicrosoft() {
			email = microsoftEmails[calendar.MicrosoftConnectionID]
		}
		out = append(out, apiCalendarFromStore(calendar, email))
	}
	return out
}

// apiCalendarByID switches one calendar's visibility, lists or hides it in the
// calendar view, makes it the reader's second or busy calendar, or sets the
// title its placeholders carry. Every field may be left out; a request naming
// none changes nothing and answers with the calendar as it is.
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
		Selected   *bool   `json:"selected"`
		CopyTarget *bool   `json:"copy_target"`
		BusyTarget *bool   `json:"busy_target"`
		BusyLabel  *string `json:"busy_label"`
		Listed     *bool   `json:"listed"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	calendar, err := s.store.Calendar(r.Context(), userID, calendarID)
	if err != nil {
		if store.IsNotFound(err) {
			http.NotFound(w, r)
			return
		}
		s.serverError(w, r, err)
		return
	}
	// Only a listed calendar may be drawn or be the second calendar: the
	// calendar view offers both only for the calendars it lists, and a hidden
	// one carrying either would be a state nothing on screen could undo.
	listed := calendar.Listed
	if in.Listed != nil {
		listed = *in.Listed
	}
	if !listed && ((in.Selected != nil && *in.Selected) || (in.CopyTarget != nil && *in.CopyTarget) ||
		(in.BusyTarget != nil && *in.BusyTarget)) {
		writeAPIError(w, http.StatusBadRequest, calendarHiddenMessage)
		return
	}
	// One calendar holds an event either in full or as a placeholder, never
	// both; choosing it for one role takes the other away, and a request
	// asking for both at once is asking for neither.
	if in.CopyTarget != nil && *in.CopyTarget && in.BusyTarget != nil && *in.BusyTarget {
		writeAPIError(w, http.StatusBadRequest, "A calendar can be the second calendar or the busy calendar, not both.")
		return
	}
	if in.BusyLabel != nil && len([]rune(strings.TrimSpace(*in.BusyLabel))) > maxBusyLabelRunes {
		writeAPIError(w, http.StatusBadRequest, "That placeholder title is too long.")
		return
	}
	// A copy is a write, so a calendar shared read-only would turn every event
	// entered with a copy into a copy Google refuses. Refused before anything
	// is written, so a request that also lists the calendar changes nothing.
	if ((in.CopyTarget != nil && *in.CopyTarget) || (in.BusyTarget != nil && *in.BusyTarget)) && !calendar.CanWrite() {
		writeAPIError(w, http.StatusBadRequest, "This calendar is shared read-only, so events cannot be copied into it.")
		return
	}
	// The listing goes first, because hiding a calendar also switches it off
	// and clears the second-calendar mark; a request that hides it and names
	// either of those as false asks for nothing more.
	if in.Listed != nil {
		if err := s.store.SetCalendarListed(r.Context(), userID, calendarID, *in.Listed); err != nil {
			s.serverError(w, r, err)
			return
		}
	}
	// The store refuses either for a calendar hidden since it was read above,
	// which is the same answer the check above gives.
	if in.CopyTarget != nil {
		if err := s.store.SetCalendarCopyTarget(r.Context(), userID, calendarID, *in.CopyTarget); err != nil {
			s.writeCalendarSwitchError(w, r, err)
			return
		}
	}
	if in.BusyTarget != nil {
		if err := s.store.SetCalendarBusyTarget(r.Context(), userID, calendarID, *in.BusyTarget); err != nil {
			s.writeCalendarSwitchError(w, r, err)
			return
		}
	}
	if in.BusyLabel != nil {
		if err := s.store.SetCalendarBusyLabel(r.Context(), userID, calendarID, *in.BusyLabel); err != nil {
			s.writeCalendarSwitchError(w, r, err)
			return
		}
	}
	if in.Selected != nil {
		if err := s.store.SetCalendarSelected(r.Context(), userID, calendarID, *in.Selected); err != nil {
			s.writeCalendarSwitchError(w, r, err)
			return
		}
	}
	calendar, err = s.store.Calendar(r.Context(), userID, calendarID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	// A calendar switched on for the first time has no events at all, and
	// waiting for the next poll would show the user an empty week they would
	// read as "nothing scheduled". A sync failure is not fatal here: the
	// visibility change itself succeeded and the poll will retry.
	if in.Selected != nil && *in.Selected && calendar.LastSuccessAt.IsZero() && s.calendarRouter().Available() {
		if err := s.calendarRouter().SyncCalendar(r.Context(), userID, calendarID); err != nil {
			log.Printf("calendar first sync user_id=%d calendar_id=%d: %v", userID, calendarID, err)
		}
		if refreshed, err := s.store.Calendar(r.Context(), userID, calendarID); err == nil {
			calendar = refreshed
		}
	}
	writeJSON(w, map[string]any{
		"calendar": apiCalendarFromStore(calendar, s.calendarConnectionEmail(r.Context(), userID, calendar)),
	})
}

// maxBusyLabelRunes bounds a placeholder title. It is a word or two; refusing
// a longer one says so instead of letting the store cut it.
const maxBusyLabelRunes = 60

// calendarHiddenMessage refuses to draw a hidden calendar or make it the
// second calendar.
const calendarHiddenMessage = "This calendar is hidden in the calendar view. Show it there first."

// writeCalendarSwitchError answers a failed visibility or second-calendar
// write.
func (s *Server) writeCalendarSwitchError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, store.ErrCalendarHidden):
		writeAPIError(w, http.StatusBadRequest, calendarHiddenMessage)
	case store.IsNotFound(err):
		http.NotFound(w, r)
	default:
		s.serverError(w, r, err)
	}
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
	linked, err := s.store.ListLinkedCalendarEventsInRange(r.Context(), userID, from, to)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	writeJSON(w, map[string]any{"events": foldLinkedEvents(events, linked, calendars)})
}

// linkedGroupKey identifies one appointment kept in several calendars. The
// start is part of it because Google hands a link down to every occurrence of
// a series, and because a copy moved at Google is no longer the same
// appointment: it is drawn on its own rather than hidden behind the other.
func linkedGroupKey(event store.CalendarEvent) string {
	return event.LinkKey + "\x00" + strconv.FormatInt(event.StartAt.Unix(), 10)
}

// foldLinkedEvents draws each linked pair once. events are the visible
// calendars' events in drawing order; linked is every linked event in the
// range, from every calendar, which is what the drawn entry needs to name the
// copies it stands for.
//
// The entry drawn is the copy that was entered when its calendar is switched
// on -- it carries the guest list and the invitation answer -- and otherwise
// the copy in the first visible calendar, a busy calendar's placeholder only
// when no other copy is visible. Folding never hides an event whose
// group it cannot find: drawing a copy twice is recoverable, hiding the only
// one is not.
func foldLinkedEvents(events, linked []store.CalendarEvent, calendars []store.Calendar) []apiCalendarEvent {
	rank := make(map[int64]int, len(calendars))
	visible := make(map[int64]bool, len(calendars))
	for i, calendar := range calendars {
		rank[calendar.ID] = i
		visible[calendar.ID] = calendar.Selected
	}
	groups := map[string][]store.CalendarEvent{}
	for _, event := range linked {
		key := linkedGroupKey(event)
		groups[key] = append(groups[key], event)
	}
	for key, members := range groups {
		sort.SliceStable(members, func(i, j int) bool {
			a, b := members[i], members[j]
			if visible[a.CalendarID] != visible[b.CalendarID] {
				return visible[a.CalendarID]
			}
			// A placeholder stands for the event only when nothing else
			// of it is drawn: it carries none of what the dialog shows.
			if a.LinkMasked != b.LinkMasked {
				return !a.LinkMasked
			}
			if a.LinkPrimary != b.LinkPrimary {
				return a.LinkPrimary
			}
			if rank[a.CalendarID] != rank[b.CalendarID] {
				return rank[a.CalendarID] < rank[b.CalendarID]
			}
			return a.ID < b.ID
		})
		groups[key] = members
	}
	out := make([]apiCalendarEvent, 0, len(events))
	for _, event := range events {
		members := groups[linkedGroupKey(event)]
		if !event.Linked() || len(members) == 0 {
			out = append(out, apiCalendarEventFromStore(event))
			continue
		}
		head := members[0]
		if head.ID != event.ID {
			if head.CalendarID == event.CalendarID {
				// Two rows of one calendar are not a pair: an event
				// duplicated at Google carries the private link along, and
				// folding it would hide an appointment of its own.
				out = append(out, apiCalendarEventFromStore(event))
			}
			continue
		}
		copies := make([]store.CalendarEvent, 0, len(members)-1)
		for _, member := range members[1:] {
			if member.CalendarID != event.CalendarID {
				copies = append(copies, member)
			}
		}
		out = append(out, apiCalendarEventWithCopies(event, copies))
	}
	return out
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
	// Copy asks for the event to be kept in the reader's second calendar as
	// well (true) or no longer (false). Left out, an edit keeps whatever
	// copies the event has and a create makes none.
	Copy *bool `json:"copy"`
	// BusyCopy asks the same of the placeholder in the reader's busy calendar.
	BusyCopy *bool `json:"busy_copy"`
	// OnlineMeeting asks for the event to be held online -- a Teams meeting
	// in a Microsoft calendar. Once a meeting is online it stays so; false on
	// an edit does not take it back.
	OnlineMeeting bool `json:"online_meeting"`
}

// copyChange reads what one of the submission's copy fields asks.
func copyChange(asked *bool) calendarlink.CopyChange {
	switch {
	case asked == nil:
		return calendarlink.CopyKeep
	case *asked:
		return calendarlink.CopyAdd
	default:
		return calendarlink.CopyRemove
	}
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

		OnlineMeeting: in.OnlineMeeting,
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
	if !s.calendarRouter().Available() {
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
	// A hidden calendar is switched off and no longer synced, so an event
	// created there would exist at the provider and never appear in the week.
	// The picker only offers listed calendars; this is for a form a tab kept
	// open while the calendar was hidden in another one. A calendar that is not
	// found is left to the router, which answers for it as it always has.
	if calendar, err := s.store.Calendar(r.Context(), userID, in.CalendarID); err == nil && !calendar.Listed {
		writeAPIError(w, http.StatusBadRequest, "This calendar is hidden in the calendar view. Show it there first, or choose another calendar.")
		return
	} else if err != nil && !store.IsNotFound(err) {
		s.serverError(w, r, err)
		return
	}
	targets, ok := s.copyTargetsForWrite(w, r, userID, in, false)
	if !ok {
		return
	}
	created, problems, err := s.calendarRouter().CreateWithCopies(r.Context(), userID, in.CalendarID, targets, event)
	if err != nil {
		s.writeCalendarError(w, r, err)
		return
	}
	writeJSON(w, map[string]any{
		"event":    s.presentCalendarEvent(r.Context(), userID, created),
		"warnings": s.linkedProblemMessages(r.Context(), userID, problems),
	})
}

// copyTargetsForWrite resolves what a submission asks of the reader's second
// and busy calendars. Asking for a copy with no such calendar chosen is a stale
// form, not something to ignore: the reader would believe the copy was made.
// Taking a copy away needs no write access to check up front: the delete
// itself is refused on a read-only calendar and says so.
func (s *Server) copyTargetsForWrite(w http.ResponseWriter, r *http.Request, userID int64, in calendarEventInput, editing bool) ([]calendarlink.Target, bool) {
	kinds := []struct {
		change  calendarlink.CopyChange
		masked  bool
		lookup  func(context.Context, int64) (store.Calendar, error)
		missing string
		shared  string
	}{
		{copyChange(in.Copy), false, s.store.CopyTargetCalendar,
			"No second calendar is chosen to copy events into.",
			"Your second calendar is shared read-only, so nothing can be copied into it."},
		{copyChange(in.BusyCopy), true, s.store.BusyTargetCalendar,
			"No busy calendar is chosen to block time in.",
			"Your busy calendar is shared read-only, so no time can be blocked in it."},
	}
	targets := []calendarlink.Target{}
	for _, kind := range kinds {
		// A create only ever adds copies; there is nothing yet to remove.
		if kind.change == calendarlink.CopyKeep || (!editing && kind.change != calendarlink.CopyAdd) {
			continue
		}
		calendar, err := kind.lookup(r.Context(), userID)
		if store.IsNotFound(err) {
			if kind.change == calendarlink.CopyAdd {
				writeAPIError(w, http.StatusBadRequest, kind.missing)
				return nil, false
			}
			continue
		}
		if err != nil {
			s.serverError(w, r, err)
			return nil, false
		}
		if kind.change == calendarlink.CopyAdd && !calendar.CanWrite() {
			writeAPIError(w, http.StatusBadRequest, kind.shared)
			return nil, false
		}
		target := calendarlink.Target{CalendarID: calendar.ID, Masked: kind.masked, Change: kind.change}
		if kind.masked {
			target.Title = calendar.PlaceholderTitle()
		}
		targets = append(targets, target)
	}
	// A placeholder that is merely carried along keeps its busy calendar's
	// current title, so that calendar is named even when nothing is asked of
	// it. A create has no placeholder to carry.
	if editing && copyChange(in.BusyCopy) == calendarlink.CopyKeep {
		if busy, err := s.store.BusyTargetCalendar(r.Context(), userID); err == nil {
			targets = append(targets, calendarlink.Target{
				CalendarID: busy.ID, Masked: true, Title: busy.PlaceholderTitle(), Change: calendarlink.CopyKeep,
			})
		} else if !store.IsNotFound(err) {
			s.serverError(w, r, err)
			return nil, false
		}
	}
	return targets, true
}

// linkedProblemMessages describes the copies a write could not bring along.
// The write itself succeeded, so these travel beside its answer rather than as
// an error. The reason is chosen from the error's kind, never its text, for
// the same reason writeCalendarError never forwards Google's.
func (s *Server) linkedProblemMessages(ctx context.Context, userID int64, problems []calendarlink.LinkedProblem) []string {
	out := []string{}
	for _, problem := range problems {
		log.Printf("calendar linked copy user_id=%d calendar_id=%d action=%s: %v",
			userID, problem.CalendarID, problem.Action, problem.Err)
		name := "the other calendar"
		if calendar, err := s.store.Calendar(ctx, userID, problem.CalendarID); err == nil && strings.TrimSpace(calendar.Summary) != "" {
			name = "“" + calendar.Summary + "”"
		}
		var what string
		switch problem.Action {
		case calendarlink.LinkedCreate:
			what = "The event could not be copied to " + name
		case calendarlink.LinkedDelete:
			what = "The copy in " + name + " could not be deleted"
		default:
			what = "The copy in " + name + " could not be updated"
		}
		out = append(out, what+": "+linkedProblemReason(problem.Err))
	}
	return out
}

func linkedProblemReason(err error) string {
	provider := calendarProviderName(err)
	switch {
	case errors.Is(err, calendarlink.ErrReadOnlyCalendar):
		return "that calendar is shared read-only."
	case errors.Is(err, calendarlink.ErrRemoteDeleted):
		return "it was deleted in " + provider + "."
	case errors.Is(err, calendarlink.ErrRemoteChanged), errors.Is(err, calendarlink.ErrConflict):
		return "it was changed in " + provider + ", and that version was kept."
	case errors.Is(err, calendarlink.ErrUnauthorized):
		return "its " + provider + " account needs to be authorized again."
	case errors.Is(err, calendarlink.ErrScopeMissing), errors.Is(err, calendarlink.ErrForbidden):
		return "its " + provider + " account has not granted access to calendars."
	case errors.Is(err, calendarlink.ErrProviderUnavailable):
		return "its provider is not available on this server."
	case errors.Is(err, calendarlink.ErrUpstream), errors.Is(err, context.DeadlineExceeded):
		return provider + " could not be reached."
	default:
		return provider + " refused it."
	}
}

// calendarProviderName names the provider a failure came from, for a message.
// A failure Microsoft's package does not claim keeps the Google wording every
// message had before there were two providers.
func calendarProviderName(err error) string {
	if m365calendar.IsOwnError(err) {
		return "Microsoft 365"
	}
	return "Google"
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
	if !s.calendarRouter().Available() {
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
	targets, ok := s.copyTargetsForWrite(w, r, userID, in, true)
	if !ok {
		return
	}
	copies, err := s.store.ListCalendarEventCopies(r.Context(), userID, existing)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	updated, problems, err := s.calendarRouter().UpdateGroupTargets(r.Context(), userID, existing, edited, copies, targets)
	if errors.Is(err, calendarlink.ErrRemoteChanged) {
		// The user's edit lost, and the version that won is what they need to
		// see. 409 with the winning event is more useful than a bare error.
		provider := calendarProviderName(err)
		writeJSONStatus(w, http.StatusConflict, map[string]any{
			"error": "This event was changed in " + provider + " while you were editing it. The " + provider + " version is now shown.",
			"event": s.presentCalendarEvent(r.Context(), userID, updated),
		})
		return
	}
	if err != nil {
		s.writeCalendarError(w, r, err)
		return
	}
	writeJSON(w, map[string]any{
		"event":    s.presentCalendarEvent(r.Context(), userID, updated),
		"warnings": s.linkedProblemMessages(r.Context(), userID, problems),
	})
}

func (s *Server) calendarEventDelete(w http.ResponseWriter, r *http.Request, userID, eventID int64) {
	if !s.verifyCSRF(w, r) {
		return
	}
	if !s.calendarRouter().Available() {
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
	copies, err := s.store.ListCalendarEventCopies(r.Context(), userID, event)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	// Already gone at Google is the end state the user asked for, and is
	// absorbed inside the group delete so the copies still go with it.
	problems, err := s.calendarRouter().DeleteGroup(r.Context(), userID, event, copies)
	if err != nil {
		s.writeCalendarError(w, r, err)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "warnings": s.linkedProblemMessages(r.Context(), userID, problems)})
}

func (s *Server) calendarEventRespond(w http.ResponseWriter, r *http.Request, userID, eventID int64) {
	if !s.verifyCSRF(w, r) {
		return
	}
	if !s.calendarRouter().Available() {
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
	answered, err := s.calendarRouter().RespondToRemoteEvent(r.Context(), userID, event, strings.TrimSpace(in.Response))
	if errors.Is(err, calendarlink.ErrRemoteChanged) {
		provider := calendarProviderName(err)
		writeJSONStatus(w, http.StatusConflict, map[string]any{
			"error": "This event was changed in " + provider + " while you were answering it. The " + provider + " version is now shown.",
			"event": s.presentCalendarEvent(r.Context(), userID, answered),
		})
		return
	}
	if err != nil {
		s.writeCalendarError(w, r, err)
		return
	}
	writeJSON(w, map[string]any{"event": s.presentCalendarEvent(r.Context(), userID, answered)})
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
	provider := calendarProviderName(err)
	microsoft := provider == "Microsoft 365"
	switch {
	// A disabled API is a server-configuration fault, not something the account
	// holder can grant their way out of, so it must not be answered with the
	// reconnect instruction they have probably just followed.
	case errors.Is(err, googlecalendar.ErrServiceDisabled):
		writeAPIError(w, http.StatusConflict,
			"The Google Calendar API is switched off for the Google Cloud project this connection's OAuth client belongs to. Enable it there; reconnecting does not help.")
	case errors.Is(err, calendarlink.ErrProviderUnavailable):
		writeAPIError(w, http.StatusServiceUnavailable,
			"This calendar's provider is not configured on this server, so it cannot be changed here.")
	case errors.Is(err, calendarlink.ErrNoOnlineMeeting):
		writeAPIError(w, http.StatusBadRequest, "This calendar cannot hold Teams meetings.")
	case microsoft && errors.Is(err, calendarlink.ErrScopeMissing):
		writeAPIError(w, http.StatusConflict,
			"This Microsoft account has not granted access to calendars. Sign in again in Microsoft 365 settings.")
	case microsoft && errors.Is(err, calendarlink.ErrForbidden):
		writeAPIError(w, http.StatusConflict,
			"Microsoft 365 refused the request. Your organisation may not allow Rolltop to change this calendar.")
	case errors.Is(err, calendarlink.ErrScopeMissing), errors.Is(err, calendarlink.ErrForbidden):
		writeAPIError(w, http.StatusConflict,
			"This Google account has not granted access to calendars. Reconnect it in Google settings.")
	case errors.Is(err, calendarlink.ErrReadOnlyCalendar):
		writeAPIError(w, http.StatusForbidden, "This calendar is shared read-only.")
	case errors.Is(err, calendarlink.ErrNotAnInvitation):
		writeAPIError(w, http.StatusBadRequest, "There is no invitation to answer on this event.")
	case errors.Is(err, calendarlink.ErrRemoteDeleted):
		writeAPIError(w, http.StatusConflict,
			"This event was deleted in "+provider+" while you were editing it, so the change was not saved.")
	case errors.Is(err, calendarlink.ErrRemoteChanged):
		writeAPIError(w, http.StatusConflict, "This event was changed in "+provider+" while you were editing it.")
	// A bare conflict is a write refused on a stale version that no caller
	// resolved into one of the two above -- a delete is the case that reaches
	// here. Without this it would fall through to the server-error branch and
	// be logged as an internal fault, which it is not: the event simply moved
	// on since the last poll.
	case errors.Is(err, calendarlink.ErrConflict):
		writeAPIError(w, http.StatusConflict,
			"This event was changed in "+provider+" since it was last synced. Reload the week and try again.")
	case errors.Is(err, calendarlink.ErrUnauthorized):
		writeAPIError(w, http.StatusConflict, "This "+provider+" account needs to be authorized again.")
	case errors.Is(err, calendarlink.ErrNotFound), store.IsNotFound(err):
		http.NotFound(w, r)
	case errors.Is(err, calendarlink.ErrUpstream), errors.Is(err, googlecalendar.ErrSyncTokenExpired):
		writeAPIError(w, http.StatusBadGateway, provider+" could not be reached.")
	case errors.Is(err, context.DeadlineExceeded):
		writeAPIError(w, http.StatusGatewayTimeout, "The "+provider+" request took too long.")
	default:
		s.serverError(w, r, err)
	}
}

// calendarConnectionEmail labels one calendar with the account it came from.
// A lookup failure costs the label, not the response.
func (s *Server) calendarConnectionEmail(ctx context.Context, userID int64, calendar store.Calendar) string {
	if calendar.IsMicrosoft() {
		if s.microsoftAuth == nil || calendar.MicrosoftConnectionID <= 0 {
			return ""
		}
		connection, err := s.microsoftAuth.Get(ctx, userID, calendar.MicrosoftConnectionID)
		if err != nil {
			return ""
		}
		return connection.Email
	}
	if s.googleAuth == nil || calendar.GoogleConnectionID <= 0 {
		return ""
	}
	connection, err := s.googleAuth.Get(ctx, userID, calendar.GoogleConnectionID)
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
