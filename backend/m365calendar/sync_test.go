// File overview: The Microsoft 365 calendar sync and write-back against a fake
// Microsoft Graph. The fake is the contract: what a window read keeps and
// removes, how an all-day event lands on its date, what a write sends and
// what a conflict leaves behind are asserted against it rather than against a
// tenant nobody can run in CI.

package m365calendar

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"rolltop/backend/calendarlink"
	"rolltop/backend/store"
	"rolltop/backend/store/storetest"
)

type fakeGraph struct {
	mu        sync.Mutex
	calendars []Calendar
	// events holds every event by id; eventCalendar says which calendar.
	events        map[string]Event
	eventCalendar map[string]string
	nextID        int

	// views records the window and expand of each calendarView call.
	views []viewCall
	// rejectExpand answers a calendarView asking for $expand with 400, as a
	// Graph that does not support it would.
	rejectExpand bool
	// conflictNextPatch fails the next PATCH with 412.
	conflictNextPatch bool

	created   []map[string]any
	createdIn []string
	patched   []map[string]any
	ifMatch   []string
	deleted   []string
	responded []string
	prefer    []string
}

type viewCall struct {
	calendarID string
	start      time.Time
	end        time.Time
	expand     bool
}

func newFakeGraph() *fakeGraph {
	return &fakeGraph{events: map[string]Event{}, eventCalendar: map[string]string{}}
}

func (f *fakeGraph) put(calendarID string, event Event) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events[event.ID] = event
	f.eventCalendar[event.ID] = calendarID
}

func (f *fakeGraph) remove(eventID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.events, eventID)
	delete(f.eventCalendar, eventID)
}

func (f *fakeGraph) handler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer access-token" {
			graphError(w, http.StatusUnauthorized, "InvalidAuthenticationToken")
			return
		}
		f.prefer = append(f.prefer, r.Header.Get("Prefer"))
		path := strings.TrimPrefix(r.URL.Path, "/v1.0")
		switch {
		case path == "/me/calendars" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(CalendarsPage{Value: f.calendars})
		case strings.HasPrefix(path, "/me/calendars/") && strings.HasSuffix(path, "/calendarView"):
			f.serveView(w, r, strings.TrimSuffix(strings.TrimPrefix(path, "/me/calendars/"), "/calendarView"))
		case strings.HasPrefix(path, "/me/calendars/") && strings.HasSuffix(path, "/events") && r.Method == http.MethodPost:
			f.serveCreate(w, r, strings.TrimSuffix(strings.TrimPrefix(path, "/me/calendars/"), "/events"))
		case strings.HasPrefix(path, "/me/events/"):
			f.serveEvent(t, w, r, strings.TrimPrefix(path, "/me/events/"))
		default:
			graphError(w, http.StatusNotFound, "ResourceNotFound")
		}
	})
}

func (f *fakeGraph) serveView(w http.ResponseWriter, r *http.Request, calendarID string) {
	query := r.URL.Query()
	start, _ := time.Parse(time.RFC3339, query.Get("startDateTime"))
	end, _ := time.Parse(time.RFC3339, query.Get("endDateTime"))
	expand := query.Get("$expand") != ""
	f.views = append(f.views, viewCall{calendarID: calendarID, start: start, end: end, expand: expand})
	if expand && f.rejectExpand {
		graphError(w, http.StatusBadRequest, "ErrorInvalidProperty")
		return
	}
	page := EventsPage{Value: []Event{}}
	for id, event := range f.events {
		if f.eventCalendar[id] != calendarID {
			continue
		}
		eventStart, eventEnd := parseGraphTime(event.Start), parseGraphTime(event.End)
		if !eventStart.Before(end) || !eventEnd.After(start) {
			continue
		}
		if !expand {
			event.ExtendedProperties = nil
		}
		page.Value = append(page.Value, event)
	}
	_ = json.NewEncoder(w).Encode(page)
}

func (f *fakeGraph) serveCreate(w http.ResponseWriter, r *http.Request, calendarID string) {
	var payload map[string]any
	_ = json.NewDecoder(r.Body).Decode(&payload)
	f.created = append(f.created, payload)
	f.createdIn = append(f.createdIn, calendarID)
	f.nextID++
	event := Event{ID: "created-" + strconv.Itoa(f.nextID), ETag: `W/"created"`, IsOrganizer: true,
		Organizer: &Recipient{EmailAddress: EmailAddress{Address: "reader@contoso.example", Name: "Reader"}}}
	applyPayload(&event, payload)
	if event.IsOnlineMeeting {
		event.OnlineMeeting = &OnlineMeetingInfo{JoinURL: "https://teams.microsoft.com/l/meetup-join/" + event.ID}
	}
	f.events[event.ID] = event
	f.eventCalendar[event.ID] = calendarID
	// A write never answers with extended properties.
	event.ExtendedProperties = nil
	_ = json.NewEncoder(w).Encode(event)
}

