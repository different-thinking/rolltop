// File overview: Route-level tests for Google contact sync: who may trigger it,
// what a connection without the contacts scope is told, and that deleting a
// mirrored contact reaches Google before the local row disappears.

package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"

	"rolltop/backend/googlepeople"
	"rolltop/backend/googletoken"
	"rolltop/backend/store"
)

// fakePeopleAPI records what the routes actually asked Google to do.
type fakePeopleAPI struct {
	mu      sync.Mutex
	deleted []string
	listed  int
}

func (f *fakePeopleAPI) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/people/me/connections", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.listed++
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"nextSyncToken": "token-1"})
	})
	mux.HandleFunc("/v1/people/", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.deleted = append(f.deleted, r.URL.Path)
		f.mu.Unlock()
		_, _ = w.Write([]byte(`{}`))
	})
	return mux
}

// contactScopeConnections reads connections straight from the store, so the
// routes see the same rows the consent flow wrote.
type contactScopeConnections struct {
	db *store.Store
}

func (c contactScopeConnections) List(ctx context.Context, userID int64) ([]store.GoogleConnection, error) {
	return c.db.ListGoogleConnections(ctx, userID)
}

func (c contactScopeConnections) Get(ctx context.Context, userID, connectionID int64) (store.GoogleConnection, error) {
	return c.db.GoogleConnection(ctx, userID, connectionID)
}

func withContactSync(t *testing.T, env *googleTestEnv, fake *fakePeopleAPI, scopeGranted bool) {
	t.Helper()
	server := httptest.NewServer(fake.handler())
	t.Cleanup(server.Close)
	client := googlepeople.NewClient()
	client.BaseURL = server.URL
	env.server.googleContacts = &googlepeople.Syncer{
		Store:        env.db,
		Client:       client,
		Tokens:       &googletoken.StubTokenSource{Tokens: []string{"access-token"}},
		Connections:  contactScopeConnections{db: env.db},
		ScopeGranted: func(store.GoogleConnection) bool { return scopeGranted },
	}
}

