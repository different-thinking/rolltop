// File overview: Route-level tests for the calendar surface: who may read a
// range, who may switch a calendar on, and that a write reaches Google before
// the local row changes.

package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"rolltop/backend/googleauth"
	"rolltop/backend/googlecalendar"
	"rolltop/backend/googletoken"
	"rolltop/backend/store"
)

// fakeCalendarAPI records what the routes actually asked Google to do.
type fakeCalendarAPI struct {
	mu      sync.Mutex
	listed  int
	created int
	deleted []string
	// event is what a read of an event answers with.
	event map[string]any
}

func (f *fakeCalendarAPI) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/calendar/v3/users/me/calendarList", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.listed++
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"nextSyncToken": "calendars-1"})
	})
	mux.HandleFunc("/calendar/v3/calendars/", func(w http.ResponseWriter, r *http.Request) {
		rest := strings.TrimPrefix(r.URL.Path, "/calendar/v3/calendars/")
		_, tail, _ := strings.Cut(rest, "/events")
		eventID := strings.TrimPrefix(tail, "/")
		f.mu.Lock()
		defer f.mu.Unlock()
		switch {
		case eventID == "" && r.Method == http.MethodPost:
			f.created++
			var write map[string]any
			_ = json.NewDecoder(r.Body).Decode(&write)
			write["id"] = "created-1"
			write["etag"] = "etag-created"
			write["status"] = "confirmed"
			_ = json.NewEncoder(w).Encode(write)
		case eventID == "":
			_ = json.NewEncoder(w).Encode(map[string]any{"nextSyncToken": "events-1"})
		case r.Method == http.MethodDelete:
			f.deleted = append(f.deleted, eventID)
			w.WriteHeader(http.StatusNoContent)
		default:
			_ = json.NewEncoder(w).Encode(f.event)
		}
	})
	return mux
}

// calendarConnections reads connections straight from the store, so the routes
// see the same rows the consent flow wrote.
type calendarConnections struct {
	db *store.Store
}

func (c calendarConnections) List(ctx context.Context, userID int64) ([]store.GoogleConnection, error) {
	return c.db.ListGoogleConnections(ctx, userID)
}

func (c calendarConnections) Get(ctx context.Context, userID, connectionID int64) (store.GoogleConnection, error) {
	return c.db.GoogleConnection(ctx, userID, connectionID)
}

func withCalendarSync(t *testing.T, env *googleTestEnv, fake *fakeCalendarAPI, scopeGranted bool) {
	t.Helper()
	server := httptest.NewServer(fake.handler())
	t.Cleanup(server.Close)
	client := googlecalendar.NewClient()
	client.BaseURL = server.URL
	client.RetryDelay = func(int) time.Duration { return time.Millisecond }
	env.server.googleCalendar = &googlecalendar.Syncer{
		Store:        env.db,
		Client:       client,
		Tokens:       &googletoken.StubTokenSource{Tokens: []string{"access-token"}},
		Connections:  calendarConnections{db: env.db},
		ScopeGranted: func(store.GoogleConnection) bool { return scopeGranted },
	}
}

// storedCalendar puts a synced calendar in place without going through Google.
func storedCalendar(t *testing.T, env *googleTestEnv, user store.User, connectionID int64, accessRole string) store.Calendar {
	t.Helper()
	calendar, err := env.db.UpsertCalendar(context.Background(), user.ID, store.CalendarUpsert{
		GoogleConnectionID: connectionID,
		GoogleCalendarID:   "primary",
		Summary:            "Work",
		AccessRole:         accessRole,
		IsPrimary:          true,
		Selected:           true,
	})
	if err != nil {
		t.Fatal(err)
	}
	return calendar
}