func (f *fakeGraph) serveEvent(t *testing.T, w http.ResponseWriter, r *http.Request, rest string) {
	eventID, action, _ := strings.Cut(rest, "/")
	event, ok := f.events[eventID]
	if !ok {
		graphError(w, http.StatusNotFound, "ErrorItemNotFound")
		return
	}
	switch {
	case action != "" && r.Method == http.MethodPost:
		f.responded = append(f.responded, action)
		if event.ResponseStatus == nil {
			event.ResponseStatus = &ResponseStatus{}
		}
		event.ResponseStatus.Response = map[string]string{
			"accept": graphResponseAccepted, "tentativelyAccept": graphResponseTentative, "decline": graphResponseDeclined,
		}[action]
		event.ETag = `W/"answered"`
		f.events[eventID] = event
		w.WriteHeader(http.StatusAccepted)
	case r.Method == http.MethodGet:
		if r.URL.Query().Get("$expand") == "" {
			event.ExtendedProperties = nil
		}
		_ = json.NewEncoder(w).Encode(event)
	case r.Method == http.MethodPatch:
		f.ifMatch = append(f.ifMatch, r.Header.Get("If-Match"))
		if f.conflictNextPatch {
			f.conflictNextPatch = false
			graphError(w, http.StatusPreconditionFailed, "ErrorIrresolvableConflict")
			return
		}
		var payload map[string]any
		_ = json.NewDecoder(r.Body).Decode(&payload)
		f.patched = append(f.patched, payload)
		applyPayload(&event, payload)
		event.ETag = `W/"patched-` + strconv.Itoa(len(f.patched)) + `"`
		if event.IsOnlineMeeting && event.OnlineMeeting == nil {
			event.OnlineMeeting = &OnlineMeetingInfo{JoinURL: "https://teams.microsoft.com/l/meetup-join/" + eventID}
		}
		f.events[eventID] = event
		event.ExtendedProperties = nil
		_ = json.NewEncoder(w).Encode(event)
	case r.Method == http.MethodDelete:
		f.deleted = append(f.deleted, eventID)
		delete(f.events, eventID)
		w.WriteHeader(http.StatusNoContent)
	default:
		t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		graphError(w, http.StatusMethodNotAllowed, "notAllowed")
	}
}

// applyPayload mirrors Graph's PATCH merge closely enough for the assertions
// that care: a property present in the body replaces the stored one.
func applyPayload(event *Event, payload map[string]any) {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return
	}
	_ = json.Unmarshal(encoded, event)
}

func graphError(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{"code": code, "message": "Secret title quoted back"},
	})
}

type stubTokens struct{}

func (stubTokens) AccessToken(context.Context, int64, int64) (string, error) {
	return "access-token", nil
}

func (stubTokens) ForceRefresh(context.Context, int64, int64) (string, error) {
	return "access-token", nil
}

type stubConnections struct {
	connection store.MicrosoftConnection
}

func (s stubConnections) List(context.Context, int64) ([]store.MicrosoftConnection, error) {
	return []store.MicrosoftConnection{s.connection}, nil
}

func (s stubConnections) Get(_ context.Context, _, connectionID int64) (store.MicrosoftConnection, error) {
	if connectionID != s.connection.ID {
		return store.MicrosoftConnection{}, store.ErrNotFound
	}
	return s.connection, nil
}

const selfEmail = "reader@contoso.example"

var fixedNow = time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)

