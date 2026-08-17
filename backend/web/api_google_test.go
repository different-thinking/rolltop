// File overview: Route-level tests for the Google connection API, including
// tenant isolation, CSRF enforcement, and that no response leaks a token.

package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"rolltop/backend/googleauth"
	"rolltop/backend/store"
)

var googleTestMasterKey = bytes.Repeat([]byte{7}, 32)

// fakeGoogleServer is a minimal stand-in for Google's OAuth endpoints.
func fakeGoogleServer(t *testing.T, email string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "test-access-token",
			"refresh_token": "test-refresh-token",
			"expires_in":    3600,
			"scope":         "openid email " + googleauth.ScopeMail,
		})
	})
	mux.HandleFunc("/userinfo", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"sub": "subject-1", "email": email, "email_verified": true,
		})
	})
	mux.HandleFunc("/revoke", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

type googleTestEnv struct {
	server  *Server
	db      *store.Store
	owner   store.User
	other   store.User
	manager *googleauth.Manager
}

func newGoogleTestEnv(t *testing.T) *googleTestEnv {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(filepath.Join(t.TempDir(), "rolltop.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	owner, err := db.CreateUser(ctx, "google-owner@example.test", "Owner", "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	other, err := db.CreateUser(ctx, "google-other@example.test", "Other", "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	google := fakeGoogleServer(t, "connected@gmail.example.test")
	manager := googleauth.NewManager(googleauth.Config{
		ClientID:              "client-id",
		ClientSecret:          "client-secret",
		RedirectURLs:          []string{"https://rolltop.example.test" + googleauth.CallbackPath},
		Scopes:                googleauth.DefaultScopes,
		AuthorizationEndpoint: google.URL + "/auth",
		TokenEndpoint:         google.URL + "/token",
		RevokeEndpoint:        google.URL + "/revoke",
		UserinfoEndpoint:      google.URL + "/userinfo",
	}, db, googleTestMasterKey)
	return &googleTestEnv{
		server: &Server{
			store: db, masterKey: googleTestMasterKey, mailListCache: newMailListCache(),
			events: newEventHub(), googleAuth: manager,
		},
		db: db, owner: owner, other: other, manager: manager,
	}
}

func (e *googleTestEnv) request(t *testing.T, user store.User, method, target string, body []byte) *http.Request {
	t.Helper()
	request := httptest.NewRequest(method, target, bytes.NewReader(body))
	request.Host = "rolltop.example.test"
	request = request.WithContext(context.WithValue(request.Context(), userContextKey, currentUser{User: user}))
	if method != http.MethodGet {
		const session = "google-test-session"
		request.AddCookie(&http.Cookie{Name: sessionCookie, Value: session})
		request.Header.Set("X-CSRF-Token", e.server.csrfForBase(session))
		request.Header.Set("Content-Type", "application/json")
	}
	return request
}

// send drives a request through the real /api dispatcher, so these tests cover
// the path parsing and method routing a handler call would skip.
func (e *googleTestEnv) send(t *testing.T, user store.User, method, target string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	response := httptest.NewRecorder()
	e.server.handleAPI(response, e.request(t, user, method, target, body))
	return response
}

// connect drives a full consent round trip through the real routes.
func (e *googleTestEnv) connect(t *testing.T, user store.User) store.GoogleConnection {
	t.Helper()
	state := e.startConnect(t, user, nil)
	callbackResponse := e.send(t, user, http.MethodGet,
		"/api/google/callback?code=auth-code&state="+url.QueryEscape(state), nil)
	if callbackResponse.Code != http.StatusFound {
		t.Fatalf("callback status=%d body=%s", callbackResponse.Code, callbackResponse.Body.String())
	}
	if !strings.HasPrefix(callbackResponse.Header().Get("Location"), googleSettingsPath+"?connected=") {
		t.Fatalf("callback redirected to %q", callbackResponse.Header().Get("Location"))
	}
	connections, err := e.db.ListGoogleConnections(context.Background(), user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(connections) != 1 {
		t.Fatalf("stored connections = %d, want 1", len(connections))
	}
	return connections[0]
}

// startConnect performs the connect request and returns the pending state value.
func (e *googleTestEnv) startConnect(t *testing.T, user store.User, body []byte) string {
	t.Helper()
	if body == nil {
		body = []byte(`{}`)
	}
	response := e.send(t, user, http.MethodPost, "/api/google/connect", body)
	if response.Code != http.StatusOK {
		t.Fatalf("connect status=%d body=%s", response.Code, response.Body.String())
	}
	var payload struct {
		AuthorizationURL string `json:"authorization_url"`
	}
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	authURL, err := url.Parse(payload.AuthorizationURL)
	if err != nil {
		t.Fatal(err)
	}
	state := authURL.Query().Get("state")
	if state == "" {
		t.Fatal("connect response carried no state")
	}
	return state
}

func TestGoogleConnectFlowStoresConnectionAndListsIt(t *testing.T) {
	env := newGoogleTestEnv(t)
	connection := env.connect(t, env.owner)
	if connection.GoogleEmail != "connected@gmail.example.test" {
		t.Fatalf("connection email = %q", connection.GoogleEmail)
	}

	listResponse := env.send(t, env.owner, http.MethodGet, "/api/google/connections", nil)
	if listResponse.Code != http.StatusOK {
		t.Fatalf("list status=%d body=%s", listResponse.Code, listResponse.Body.String())
	}
	raw := listResponse.Body.String()
	// The list feeds a settings page; a token in it would end up in the browser.
	for _, secret := range []string{"test-access-token", "test-refresh-token", "client-secret", "v1:"} {
		if strings.Contains(raw, secret) {
			t.Fatalf("connection list leaked %q: %s", secret, raw)
		}
	}
	var payload struct {
		Configured  bool                  `json:"configured"`
		Connections []apiGoogleConnection `json:"connections"`
	}
	if err := json.NewDecoder(strings.NewReader(raw)).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if !payload.Configured || len(payload.Connections) != 1 {
		t.Fatalf("list payload = %+v", payload)
	}
	if !payload.Connections[0].HasMailScope || payload.Connections[0].NeedsReauth {
		t.Fatalf("connection presentation = %+v", payload.Connections[0])
	}
}

func TestGoogleConnectionsAreNotVisibleToOtherTenants(t *testing.T) {
	env := newGoogleTestEnv(t)
	connection := env.connect(t, env.owner)

	listResponse := env.send(t, env.other, http.MethodGet, "/api/google/connections", nil)
	if listResponse.Code != http.StatusOK {
		t.Fatalf("other tenant list status=%d", listResponse.Code)
	}
	if strings.Contains(listResponse.Body.String(), "connected@gmail.example.test") {
		t.Fatalf("other tenant saw the owner's connection: %s", listResponse.Body.String())
	}

	target := "/api/google/connections/" + strconv.FormatInt(connection.ID, 10)
	deleteResponse := env.send(t, env.other, http.MethodDelete, target, nil)
	if deleteResponse.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant delete status=%d, want 404", deleteResponse.Code)
	}
	testResponse := env.send(t, env.other, http.MethodPost, target+"/test", nil)
	if testResponse.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant test status=%d, want 404", testResponse.Code)
	}
	if _, err := env.db.GoogleConnection(context.Background(), env.owner.ID, connection.ID); err != nil {
		t.Fatalf("owner's connection was disturbed: %v", err)
	}

	// A connect started by the owner must not be completable by another user.
	state := env.startConnect(t, env.owner, nil)
	stolenResponse := env.send(t, env.other, http.MethodGet,
		"/api/google/callback?code=auth-code&state="+url.QueryEscape(state), nil)
	if location := stolenResponse.Header().Get("Location"); !strings.Contains(location, "error=expired") {
		t.Fatalf("hijacked callback redirected to %q, want an error", location)
	}
	if connections, err := env.db.ListGoogleConnections(context.Background(), env.other.ID); err != nil || len(connections) != 0 {
		t.Fatalf("hijacked callback created %v connections for the other tenant (err=%v)", connections, err)
	}
}

func TestGoogleMutationsRequireCSRF(t *testing.T) {
	env := newGoogleTestEnv(t)
	connection := env.connect(t, env.owner)
	target := "/api/google/connections/" + strconv.FormatInt(connection.ID, 10)

	for _, tc := range []struct{ method, path string }{
		{http.MethodDelete, target},
		{http.MethodPost, target + "/test"},
		// Starting a flow takes one of this user's pending slots, so it is a
		// mutation and must be unreachable without a token too.
		{http.MethodPost, "/api/google/connect"},
	} {
		request := httptest.NewRequest(tc.method, tc.path, nil)
		request.Host = "rolltop.example.test"
		request = request.WithContext(context.WithValue(request.Context(), userContextKey, currentUser{User: env.owner}))
		request.AddCookie(&http.Cookie{Name: sessionCookie, Value: "google-test-session"})
		response := httptest.NewRecorder()
		env.server.handleAPI(response, request)
		if response.Code != http.StatusForbidden {
			t.Fatalf("%s %s without CSRF token status=%d, want 403", tc.method, tc.path, response.Code)
		}
	}
	if _, err := env.db.GoogleConnection(context.Background(), env.owner.ID, connection.ID); err != nil {
		t.Fatalf("connection removed by a CSRF-less request: %v", err)
	}
}

func TestGoogleRoutesRequireAuthentication(t *testing.T) {
	env := newGoogleTestEnv(t)
	for _, target := range []string{"/api/google/connections", "/api/google/connect"} {
		request := httptest.NewRequest(http.MethodGet, target, nil)
		response := httptest.NewRecorder()
		env.server.handleAPI(response, request)
		if response.Code != http.StatusUnauthorized && response.Code != http.StatusMethodNotAllowed {
			t.Fatalf("anonymous %s status=%d, want 401 or 405", target, response.Code)
		}
	}
	// The callback is a browser navigation, so an unresolvable session must
	// land the user on a page rather than on a raw JSON error body.
	response := httptest.NewRecorder()
	env.server.handleAPI(response, httptest.NewRequest(http.MethodGet, "/api/google/callback?code=c&state=s", nil))
	if response.Code != http.StatusFound {
		t.Fatalf("anonymous callback status=%d, want a redirect", response.Code)
	}
	if location := response.Header().Get("Location"); location != "/login" {
		t.Fatalf("anonymous callback redirected to %q, want /login", location)
	}
	if body := response.Body.String(); strings.Contains(body, "\"error\"") {
		t.Fatalf("anonymous callback rendered a JSON error body: %s", body)
	}
}

func TestGoogleConnectionTestReportsAccount(t *testing.T) {
	env := newGoogleTestEnv(t)
	connection := env.connect(t, env.owner)
	target := "/api/google/connections/" + strconv.FormatInt(connection.ID, 10) + "/test"

	response := env.send(t, env.owner, http.MethodPost, target, nil)
	if response.Code != http.StatusOK {
		t.Fatalf("test status=%d body=%s", response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), "test-access-token") {
		t.Fatalf("test response leaked the access token: %s", response.Body.String())
	}
	var payload struct {
		OK    bool   `json:"ok"`
		Email string `json:"email"`
	}
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if !payload.OK || payload.Email != "connected@gmail.example.test" {
		t.Fatalf("test payload = %+v", payload)
	}
}

func TestGoogleDisconnectRemovesConnection(t *testing.T) {
	env := newGoogleTestEnv(t)
	connection := env.connect(t, env.owner)
	target := "/api/google/connections/" + strconv.FormatInt(connection.ID, 10)

	response := env.send(t, env.owner, http.MethodDelete, target, nil)
	if response.Code != http.StatusOK {
		t.Fatalf("disconnect status=%d body=%s", response.Code, response.Body.String())
	}
	if _, err := env.db.GoogleConnection(context.Background(), env.owner.ID, connection.ID); !store.IsNotFound(err) {
		t.Fatalf("connection survived disconnect: %v", err)
	}
}

func TestGoogleReauthConnectUsesLoginHintForOwnConnectionOnly(t *testing.T) {
	env := newGoogleTestEnv(t)
	connection := env.connect(t, env.owner)
	body := []byte(`{"connection_id":` + strconv.FormatInt(connection.ID, 10) + `}`)

	response := env.send(t, env.owner, http.MethodPost, "/api/google/connect", body)
	if response.Code != http.StatusOK {
		t.Fatalf("reauth connect status=%d body=%s", response.Code, response.Body.String())
	}
	var payload struct {
		AuthorizationURL string `json:"authorization_url"`
	}
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	authURL, err := url.Parse(payload.AuthorizationURL)
	if err != nil {
		t.Fatal(err)
	}
	if authURL.Query().Get("login_hint") != "connected@gmail.example.test" {
		t.Fatalf("login_hint = %q", authURL.Query().Get("login_hint"))
	}

	// Another tenant may not probe which connection ids exist.
	foreign := env.send(t, env.other, http.MethodPost, "/api/google/connect", body)
	if foreign.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant reauth connect status=%d, want 404", foreign.Code)
	}
}

func TestGoogleRoutesReportUnconfiguredServer(t *testing.T) {
	env := newGoogleTestEnv(t)
	env.server.googleAuth = googleauth.NewManager(googleauth.Config{}, env.db, googleTestMasterKey)

	listResponse := env.send(t, env.owner, http.MethodGet, "/api/google/connections", nil)
	if listResponse.Code != http.StatusOK {
		t.Fatalf("list status=%d", listResponse.Code)
	}
	var payload struct {
		Configured bool `json:"configured"`
	}
	if err := json.NewDecoder(listResponse.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if payload.Configured {
		t.Fatal("unconfigured server reported Google as configured")
	}
	connectResponse := env.send(t, env.owner, http.MethodPost, "/api/google/connect", []byte(`{}`))
	if connectResponse.Code != http.StatusServiceUnavailable {
		t.Fatalf("connect on unconfigured server status=%d, want 503", connectResponse.Code)
	}
}

func TestGoogleCallbackSurfacesConsentDenial(t *testing.T) {
	env := newGoogleTestEnv(t)
	response := env.send(t, env.owner, http.MethodGet, "/api/google/callback?error=access_denied&state=whatever", nil)
	if response.Code != http.StatusFound {
		t.Fatalf("denied callback status=%d", response.Code)
	}
	if location := response.Header().Get("Location"); !strings.Contains(location, "error=access_denied") {
		t.Fatalf("denied callback redirected to %q", location)
	}
	if connections, _ := env.db.ListGoogleConnections(context.Background(), env.owner.ID); len(connections) != 0 {
		t.Fatalf("denied consent created %d connections", len(connections))
	}
}

// deleteFailingStore makes only the local delete fail, leaving the row behind
// after the grant was already revoked at Google.
type deleteFailingStore struct {
	googleauth.ConnectionStore
}

func (s *deleteFailingStore) DeleteGoogleConnection(ctx context.Context, userID, connectionID int64) error {
	return errors.New("database is locked")
}

func TestGoogleDisconnectDoesNotReportSuccessWhenTheRowSurvives(t *testing.T) {
	env := newGoogleTestEnv(t)
	connection := env.connect(t, env.owner)
	env.server.googleAuth = googleauth.NewManager(
		env.manager.Config(), &deleteFailingStore{ConnectionStore: env.db}, googleTestMasterKey)

	target := "/api/google/connections/" + strconv.FormatInt(connection.ID, 10)
	response := env.send(t, env.owner, http.MethodDelete, target, nil)

	// The connection is still there, so telling the user it was disconnected
	// would leave them believing a revoked, dead account was cleaned up.
	if response.Code == http.StatusOK {
		t.Fatalf("failed delete reported as success: %s", response.Body.String())
	}
	if strings.Contains(response.Body.String(), "disconnected") {
		t.Fatalf("failed delete claimed disconnection: %s", response.Body.String())
	}
	if _, err := env.db.GoogleConnection(context.Background(), env.owner.ID, connection.ID); err != nil {
		t.Fatalf("connection should still exist: %v", err)
	}
}

func TestGoogleConnectReportsUnmatchedRedirectURI(t *testing.T) {
	env := newGoogleTestEnv(t)
	cfg := env.manager.Config()
	cfg.RedirectURLs = []string{
		"https://elsewhere.example.test" + googleauth.CallbackPath,
		"http://localhost:8080" + googleauth.CallbackPath,
	}
	env.server.googleAuth = googleauth.NewManager(cfg, env.db, googleTestMasterKey)

	response := env.send(t, env.owner, http.MethodPost, "/api/google/connect", []byte(`{}`))
	// Redirecting to Google with a URI it has never seen only yields an opaque
	// redirect_uri_mismatch page, so the misconfiguration is reported here.
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("unmatched redirect URI status=%d body=%s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "ROLLTOP_GOOGLE_REDIRECT_URLS") {
		t.Fatalf("error does not name the setting to fix: %s", response.Body.String())
	}
}

func TestGoogleConnectionByIDToleratesATrailingSlash(t *testing.T) {
	env := newGoogleTestEnv(t)
	connection := env.connect(t, env.owner)
	// The dispatcher normalizes the path, so a handler that re-parses the raw
	// URL instead of taking the trimmed remainder answers 405 here.
	target := "/api/google/connections/" + strconv.FormatInt(connection.ID, 10) + "/test/"
	response := env.send(t, env.owner, http.MethodPost, target, nil)
	if response.Code != http.StatusOK {
		t.Fatalf("test with a trailing slash status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestGoogleCallbackReleasesTheFlowWhenConsentIsDenied(t *testing.T) {
	env := newGoogleTestEnv(t)
	state := env.startConnect(t, env.owner, nil)

	denied := env.send(t, env.owner, http.MethodGet,
		"/api/google/callback?error=access_denied&state="+url.QueryEscape(state), nil)
	if denied.Code != http.StatusFound {
		t.Fatalf("denied callback status=%d", denied.Code)
	}
	// A cancelled consent must not keep occupying one of this user's few
	// pending slots until the TTL expires.
	completed := env.send(t, env.owner, http.MethodGet,
		"/api/google/callback?code=auth-code&state="+url.QueryEscape(state), nil)
	if location := completed.Header().Get("Location"); !strings.Contains(location, "error=expired") {
		t.Fatalf("cancelled flow was still pending, callback went to %q", location)
	}
}

// The fake Google in this test rejects the stored access token from userinfo and
// then reports invalid_grant on refresh, the way a grant revoked from the Google
// account settings does.
func TestGoogleConnectionTestRefreshesWhenTheStoredTokenIsRejected(t *testing.T) {
	env := newGoogleTestEnv(t)
	connection := env.connect(t, env.owner)

	var rejectOnce sync.Once
	rejected := make(chan struct{})
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		// The refresh that follows the rejection reports the grant is gone.
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
	})
	mux.HandleFunc("/userinfo", func(w http.ResponseWriter, r *http.Request) {
		rejectOnce.Do(func() { close(rejected) })
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"invalid_token"}`))
	})
	google := httptest.NewServer(mux)
	t.Cleanup(google.Close)

	cfg := env.manager.Config()
	cfg.TokenEndpoint = google.URL + "/token"
	cfg.UserinfoEndpoint = google.URL + "/userinfo"
	manager := googleauth.NewManager(cfg, env.db, googleTestMasterKey)
	manager.Client().RetryDelay = func(int) time.Duration { return time.Millisecond }
	env.server.googleAuth = manager

	target := "/api/google/connections/" + strconv.FormatInt(connection.ID, 10) + "/test"
	response := env.send(t, env.owner, http.MethodPost, target, nil)

	<-rejected
	// Reporting a Google outage would be wrong and would repeat for as long as
	// the unexpired token stays cached; the user needs to be told to reconnect.
	if response.Code != http.StatusConflict {
		t.Fatalf("test against a revoked grant status=%d body=%s", response.Code, response.Body.String())
	}
	stored, err := env.db.GoogleConnection(context.Background(), env.owner.ID, connection.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !stored.NeedsReauth() {
		t.Fatalf("connection status=%q, want reauth_required", stored.Status)
	}
}

// lockedStore fails every read the way a busy SQLite does.
type lockedStore struct {
	googleauth.ConnectionStore
}

func (s *lockedStore) GoogleConnection(ctx context.Context, userID, connectionID int64) (store.GoogleConnection, error) {
	return store.GoogleConnection{}, errors.New("database is locked")
}

func TestGoogleLocalFailuresAreNotReportedAsAGoogleOutage(t *testing.T) {
	env := newGoogleTestEnv(t)
	connection := env.connect(t, env.owner)
	env.server.googleAuth = googleauth.NewManager(
		env.manager.Config(), &lockedStore{ConnectionStore: env.db}, googleTestMasterKey)

	target := "/api/google/connections/" + strconv.FormatInt(connection.ID, 10) + "/test"
	response := env.send(t, env.owner, http.MethodPost, target, nil)
	// Blaming Google sends the operator off debugging OAuth while the fault is
	// on this machine.
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("local store failure status=%d body=%s", response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), "Google could not be reached") {
		t.Fatalf("local store failure blamed on Google: %s", response.Body.String())
	}
}