func storedEvent(t *testing.T, env *googleTestEnv, user store.User, calendarID int64, start time.Time) store.CalendarEvent {
	t.Helper()
	event, err := env.db.UpsertCalendarEvent(context.Background(), user.ID, store.CalendarEvent{
		CalendarID: calendarID, ExternalID: "e1", ETag: "etag-1",
		Summary: "Standup", StartAt: start, EndAt: start.Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	return event
}

func rangeQuery(from time.Time, days int) string {
	values := url.Values{
		"from": []string{from.UTC().Format(time.RFC3339)},
		"to":   []string{from.AddDate(0, 0, days).UTC().Format(time.RFC3339)},
	}
	return "/api/calendar/events?" + values.Encode()
}

// Calendar ids are guessable, and a range query naming somebody else's calendar
// must not hand their week over.
func TestCalendarEventsAreScopedToTheSignedInUser(t *testing.T) {
	env := newGoogleTestEnv(t)
	connection := env.connect(t, env.owner)
	calendar := storedCalendar(t, env, env.owner, connection.ID, store.CalendarAccessRoleOwner)
	start := time.Date(2026, 8, 18, 9, 0, 0, 0, time.UTC)
	storedEvent(t, env, env.owner, calendar.ID, start)

	response := env.send(t, env.owner, http.MethodGet, rangeQuery(start.AddDate(0, 0, -1), 7), nil)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var payload struct {
		Events []apiCalendarEvent `json:"events"`
	}
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Events) != 1 || payload.Events[0].Summary != "Standup" {
		t.Fatalf("events = %+v, want the owner's event", payload.Events)
	}

	otherResponse := env.send(t, env.other, http.MethodGet, rangeQuery(start.AddDate(0, 0, -1), 7), nil)
	if otherResponse.Code != http.StatusOK {
		t.Fatalf("other tenant status=%d body=%s", otherResponse.Code, otherResponse.Body.String())
	}
	var otherPayload struct {
		Events []apiCalendarEvent `json:"events"`
	}
	if err := json.NewDecoder(otherResponse.Body).Decode(&otherPayload); err != nil {
		t.Fatal(err)
	}
	if len(otherPayload.Events) != 0 {
		t.Fatalf("other tenant saw %+v, want nothing", otherPayload.Events)
	}
}

// A calendar switched off is not drawn, and the server decides that from stored
// state rather than from anything the request carries.
func TestCalendarRangeSkipsHiddenCalendars(t *testing.T) {
	env := newGoogleTestEnv(t)
	connection := env.connect(t, env.owner)
	calendar := storedCalendar(t, env, env.owner, connection.ID, store.CalendarAccessRoleOwner)
	start := time.Date(2026, 8, 18, 9, 0, 0, 0, time.UTC)
	storedEvent(t, env, env.owner, calendar.ID, start)
	if err := env.db.SetCalendarSelected(context.Background(), env.owner.ID, calendar.ID, false); err != nil {
		t.Fatal(err)
	}

	response := env.send(t, env.owner, http.MethodGet, rangeQuery(start.AddDate(0, 0, -1), 7), nil)
	var payload struct {
		Events []apiCalendarEvent `json:"events"`
	}
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Events) != 0 {
		t.Fatalf("events = %+v, want none from a hidden calendar", payload.Events)
	}
}

// A range without bounds, or one long enough to mean the whole mirror, is a
// client bug rather than a request to assemble everything.
func TestCalendarRangeRejectsUnusableBounds(t *testing.T) {
	env := newGoogleTestEnv(t)
	env.connect(t, env.owner)
	start := time.Date(2026, 8, 18, 0, 0, 0, 0, time.UTC)

	if response := env.send(t, env.owner, http.MethodGet, "/api/calendar/events", nil); response.Code != http.StatusBadRequest {
		t.Fatalf("range without bounds status=%d, want 400", response.Code)
	}
	if response := env.send(t, env.owner, http.MethodGet, rangeQuery(start, 500), nil); response.Code != http.StatusBadRequest {
		t.Fatalf("overlong range status=%d, want 400", response.Code)
	}
	// A backwards range is not an empty week, it is a mistake.
	values := url.Values{
		"from": []string{start.Format(time.RFC3339)},
		"to":   []string{start.AddDate(0, 0, -1).Format(time.RFC3339)},
	}
	if response := env.send(t, env.owner, http.MethodGet, "/api/calendar/events?"+values.Encode(), nil); response.Code != http.StatusBadRequest {
		t.Fatalf("backwards range status=%d, want 400", response.Code)
	}
}