type fixture struct {
	syncer     *Syncer
	db         *store.Store
	userID     int64
	connection store.MicrosoftConnection
	graph      *fakeGraph
	now        time.Time
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	graph := newFakeGraph()
	graph.calendars = []Calendar{
		{ID: "cal-main", Name: "Calendar", HexColor: "#0078d4", IsDefaultCalendar: true, CanEdit: true,
			Owner:                         &EmailAddress{Address: selfEmail},
			AllowedOnlineMeetingProviders: []string{"teamsForBusiness"}, DefaultOnlineMeetingProvider: "teamsForBusiness"},
		{ID: "cal-shared", Name: "Team", Color: "lightGreen", CanEdit: false,
			Owner:                         &EmailAddress{Address: "boss@contoso.example"},
			AllowedOnlineMeetingProviders: []string{"unknown"}},
	}
	server := httptest.NewServer(graph.handler(t))
	t.Cleanup(server.Close)
	db, err := storetest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	ctx := context.Background()
	user, err := db.CreateUser(ctx, "m365-owner@example.test", "Owner", "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	connection, err := db.UpsertMicrosoftConnection(ctx, user.ID, store.MicrosoftConnectionUpsert{
		Email: selfEmail, Subject: "oid", EncryptedRefreshToken: "v1:refresh",
		GrantedScopes: []string{"Calendars.ReadWrite", "User.Read"},
	})
	if err != nil {
		t.Fatal(err)
	}
	client := NewClient()
	client.BaseURL = server.URL + "/v1.0"
	client.RetryDelay = func(int) time.Duration { return time.Millisecond }
	f := &fixture{db: db, userID: user.ID, connection: connection, graph: graph, now: fixedNow}
	f.syncer = &Syncer{
		Store:       db,
		Client:      client,
		Tokens:      stubTokens{},
		Connections: stubConnections{connection: connection},
		Now:         func() time.Time { return f.now },
	}
	return f
}

