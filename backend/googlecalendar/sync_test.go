// File overview: Calendar sync against a fake Calendar API. No test here talks
// to Google: the fake is the contract, and every behaviour that matters -- what
// a delta deletes, what a full read prunes, which cursor survives an expiry --
// is asserted against it rather than against a live account nobody can run in CI.

package googlecalendar

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"rolltop/backend/googletoken"
	"rolltop/backend/store"
)

// fakeCalendar serves the endpoints the sync and the write-back use. Pages are
// scripted per call so a test can describe a sequence of Google states.
type fakeCalendar struct {
	mu sync.Mutex
	// calendarPages is the scripted reply for each successive calendarList.list
	// call; the last entry repeats.
	calendarPages []CalendarListPage
	calendarCalls int
	// eventPages is the same for events.list.
	eventPages []EventsPage
	eventCalls int
	// eventStatus forces an error status on the nth events.list call.
	eventStatus []int
	// eventSyncTokens and eventTimeMins record what each events.list asked for.
	eventSyncTokens []string
	eventTimeMins   []string

	// events answers reads and receives writes, keyed by event id.
	events map[string]Event
	// created, patched and deleted record the write-back calls.
	created []EventWrite
	patched []map[string]any
	deleted []string
	// patchConflict makes the next patch fail with a stale-etag precondition.
	patchConflict bool
	// ifMatch records the precondition header of each write.
	ifMatch []string
	// sendUpdates records the notification choice of each write.
	sendUpdates []string
}

func (f *fakeCalendar) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/calendar/v3/users/me/calendarList", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		index := f.calendarCalls
		f.calendarCalls++
		page := CalendarListPage{}
		if index < len(f.calendarPages) {
			page = f.calendarPages[index]
		} else if len(f.calendarPages) > 0 {
			page = f.calendarPages[len(f.calendarPages)-1]
		}
		_ = json.NewEncoder(w).Encode(page)
	})
	mux.HandleFunc("/calendar/v3/calendars/", func(w http.ResponseWriter, r *http.Request) {
		rest := strings.TrimPrefix(r.URL.Path, "/calendar/v3/calendars/")
		_, tail, _ := strings.Cut(rest, "/events")
		eventID := strings.TrimPrefix(tail, "/")
		f.mu.Lock()
		defer f.mu.Unlock()
		if eventID == "" {
			f.serveEventList(w, r)
			return
		}
		f.serveEvent(w, r, eventID)
	})
	return mux
}

func (f *fakeCalendar) serveEventList(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		var write EventWrite
		_ = json.NewDecoder(r.Body).Decode(&write)
		f.created = append(f.created, write)
		f.sendUpdates = append(f.sendUpdates, r.URL.Query().Get("sendUpdates"))
		created := Event{
			ID: "created-1", ETag: "etag-created", Status: "confirmed",
			Summary: write.Summary, Description: write.Description, Location: write.Location,
			Start: write.Start, End: write.End,
		}
		if write.Attendees != nil {
			created.Attendees = *write.Attendees
		}
		f.events[created.ID] = created
		_ = json.NewEncoder(w).Encode(created)
		return
	}
	f.eventSyncTokens = append(f.eventSyncTokens, r.URL.Query().Get("syncToken"))
	f.eventTimeMins = append(f.eventTimeMins, r.URL.Query().Get("timeMin"))
	index := f.eventCalls
	f.eventCalls++
	if index < len(f.eventStatus) && f.eventStatus[index] != 0 {
		writeCalendarError(w, f.eventStatus[index], "fullSyncRequired")
		return
	}
	page := EventsPage{}
	if index < len(f.eventPages) {
		page = f.eventPages[index]
	} else if len(f.eventPages) > 0 {
		page = f.eventPages[len(f.eventPages)-1]
	}
	_ = json.NewEncoder(w).Encode(page)
}

