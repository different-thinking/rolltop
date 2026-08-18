// File overview: Tests for the Google OAuth flow, token refresh, and revocation
// against a fake Google. No test may depend on reaching accounts.google.com.

package googleauth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"rolltop/backend/crypto"
	"rolltop/backend/store"
)

var testMasterKey = []byte("0123456789abcdef0123456789abcdef")

// fakeGoogle stands in for Google's OAuth and userinfo endpoints.
type fakeGoogle struct {
	server *httptest.Server

	mu             sync.Mutex
	tokenCalls     int
	revokeCalls    int
	revokedTokens  []string
	lastTokenForm  url.Values
	accessCounter  int
	refreshFailure string
	// tokenStatuses is consumed one entry per token request, letting a test
	// script transient failures before a success.
	tokenStatuses []int
	email         string
	scope         string
	omitRefresh   bool
	// beforeToken blocks inside the token handler so a test can hold a refresh
	// in flight while it starts a second one.
	beforeToken func()
}

func newFakeGoogle(t *testing.T) *fakeGoogle {
	t.Helper()
	g := &fakeGoogle{email: "user@gmail.example.test", scope: "openid email " + ScopeMail}
	mux := http.NewServeMux()
	mux.HandleFunc("/token", g.handleToken)
	mux.HandleFunc("/revoke", g.handleRevoke)
	mux.HandleFunc("/userinfo", g.handleUserinfo)
	g.server = httptest.NewServer(mux)
	t.Cleanup(g.server.Close)
	return g
}

func (g *fakeGoogle) handleToken(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	g.mu.Lock()
	hold := g.beforeToken
	g.mu.Unlock()
	if hold != nil {
		hold()
	}
	g.mu.Lock()
	g.tokenCalls++
	g.lastTokenForm = r.PostForm
	if len(g.tokenStatuses) > 0 {
		status := g.tokenStatuses[0]
		g.tokenStatuses = g.tokenStatuses[1:]
		g.mu.Unlock()
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"error":"backend_error"}`))
		return
	}
	if g.refreshFailure != "" && r.PostForm.Get("grant_type") == "refresh_token" {
		failure := g.refreshFailure
		g.mu.Unlock()
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"` + failure + `","error_description":"Token has been expired or revoked."}`))
		return
	}
	g.accessCounter++
	access := "access-token-" + strconv.Itoa(g.accessCounter)
	scope := g.scope
	omitRefresh := g.omitRefresh
	g.mu.Unlock()

	payload := map[string]any{
		"access_token": access,
		"expires_in":   3600,
		"scope":        scope,
		"token_type":   "Bearer",
	}
	if !omitRefresh {
		payload["refresh_token"] = "refresh-token-1"
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(payload)
}