// Switching a calendar changes stored state, so it has to be a CSRF-checked
// write, and it must not be possible against another tenant's calendar.
func TestCalendarVisibilityIsProtected(t *testing.T) {
	env := newGoogleTestEnv(t)
	connection := env.connect(t, env.owner)
	calendar := storedCalendar(t, env, env.owner, connection.ID, store.CalendarAccessRoleOwner)
	withCalendarSync(t, env, &fakeCalendarAPI{}, true)
	target := "/api/calendar/calendars/" + strconv.FormatInt(calendar.ID, 10)

	request := env.request(t, env.owner, http.MethodPut, target, []byte(`{"selected":false}`))
	request.Header.Del("X-CSRF-Token")
	response := httptest.NewRecorder()
	env.server.handleAPI(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("PUT without a CSRF token status=%d, want 403", response.Code)
	}

	if other := env.send(t, env.other, http.MethodPut, target, []byte(`{"selected":false}`)); other.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant switch status=%d body=%s, want 404", other.Code, other.Body.String())
	}
	stored, err := env.db.Calendar(context.Background(), env.owner.ID, calendar.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !stored.Selected {
		t.Fatal("a rejected request switched the calendar off anyway")
	}

	if ok := env.send(t, env.owner, http.MethodPut, target, []byte(`{"selected":false}`)); ok.Code != http.StatusOK {
		t.Fatalf("owner switch status=%d body=%s", ok.Code, ok.Body.String())
	}
	stored, err = env.db.Calendar(context.Background(), env.owner.ID, calendar.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Selected {
		t.Fatal("the calendar was not switched off")
	}
}

// An event is created at Google first, so the row Rolltop stores carries the
// identifiers Google assigned rather than ones invented here.
func TestCreateEventGoesThroughGoogle(t *testing.T) {
	env := newGoogleTestEnv(t)
	connection := env.connect(t, env.owner)
	calendar := storedCalendar(t, env, env.owner, connection.ID, store.CalendarAccessRoleOwner)
	fake := &fakeCalendarAPI{}
	withCalendarSync(t, env, fake, true)

	start := time.Date(2026, 8, 18, 9, 0, 0, 0, time.UTC)
	body := []byte(`{"calendar_id":` + strconv.FormatInt(calendar.ID, 10) +
		`,"summary":"Planning","start_at":"` + start.Format(time.RFC3339) +
		`","end_at":"` + start.Add(time.Hour).Format(time.RFC3339) + `"}`)
	response := env.send(t, env.owner, http.MethodPost, "/api/calendar/events", body)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	fake.mu.Lock()
	created := fake.created
	fake.mu.Unlock()
	if created != 1 {
		t.Fatalf("Google was asked to create %d events, want 1", created)
	}
	var payload struct {
		Event apiCalendarEvent `json:"event"`
	}
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if payload.Event.Summary != "Planning" || payload.Event.ID == 0 {
		t.Fatalf("event = %+v, want the stored mirror", payload.Event)
	}
}

// An event without a title or with an end before its start is refused before
// anything reaches Google.
func TestCreateEventValidatesTheSubmission(t *testing.T) {
	env := newGoogleTestEnv(t)
	connection := env.connect(t, env.owner)
	calendar := storedCalendar(t, env, env.owner, connection.ID, store.CalendarAccessRoleOwner)
	fake := &fakeCalendarAPI{}
	withCalendarSync(t, env, fake, true)
	start := time.Date(2026, 8, 18, 9, 0, 0, 0, time.UTC)
	id := strconv.FormatInt(calendar.ID, 10)

	cases := map[string][]byte{
		"no title": []byte(`{"calendar_id":` + id + `,"summary":"  ","start_at":"` +
			start.Format(time.RFC3339) + `","end_at":"` + start.Add(time.Hour).Format(time.RFC3339) + `"}`),
		"backwards": []byte(`{"calendar_id":` + id + `,"summary":"Planning","start_at":"` +
			start.Format(time.RFC3339) + `","end_at":"` + start.Add(-time.Hour).Format(time.RFC3339) + `"}`),
		"no calendar": []byte(`{"summary":"Planning","start_at":"` +
			start.Format(time.RFC3339) + `","end_at":"` + start.Add(time.Hour).Format(time.RFC3339) + `"}`),
	}
	for name, body := range cases {
		if response := env.send(t, env.owner, http.MethodPost, "/api/calendar/events", body); response.Code != http.StatusBadRequest {
			t.Fatalf("%s: status=%d body=%s, want 400", name, response.Code, response.Body.String())
		}
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.created != 0 {
		t.Fatalf("Google was called %d times for invalid submissions, want none", fake.created)
	}
}

// A calendar shared read-only is still drawn, but a write into it is refused
// here rather than failing at Google with a message nobody can act on.
func TestWritesRefuseAReadOnlyCalendar(t *testing.T) {
	env := newGoogleTestEnv(t)
	connection := env.connect(t, env.owner)
	calendar := storedCalendar(t, env, env.owner, connection.ID, "reader")
	fake := &fakeCalendarAPI{}
	withCalendarSync(t, env, fake, true)
	start := time.Date(2026, 8, 18, 9, 0, 0, 0, time.UTC)
	event := storedEvent(t, env, env.owner, calendar.ID, start)

	body := []byte(`{"calendar_id":` + strconv.FormatInt(calendar.ID, 10) +
		`,"summary":"Planning","start_at":"` + start.Format(time.RFC3339) +
		`","end_at":"` + start.Add(time.Hour).Format(time.RFC3339) + `"}`)
	if response := env.send(t, env.owner, http.MethodPost, "/api/calendar/events", body); response.Code != http.StatusForbidden {
		t.Fatalf("create status=%d body=%s, want 403", response.Code, response.Body.String())
	}
	target := "/api/calendar/events/" + strconv.FormatInt(event.ID, 10)
	if response := env.send(t, env.owner, http.MethodDelete, target, nil); response.Code != http.StatusForbidden {
		t.Fatalf("delete status=%d body=%s, want 403", response.Code, response.Body.String())
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.created != 0 || len(fake.deleted) != 0 {
		t.Fatalf("Google was called anyway: created=%d deleted=%v", fake.created, fake.deleted)
	}
}

// A delete reaches Google before the mirror goes, because a local-only delete
// comes back on the next sync.
func TestDeleteEventReachesGoogleFirst(t *testing.T) {
	env := newGoogleTestEnv(t)
	connection := env.connect(t, env.owner)
	calendar := storedCalendar(t, env, env.owner, connection.ID, store.CalendarAccessRoleOwner)
	fake := &fakeCalendarAPI{}
	withCalendarSync(t, env, fake, true)
	start := time.Date(2026, 8, 18, 9, 0, 0, 0, time.UTC)
	event := storedEvent(t, env, env.owner, calendar.ID, start)
	target := "/api/calendar/events/" + strconv.FormatInt(event.ID, 10)

	// Another tenant must not be able to reach it at all.
	if other := env.send(t, env.other, http.MethodDelete, target, nil); other.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant delete status=%d, want 404", other.Code)
	}
	if response := env.send(t, env.owner, http.MethodDelete, target, nil); response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	fake.mu.Lock()
	deleted := append([]string(nil), fake.deleted...)
	fake.mu.Unlock()
	if len(deleted) != 1 || deleted[0] != "e1" {
		t.Fatalf("deleted at Google = %v, want the event", deleted)
	}
	if _, err := env.db.CalendarEvent(context.Background(), env.owner.ID, event.ID); !store.IsNotFound(err) {
		t.Fatalf("local mirror survived: err=%v", err)
	}
}

// A connection authorized before calendar sync existed still works for mail.
// Reporting its sync as a server error would hide the one thing that fixes it.
func TestCalendarSyncExplainsAMissingScope(t *testing.T) {
	env := newGoogleTestEnv(t)
	connection := env.connect(t, env.owner)
	withCalendarSync(t, env, &fakeCalendarAPI{}, false)

	response := env.send(t, env.owner, http.MethodPost,
		"/api/google/connections/"+strconv.FormatInt(connection.ID, 10)+"/calendar/sync", nil)
	if response.Code != http.StatusConflict {
		t.Fatalf("status=%d body=%s, want 409", response.Code, response.Body.String())
	}
}

// Connection ids are guessable, and a sync against somebody else's account
// would pull their calendars into this tenant.
func TestCalendarSyncRefusesAnotherTenantsConnection(t *testing.T) {
	env := newGoogleTestEnv(t)
	connection := env.connect(t, env.owner)
	fake := &fakeCalendarAPI{}
	withCalendarSync(t, env, fake, true)

	response := env.send(t, env.other, http.MethodPost,
		"/api/google/connections/"+strconv.FormatInt(connection.ID, 10)+"/calendar/sync", nil)
	if response.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant sync status=%d body=%s, want 404", response.Code, response.Body.String())
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.listed != 0 {
		t.Fatalf("Google was called %d times for another tenant's connection, want none", fake.listed)
	}
}

// The connections list is what the settings page renders. Whether the calendar
// scope was granted decides between offering a sync and offering re-consent, so
// both answers have to be right.
func TestConnectionsListReportsTheCalendarScope(t *testing.T) {
	env := newGoogleTestEnv(t)
	connection := env.connect(t, env.owner)
	withCalendarSync(t, env, &fakeCalendarAPI{}, true)

	// The fake consent grants openid, email and mail only, which is exactly the
	// shape of a connection made before this phase existed.
	before := connectionsList(t, env)
	if before.HasCalendarScope {
		t.Fatalf("connection = %+v, want the calendar scope reported as missing", before)
	}
	if before.CalendarSync != nil {
		t.Fatal("a connection that cannot sync calendars reported a sync state")
	}

	if _, err := env.db.UpsertGoogleConnection(context.Background(), env.owner.ID, store.GoogleConnectionUpsert{
		GoogleEmail:           connection.GoogleEmail,
		GoogleSubject:         connection.GoogleSubject,
		EncryptedRefreshToken: connection.EncryptedRefreshToken,
		GrantedScopes:         append(connection.GrantedScopes, googleauth.ScopeCalendar),
	}); err != nil {
		t.Fatal(err)
	}
	after := connectionsList(t, env)
	if !after.HasCalendarScope {
		t.Fatalf("connection = %+v, want the calendar scope reported", after)
	}
	if after.CalendarSync == nil || after.CalendarSync.EverSynced {
		t.Fatalf("calendar sync = %+v, want a state that has never run", after.CalendarSync)
	}
}

func connectionsList(t *testing.T, env *googleTestEnv) apiGoogleConnection {
	t.Helper()
	response := env.send(t, env.owner, http.MethodGet, "/api/google/connections", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var payload struct {
		Connections []apiGoogleConnection `json:"connections"`
	}
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Connections) != 1 {
		t.Fatalf("connections = %+v, want one", payload.Connections)
	}
	return payload.Connections[0]
}