func (f *fakeCalendar) serveEvent(w http.ResponseWriter, r *http.Request, eventID string) {
	switch r.Method {
	case http.MethodGet:
		event, ok := f.events[eventID]
		if !ok {
			writeCalendarError(w, http.StatusNotFound, "notFound")
			return
		}
		_ = json.NewEncoder(w).Encode(event)
	case http.MethodPatch:
		f.ifMatch = append(f.ifMatch, r.Header.Get("If-Match"))
		f.sendUpdates = append(f.sendUpdates, r.URL.Query().Get("sendUpdates"))
		if f.patchConflict {
			f.patchConflict = false
			writeCalendarError(w, http.StatusPreconditionFailed, "conditionNotMet")
			return
		}
		var patch map[string]any
		_ = json.NewDecoder(r.Body).Decode(&patch)
		f.patched = append(f.patched, patch)
		event := f.events[eventID]
		event.ID = eventID
		event.ETag = "etag-patched"
		applyPatch(&event, patch)
		f.events[eventID] = event
		_ = json.NewEncoder(w).Encode(event)
	case http.MethodDelete:
		f.ifMatch = append(f.ifMatch, r.Header.Get("If-Match"))
		f.deleted = append(f.deleted, eventID)
		delete(f.events, eventID)
		w.WriteHeader(http.StatusNoContent)
	default:
		writeCalendarError(w, http.StatusMethodNotAllowed, "notAllowed")
	}
}

// applyPatch mirrors just enough of Google's merge behaviour for the assertions
// that care: a field present in the body replaces the stored one.
func applyPatch(event *Event, patch map[string]any) {
	encoded, err := json.Marshal(patch)
	if err != nil {
		return
	}
	_ = json.Unmarshal(encoded, event)
}

func writeCalendarError(w http.ResponseWriter, status int, reason string) {
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{
			"errors": []map[string]any{{"reason": reason}},
		},
	})
}

// stubConnections stands in for the auth manager. It hands back a connection
// with whatever state the test wants without going near stored tokens.
type stubConnections struct {
	connection store.GoogleConnection
}

func (s stubConnections) List(context.Context, int64) ([]store.GoogleConnection, error) {
	return []store.GoogleConnection{s.connection}, nil
}

func (s stubConnections) Get(_ context.Context, _, connectionID int64) (store.GoogleConnection, error) {
	if connectionID != s.connection.ID {
		return store.GoogleConnection{}, store.ErrNotFound
	}
	return s.connection, nil
}

const fixtureConnectionID = 7

// fixedNow keeps the window boundary of a full read predictable.
var fixedNow = time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)

