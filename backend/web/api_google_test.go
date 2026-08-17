// File overview: Route-level tests for the Google connection API, including
// tenant isolation, CSRF enforcement, and that no response leaks a token.

package web

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

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

// connect drives a full consent round trip through the real routes.
func (e *googleTestEnv) connect(t *testing.T, user store.User) store.GoogleConnection {
	t.Helper()
	connectResponse := httptest.NewRecorder()
	e.server.apiGoogleConnect(connectResponse, e.request(t, user, http.MethodGet, "/api/google/connect", nil))
	if connectResponse.Code != http.StatusFound {
		t.Fatalf("connect status=%d body=%s", connectResponse.Code, connectResponse.Body.String())
	}
	authURL, err := url.Parse(connectResponse.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	state := authURL.Query().Get("state")
	if state == "" {
		t.Fatal("connect redirect carried no state")
	}
	callback := e.request(t, user, http.MethodGet, "/api/google/callback?code=auth-code&state="+url.QueryEscape(state), nil)
	callbackResponse := httptest.NewRecorder()
	e.server.apiGoogleCallback(callbackResponse, callback)
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

func TestGoogleConnectFlowStoresConnectionAndListsIt(t *testing.T) {
	env := newGoogleTestEnv(t)
	connection := env.connect(t, env.owner)
	if connection.GoogleEmail != "connected@gmail.example.test" {
		t.Fatalf("connection email = %q", connection.GoogleEmail)
	}

	listResponse := httptest.NewRecorder()
	env.server.apiGoogleConnections(listResponse, env.request(t, env.owner, http.MethodGet, "/api/google/connections", nil))
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

	listResponse := httptest.NewRecorder()
	env.server.apiGoogleConnections(listResponse, env.request(t, env.other, http.MethodGet, "/api/google/connections", nil))
	if listResponse.Code != http.StatusOK {
		t.Fatalf("other tenant list status=%d", listResponse.Code)
	}
	if strings.Contains(listResponse.Body.String(), "connected@gmail.example.test") {
		t.Fatalf("other tenant saw the owner's connection: %s", listResponse.Body.String())
	}

	target := "/api/google/connections/" + strconv.FormatInt(connection.ID, 10)
	deleteResponse := httptest.NewRecorder()
	env.server.apiGoogleConnectionByID(deleteResponse, env.request(t, env.other, http.MethodDelete, target, nil))
	if deleteResponse.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant delete status=%d, want 404", deleteResponse.Code)
	}
	testResponse := httptest.NewRecorder()
	env.server.apiGoogleConnectionByID(testResponse, env.request(t, env.other, http.MethodPost, target+"/test", nil))
	if testResponse.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant test status=%d, want 404", testResponse.Code)
	}
	if _, err := env.db.GoogleConnection(context.Background(), env.owner.ID, connection.ID); err != nil {
		t.Fatalf("owner's connection was disturbed: %v", err)
	}

	// A connect started by the owner must not be completable by another user.
	connectResponse := httptest.NewRecorder()
	env.server.apiGoogleConnect(connectResponse, env.request(t, env.owner, http.MethodGet, "/api/google/connect", nil))
	authURL, _ := url.Parse(connectResponse.Header().Get("Location"))
	stolen := env.request(t, env.other, http.MethodGet,
		"/api/google/callback?code=auth-code&state="+url.QueryEscape(authURL.Query().Get("state")), nil)
	stolenResponse := httptest.NewRecorder()
	env.server.apiGoogleCallback(stolenResponse, stolen)
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
	} {
		request := httptest.NewRequest(tc.method, tc.path, nil)
		request.Host = "rolltop.example.test"
		request = request.WithContext(context.WithValue(request.Context(), userContextKey, currentUser{User: env.owner}))
		request.AddCookie(&http.Cookie{Name: sessionCookie, Value: "google-test-session"})
		response := httptest.NewRecorder()
		env.server.apiGoogleConnectionByID(response, request)
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
	for _, target := range []string{"/api/google/connections", "/api/google/connect", "/api/google/callback"} {
		request := httptest.NewRequest(http.MethodGet, target, nil)
		response := httptest.NewRecorder()
		switch target {
		case "/api/google/connections":
			env.server.apiGoogleConnections(response, request)
		case "/api/google/connect":
			env.server.apiGoogleConnect(response, request)
		default:
			env.server.apiGoogleCallback(response, request)
		}
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("anonymous %s status=%d, want 401", target, response.Code)
		}
	}
}

func TestGoogleConnectionTestReportsAccount(t *testing.T) {
	env := newGoogleTestEnv(t)
	connection := env.connect(t, env.owner)
	target := "/api/google/connections/" + strconv.FormatInt(connection.ID, 10) + "/test"

	response := httptest.NewRecorder()
	env.server.apiGoogleConnectionByID(response, env.request(t, env.owner, http.MethodPost, target, nil))
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

	response := httptest.NewRecorder()
	env.server.apiGoogleConnectionByID(response, env.request(t, env.owner, http.MethodDelete, target, nil))
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
	suffix := "?connection_id=" + strconv.FormatInt(connection.ID, 10)

	response := httptest.NewRecorder()
	env.server.apiGoogleConnect(response, env.request(t, env.owner, http.MethodGet, "/api/google/connect"+suffix, nil))
	if response.Code != http.StatusFound {
		t.Fatalf("reauth connect status=%d", response.Code)
	}
	authURL, err := url.Parse(response.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	if authURL.Query().Get("login_hint") != "connected@gmail.example.test" {
		t.Fatalf("login_hint = %q", authURL.Query().Get("login_hint"))
	}

	// Another tenant may not probe which connection ids exist.
	foreign := httptest.NewRecorder()
	env.server.apiGoogleConnect(foreign, env.request(t, env.other, http.MethodGet, "/api/google/connect"+suffix, nil))
	if foreign.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant reauth connect status=%d, want 404", foreign.Code)
	}
}

func TestGoogleRoutesReportUnconfiguredServer(t *testing.T) {
	env := newGoogleTestEnv(t)
	env.server.googleAuth = googleauth.NewManager(googleauth.Config{}, env.db, googleTestMasterKey)

	listResponse := httptest.NewRecorder()
	env.server.apiGoogleConnections(listResponse, env.request(t, env.owner, http.MethodGet, "/api/google/connections", nil))
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
	connectResponse := httptest.NewRecorder()
	env.server.apiGoogleConnect(connectResponse, env.request(t, env.owner, http.MethodGet, "/api/google/connect", nil))
	if connectResponse.Code != http.StatusServiceUnavailable {
		t.Fatalf("connect on unconfigured server status=%d, want 503", connectResponse.Code)
	}
}

func TestGoogleCallbackSurfacesConsentDenial(t *testing.T) {
	env := newGoogleTestEnv(t)
	request := env.request(t, env.owner, http.MethodGet, "/api/google/callback?error=access_denied&state=whatever", nil)
	response := httptest.NewRecorder()
	env.server.apiGoogleCallback(response, request)
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
