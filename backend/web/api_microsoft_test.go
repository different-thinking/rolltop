// File overview: Route-level tests for Microsoft 365: the connection routes,
// tenant isolation, and a calendar week that mixes Microsoft and Google
// calendars -- including one appointment kept in both.

package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"rolltop/backend/m365calendar"
	"rolltop/backend/microsoftauth"
	"rolltop/backend/store"
)

// fakeGraphAPI records what the routes asked Microsoft Graph to do.
type fakeGraphAPI struct {
	mu      sync.Mutex
	created []map[string]any
	deleted []string
	events  map[string]map[string]any
}

func (f *fakeGraphAPI) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		path := strings.TrimPrefix(r.URL.Path, "/v1.0")
		switch {
		case strings.HasSuffix(path, "/events") && r.Method == http.MethodPost:
			var payload map[string]any
			_ = json.NewDecoder(r.Body).Decode(&payload)
			f.created = append(f.created, payload)
			id := "graph-" + strconv.Itoa(len(f.created))
			answer := map[string]any{
				"id": id, "@odata.etag": `W/"1"`, "subject": payload["subject"],
				"start": payload["start"], "end": payload["end"], "isAllDay": payload["isAllDay"],
				"isOrganizer": true, "attendees": payload["attendees"],
				"webLink": "https://outlook.office365.com/owa/?itemid=" + id,
			}
			if payload["isOnlineMeeting"] == true {
				answer["isOnlineMeeting"] = true
				answer["onlineMeetingProvider"] = payload["onlineMeetingProvider"]
				answer["onlineMeeting"] = map[string]any{"joinUrl": "https://teams.microsoft.com/l/meetup-join/" + id}
			}
			f.events[id] = answer
			_ = json.NewEncoder(w).Encode(answer)
		case strings.HasPrefix(path, "/me/events/") && r.Method == http.MethodDelete:
			f.deleted = append(f.deleted, strings.TrimPrefix(path, "/me/events/"))
			w.WriteHeader(http.StatusNoContent)
		case strings.HasPrefix(path, "/me/events/"):
			event, ok := f.events[strings.TrimPrefix(path, "/me/events/")]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_ = json.NewEncoder(w).Encode(event)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
}

type microsoftConnections struct {
	db *store.Store
}

func (c microsoftConnections) List(ctx context.Context, userID int64) ([]store.MicrosoftConnection, error) {
	return c.db.ListMicrosoftConnections(ctx, userID)
}

func (c microsoftConnections) Get(ctx context.Context, userID, connectionID int64) (store.MicrosoftConnection, error) {
	return c.db.MicrosoftConnection(ctx, userID, connectionID)
}

type staticMicrosoftTokens struct{}

func (staticMicrosoftTokens) AccessToken(context.Context, int64, int64) (string, error) {
	return "graph-token", nil
}

func (staticMicrosoftTokens) ForceRefresh(context.Context, int64, int64) (string, error) {
	return "graph-token", nil
}

// withMicrosoft wires a Microsoft manager for the routes that list and remove
// connections, and a calendar syncer pointed at a fake Graph.
func withMicrosoft(t *testing.T, env *googleTestEnv, fake *fakeGraphAPI) {
	t.Helper()
	env.server.microsoftAuth = microsoftauth.NewManager(
		microsoftauth.New("ms-client", "ms-secret", "", []string{"https://rolltop.example.test" + microsoftauth.CallbackPath}, nil),
		env.db, googleTestMasterKey)
	if fake == nil {
		return
	}
	if fake.events == nil {
		fake.events = map[string]map[string]any{}
	}
	server := httptest.NewServer(fake.handler())
	t.Cleanup(server.Close)
	client := m365calendar.NewClient()
	client.BaseURL = server.URL + "/v1.0"
	client.RetryDelay = func(int) time.Duration { return time.Millisecond }
	env.server.microsoftCalendar = &m365calendar.Syncer{
		Store: env.db, Client: client, Tokens: staticMicrosoftTokens{},
		Connections: microsoftConnections{db: env.db},
	}
}