func (f *fixture) sync(t *testing.T) Result {
	t.Helper()
	result, err := f.syncer.SyncConnection(context.Background(), f.userID, f.connection.ID)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func (f *fixture) calendar(t *testing.T, remoteID string) store.Calendar {
	t.Helper()
	calendar, err := f.db.CalendarByMicrosoftID(context.Background(), f.userID, f.connection.ID, remoteID)
	if err != nil {
		t.Fatal(err)
	}
	return calendar
}

func (f *fixture) event(t *testing.T, calendar store.Calendar, externalID string) store.CalendarEvent {
	t.Helper()
	event, err := f.db.CalendarEventByExternalID(context.Background(), f.userID, calendar.ID, externalID)
	if err != nil {
		t.Fatalf("event %s: %v", externalID, err)
	}
	return event
}

func utc(at time.Time) *DateTimeTimeZone {
	return &DateTimeTimeZone{DateTime: at.UTC().Format(graphDateLayout), TimeZone: "UTC"}
}

func timed(id, summary string, start time.Time) Event {
	return Event{ID: id, ETag: `W/"` + id + `-1"`, Subject: summary, Start: utc(start), End: utc(start.Add(time.Hour)),
		IsOrganizer: true, Type: "singleInstance", Body: &ItemBody{ContentType: "text", Content: "Agenda"}}
}

func TestSyncMirrorsCalendarsAndEvents(t *testing.T) {
	f := newFixture(t)
	start := fixedNow.Add(48 * time.Hour)
	f.graph.put("cal-main", timed("evt-1", "Planning", start))
	// An all-day event entered in Berlin: Graph asked for UTC states its
	// midnight as 22:00 the day before.
	f.graph.put("cal-main", Event{ID: "evt-allday", ETag: `W/"a"`, Subject: "Holiday", IsAllDay: true,
		Start: &DateTimeTimeZone{DateTime: "2026-10-14T22:00:00.0000000", TimeZone: "UTC"},
		End:   &DateTimeTimeZone{DateTime: "2026-10-15T22:00:00.0000000", TimeZone: "UTC"}})
	invitation := timed("evt-invite", "Review", start.Add(3*time.Hour))
	invitation.IsOrganizer = false
	invitation.Organizer = &Recipient{EmailAddress: EmailAddress{Address: "boss@contoso.example", Name: "Boss"}}
	invitation.Attendees = []Attendee{
		{Type: attendeeRequired, EmailAddress: EmailAddress{Address: selfEmail, Name: "Reader"}, Status: &ResponseStatus{Response: graphResponseNone}},
		{Type: attendeeOptional, EmailAddress: EmailAddress{Address: "peer@contoso.example"}, Status: &ResponseStatus{Response: graphResponseTentative}},
	}
	invitation.ResponseStatus = &ResponseStatus{Response: graphResponseNone}
	invitation.IsOnlineMeeting = true
	invitation.OnlineMeetingProvider = providerTeamsForBusiness
	invitation.OnlineMeeting = &OnlineMeetingInfo{JoinURL: "https://teams.microsoft.com/l/meetup-join/abc"}
	invitation.ExtendedProperties = []ExtendedProperty{{ID: linkPropertyID, Value: "key-1:primary"}}
	f.graph.put("cal-main", invitation)
	f.graph.put("cal-shared", timed("evt-shared", "Not synced while off", start))

	result := f.sync(t)
	if result.Calendars != 2 || result.Synced != 1 || result.Created != 3 || !result.FullSync {
		t.Fatalf("result = %+v", result)
	}

	main := f.calendar(t, "cal-main")
	if !main.IsMicrosoft() || !main.Selected || !main.IsPrimary || !main.CanWrite() || main.Color != "#0078d4" {
		t.Fatalf("main calendar = %+v", main)
	}
	if len(main.OnlineMeetingProviders) != 1 || main.OnlineMeetingProviders[0] != providerTeamsForBusiness {
		t.Fatalf("meeting providers = %v", main.OnlineMeetingProviders)
	}
	if main.FullReadAt.IsZero() || !main.WindowStartAt.Equal(fixedNow.Add(-fullWindowBack)) {
		t.Fatalf("window = %v full read = %v", main.WindowStartAt, main.FullReadAt)
	}
	shared := f.calendar(t, "cal-shared")
	if shared.Selected || shared.CanWrite() || shared.Color != namedColors["lightgreen"] || len(shared.OnlineMeetingProviders) != 0 {
		t.Fatalf("shared calendar = %+v", shared)
	}

	planning := f.event(t, main, "evt-1")
	if !planning.StartAt.Equal(start) || planning.Description != "Agenda" || planning.MyResponse != "" {
		t.Fatalf("planning = %+v", planning)
	}
	holiday := f.event(t, main, "evt-allday")
	if !holiday.AllDay || !holiday.StartAt.Equal(time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)) ||
		!holiday.EndAt.Equal(time.Date(2026, 10, 16, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("all-day bounds = %v - %v", holiday.StartAt, holiday.EndAt)
	}
	review := f.event(t, main, "evt-invite")
	if review.MyResponse != store.CalendarResponseNeedsAction || review.OrganizerEmail != "boss@contoso.example" {
		t.Fatalf("invitation = %+v", review)
	}
	if !review.OnlineMeeting || review.OnlineMeetingURL != "https://teams.microsoft.com/l/meetup-join/abc" {
		t.Fatalf("teams meeting = %+v", review)
	}
	if review.LinkKey != "key-1" || !review.LinkPrimary {
		t.Fatalf("link = %q primary=%v", review.LinkKey, review.LinkPrimary)
	}
	if len(review.Attendees) != 3 || !review.Attendees[0].Organizer || !review.Attendees[1].Self ||
		!review.Attendees[2].Optional || review.Attendees[2].Response != store.CalendarResponseTentative {
		t.Fatalf("attendees = %+v", review.Attendees)
	}
	for _, prefer := range f.graph.prefer {
		if !strings.Contains(prefer, `outlook.timezone="UTC"`) {
			t.Fatalf("a request did not ask for UTC: %q", prefer)
		}
	}
}

// Between full reads only the near window is read, and an event deleted in
// Outlook disappears from it on the next poll; one far away waits for the next
// full read, which is the only read that could have noticed it.
func TestNearAndFullWindows(t *testing.T) {
	f := newFixture(t)
	near := fixedNow.Add(24 * time.Hour)
	far := fixedNow.Add(300 * 24 * time.Hour)
	old := fixedNow.Add(-200 * 24 * time.Hour)
	f.graph.put("cal-main", timed("near", "Near", near))
	f.graph.put("cal-main", timed("far", "Far", far))
	f.graph.put("cal-main", timed("old", "Old", old))
	f.sync(t)
	main := f.calendar(t, "cal-main")

	f.graph.remove("near")
	f.graph.remove("far")
	f.now = fixedNow.Add(time.Hour)
	f.graph.views = nil
	result := f.sync(t)
	if result.FullSync || len(f.graph.views) != 1 {
		t.Fatalf("second poll full=%v views=%d, want one near read", result.FullSync, len(f.graph.views))
	}
	view := f.graph.views[0]
	if !view.start.Equal(f.now.Add(-nearWindowBack)) || !view.end.Equal(f.now.Add(nearWindowAhead)) || !view.expand {
		t.Fatalf("near view = %+v", view)
	}
	if _, err := f.db.CalendarEventByExternalID(context.Background(), f.userID, main.ID, "near"); !store.IsNotFound(err) {
		t.Fatalf("event deleted in Outlook survived the near read: %v", err)
	}
	f.event(t, main, "far")
	f.event(t, main, "old")

	f.now = fixedNow.Add(fullReadInterval + time.Hour)
	if result := f.sync(t); !result.FullSync {
		t.Fatal("the full read did not come round")
	}
	if _, err := f.db.CalendarEventByExternalID(context.Background(), f.userID, main.ID, "far"); !store.IsNotFound(err) {
		t.Fatalf("far event deleted in Outlook survived the full read: %v", err)
	}
	f.event(t, main, "old")
}

// A row close to the window's edge is one Graph may rightly have left out,
// and is not deleted for its absence.
func TestReconciliationSparesTheWindowEdge(t *testing.T) {
	f := newFixture(t)
	f.sync(t)
	main := f.calendar(t, "cal-main")
	edge := fixedNow.Add(nearWindowAhead - time.Hour)
	if _, err := f.db.UpsertCalendarEvent(context.Background(), f.userID, store.CalendarEvent{
		CalendarID: main.ID, ExternalID: "edge", StartAt: edge, EndAt: edge.Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	f.now = fixedNow.Add(time.Minute)
	f.sync(t)
	f.event(t, main, "edge")
}

func TestCreateTeamsMeetingWithGuests(t *testing.T) {
	f := newFixture(t)
	f.sync(t)
	main := f.calendar(t, "cal-main")
	start := time.Date(2026, 10, 20, 8, 0, 0, 0, time.UTC)
	created, err := f.syncer.CreateRemoteEvent(context.Background(), f.userID, main.ID, store.CalendarEvent{
		Summary: "Kick-off", Description: "Agenda", Location: "Room 1", StartAt: start, EndAt: start.Add(time.Hour),
		TimeZone:      "Europe/Berlin",
		Attendees:     []store.CalendarAttendee{{Email: "guest@example.test", Name: "Guest"}, {Email: "maybe@example.test", Optional: true}},
		OnlineMeeting: true, LinkKey: "pair", LinkPrimary: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	payload := f.graph.created[0]
	if payload["isOnlineMeeting"] != true || payload["onlineMeetingProvider"] != providerTeamsForBusiness {
		t.Fatalf("teams not requested: %v", payload)
	}
	startPayload := payload["start"].(map[string]any)
	if startPayload["dateTime"] != "2026-10-20T10:00:00" || startPayload["timeZone"] != "Europe/Berlin" {
		t.Fatalf("start written as %v", startPayload)
	}
	guests := payload["attendees"].([]any)
	if len(guests) != 2 || guests[1].(map[string]any)["type"] != attendeeOptional {
		t.Fatalf("attendees written as %v", guests)
	}
	properties := payload["singleValueExtendedProperties"].([]any)
	if properties[0].(map[string]any)["value"] != "pair:primary" {
		t.Fatalf("link written as %v", properties)
	}
	if !created.OnlineMeeting || !strings.HasPrefix(created.OnlineMeetingURL, "https://teams.microsoft.com/") {
		t.Fatalf("created event lacks its join link: %+v", created)
	}
	if created.LinkKey != "pair" || !created.LinkPrimary || !created.StartAt.Equal(start) {
		t.Fatalf("created = %+v", created)
	}
	// The next poll reads the link back from Graph itself.
	f.now = fixedNow.Add(time.Minute)
	f.sync(t)
	if reread := f.event(t, main, created.ExternalID); reread.LinkKey != "pair" || !reread.LinkPrimary {
		t.Fatalf("link after resync = %q %v", reread.LinkKey, reread.LinkPrimary)
	}
}

func TestCreateRefusesWhatTheCalendarCannotDo(t *testing.T) {
	f := newFixture(t)
	f.graph.calendars[0].AllowedOnlineMeetingProviders = []string{"unknown"}
	f.sync(t)
	main := f.calendar(t, "cal-main")
	start := fixedNow.Add(time.Hour)
	_, err := f.syncer.CreateRemoteEvent(context.Background(), f.userID, main.ID, store.CalendarEvent{
		Summary: "x", StartAt: start, EndAt: start.Add(time.Hour), OnlineMeeting: true,
	})
	if !errors.Is(err, ErrNoOnlineMeeting) || !errors.Is(err, calendarlink.ErrNoOnlineMeeting) {
		t.Fatalf("teams in a calendar without it: %v", err)
	}
	shared := f.calendar(t, "cal-shared")
	if _, err := f.syncer.CreateRemoteEvent(context.Background(), f.userID, shared.ID, store.CalendarEvent{
		Summary: "x", StartAt: start, EndAt: start.Add(time.Hour),
	}); !errors.Is(err, calendarlink.ErrReadOnlyCalendar) {
		t.Fatalf("write to a read-only calendar: %v", err)
	}
	if len(f.graph.created) != 0 {
		t.Fatalf("a refused create reached Graph: %v", f.graph.created)
	}
}

// An edit sends only what it changed beyond the dialog's own fields, with the
// etag as a precondition.
func TestUpdateSendsOnlyWhatChanged(t *testing.T) {
	f := newFixture(t)
	start := fixedNow.Add(24 * time.Hour)
	f.graph.put("cal-main", timed("evt", "Planning", start))
	f.sync(t)
	main := f.calendar(t, "cal-main")
	existing := f.event(t, main, "evt")

	edited := existing
	edited.Summary = "Planning (moved)"
	edited.StartAt = start.Add(time.Hour)
	edited.EndAt = start.Add(2 * time.Hour)
	updated, err := f.syncer.UpdateRemoteEvent(context.Background(), f.userID, existing, edited)
	if err != nil {
		t.Fatal(err)
	}
	patch := f.graph.patched[0]
	for _, absent := range []string{"body", "attendees", "isOnlineMeeting"} {
		if _, ok := patch[absent]; ok {
			t.Fatalf("patch carried %s it did not change: %v", absent, patch)
		}
	}
	if f.graph.ifMatch[0] != existing.ETag {
		t.Fatalf("If-Match = %q, want %q", f.graph.ifMatch[0], existing.ETag)
	}
	if updated.Summary != "Planning (moved)" || !updated.StartAt.Equal(start.Add(time.Hour)) || updated.ETag == existing.ETag {
		t.Fatalf("updated = %+v", updated)
	}

	// Turning the event into a Teams meeting and changing the notes and guests.
	again := updated
	again.Description = "New agenda"
	again.OnlineMeeting = true
	again.Attendees = []store.CalendarAttendee{{Email: "guest@example.test"}}
	teams, err := f.syncer.UpdateRemoteEvent(context.Background(), f.userID, updated, again)
	if err != nil {
		t.Fatal(err)
	}
	patch = f.graph.patched[1]
	if patch["isOnlineMeeting"] != true || patch["body"].(map[string]any)["content"] != "New agenda" || len(patch["attendees"].([]any)) != 1 {
		t.Fatalf("second patch = %v", patch)
	}
	if !teams.OnlineMeeting || teams.OnlineMeetingURL == "" {
		t.Fatalf("teams after update = %+v", teams)
	}
}

// A write refused on a stale etag adopts Microsoft's version: the leading
// system wins and the reader is shown what it holds.
func TestUpdateConflictAdoptsMicrosoftsVersion(t *testing.T) {
	f := newFixture(t)
	start := fixedNow.Add(24 * time.Hour)
	f.graph.put("cal-main", timed("evt", "Planning", start))
	f.sync(t)
	main := f.calendar(t, "cal-main")
	existing := f.event(t, main, "evt")

	changed := timed("evt", "Renamed in Outlook", start)
	changed.ETag = `W/"outlook"`
	f.graph.put("cal-main", changed)
	f.graph.conflictNextPatch = true
	edited := existing
	edited.Summary = "Mine"
	adopted, err := f.syncer.UpdateRemoteEvent(context.Background(), f.userID, existing, edited)
	if !errors.Is(err, ErrRemoteChanged) || !errors.Is(err, calendarlink.ErrRemoteChanged) {
		t.Fatalf("error = %v, want remote changed", err)
	}
	if adopted.Summary != "Renamed in Outlook" || f.event(t, main, "evt").Summary != "Renamed in Outlook" {
		t.Fatalf("adopted = %+v", adopted)
	}

	// A guest-list change against a mirror Outlook has moved past is resolved
	// before any write is attempted.
	moved := timed("evt", "Moved again", start)
	moved.ETag = `W/"outlook-2"`
	f.graph.put("cal-main", moved)
	stale := f.event(t, main, "evt")
	withGuest := stale
	withGuest.Attendees = []store.CalendarAttendee{{Email: "guest@example.test"}}
	patches := len(f.graph.patched)
	if _, err := f.syncer.UpdateRemoteEvent(context.Background(), f.userID, stale, withGuest); !errors.Is(err, ErrRemoteChanged) {
		t.Fatalf("stale guest edit: %v", err)
	}
	if len(f.graph.patched) != patches {
		t.Fatal("a guest list was written against a stale version")
	}
}

func TestUpdateOfAnEventDeletedInOutlook(t *testing.T) {
	f := newFixture(t)
	start := fixedNow.Add(24 * time.Hour)
	f.graph.put("cal-main", timed("evt", "Planning", start))
	f.sync(t)
	main := f.calendar(t, "cal-main")
	existing := f.event(t, main, "evt")
	f.graph.remove("evt")
	if _, err := f.syncer.UpdateRemoteEvent(context.Background(), f.userID, existing, existing); !errors.Is(err, calendarlink.ErrRemoteDeleted) {
		t.Fatalf("error = %v, want remote deleted", err)
	}
	if _, err := f.db.CalendarEvent(context.Background(), f.userID, existing.ID); !store.IsNotFound(err) {
		t.Fatalf("mirror of a deleted event survived: %v", err)
	}
}

func TestRespondAndDelete(t *testing.T) {
	f := newFixture(t)
	start := fixedNow.Add(24 * time.Hour)
	invitation := timed("invite", "Review", start)
	invitation.IsOrganizer = false
	invitation.Organizer = &Recipient{EmailAddress: EmailAddress{Address: "boss@contoso.example"}}
	invitation.Attendees = []Attendee{{Type: attendeeRequired, EmailAddress: EmailAddress{Address: selfEmail}}}
	invitation.ResponseStatus = &ResponseStatus{Response: graphResponseNone}
	f.graph.put("cal-main", invitation)
	f.graph.put("cal-main", timed("mine", "Mine", start))
	f.sync(t)
	main := f.calendar(t, "cal-main")

	answered, err := f.syncer.RespondToRemoteEvent(context.Background(), f.userID, f.event(t, main, "invite"), store.CalendarResponseTentative)
	if err != nil {
		t.Fatal(err)
	}
	if f.graph.responded[0] != "tentativelyAccept" || answered.MyResponse != store.CalendarResponseTentative {
		t.Fatalf("answer: action=%v response=%q", f.graph.responded, answered.MyResponse)
	}
	if _, err := f.syncer.RespondToRemoteEvent(context.Background(), f.userID, f.event(t, main, "mine"), store.CalendarResponseAccepted); !errors.Is(err, calendarlink.ErrNotAnInvitation) {
		t.Fatalf("answering one's own event: %v", err)
	}

	mine := f.event(t, main, "mine")
	if err := f.syncer.DeleteRemoteEvent(context.Background(), f.userID, mine); err != nil {
		t.Fatal(err)
	}
	if len(f.graph.deleted) != 1 || f.graph.deleted[0] != "mine" {
		t.Fatalf("deleted = %v", f.graph.deleted)
	}
	if _, err := f.db.CalendarEvent(context.Background(), f.userID, mine.ID); !store.IsNotFound(err) {
		t.Fatalf("local row after delete: %v", err)
	}
	// Deleting what Microsoft no longer has is the end state, not a failure.
	ghost := f.event(t, main, "invite")
	f.graph.remove("invite")
	if err := f.syncer.DeleteRemoteEvent(context.Background(), f.userID, ghost); err != nil {
		t.Fatalf("delete of an event already gone: %v", err)
	}
}

// A Graph that refuses the link property on a calendarView still syncs, and
// keeps the links already mirrored instead of clearing them.
func TestSyncWithoutTheLinkPropertyKeepsMirroredLinks(t *testing.T) {
	f := newFixture(t)
	start := fixedNow.Add(24 * time.Hour)
	linked := timed("evt", "Linked", start)
	linked.ExtendedProperties = []ExtendedProperty{{ID: linkPropertyID, Value: "k:copy"}}
	f.graph.put("cal-main", linked)
	f.sync(t)
	main := f.calendar(t, "cal-main")
	if event := f.event(t, main, "evt"); event.LinkKey != "k" || event.LinkPrimary {
		t.Fatalf("link = %q %v", event.LinkKey, event.LinkPrimary)
	}

	f.graph.rejectExpand = true
	renamed := linked
	renamed.Subject = "Renamed"
	renamed.ETag = `W/"2"`
	f.graph.put("cal-main", renamed)
	f.now = fixedNow.Add(time.Minute)
	f.sync(t)
	event := f.event(t, main, "evt")
	if event.Summary != "Renamed" || event.LinkKey != "k" {
		t.Fatalf("after fallback = %+v", event)
	}
	if !f.syncer.linksUnsupported(main.ID) {
		t.Fatal("the fallback was not remembered")
	}
}

// Graph's error text quotes the request; none of it may reach an error a
// caller logs or shows.
func TestGraphErrorsDoNotQuoteTheRequest(t *testing.T) {
	f := newFixture(t)
	f.sync(t)
	main := f.calendar(t, "cal-main")
	_, err := f.syncer.UpdateRemoteEvent(context.Background(), f.userID,
		store.CalendarEvent{ID: 99, CalendarID: main.ID, ExternalID: "missing"}, store.CalendarEvent{Summary: "Secret"})
	if err == nil || strings.Contains(err.Error(), "Secret") {
		t.Fatalf("error = %v", err)
	}
}

func TestNearestMidnight(t *testing.T) {
	cases := map[string]string{
		"2026-10-14T22:00:00Z": "2026-10-15T00:00:00Z", // Berlin
		"2026-10-15T04:00:00Z": "2026-10-15T00:00:00Z", // New York
		"2026-10-15T00:00:00Z": "2026-10-15T00:00:00Z", // UTC
		"2026-10-14T13:00:00Z": "2026-10-15T00:00:00Z", // Auckland
	}
	for in, want := range cases {
		at, _ := time.Parse(time.RFC3339, in)
		if got := nearestMidnight(at).Format(time.RFC3339); got != want {
			t.Errorf("nearestMidnight(%s) = %s, want %s", in, got, want)
		}
	}
}

// The guest list the dialog sends back carries the organizer row ToEvent put
// first, without its flag. Saving a meeting unchanged must not read that as a
// new guest -- which would mail every guest an update and invite the
// organizer to their own meeting.
func TestOrganizerRowDoesNotTravelAsAGuest(t *testing.T) {
	f := newFixture(t)
	start := fixedNow.Add(24 * time.Hour)
	meeting := timed("meeting", "Sync", start)
	meeting.Organizer = &Recipient{EmailAddress: EmailAddress{Address: selfEmail, Name: "Reader"}}
	meeting.Attendees = []Attendee{{Type: attendeeRequired, EmailAddress: EmailAddress{Address: "guest@example.test"},
		Status: &ResponseStatus{Response: graphResponseAccepted}}}
	f.graph.put("cal-main", meeting)
	f.sync(t)
	main := f.calendar(t, "cal-main")
	existing := f.event(t, main, "meeting")
	if len(existing.Attendees) != 2 || !existing.Attendees[0].Organizer {
		t.Fatalf("attendees = %+v", existing.Attendees)
	}

	edited := existing
	edited.Summary = "Sync (renamed)"
	edited.Attendees = nil
	for _, attendee := range existing.Attendees {
		// What the dialog submits: address, name and optional only.
		edited.Attendees = append(edited.Attendees, store.CalendarAttendee{Email: attendee.Email, Name: attendee.Name})
	}
	if _, err := f.syncer.UpdateRemoteEvent(context.Background(), f.userID, existing, edited); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.graph.patched[0]["attendees"]; ok {
		t.Fatalf("an unchanged guest list travelled: %v", f.graph.patched[0])
	}

	// Adding a guest sends the list without the organizer in it.
	renamed := f.event(t, main, "meeting")
	more := renamed
	more.Attendees = append(append([]store.CalendarAttendee(nil), edited.Attendees...), store.CalendarAttendee{Email: "new@example.test"})
	if _, err := f.syncer.UpdateRemoteEvent(context.Background(), f.userID, renamed, more); err != nil {
		t.Fatal(err)
	}
	sent := f.graph.patched[1]["attendees"].([]any)
	if len(sent) != 2 {
		t.Fatalf("attendees sent = %v, want the two guests only", sent)
	}
	for _, guest := range sent {
		if strings.EqualFold(guest.(map[string]any)["emailAddress"].(map[string]any)["address"].(string), selfEmail) {
			t.Fatalf("the organizer was sent as a guest: %v", sent)
		}
	}
}

// A create that Graph may already have carried out is not sent again: a
// second POST is a second event, a second set of invitations and a second
// Teams meeting.
func TestCreateIsNotRetriedAfterAnUncertainFailure(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusGatewayTimeout)
	}))
	defer server.Close()
	client := &Client{HTTPClient: server.Client(), BaseURL: server.URL, RetryDelay: func(int) time.Duration { return 0 }}
	if _, err := client.CreateEvent(context.Background(), "token", "cal", map[string]any{"subject": "x"}); err == nil {
		t.Fatal("create succeeded against a failing Graph")
	}
	if calls != 1 {
		t.Fatalf("POST sent %d times, want once", calls)
	}
	calls = 0
	if _, err := client.GetEvent(context.Background(), "token", "evt"); err == nil {
		t.Fatal("read succeeded against a failing Graph")
	}
	if calls != defaultMaxAttempts {
		t.Fatalf("GET sent %d times, want %d", calls, defaultMaxAttempts)
	}
}

// Outlook names zones the Windows way; only an IANA name may travel on, into a
// Google copy or back into Graph.
func TestPortableZone(t *testing.T) {
	cases := map[string]string{
		"Europe/Berlin":            "Europe/Berlin",
		"UTC":                      "UTC",
		"W. Europe Standard Time":  "",
		"tzone://Microsoft/Custom": "",
		"":                         "",
	}
	for in, want := range cases {
		if got := portableZone(in); got != want {
			t.Errorf("portableZone(%q) = %q, want %q", in, got, want)
		}
	}
}