func linkedContact(t *testing.T, env *googleTestEnv, user store.User, connectionID int64, email string) store.Contact {
	t.Helper()
	contact, err := env.db.CreateContact(context.Background(), user.ID, store.Contact{
		DisplayName:        "Mirrored Person",
		Source:             store.ContactSourceGoogle,
		GoogleConnectionID: connectionID,
		ExternalID:         "people/c1",
		ETag:               "etag-1",
		Emails:             []store.ContactEmail{{Label: "Email", Email: email, IsPrimary: true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !contact.IsGoogleContact() {
		t.Fatalf("fixture contact = %+v, want it linked to the connection", contact)
	}
	return contact
}

// Running a sync changes stored data, so it has to be a CSRF-checked POST like
// every other write. A GET that syncs would be triggerable from any page.
func TestContactSyncRejectsUncheckedRequests(t *testing.T) {
	env := newGoogleTestEnv(t)
	connection := env.connect(t, env.owner)
	withContactSync(t, env, &fakePeopleAPI{}, true)
	target := "/api/google/connections/" + strconv.FormatInt(connection.ID, 10) + "/contacts/sync"

	if response := env.send(t, env.owner, http.MethodGet, target, nil); response.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET status=%d, want 405", response.Code)
	}
	request := env.request(t, env.owner, http.MethodPost, target, nil)
	request.Header.Del("X-CSRF-Token")
	response := httptest.NewRecorder()
	env.server.handleAPI(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("POST without a CSRF token status=%d, want 403", response.Code)
	}
}

// Connection ids are guessable, and a sync against somebody else's account
// would pull their address book into this tenant's contacts.
func TestContactSyncRefusesAnotherTenantsConnection(t *testing.T) {
	env := newGoogleTestEnv(t)
	connection := env.connect(t, env.owner)
	fake := &fakePeopleAPI{}
	withContactSync(t, env, fake, true)

	response := env.send(t, env.other, http.MethodPost,
		"/api/google/connections/"+strconv.FormatInt(connection.ID, 10)+"/contacts/sync", nil)
	if response.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant sync status=%d body=%s, want 404", response.Code, response.Body.String())
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.listed != 0 {
		t.Fatalf("Google was called %d times for another tenant's connection, want none", fake.listed)
	}
}

// A connection authorized before contact sync existed still works for mail.
// Reporting its sync as a server error would hide the one thing that fixes it.
func TestContactSyncExplainsAMissingScope(t *testing.T) {
	env := newGoogleTestEnv(t)
	connection := env.connect(t, env.owner)
	withContactSync(t, env, &fakePeopleAPI{}, false)

	response := env.send(t, env.owner, http.MethodPost,
		"/api/google/connections/"+strconv.FormatInt(connection.ID, 10)+"/contacts/sync", nil)
	if response.Code != http.StatusConflict {
		t.Fatalf("status=%d body=%s, want 409", response.Code, response.Body.String())
	}
	var payload struct {
		Error string `json:"error"`
	}
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if payload.Error == "" {
		t.Fatal("the response carried no explanation")
	}
}

// The connections list is what the settings page renders. A grant without the
// contacts scope has to say so, and must not claim a sync state it never had.
func TestConnectionsListReportsTheContactsScope(t *testing.T) {
	env := newGoogleTestEnv(t)
	env.connect(t, env.owner)
	withContactSync(t, env, &fakePeopleAPI{}, true)

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
		t.Fatalf("connections = %d, want 1", len(payload.Connections))
	}
	// The fake consent grants openid, email and mail only.
	entry := payload.Connections[0]
	if entry.HasContactsScope {
		t.Fatalf("connection = %+v, want the contacts scope reported as missing", entry)
	}
	if entry.ContactsSync != nil {
		t.Fatal("a connection that cannot sync contacts reported a sync state")
	}
}

// Deleting a mirrored contact has to reach Google. A delete that only succeeded
// locally would be undone by the next sync, and the confirmation the user gave
// would have meant nothing.
func TestDeletingAMirroredContactRemovesItAtGoogleFirst(t *testing.T) {
	env := newGoogleTestEnv(t)
	connection := env.connect(t, env.owner)
	fake := &fakePeopleAPI{}
	withContactSync(t, env, fake, true)
	contact := linkedContact(t, env, env.owner, connection.ID, "mirrored@example.test")

	response := env.send(t, env.owner, http.MethodDelete,
		"/api/contacts/"+strconv.FormatInt(contact.ID, 10), nil)
	if response.Code != http.StatusOK {
		t.Fatalf("delete status=%d body=%s", response.Code, response.Body.String())
	}
	fake.mu.Lock()
	deleted := append([]string(nil), fake.deleted...)
	fake.mu.Unlock()
	if len(deleted) != 1 || deleted[0] != "/v1/people/c1:deleteContact" {
		t.Fatalf("Google calls = %v, want the contact deleted there", deleted)
	}
	if _, err := env.db.GetContactForUser(context.Background(), env.owner.ID, contact.ID); !store.IsNotFound(err) {
		t.Fatalf("local contact lookup err = %v, want not found", err)
	}
}

// A contact belongs to one tenant. Reaching it by id from another session would
// delete somebody else's contact at Google as well as locally.
func TestDeletingAMirroredContactRefusesAnotherTenant(t *testing.T) {
	env := newGoogleTestEnv(t)
	connection := env.connect(t, env.owner)
	fake := &fakePeopleAPI{}
	withContactSync(t, env, fake, true)
	contact := linkedContact(t, env, env.owner, connection.ID, "mirrored@example.test")

	response := env.send(t, env.other, http.MethodDelete,
		"/api/contacts/"+strconv.FormatInt(contact.ID, 10), nil)
	if response.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant delete status=%d, want 404", response.Code)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.deleted) != 0 {
		t.Fatalf("Google calls = %v, want none for another tenant's contact", fake.deleted)
	}
	if _, err := env.db.GetContactForUser(context.Background(), env.owner.ID, contact.ID); err != nil {
		t.Fatalf("owner's contact err = %v, want it untouched", err)
	}
}

// Contact provenance decides whether an edit travels to Google, so the API has
// to report it. Without it the UI cannot warn before a destructive delete.
func TestContactResponsesCarryTheirSource(t *testing.T) {
	env := newGoogleTestEnv(t)
	connection := env.connect(t, env.owner)
	withContactSync(t, env, &fakePeopleAPI{}, true)
	mirrored := linkedContact(t, env, env.owner, connection.ID, "mirrored@example.test")
	if _, err := env.db.CreateContact(context.Background(), env.owner.ID, store.Contact{
		DisplayName: "Local Person",
		Emails:      []store.ContactEmail{{Label: "Email", Email: "local@example.test", IsPrimary: true}},
	}); err != nil {
		t.Fatal(err)
	}

	response := env.send(t, env.owner, http.MethodGet, "/api/contacts", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var payload struct {
		Contacts []apiContact `json:"contacts"`
	}
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	sources := map[string]apiContact{}
	for _, contact := range payload.Contacts {
		sources[contact.DisplayName] = contact
	}
	if got := sources["Mirrored Person"]; got.Source != store.ContactSourceGoogle || got.GoogleConnectionID != mirrored.GoogleConnectionID {
		t.Fatalf("mirrored contact = %+v, want it reported as a Google contact", got)
	}
	if got := sources["Local Person"]; got.Source != store.ContactSourceLocal || got.GoogleConnectionID != 0 {
		t.Fatalf("local contact = %+v, want it reported as local", got)
	}
}

// The source filter has to reach the query. Applied to the answer instead it
// would only see the first page of every contact, so an account with more
// contacts than the listing cap would appear to hold a fraction of them.
func TestContactListFilterTravelsToTheStore(t *testing.T) {
	env := newGoogleTestEnv(t)
	connection := env.connect(t, env.owner)
	withContactSync(t, env, &fakePeopleAPI{}, true)
	linkedContact(t, env, env.owner, connection.ID, "mirrored@example.test")
	if _, err := env.db.CreateContact(context.Background(), env.owner.ID, store.Contact{
		DisplayName: "Local Person",
		Emails:      []store.ContactEmail{{Label: "Email", Email: "local@example.test", IsPrimary: true}},
	}); err != nil {
		t.Fatal(err)
	}

	names := func(query string) []string {
		response := env.send(t, env.owner, http.MethodGet, "/api/contacts"+query, nil)
		if response.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
		}
		var payload struct {
			Contacts []apiContact `json:"contacts"`
		}
		if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		out := make([]string, 0, len(payload.Contacts))
		for _, contact := range payload.Contacts {
			out = append(out, contact.DisplayName)
		}
		return out
	}

	if got := names(""); len(got) != 2 {
		t.Fatalf("unfiltered = %v, want both contacts", got)
	}
	if got := names("?source=local"); len(got) != 1 || got[0] != "Local Person" {
		t.Fatalf("local = %v, want only the local contact", got)
	}
	if got := names("?source=google:" + strconv.FormatInt(connection.ID, 10)); len(got) != 1 || got[0] != "Mirrored Person" {
		t.Fatalf("per-account = %v, want only that account's contact", got)
	}
	// A malformed source is a client bug, not a reason to silently answer with
	// the whole address book.
	if response := env.send(t, env.owner, http.MethodGet, "/api/contacts?source=nonsense", nil); response.Code != http.StatusBadRequest {
		t.Fatalf("unknown source status=%d, want 400", response.Code)
	}
}

// Disconnecting is not a request to lose the address book: the contacts stay
// and simply stop being mirrors. Nothing else runs on disconnect, so the route
// is the only place this can be verified end to end.
func TestDisconnectingKeepsContactsAndDropsTheCursor(t *testing.T) {
	env := newGoogleTestEnv(t)
	connection := env.connect(t, env.owner)
	withContactSync(t, env, &fakePeopleAPI{}, true)
	contact := linkedContact(t, env, env.owner, connection.ID, "mirrored@example.test")
	ctx := context.Background()
	if err := env.db.SaveGooglePeopleSync(ctx, store.GooglePeopleSync{
		UserID: env.owner.ID, ConnectionID: connection.ID, SyncToken: "token-1",
		Status: store.GooglePeopleSyncStatusOK,
	}); err != nil {
		t.Fatal(err)
	}

	response := env.send(t, env.owner, http.MethodDelete,
		"/api/google/connections/"+strconv.FormatInt(connection.ID, 10), nil)
	if response.Code != http.StatusOK {
		t.Fatalf("disconnect status=%d body=%s", response.Code, response.Body.String())
	}
	kept, err := env.db.GetContactForUser(ctx, env.owner.ID, contact.ID)
	if err != nil {
		t.Fatalf("contact lookup after disconnect: %v", err)
	}
	if kept.IsGoogleContact() {
		t.Fatalf("contact = %+v, want it demoted to a local contact", kept)
	}
	state, err := env.db.GetGooglePeopleSync(ctx, env.owner.ID, connection.ID)
	if err != nil {
		t.Fatal(err)
	}
	if state.SyncToken != "" {
		t.Fatalf("sync token = %q, want it dropped with the connection", state.SyncToken)
	}
}