func storedMicrosoftConnection(t *testing.T, env *googleTestEnv, user store.User, email string) store.MicrosoftConnection {
	t.Helper()
	connection, err := env.db.UpsertMicrosoftConnection(context.Background(), user.ID, store.MicrosoftConnectionUpsert{
		Email: email, Subject: "oid-" + email, EncryptedRefreshToken: "v1:refresh",
		GrantedScopes: []string{"Calendars.ReadWrite", "User.Read", "offline_access"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return connection
}

func storedMicrosoftCalendar(t *testing.T, env *googleTestEnv, user store.User, connectionID int64) store.Calendar {
	t.Helper()
	calendar, err := env.db.UpsertMicrosoftCalendar(context.Background(), user.ID, store.MicrosoftCalendarUpsert{
		ConnectionID: connectionID, RemoteCalendarID: "AAMk-default", Summary: "Calendar",
		AccessRole: store.CalendarAccessRoleOwner, IsPrimary: true,
		OnlineMeetingProviders: []string{"teamsForBusiness"}, Selected: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	return calendar
}

func TestMicrosoftConnectionsRoutes(t *testing.T) {
	env := newGoogleTestEnv(t)
	// Without a manager the server answers that Microsoft is unconfigured.
	response := env.send(t, env.owner, http.MethodGet, "/api/microsoft/connections", nil)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"configured":false`) {
		t.Fatalf("unconfigured list: %d %s", response.Code, response.Body.String())
	}
	if response := env.send(t, env.owner, http.MethodPost, "/api/microsoft/connect", nil); response.Code != http.StatusServiceUnavailable {
		t.Fatalf("connect without configuration: %d", response.Code)
	}

	withMicrosoft(t, env, nil)
	connection := storedMicrosoftConnection(t, env, env.owner, "reader@contoso.example")
	response = env.send(t, env.owner, http.MethodGet, "/api/microsoft/connections", nil)
	var listed struct {
		Configured  bool                     `json:"configured"`
		Connections []apiMicrosoftConnection `json:"connections"`
	}
	if err := json.NewDecoder(response.Body).Decode(&listed); err != nil {
		t.Fatal(err)
	}
	if !listed.Configured || len(listed.Connections) != 1 || !listed.Connections[0].HasCalendarScope ||
		listed.Connections[0].Email != "reader@contoso.example" {
		t.Fatalf("list = %+v", listed)
	}
	if strings.Contains(response.Body.String(), "refresh") {
		t.Fatalf("the list leaks token material: %s", response.Body.String())
	}

	// Another tenant sees nothing and can remove nothing.
	response = env.send(t, env.other, http.MethodGet, "/api/microsoft/connections", nil)
	if strings.Contains(response.Body.String(), "contoso") {
		t.Fatalf("another tenant sees the connection: %s", response.Body.String())
	}
	target := "/api/microsoft/connections/" + strconv.FormatInt(connection.ID, 10)
	if response := env.send(t, env.other, http.MethodDelete, target, nil); response.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant disconnect: %d %s", response.Code, response.Body.String())
	}
	if response := env.send(t, env.other, http.MethodPost, target+"/calendar/sync", nil); response.Code == http.StatusOK {
		t.Fatalf("cross-tenant sync answered %d", response.Code)
	}

	// Starting a sign-in hands back Microsoft's authorization URL.
	response = env.send(t, env.owner, http.MethodPost, "/api/microsoft/connect", nil)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "login.microsoftonline.com") {
		t.Fatalf("connect: %d %s", response.Code, response.Body.String())
	}

	if response := env.send(t, env.owner, http.MethodDelete, target, nil); response.Code != http.StatusOK {
		t.Fatalf("disconnect: %d %s", response.Code, response.Body.String())
	}
	if _, err := env.db.MicrosoftConnection(context.Background(), env.owner.ID, connection.ID); !store.IsNotFound(err) {
		t.Fatalf("connection after disconnect: %v", err)
	}
}

// An appointment entered in Microsoft 365 as a Teams meeting, kept in a Google
// second calendar as well: each provider is asked for its own copy, the copy is
// no meeting of its own, the week draws it once, and deleting it removes both.
func TestTeamsMeetingWithACopyInGoogle(t *testing.T) {
	env := newGoogleTestEnv(t)
	googleConnection := env.connect(t, env.owner)
	family := storedCalendar(t, env, env.owner, googleConnection.ID, store.CalendarAccessRoleOwner)
	googleFake := &fakeCalendarAPI{}
	withCalendarSync(t, env, googleFake, true)
	graph := &fakeGraphAPI{}
	withMicrosoft(t, env, graph)
	microsoftConnection := storedMicrosoftConnection(t, env, env.owner, "reader@contoso.example")
	work := storedMicrosoftCalendar(t, env, env.owner, microsoftConnection.ID)
	if err := env.db.SetCalendarCopyTarget(context.Background(), env.owner.ID, family.ID, true); err != nil {
		t.Fatal(err)
	}

	start := time.Date(2026, 10, 20, 8, 0, 0, 0, time.UTC)
	body := []byte(`{"calendar_id":` + strconv.FormatInt(work.ID, 10) +
		`,"summary":"Quarterly review","copy":true,"online_meeting":true,"time_zone":"Europe/Berlin",` +
		`"attendees":[{"email":"guest@example.test","name":"Guest"}],` +
		`"start_at":"` + start.Format(time.RFC3339) + `","end_at":"` + start.Add(time.Hour).Format(time.RFC3339) + `"}`)
	response := env.send(t, env.owner, http.MethodPost, "/api/calendar/events", body)
	if response.Code != http.StatusOK {
		t.Fatalf("create: %d %s", response.Code, response.Body.String())
	}
	var payload struct {
		Event    apiCalendarEvent `json:"event"`
		Warnings []string         `json:"warnings"`
	}
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Warnings) != 0 {
		t.Fatalf("warnings = %v", payload.Warnings)
	}
	if !payload.Event.OnlineMeeting || !strings.HasPrefix(payload.Event.OnlineMeetingURL, "https://teams.microsoft.com/") {
		t.Fatalf("event lacks its Teams link: %+v", payload.Event)
	}
	if len(payload.Event.AlsoIn) != 1 || payload.Event.AlsoIn[0].CalendarID != family.ID || !payload.Event.LinkPrimary {
		t.Fatalf("event does not name its Google copy: %+v", payload.Event)
	}
	graph.mu.Lock()
	if len(graph.created) != 1 || graph.created[0]["isOnlineMeeting"] != true || len(graph.created[0]["attendees"].([]any)) != 1 {
		graph.mu.Unlock()
		t.Fatalf("graph created = %v", graph.created)
	}
	graph.mu.Unlock()
	googleFake.mu.Lock()
	if googleFake.created != 1 {
		googleFake.mu.Unlock()
		t.Fatalf("google was asked for %d copies, want 1", googleFake.created)
	}
	googleFake.mu.Unlock()

	// The calendar list says which provider each calendar is.
	response = env.send(t, env.owner, http.MethodGet, "/api/calendar/calendars", nil)
	var calendars struct {
		Calendars []apiCalendar `json:"calendars"`
	}
	if err := json.NewDecoder(response.Body).Decode(&calendars); err != nil {
		t.Fatal(err)
	}
	providers := map[int64]apiCalendar{}
	for _, calendar := range calendars.Calendars {
		providers[calendar.ID] = calendar
	}
	if got := providers[work.ID]; got.Provider != "microsoft" || got.ConnectionEmail != "reader@contoso.example" ||
		got.ConnectionID != microsoftConnection.ID || len(got.OnlineMeetingProviders) != 1 {
		t.Fatalf("microsoft calendar = %+v", got)
	}
	if got := providers[family.ID]; got.Provider != "google" || len(got.OnlineMeetingProviders) != 0 {
		t.Fatalf("google calendar = %+v", got)
	}

	if events := rangeEvents(t, env, env.owner, start.AddDate(0, 0, -1)); len(events) != 1 || events[0].ID != payload.Event.ID {
		t.Fatalf("week = %+v, want the pair drawn once", events)
	}

	target := "/api/calendar/events/" + strconv.FormatInt(payload.Event.ID, 10)
	if response := env.send(t, env.owner, http.MethodDelete, target, nil); response.Code != http.StatusOK {
		t.Fatalf("delete: %d %s", response.Code, response.Body.String())
	}
	graph.mu.Lock()
	graphDeleted := len(graph.deleted)
	graph.mu.Unlock()
	googleFake.mu.Lock()
	googleDeleted := len(googleFake.deleted)
	googleFake.mu.Unlock()
	if graphDeleted != 1 || googleDeleted != 1 {
		t.Fatalf("deleted microsoft=%d google=%d, want one each", graphDeleted, googleDeleted)
	}
}

// A Teams meeting asked for in a calendar that offers none is refused with a
// reason, before Graph is asked for anything.
func TestTeamsMeetingInACalendarWithoutTeams(t *testing.T) {
	env := newGoogleTestEnv(t)
	graph := &fakeGraphAPI{}
	withMicrosoft(t, env, graph)
	connection := storedMicrosoftConnection(t, env, env.owner, "reader@outlook.example")
	calendar, err := env.db.UpsertMicrosoftCalendar(context.Background(), env.owner.ID, store.MicrosoftCalendarUpsert{
		ConnectionID: connection.ID, RemoteCalendarID: "AAMk-personal", Summary: "Calendar",
		AccessRole: store.CalendarAccessRoleOwner, IsPrimary: true, Selected: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 10, 20, 8, 0, 0, 0, time.UTC)
	body := []byte(`{"calendar_id":` + strconv.FormatInt(calendar.ID, 10) +
		`,"summary":"Call","online_meeting":true,"start_at":"` + start.Format(time.RFC3339) +
		`","end_at":"` + start.Add(time.Hour).Format(time.RFC3339) + `"}`)
	response := env.send(t, env.owner, http.MethodPost, "/api/calendar/events", body)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "Teams") {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	graph.mu.Lock()
	defer graph.mu.Unlock()
	if len(graph.created) != 0 {
		t.Fatalf("graph was asked to create %v", graph.created)
	}
}