func newSyncFixture(t *testing.T, fake *fakeCalendar) (*Syncer, *store.Store, store.User) {
	t.Helper()
	if fake.events == nil {
		fake.events = map[string]Event{}
	}
	server := httptest.NewServer(fake.handler())
	t.Cleanup(server.Close)

	db, err := store.Open(filepath.Join(t.TempDir(), "rolltop.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	user, err := db.CreateUser(context.Background(), "calendar-owner@example.test", "Calendar Owner", "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	client := NewClient()
	client.BaseURL = server.URL
	client.RetryDelay = func(int) time.Duration { return time.Millisecond }
	syncer := &Syncer{
		Store:  db,
		Client: client,
		Tokens: &googletoken.StubTokenSource{Tokens: []string{"access-token"}},
		Connections: stubConnections{connection: store.GoogleConnection{
			ID: fixtureConnectionID, UserID: user.ID,
			GoogleEmail: "owner@gmail.example.test", Status: store.GoogleConnectionStatusOK,
		}},
		ScopeGranted: func(store.GoogleConnection) bool { return true },
		Now:          func() time.Time { return fixedNow },
	}
	return syncer, db, user
}

func ownedCalendar(id, name string) CalendarListEntry {
	return CalendarListEntry{
		ID: id, Summary: name, AccessRole: store.CalendarAccessRoleOwner,
		Primary: id == "primary", Selected: true, BackgroundColor: "#123456",
	}
}

func timedEvent(id, etag, summary string, start time.Time) Event {
	return Event{
		ID: id, ETag: etag, Status: "confirmed", Summary: summary,
		Start: EventDateTime{DateTime: start.Format(time.RFC3339), TimeZone: "Europe/Berlin"},
		End:   EventDateTime{DateTime: start.Add(time.Hour).Format(time.RFC3339)},
	}
}

func onlyCalendar(t *testing.T, db *store.Store, userID int64) store.Calendar {
	t.Helper()
	calendars, err := db.ListCalendars(context.Background(), userID)
	if err != nil {
		t.Fatal(err)
	}
	if len(calendars) != 1 {
		t.Fatalf("calendars = %+v, want exactly one", calendars)
	}
	return calendars[0]
}

func eventsOf(t *testing.T, db *store.Store, userID int64, calendar store.Calendar) []store.CalendarEvent {
	t.Helper()
	events, err := db.ListCalendarEventsInRange(context.Background(), userID, []int64{calendar.ID},
		fixedNow.AddDate(-2, 0, 0), fixedNow.AddDate(2, 0, 0))
	if err != nil {
		t.Fatal(err)
	}
	return events
}

// The first run has no cursor, so it reads the whole window and stores the
// tokens the next run needs. Without them every run would be a full read.
func TestSyncStoresCursorsFromTheFirstFullRead(t *testing.T) {
	start := fixedNow.Add(24 * time.Hour)
	fake := &fakeCalendar{
		calendarPages: []CalendarListPage{{
			Items:         []CalendarListEntry{ownedCalendar("primary", "Work")},
			NextSyncToken: "calendars-1",
		}},
		eventPages: []EventsPage{{
			Items:         []Event{timedEvent("e1", "etag-1", "Standup", start)},
			NextSyncToken: "events-1",
		}},
	}
	syncer, db, user := newSyncFixture(t, fake)

	result, err := syncer.SyncConnection(context.Background(), user.ID, fixtureConnectionID)
	if err != nil {
		t.Fatal(err)
	}
	if !result.FullSync || result.Calendars != 1 || result.Synced != 1 || result.Created != 1 {
		t.Fatalf("first sync = %+v, want one calendar with one created event from a full read", result)
	}

	calendar := onlyCalendar(t, db, user.ID)
	if calendar.SyncToken != "events-1" {
		t.Fatalf("calendar cursor = %q, want the token from the last events page", calendar.SyncToken)
	}
	if calendar.Color != "#123456" || !calendar.IsPrimary || !calendar.CanWrite() {
		t.Fatalf("calendar = %+v, want the fields the list sync carries", calendar)
	}
	// A full read must ask for the window; anything else mirrors all of history.
	if len(fake.eventTimeMins) == 0 || fake.eventTimeMins[0] == "" {
		t.Fatalf("events.list timeMin = %v, want the window boundary on a full read", fake.eventTimeMins)
	}
	if !calendar.WindowStartAt.Equal(fixedNow.Add(-DefaultWindow)) {
		t.Fatalf("window start = %v, want %v", calendar.WindowStartAt, fixedNow.Add(-DefaultWindow))
	}

	state, err := db.GetGoogleCalendarSync(context.Background(), user.ID, fixtureConnectionID)
	if err != nil {
		t.Fatal(err)
	}
	if state.SyncToken != "calendars-1" || state.Status != store.CalendarSyncStatusOK {
		t.Fatalf("list state = %+v, want the stored calendar-list cursor", state)
	}

	events := eventsOf(t, db, user.ID, calendar)
	if len(events) != 1 || events[0].ExternalID != "e1" || events[0].ETag != "etag-1" {
		t.Fatalf("events = %+v, want the mirrored occurrence", events)
	}
	if events[0].TimeZone != "Europe/Berlin" || events[0].AllDay {
		t.Fatalf("event = %+v, want a timed event carrying its zone", events[0])
	}
}

// The second run must send the cursor and must not send a window: Google
// encodes the window into the token and rejects a request that carries both.
func TestSyncSendsTheCursorWithoutAWindow(t *testing.T) {
	start := fixedNow.Add(24 * time.Hour)
	fake := &fakeCalendar{
		calendarPages: []CalendarListPage{{
			Items:         []CalendarListEntry{ownedCalendar("primary", "Work")},
			NextSyncToken: "calendars-1",
		}},
		eventPages: []EventsPage{
			{Items: []Event{timedEvent("e1", "etag-1", "Standup", start)}, NextSyncToken: "events-1"},
			{Items: []Event{timedEvent("e1", "etag-2", "Standup moved", start)}, NextSyncToken: "events-2"},
		},
	}
	syncer, db, user := newSyncFixture(t, fake)
	ctx := context.Background()

	if _, err := syncer.SyncConnection(ctx, user.ID, fixtureConnectionID); err != nil {
		t.Fatal(err)
	}
	result, err := syncer.SyncConnection(ctx, user.ID, fixtureConnectionID)
	if err != nil {
		t.Fatal(err)
	}
	if result.FullSync || result.Updated != 1 || result.Created != 0 {
		t.Fatalf("second sync = %+v, want an incremental update", result)
	}
	if len(fake.eventSyncTokens) < 2 || fake.eventSyncTokens[1] != "events-1" {
		t.Fatalf("events.list syncTokens = %v, want the stored cursor on the second call", fake.eventSyncTokens)
	}
	if len(fake.eventTimeMins) < 2 || fake.eventTimeMins[1] != "" {
		t.Fatalf("events.list timeMins = %v, want no window alongside a cursor", fake.eventTimeMins)
	}
	calendar := onlyCalendar(t, db, user.ID)
	if calendar.SyncToken != "events-2" {
		t.Fatalf("calendar cursor = %q, want it advanced", calendar.SyncToken)
	}
	events := eventsOf(t, db, user.ID, calendar)
	if len(events) != 1 || events[0].Summary != "Standup moved" {
		t.Fatalf("events = %+v, want the delta applied", events)
	}
}

// An unchanged occurrence is not an update. A full read re-reports everything,
// so counting those as updates would report an untouched calendar as rewritten.
func TestSyncDoesNotCountUnchangedEvents(t *testing.T) {
	start := fixedNow.Add(24 * time.Hour)
	page := EventsPage{
		Items:         []Event{timedEvent("e1", "etag-1", "Standup", start)},
		NextSyncToken: "events-1",
	}
	fake := &fakeCalendar{
		calendarPages: []CalendarListPage{{
			Items: []CalendarListEntry{ownedCalendar("primary", "Work")}, NextSyncToken: "calendars-1",
		}},
		eventPages: []EventsPage{page, page},
	}
	syncer, _, user := newSyncFixture(t, fake)
	ctx := context.Background()

	if _, err := syncer.SyncConnection(ctx, user.ID, fixtureConnectionID); err != nil {
		t.Fatal(err)
	}
	result, err := syncer.SyncConnection(ctx, user.ID, fixtureConnectionID)
	if err != nil {
		t.Fatal(err)
	}
	if result.Created != 0 || result.Updated != 0 {
		t.Fatalf("second sync = %+v, want nothing counted for an unchanged event", result)
	}
}

// A delta reports a removed occurrence as a cancelled entry rather than by
// leaving it out, and that is what has to take the local row with it.
func TestSyncRemovesCancelledOccurrences(t *testing.T) {
	start := fixedNow.Add(24 * time.Hour)
	fake := &fakeCalendar{
		calendarPages: []CalendarListPage{{
			Items: []CalendarListEntry{ownedCalendar("primary", "Work")}, NextSyncToken: "calendars-1",
		}},
		eventPages: []EventsPage{
			{Items: []Event{timedEvent("e1", "etag-1", "Standup", start)}, NextSyncToken: "events-1"},
			{Items: []Event{{ID: "e1", Status: "cancelled"}}, NextSyncToken: "events-2"},
		},
	}
	syncer, db, user := newSyncFixture(t, fake)
	ctx := context.Background()

	if _, err := syncer.SyncConnection(ctx, user.ID, fixtureConnectionID); err != nil {
		t.Fatal(err)
	}
	result, err := syncer.SyncConnection(ctx, user.ID, fixtureConnectionID)
	if err != nil {
		t.Fatal(err)
	}
	if result.Deleted != 1 {
		t.Fatalf("second sync = %+v, want the cancelled occurrence deleted", result)
	}
	calendar := onlyCalendar(t, db, user.ID)
	if events := eventsOf(t, db, user.ID, calendar); len(events) != 0 {
		t.Fatalf("events = %+v, want the cancelled occurrence gone", events)
	}
}

// Google drops a cursor after a while. Recovery is a full read of the window,
// and the rejected token must not be put back: doing so would fail the same way
// on every poll and re-read the window each time.
func TestSyncRecoversFromAnExpiredCursorWithoutRestoringIt(t *testing.T) {
	start := fixedNow.Add(24 * time.Hour)
	fake := &fakeCalendar{
		calendarPages: []CalendarListPage{{
			Items: []CalendarListEntry{ownedCalendar("primary", "Work")}, NextSyncToken: "calendars-1",
		}},
		eventPages: []EventsPage{
			{Items: []Event{timedEvent("e1", "etag-1", "Standup", start)}, NextSyncToken: "events-1"},
			// The expired delta, then the full read that recovers it. The full
			// read answers without a cursor, which is the case that used to put
			// the rejected one back.
			{},
			{Items: []Event{timedEvent("e1", "etag-1", "Standup", start)}},
		},
		// The second call is the delta and is the one that expires.
		eventStatus: []int{0, http.StatusGone},
	}
	syncer, db, user := newSyncFixture(t, fake)
	ctx := context.Background()

	if _, err := syncer.SyncConnection(ctx, user.ID, fixtureConnectionID); err != nil {
		t.Fatal(err)
	}
	result, err := syncer.SyncConnection(ctx, user.ID, fixtureConnectionID)
	if err != nil {
		t.Fatal(err)
	}
	if !result.FullSync {
		t.Fatalf("recovery sync = %+v, want it reported as a full read", result)
	}
	calendar := onlyCalendar(t, db, user.ID)
	if calendar.SyncToken != "" {
		t.Fatalf("calendar cursor = %q, want the rejected cursor discarded", calendar.SyncToken)
	}
	if calendar.Status != store.CalendarSyncStatusOK {
		t.Fatalf("calendar status = %q, want a recovered sync to count as a success", calendar.Status)
	}
}

// A full read is the only one that can conclude an event is gone. Anything it
// never mentioned has either been deleted or fallen out of the window.
func TestFullReadPrunesEventsGoogleNoLongerReturns(t *testing.T) {
	start := fixedNow.Add(24 * time.Hour)
	fake := &fakeCalendar{
		calendarPages: []CalendarListPage{{
			Items: []CalendarListEntry{ownedCalendar("primary", "Work")}, NextSyncToken: "calendars-1",
		}},
		eventPages: []EventsPage{
			{Items: []Event{
				timedEvent("e1", "etag-1", "Standup", start),
				timedEvent("e2", "etag-1", "Retro", start.Add(2*time.Hour)),
			}},
			{Items: []Event{timedEvent("e1", "etag-1", "Standup", start)}},
		},
	}
	syncer, db, user := newSyncFixture(t, fake)
	ctx := context.Background()

	if _, err := syncer.SyncConnection(ctx, user.ID, fixtureConnectionID); err != nil {
		t.Fatal(err)
	}
	// No cursor was ever handed out, so the second run is another full read.
	result, err := syncer.SyncConnection(ctx, user.ID, fixtureConnectionID)
	if err != nil {
		t.Fatal(err)
	}
	if result.Deleted != 1 {
		t.Fatalf("second full read = %+v, want the missing event pruned", result)
	}
	calendar := onlyCalendar(t, db, user.ID)
	events := eventsOf(t, db, user.ID, calendar)
	if len(events) != 1 || events[0].ExternalID != "e1" {
		t.Fatalf("events = %+v, want only the event Google still returns", events)
	}
}

// A calendar the user switched off costs a page of requests per poll and a
// slice of the database for a week nobody looks at.
func TestSyncSkipsCalendarsTheUserSwitchedOff(t *testing.T) {
	fake := &fakeCalendar{
		calendarPages: []CalendarListPage{{
			Items:         []CalendarListEntry{ownedCalendar("primary", "Work")},
			NextSyncToken: "calendars-1",
		}},
		eventPages: []EventsPage{{NextSyncToken: "events-1"}},
	}
	syncer, db, user := newSyncFixture(t, fake)
	ctx := context.Background()

	if _, err := syncer.SyncConnection(ctx, user.ID, fixtureConnectionID); err != nil {
		t.Fatal(err)
	}
	calendar := onlyCalendar(t, db, user.ID)
	if err := db.SetCalendarSelected(ctx, user.ID, calendar.ID, false); err != nil {
		t.Fatal(err)
	}
	before := fake.eventCalls

	result, err := syncer.SyncConnection(ctx, user.ID, fixtureConnectionID)
	if err != nil {
		t.Fatal(err)
	}
	if result.Synced != 0 || result.Calendars != 1 {
		t.Fatalf("sync = %+v, want the hidden calendar listed but not read", result)
	}
	if fake.eventCalls != before {
		t.Fatalf("events.list calls went from %d to %d, want none for a hidden calendar", before, fake.eventCalls)
	}
}

// A calendar Google no longer lists takes its events with it. Left behind it
// would draw in the week view under a name that can never sync again.
func TestSyncRemovesUnsubscribedCalendars(t *testing.T) {
	start := fixedNow.Add(24 * time.Hour)
	deleted := ownedCalendar("shared", "Shared")
	deleted.Deleted = true
	fake := &fakeCalendar{
		calendarPages: []CalendarListPage{
			{Items: []CalendarListEntry{ownedCalendar("primary", "Work"), ownedCalendar("shared", "Shared")},
				NextSyncToken: "calendars-1"},
			{Items: []CalendarListEntry{deleted}, NextSyncToken: "calendars-2"},
		},
		eventPages: []EventsPage{{
			Items: []Event{timedEvent("e1", "etag-1", "Standup", start)}, NextSyncToken: "events-1",
		}},
	}
	syncer, db, user := newSyncFixture(t, fake)
	ctx := context.Background()

	if _, err := syncer.SyncConnection(ctx, user.ID, fixtureConnectionID); err != nil {
		t.Fatal(err)
	}
	calendars, err := db.ListCalendars(ctx, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(calendars) != 2 {
		t.Fatalf("calendars = %+v, want both", calendars)
	}
	if _, err := syncer.SyncConnection(ctx, user.ID, fixtureConnectionID); err != nil {
		t.Fatal(err)
	}
	remaining := onlyCalendar(t, db, user.ID)
	if remaining.GoogleCalendarID != "primary" {
		t.Fatalf("remaining calendar = %+v, want the one still subscribed", remaining)
	}
}

// All-day events name a day, not a moment. They are anchored at midnight UTC so
// a public holiday does not move to the previous day for anyone west of the
// zone that happened to be in play.
func TestSyncStoresAllDayEventsAsUTCDates(t *testing.T) {
	fake := &fakeCalendar{
		calendarPages: []CalendarListPage{{
			Items: []CalendarListEntry{ownedCalendar("primary", "Work")}, NextSyncToken: "calendars-1",
		}},
		eventPages: []EventsPage{{
			Items: []Event{{
				ID: "holiday", ETag: "etag-1", Status: "confirmed", Summary: "Holiday",
				Start: EventDateTime{Date: "2026-08-17"},
				End:   EventDateTime{Date: "2026-08-18"},
			}},
			NextSyncToken: "events-1",
		}},
	}
	syncer, db, user := newSyncFixture(t, fake)

	if _, err := syncer.SyncConnection(context.Background(), user.ID, fixtureConnectionID); err != nil {
		t.Fatal(err)
	}
	calendar := onlyCalendar(t, db, user.ID)
	events := eventsOf(t, db, user.ID, calendar)
	if len(events) != 1 {
		t.Fatalf("events = %+v, want the all-day event", events)
	}
	event := events[0]
	if !event.AllDay {
		t.Fatal("all-day event was stored as a timed one")
	}
	wantStart := time.Date(2026, 8, 17, 0, 0, 0, 0, time.UTC)
	if !event.StartAt.Equal(wantStart) || !event.EndAt.Equal(wantStart.AddDate(0, 0, 1)) {
		t.Fatalf("bounds = %v..%v, want the UTC day %v", event.StartAt, event.EndAt, wantStart)
	}
}

// A connection authorized before the calendar scope existed keeps working for
// mail. Sync must say so rather than failing at Google with a generic error.
func TestSyncRefusesAConnectionWithoutTheCalendarScope(t *testing.T) {
	syncer, _, user := newSyncFixture(t, &fakeCalendar{})
	syncer.ScopeGranted = func(store.GoogleConnection) bool { return false }

	if _, err := syncer.SyncConnection(context.Background(), user.ID, fixtureConnectionID); err != ErrScopeMissing {
		t.Fatalf("sync without the scope: err=%v, want ErrScopeMissing", err)
	}
}

// One unreadable calendar must not cost the others their sync.
func TestSyncKeepsGoingWhenOneCalendarFails(t *testing.T) {
	fake := &fakeCalendar{
		calendarPages: []CalendarListPage{{
			Items: []CalendarListEntry{ownedCalendar("primary", "Work"), ownedCalendar("shared", "Shared")},
			// No cursor, so both calendars are read in full on every run.
		}},
		eventPages: []EventsPage{{}, {}},
		// The first calendar's read fails outright; 404 is not retried.
		eventStatus: []int{http.StatusNotFound},
	}
	syncer, db, user := newSyncFixture(t, fake)

	result, err := syncer.SyncConnection(context.Background(), user.ID, fixtureConnectionID)
	if err == nil {
		t.Fatal("a failing calendar must still surface as an error")
	}
	if result.Synced != 1 {
		t.Fatalf("sync = %+v, want the second calendar synced anyway", result)
	}
	calendars, err := db.ListCalendars(context.Background(), user.ID)
	if err != nil {
		t.Fatal(err)
	}
	var failed, ok int
	for _, calendar := range calendars {
		switch calendar.Status {
		case store.CalendarSyncStatusError:
			failed++
		case store.CalendarSyncStatusOK:
			ok++
		}
	}
	if failed != 1 || ok != 1 {
		t.Fatalf("calendar states = %+v, want one failed and one ok", calendars)
	}
}