func (g *fakeGoogle) handleRevoke(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	g.mu.Lock()
	g.revokeCalls++
	g.revokedTokens = append(g.revokedTokens, r.PostForm.Get("token"))
	g.mu.Unlock()
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{}`))
}

func (g *fakeGoogle) handleUserinfo(w http.ResponseWriter, r *http.Request) {
	if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	g.mu.Lock()
	email := g.email
	g.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"sub": "google-subject-1", "email": email, "email_verified": true,
	})
}

func (g *fakeGoogle) counts() (tokenCalls, revokeCalls int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.tokenCalls, g.revokeCalls
}

func (g *fakeGoogle) config() Config {
	return Config{
		ClientID:              "client-id",
		ClientSecret:          "client-secret",
		RedirectURLs:          []string{"https://rolltop.example.test" + CallbackPath},
		Scopes:                DefaultScopes,
		AuthorizationEndpoint: g.server.URL + "/auth",
		TokenEndpoint:         g.server.URL + "/token",
		RevokeEndpoint:        g.server.URL + "/revoke",
		UserinfoEndpoint:      g.server.URL + "/userinfo",
	}
}

type testEnv struct {
	manager *Manager
	google  *fakeGoogle
	db      *store.Store
	userID  int64
	otherID int64
	now     time.Time
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(filepath.Join(t.TempDir(), "rolltop.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	user, err := db.CreateUser(ctx, "owner@example.test", "Owner", "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	other, err := db.CreateUser(ctx, "other@example.test", "Other", "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	google := newFakeGoogle(t)
	env := &testEnv{
		google:  google,
		db:      db,
		userID:  user.ID,
		otherID: other.ID,
		now:     time.Unix(1_700_000_000, 0).UTC(),
	}
	env.manager = NewManager(google.config(), db, testMasterKey)
	env.manager.SetNow(func() time.Time { return env.now })
	// Keep backoff instant so retry tests do not add wall-clock time.
	env.manager.Client().RetryDelay = func(int) time.Duration { return time.Millisecond }
	return env
}

// connect drives a full consent round trip and returns the stored connection.
func (e *testEnv) connect(t *testing.T, userID int64) store.GoogleConnection {
	t.Helper()
	authURL, err := e.manager.StartConnect(userID, "https://rolltop.example.test"+CallbackPath, "")
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(authURL)
	if err != nil {
		t.Fatal(err)
	}
	connection, err := e.manager.CompleteConnect(context.Background(), userID, parsed.Query().Get("state"), "auth-code")
	if err != nil {
		t.Fatal(err)
	}
	return connection
}

func TestAuthorizationURLRequestsOfflineConsentWithPKCE(t *testing.T) {
	env := newTestEnv(t)
	authURL, err := env.manager.StartConnect(env.userID, "https://rolltop.example.test"+CallbackPath, "hint@gmail.example.test")
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(authURL)
	if err != nil {
		t.Fatal(err)
	}
	query := parsed.Query()
	want := map[string]string{
		"client_id":              "client-id",
		"redirect_uri":           "https://rolltop.example.test" + CallbackPath,
		"response_type":          "code",
		"access_type":            "offline",
		"prompt":                 "consent",
		"include_granted_scopes": "true",
		"code_challenge_method":  "S256",
		"login_hint":             "hint@gmail.example.test",
	}
	for key, value := range want {
		if query.Get(key) != value {
			t.Fatalf("authorization URL %s = %q, want %q", key, query.Get(key), value)
		}
	}
	if query.Get("state") == "" || query.Get("code_challenge") == "" {
		t.Fatal("authorization URL is missing state or code challenge")
	}
	if !strings.Contains(query.Get("scope"), ScopeMail) {
		t.Fatalf("authorization scope = %q, want the IMAP/SMTP scope included", query.Get("scope"))
	}
	// The verifier itself must never travel to the browser.
	if strings.Contains(authURL, "code_verifier") {
		t.Fatal("authorization URL leaked the PKCE verifier")
	}
}

func TestCompleteConnectStoresEncryptedTokens(t *testing.T) {
	env := newTestEnv(t)
	connection := env.connect(t, env.userID)

	if connection.GoogleEmail != "user@gmail.example.test" {
		t.Fatalf("connection email = %q", connection.GoogleEmail)
	}
	if connection.Status != store.GoogleConnectionStatusOK {
		t.Fatalf("connection status = %q", connection.Status)
	}
	if !connection.HasScope(ScopeMail) {
		t.Fatalf("granted scopes = %v, want the mail scope", connection.GrantedScopes)
	}
	if connection.EncryptedRefreshToken == "refresh-token-1" ||
		!strings.HasPrefix(connection.EncryptedRefreshToken, "v1:") {
		t.Fatalf("refresh token is not stored as ciphertext: %q", connection.EncryptedRefreshToken)
	}
	if connection.EncryptedAccessToken == "access-token-1" ||
		!strings.HasPrefix(connection.EncryptedAccessToken, "v1:") {
		t.Fatalf("access token is not stored as ciphertext: %q", connection.EncryptedAccessToken)
	}
	decrypted, err := crypto.DecryptString(testMasterKey, connection.EncryptedRefreshToken)
	if err != nil {
		t.Fatal(err)
	}
	if decrypted != "refresh-token-1" {
		t.Fatalf("decrypted refresh token = %q", decrypted)
	}

	// PKCE: the exchange must have proven possession of the verifier.
	env.google.mu.Lock()
	verifier := env.google.lastTokenForm.Get("code_verifier")
	env.google.mu.Unlock()
	if verifier == "" {
		t.Fatal("token exchange did not send a PKCE code verifier")
	}
}

func TestCompleteConnectRejectsReplayedAndForeignState(t *testing.T) {
	env := newTestEnv(t)
	authURL, err := env.manager.StartConnect(env.userID, "https://rolltop.example.test"+CallbackPath, "")
	if err != nil {
		t.Fatal(err)
	}
	parsed, _ := url.Parse(authURL)
	state := parsed.Query().Get("state")

	// Completing consumes the flow, so a replay cannot mint a second connection.
	if _, err := env.manager.CompleteConnect(context.Background(), env.userID, state, "auth-code"); err != nil {
		t.Fatal(err)
	}
	if _, err := env.manager.CompleteConnect(context.Background(), env.userID, state, "auth-code"); !errors.Is(err, ErrUnknownFlow) {
		t.Fatalf("replayed callback error = %v, want ErrUnknownFlow", err)
	}
	if _, err := env.manager.CompleteConnect(context.Background(), env.userID, "made-up-state", "auth-code"); !errors.Is(err, ErrUnknownFlow) {
		t.Fatalf("unknown-state callback error = %v, want ErrUnknownFlow", err)
	}
}

func TestForeignCallbackLeavesTheOwnersFlowIntact(t *testing.T) {
	env := newTestEnv(t)
	authURL, err := env.manager.StartConnect(env.userID, "https://rolltop.example.test"+CallbackPath, "")
	if err != nil {
		t.Fatal(err)
	}
	parsed, _ := url.Parse(authURL)
	state := parsed.Query().Get("state")

	// A different signed-in user may not complete this flow...
	if _, err := env.manager.CompleteConnect(context.Background(), env.otherID, state, "auth-code"); !errors.Is(err, ErrUnknownFlow) {
		t.Fatalf("foreign-user callback error = %v, want ErrUnknownFlow", err)
	}
	// ...and rejecting them must not consume the flow, or a foreign callback
	// becomes a way to cancel somebody else's consent.
	if _, err := env.manager.CompleteConnect(context.Background(), env.userID, state, "auth-code"); err != nil {
		t.Fatalf("owner could not finish their own flow after a foreign callback: %v", err)
	}
}

func TestCompleteConnectRejectsExpiredFlow(t *testing.T) {
	env := newTestEnv(t)
	authURL, err := env.manager.StartConnect(env.userID, "https://rolltop.example.test"+CallbackPath, "")
	if err != nil {
		t.Fatal(err)
	}
	parsed, _ := url.Parse(authURL)
	env.now = env.now.Add(flowTTL + time.Minute)
	if _, err := env.manager.CompleteConnect(context.Background(), env.userID, parsed.Query().Get("state"), "auth-code"); !errors.Is(err, ErrUnknownFlow) {
		t.Fatalf("expired callback error = %v, want ErrUnknownFlow", err)
	}
}

func TestCompleteConnectRequiresRefreshToken(t *testing.T) {
	env := newTestEnv(t)
	env.google.mu.Lock()
	env.google.omitRefresh = true
	env.google.mu.Unlock()
	authURL, err := env.manager.StartConnect(env.userID, "https://rolltop.example.test"+CallbackPath, "")
	if err != nil {
		t.Fatal(err)
	}
	parsed, _ := url.Parse(authURL)
	if _, err := env.manager.CompleteConnect(context.Background(), env.userID, parsed.Query().Get("state"), "auth-code"); err == nil {
		t.Fatal("connect without a refresh token succeeded, want failure")
	}
	connections, err := env.db.ListGoogleConnections(context.Background(), env.userID)
	if err != nil {
		t.Fatal(err)
	}
	if len(connections) != 0 {
		t.Fatalf("stored %d connections without a refresh token, want 0", len(connections))
	}
}

func TestAccessTokenReusesStoredTokenUntilRefreshWindow(t *testing.T) {
	env := newTestEnv(t)
	connection := env.connect(t, env.userID)
	tokenCallsAfterConnect, _ := env.google.counts()

	token, err := env.manager.AccessToken(context.Background(), env.userID, connection.ID)
	if err != nil {
		t.Fatal(err)
	}
	if token != "access-token-1" {
		t.Fatalf("access token = %q, want the stored one", token)
	}
	if calls, _ := env.google.counts(); calls != tokenCallsAfterConnect {
		t.Fatalf("token endpoint called %d times, want no refresh for a fresh token", calls-tokenCallsAfterConnect)
	}

	// Inside the refresh window the token is replaced even though it has not
	// technically expired yet.
	env.now = env.now.Add(time.Hour - refreshSkew)
	token, err = env.manager.AccessToken(context.Background(), env.userID, connection.ID)
	if err != nil {
		t.Fatal(err)
	}
	if token != "access-token-2" {
		t.Fatalf("access token near expiry = %q, want a refreshed token", token)
	}
	if calls, _ := env.google.counts(); calls != tokenCallsAfterConnect+1 {
		t.Fatalf("token endpoint calls = %d, want exactly one refresh", calls-tokenCallsAfterConnect)
	}
}

func TestConcurrentAccessTokenRefreshesOnce(t *testing.T) {
	env := newTestEnv(t)
	connection := env.connect(t, env.userID)
	baseline, _ := env.google.counts()
	env.now = env.now.Add(2 * time.Hour)

	const workers = 12
	var wg sync.WaitGroup
	tokens := make([]string, workers)
	errs := make([]error, workers)
	start := make(chan struct{})
	for i := range workers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			tokens[i], errs[i] = env.manager.AccessToken(context.Background(), env.userID, connection.ID)
		}(i)
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("worker %d: %v", i, err)
		}
		if tokens[i] != tokens[0] {
			t.Fatalf("worker %d got token %q, worker 0 got %q; refresh was not shared", i, tokens[i], tokens[0])
		}
	}
	calls, _ := env.google.counts()
	if calls-baseline != 1 {
		t.Fatalf("token endpoint calls during concurrent refresh = %d, want 1", calls-baseline)
	}
}

func TestRefreshRetriesTransientFailures(t *testing.T) {
	env := newTestEnv(t)
	connection := env.connect(t, env.userID)
	baseline, _ := env.google.counts()
	env.google.mu.Lock()
	env.google.tokenStatuses = []int{http.StatusServiceUnavailable, http.StatusTooManyRequests}
	env.google.mu.Unlock()
	env.now = env.now.Add(2 * time.Hour)

	token, err := env.manager.AccessToken(context.Background(), env.userID, connection.ID)
	if err != nil {
		t.Fatal(err)
	}
	if token == "" {
		t.Fatal("retry produced an empty access token")
	}
	if calls, _ := env.google.counts(); calls-baseline != 3 {
		t.Fatalf("token endpoint calls = %d, want 2 retries then success", calls-baseline)
	}
}

func TestInvalidGrantMarksConnectionForReauth(t *testing.T) {
	env := newTestEnv(t)
	connection := env.connect(t, env.userID)
	env.google.mu.Lock()
	env.google.refreshFailure = "invalid_grant"
	env.google.mu.Unlock()
	env.now = env.now.Add(2 * time.Hour)

	_, err := env.manager.AccessToken(context.Background(), env.userID, connection.ID)
	if !errors.Is(err, ErrReauthRequired) {
		t.Fatalf("refresh after revocation error = %v, want ErrReauthRequired", err)
	}
	stored, err := env.db.GoogleConnection(context.Background(), env.userID, connection.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !stored.NeedsReauth() {
		t.Fatalf("connection status = %q, want reauth_required", stored.Status)
	}
	if stored.EncryptedAccessToken != "" {
		t.Fatal("stale access token survived a revoked grant")
	}
	// A connection awaiting consent must not keep hitting Google on every call.
	baseline, _ := env.google.counts()
	if _, err := env.manager.AccessToken(context.Background(), env.userID, connection.ID); !errors.Is(err, ErrReauthRequired) {
		t.Fatalf("second call error = %v, want ErrReauthRequired", err)
	}
	if calls, _ := env.google.counts(); calls != baseline {
		t.Fatalf("token endpoint called %d more times while awaiting reauth, want 0", calls-baseline)
	}
}

func TestReconnectHealsRevokedConnection(t *testing.T) {
	env := newTestEnv(t)
	connection := env.connect(t, env.userID)
	if err := env.db.MarkGoogleConnectionReauthRequired(context.Background(), env.userID, connection.ID, "invalid_grant"); err != nil {
		t.Fatal(err)
	}
	reconnected := env.connect(t, env.userID)
	if reconnected.ID != connection.ID {
		t.Fatalf("reconnect created a new connection %d, want %d reused", reconnected.ID, connection.ID)
	}
	if reconnected.NeedsReauth() {
		t.Fatal("reconnect left the connection in reauth_required")
	}
	if _, err := env.manager.AccessToken(context.Background(), env.userID, reconnected.ID); err != nil {
		t.Fatalf("access token after reconnect: %v", err)
	}
}

func TestDisconnectRevokesGrantAndDeletesConnection(t *testing.T) {
	env := newTestEnv(t)
	connection := env.connect(t, env.userID)

	revokeErr, err := env.manager.Disconnect(context.Background(), env.userID, connection.ID)
	if err != nil || revokeErr != nil {
		t.Fatalf("disconnect err=%v revokeErr=%v", err, revokeErr)
	}
	_, revokeCalls := env.google.counts()
	if revokeCalls != 1 {
		t.Fatalf("revoke calls = %d, want 1", revokeCalls)
	}
	env.google.mu.Lock()
	revoked := append([]string(nil), env.google.revokedTokens...)
	env.google.mu.Unlock()
	if len(revoked) != 1 || revoked[0] != "refresh-token-1" {
		t.Fatalf("revoked tokens = %v, want the refresh token", revoked)
	}
	if _, err := env.db.GoogleConnection(context.Background(), env.userID, connection.ID); !store.IsNotFound(err) {
		t.Fatalf("connection still present after disconnect: %v", err)
	}
}

func TestDisconnectDeletesLocallyWhenRevokeFails(t *testing.T) {
	env := newTestEnv(t)
	connection := env.connect(t, env.userID)
	// Point revocation at a dead endpoint to simulate Google being unreachable.
	cfg := env.manager.Client().Config
	cfg.RevokeEndpoint = "http://127.0.0.1:1/revoke"
	env.manager.Client().Config = cfg

	revokeErr, err := env.manager.Disconnect(context.Background(), env.userID, connection.ID)
	if revokeErr == nil {
		t.Fatal("disconnect hid the revocation failure, want it reported")
	}
	// The revoke failure must not masquerade as a failed disconnect: the local
	// connection really is gone, and the caller has to be able to tell.
	if err != nil {
		t.Fatalf("failed revoke reported as a disconnect failure: %v", err)
	}
	if _, err := env.db.GoogleConnection(context.Background(), env.userID, connection.ID); !store.IsNotFound(err) {
		t.Fatalf("connection survived a failed revoke: %v", err)
	}
}

func TestManagerRefusesCrossTenantConnectionAccess(t *testing.T) {
	env := newTestEnv(t)
	connection := env.connect(t, env.userID)

	if _, err := env.manager.AccessToken(context.Background(), env.otherID, connection.ID); !store.IsNotFound(err) {
		t.Fatalf("cross-tenant access token error = %v, want not found", err)
	}
	if _, err := env.manager.Disconnect(context.Background(), env.otherID, connection.ID); !store.IsNotFound(err) {
		t.Fatalf("cross-tenant disconnect error = %v, want not found", err)
	}
	if _, err := env.db.GoogleConnection(context.Background(), env.userID, connection.ID); err != nil {
		t.Fatalf("owner's connection was affected by a cross-tenant disconnect: %v", err)
	}
}

func TestErrorsDoNotEchoResponseBodies(t *testing.T) {
	// A failing token response can contain the submitted authorization code, so
	// the error text must be derived from known fields only.
	err := statusError(http.StatusBadRequest, []byte(`{"error":"invalid_request","error_description":"bad","code":"4/secret-code"}`))
	if strings.Contains(err.Error(), "secret-code") {
		t.Fatalf("error leaked response body: %v", err)
	}
	opaque := statusError(http.StatusBadGateway, []byte("upstream said 4/secret-code"))
	if strings.Contains(opaque.Error(), "secret-code") {
		t.Fatalf("non-JSON error leaked response body: %v", opaque)
	}
}

func TestUnconfiguredManagerRefusesToStartFlow(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "rolltop.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	manager := NewManager(Config{}, db, testMasterKey)
	if manager.Configured() {
		t.Fatal("empty config reported as configured")
	}
	if _, err := manager.StartConnect(1, "https://rolltop.example.test"+CallbackPath, ""); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("StartConnect error = %v, want ErrNotConfigured", err)
	}
}

func TestManagerRefusesWeakMasterKey(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "rolltop.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	manager := NewManager(Config{ClientID: "id", ClientSecret: "secret"}, db, []byte("short"))
	if _, err := manager.StartConnect(1, "https://rolltop.example.test"+CallbackPath, ""); err == nil {
		t.Fatal("StartConnect accepted a short master key")
	}
}

// TestRefreshMapIsEmptyAfterUse guards the assumption that concurrent
// AccessToken calls are the only writers to the refresh map, and that every
// claimed slot is released again.
func TestRefreshMapIsEmptyAfterUse(t *testing.T) {
	env := newTestEnv(t)
	connection := env.connect(t, env.userID)
	env.now = env.now.Add(2 * time.Hour)
	var completed atomic.Int64
	var wg sync.WaitGroup
	for range 5 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := env.manager.AccessToken(context.Background(), env.userID, connection.ID); err == nil {
				completed.Add(1)
			}
		}()
	}
	wg.Wait()
	if completed.Load() != 5 {
		t.Fatalf("completed refreshes = %d, want 5", completed.Load())
	}
	env.manager.refreshMu.Lock()
	remaining := len(env.manager.refreshes)
	env.manager.refreshMu.Unlock()
	if remaining != 0 {
		t.Fatalf("refresh map still holds %d entries", remaining)
	}
}

func TestPendingFlowsAreCappedPerUserNotGlobally(t *testing.T) {
	env := newTestEnv(t)
	redirect := "https://rolltop.example.test" + CallbackPath

	// A victim starts a normal authorization.
	victimURL, err := env.manager.StartConnect(env.otherID, redirect, "")
	if err != nil {
		t.Fatal(err)
	}
	victimState, _ := url.Parse(victimURL)

	// Another user floods connect far past the per-user cap. Starting a flow is
	// a plain GET, so a page the user merely visits can do this.
	for range maxPendingFlowsPerUser * 4 {
		if _, err := env.manager.StartConnect(env.userID, redirect, ""); err != nil {
			t.Fatal(err)
		}
	}

	env.manager.flows.mu.Lock()
	flooderFlows := env.manager.flows.countForUserLocked(env.userID)
	env.manager.flows.mu.Unlock()
	if flooderFlows > maxPendingFlowsPerUser {
		t.Fatalf("one user holds %d pending flows, want at most %d", flooderFlows, maxPendingFlowsPerUser)
	}

	// The victim's consent must still be completable.
	if _, err := env.manager.CompleteConnect(context.Background(), env.otherID,
		victimState.Query().Get("state"), "auth-code"); err != nil {
		t.Fatalf("victim's flow was evicted by another user: %v", err)
	}
}

func TestForceRefreshDoesNotAdoptAnInFlightToken(t *testing.T) {
	env := newTestEnv(t)
	connection := env.connect(t, env.userID)
	env.now = env.now.Add(2 * time.Hour)

	// Hold the token endpoint so a refresh is provably in flight while the
	// forced one starts.
	release := make(chan struct{})
	env.google.mu.Lock()
	env.google.beforeToken = func() { <-release }
	env.google.mu.Unlock()

	type refreshResult struct {
		token string
		err   error
	}
	inflight := make(chan refreshResult, 1)
	go func() {
		token, err := env.manager.AccessToken(context.Background(), env.userID, connection.ID)
		inflight <- refreshResult{token, err}
	}()
	waitForInflightRefresh(t, env.manager)

	forced := make(chan refreshResult, 1)
	go func() {
		token, err := env.manager.ForceRefresh(context.Background(), env.userID, connection.ID)
		forced <- refreshResult{token, err}
	}()

	env.google.mu.Lock()
	env.google.beforeToken = nil
	env.google.mu.Unlock()
	close(release)

	slow := <-inflight
	forcedResult := <-forced
	if slow.err != nil {
		t.Fatalf("in-flight refresh: %v", slow.err)
	}
	if forcedResult.err != nil {
		t.Fatalf("force refresh: %v", forcedResult.err)
	}
	slowToken, forcedToken := slow.token, forcedResult.token
	// A caller that forces a refresh has already established the in-flight
	// token is unusable, so handing that same token back would silently make
	// the retry a no-op.
	if forcedToken == slowToken {
		t.Fatalf("force refresh returned the in-flight token %q", forcedToken)
	}
}

func TestFailureToFlagReauthStillReportsReauthRequired(t *testing.T) {
	env := newTestEnv(t)
	connection := env.connect(t, env.userID)
	env.google.mu.Lock()
	env.google.refreshFailure = "invalid_grant"
	env.google.mu.Unlock()
	env.now = env.now.Add(2 * time.Hour)

	failing := &markFailingStore{ConnectionStore: env.db}
	manager := googleauthManagerWithStore(env, failing)

	// Even when the connection cannot be flagged, the caller must learn that
	// consent is gone; a generic upstream error would leave the settings page
	// offering "Test connection" instead of "Reauthorize".
	_, err := manager.AccessToken(context.Background(), env.userID, connection.ID)
	if !errors.Is(err, ErrReauthRequired) {
		t.Fatalf("refresh error when flagging fails = %v, want ErrReauthRequired", err)
	}
	if !failing.attempted {
		t.Fatal("no attempt was made to flag the connection")
	}
}

// markFailingStore makes only the reauth flag write fail.
type markFailingStore struct {
	ConnectionStore
	attempted bool
}

func (s *markFailingStore) MarkGoogleConnectionReauthRequired(ctx context.Context, userID, connectionID int64, detail string) error {
	s.attempted = true
	return errors.New("database is locked")
}

func googleauthManagerWithStore(env *testEnv, connections ConnectionStore) *Manager {
	manager := NewManager(env.google.config(), connections, testMasterKey)
	manager.SetNow(func() time.Time { return env.now })
	manager.Client().RetryDelay = func(int) time.Duration { return time.Millisecond }
	return manager
}

// waitForInflightRefresh blocks until a refresh has claimed its slot.
func waitForInflightRefresh(t *testing.T, manager *Manager) {
	t.Helper()
	for range 2000 {
		manager.refreshMu.Lock()
		running := len(manager.refreshes)
		manager.refreshMu.Unlock()
		if running > 0 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("no refresh became in flight")
}

func TestSharedRefreshSurvivesTheClaimantsCancelledContext(t *testing.T) {
	env := newTestEnv(t)
	connection := env.connect(t, env.userID)
	env.now = env.now.Add(2 * time.Hour)

	// Hold the token endpoint so the claimant is provably mid-refresh when its
	// own context is cancelled.
	release := make(chan struct{})
	env.google.mu.Lock()
	env.google.beforeToken = func() { <-release }
	env.google.mu.Unlock()

	claimantCtx, cancelClaimant := context.WithCancel(context.Background())
	type refreshResult struct {
		token string
		err   error
	}
	waiter := make(chan refreshResult, 1)
	go func() {
		_, _ = env.manager.AccessToken(claimantCtx, env.userID, connection.ID)
	}()
	waitForInflightRefresh(t, env.manager)
	go func() {
		token, err := env.manager.AccessToken(context.Background(), env.userID, connection.ID)
		waiter <- refreshResult{token, err}
	}()

	// The claimant walks away; a sync worker with a short per-request deadline
	// looks exactly like this. The waiter's own context is still live, so it
	// must still get a token.
	cancelClaimant()
	env.google.mu.Lock()
	env.google.beforeToken = nil
	env.google.mu.Unlock()
	close(release)

	got := <-waiter
	if got.err != nil {
		t.Fatalf("waiter inherited the claimant's cancellation: %v", got.err)
	}
	if got.token == "" {
		t.Fatal("waiter received an empty token")
	}
}

// A reconnect exists to widen a grant, and the account holder is told to do one
// when Google refuses a request for want of a scope. Handing the token issued
// under the old grant to the next request would keep that refusal coming for
// the rest of the token's hour, with the fix being the thing they just did.
func TestReconnectReplacesTheCachedAccessToken(t *testing.T) {
	env := newTestEnv(t)
	connection := env.connect(t, env.userID)

	first, err := env.manager.AccessToken(context.Background(), env.userID, connection.ID)
	if err != nil {
		t.Fatal(err)
	}

	env.google.mu.Lock()
	env.google.scope = "openid email " + ScopeMail + " " + ScopeContacts + " " + ScopeCalendar
	env.google.mu.Unlock()
	reconnected := env.connect(t, env.userID)
	if reconnected.ID != connection.ID {
		t.Fatalf("reconnect forked the connection: before=%d after=%d", connection.ID, reconnected.ID)
	}

	second, err := env.manager.AccessToken(context.Background(), env.userID, connection.ID)
	if err != nil {
		t.Fatal(err)
	}
	if second == first {
		t.Fatal("reconnect served the access token issued under the previous grant")
	}
	if !slices.Contains(reconnected.GrantedScopes, ScopeContacts) {
		t.Fatalf("reconnect did not record the widened scopes: %q", reconnected.GrantedScopes)
	}
}
