// File overview: Tests for the Microsoft sign-in and token lifecycle against a
// fake identity platform and Graph.

package microsoftauth

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"rolltop/backend/crypto"
	"rolltop/backend/store"
)

type memoryStore struct {
	mu          sync.Mutex
	nextID      int64
	connections map[int64]store.MicrosoftConnection
}

func newMemoryStore() *memoryStore {
	return &memoryStore{connections: map[int64]store.MicrosoftConnection{}}
}

func (s *memoryStore) UpsertMicrosoftConnection(_ context.Context, userID int64, in store.MicrosoftConnectionUpsert) (store.MicrosoftConnection, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, existing := range s.connections {
		if existing.UserID == userID && existing.Subject == in.Subject {
			existing.Email = in.Email
			existing.EncryptedRefreshToken = in.EncryptedRefreshToken
			existing.EncryptedAccessToken = in.EncryptedAccessToken
			existing.AccessTokenExpiresAt = in.AccessTokenExpiresAt
			existing.GrantedScopes = in.GrantedScopes
			existing.Status = store.MicrosoftConnectionStatusOK
			s.connections[id] = existing
			return existing, nil
		}
	}
	s.nextID++
	connection := store.MicrosoftConnection{
		ID: s.nextID, UserID: userID, Email: in.Email, Subject: in.Subject, DisplayName: in.DisplayName,
		EncryptedRefreshToken: in.EncryptedRefreshToken, EncryptedAccessToken: in.EncryptedAccessToken,
		AccessTokenExpiresAt: in.AccessTokenExpiresAt, GrantedScopes: in.GrantedScopes, Status: store.MicrosoftConnectionStatusOK,
	}
	s.connections[connection.ID] = connection
	return connection, nil
}

func (s *memoryStore) ListMicrosoftConnections(_ context.Context, userID int64) ([]store.MicrosoftConnection, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []store.MicrosoftConnection{}
	for _, connection := range s.connections {
		if connection.UserID == userID {
			out = append(out, connection)
		}
	}
	return out, nil
}

func (s *memoryStore) MicrosoftConnection(_ context.Context, userID, connectionID int64) (store.MicrosoftConnection, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	connection, ok := s.connections[connectionID]
	if !ok || connection.UserID != userID {
		return store.MicrosoftConnection{}, sql.ErrNoRows
	}
	return connection, nil
}

func (s *memoryStore) UpdateMicrosoftAccessToken(_ context.Context, userID, connectionID int64, access string, expires time.Time, refresh string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	connection, ok := s.connections[connectionID]
	if !ok || connection.UserID != userID {
		return sql.ErrNoRows
	}
	connection.EncryptedAccessToken = access
	connection.AccessTokenExpiresAt = expires
	if refresh != "" {
		connection.EncryptedRefreshToken = refresh
	}
	connection.Status = store.MicrosoftConnectionStatusOK
	s.connections[connectionID] = connection
	return nil
}

func (s *memoryStore) MarkMicrosoftConnectionReauthRequired(_ context.Context, userID, connectionID int64, detail string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	connection, ok := s.connections[connectionID]
	if !ok || connection.UserID != userID {
		return sql.ErrNoRows
	}
	connection.Status = store.MicrosoftConnectionStatusReauthRequired
	connection.StatusDetail = detail
	connection.EncryptedAccessToken = ""
	s.connections[connectionID] = connection
	return nil
}

func (s *memoryStore) DeleteMicrosoftConnection(_ context.Context, userID, connectionID int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	connection, ok := s.connections[connectionID]
	if !ok || connection.UserID != userID {
		return sql.ErrNoRows
	}
	delete(s.connections, connectionID)
	return nil
}

// fakeMicrosoft is the identity platform and Graph's /me. Every refresh
// rotates the refresh token, as Microsoft does.
type fakeMicrosoft struct {
	mu            sync.Mutex
	refreshCount  int
	validRefresh  string
	rejectRefresh bool
}

func (f *fakeMicrosoft) handler(t *testing.T) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.Form.Get("grant_type") {
		case "authorization_code":
			if r.Form.Get("code") != "auth-code" || r.Form.Get("code_verifier") == "" || r.Form.Get("client_secret") != "secret" {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":"invalid_request"}`))
				return
			}
			f.validRefresh = "refresh-0"
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token": "access-0", "refresh_token": "refresh-0", "expires_in": 3600,
				"scope": "Calendars.ReadWrite User.Read openid profile email",
			})
		case "refresh_token":
			if f.rejectRefresh || r.Form.Get("refresh_token") != f.validRefresh {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":"invalid_grant","error_description":"AADSTS70000: secret detail"}`))
				return
			}
			f.refreshCount++
			f.validRefresh = "refresh-" + string(rune('0'+f.refreshCount))
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token": "access-" + string(rune('0'+f.refreshCount)), "refresh_token": f.validRefresh, "expires_in": 3600,
				"scope": "Calendars.ReadWrite User.Read",
			})
		}
	})
	mux.HandleFunc("/v1.0/me", func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer access-") {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{
			"id": "oid-1", "mail": "", "userPrincipalName": "Reader@Contoso.example", "displayName": "Reader",
		})
	})
	return mux
}

type testEnv struct {
	manager *Manager
	store   *memoryStore
	fake    *fakeMicrosoft
	now     time.Time
	key     []byte
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	fake := &fakeMicrosoft{}
	server := httptest.NewServer(fake.handler(t))
	t.Cleanup(server.Close)
	cfg := New("client", "secret", "", []string{"https://rolltop.example.test" + CallbackPath}, nil)
	cfg.AuthorizationEndpoint = server.URL + "/authorize"
	cfg.TokenEndpoint = server.URL + "/token"
	cfg.GraphEndpoint = server.URL + "/v1.0"
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}
	env := &testEnv{store: newMemoryStore(), fake: fake, now: time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC), key: key}
	env.manager = NewManager(cfg, env.store, key)
	env.manager.client.Config = cfg
	env.manager.client.RetryDelay = func(int) time.Duration { return 0 }
	env.manager.SetNow(func() time.Time { return env.now })
	return env
}

func (env *testEnv) connect(t *testing.T, userID int64) store.MicrosoftConnection {
	t.Helper()
	authURL, err := env.manager.StartConnect(userID, "https://rolltop.example.test"+CallbackPath, "")
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(authURL)
	if err != nil {
		t.Fatal(err)
	}
	query := parsed.Query()
	for _, want := range []string{"offline_access", "Calendars.ReadWrite", "User.Read"} {
		if !strings.Contains(query.Get("scope"), want) {
			t.Fatalf("scope %q lacks %s", query.Get("scope"), want)
		}
	}
	if query.Get("code_challenge_method") != "S256" || query.Get("code_challenge") == "" {
		t.Fatalf("authorization URL without PKCE: %s", authURL)
	}
	connection, err := env.manager.CompleteConnect(context.Background(), userID, query.Get("state"), "auth-code")
	if err != nil {
		t.Fatal(err)
	}
	return connection
}

func TestConnectStoresEncryptedTokensAndIdentity(t *testing.T) {
	env := newTestEnv(t)
	connection := env.connect(t, 1)
	if connection.Email != "Reader@Contoso.example" || connection.Subject != "oid-1" {
		t.Fatalf("identity = %+v", connection)
	}
	if strings.Contains(connection.EncryptedRefreshToken, "refresh-0") || strings.Contains(connection.EncryptedAccessToken, "access-0") {
		t.Fatal("tokens were stored in the clear")
	}
	refresh, err := crypto.DecryptString(env.key, connection.EncryptedRefreshToken)
	if err != nil || refresh != "refresh-0" {
		t.Fatalf("stored refresh token = %q, %v", refresh, err)
	}
	token, err := env.manager.AccessToken(context.Background(), 1, connection.ID)
	if err != nil || token != "access-0" {
		t.Fatalf("access token = %q, %v", token, err)
	}
}

func TestAStateCannotBeFinishedByAnotherUser(t *testing.T) {
	env := newTestEnv(t)
	authURL, err := env.manager.StartConnect(1, "https://rolltop.example.test"+CallbackPath, "")
	if err != nil {
		t.Fatal(err)
	}
	parsed, _ := url.Parse(authURL)
	state := parsed.Query().Get("state")
	if _, err := env.manager.CompleteConnect(context.Background(), 2, state, "auth-code"); !errors.Is(err, ErrUnknownFlow) {
		t.Fatalf("foreign completion: %v", err)
	}
	if _, err := env.manager.CompleteConnect(context.Background(), 1, state, "auth-code"); err != nil {
		t.Fatalf("owner's flow did not survive a foreign attempt: %v", err)
	}
	if _, err := env.manager.CompleteConnect(context.Background(), 1, state, "auth-code"); !errors.Is(err, ErrUnknownFlow) {
		t.Fatalf("replayed state: %v", err)
	}
}

// Microsoft rotates the refresh token on use; the manager has to keep the new
// one, or the second refresh presents a spent token.
func TestRefreshStoresTheRotatedRefreshToken(t *testing.T) {
	env := newTestEnv(t)
	connection := env.connect(t, 1)
	for i := 1; i <= 2; i++ {
		env.now = env.now.Add(2 * time.Hour)
		token, err := env.manager.AccessToken(context.Background(), 1, connection.ID)
		if err != nil {
			t.Fatalf("refresh %d: %v", i, err)
		}
		if token != "access-"+string(rune('0'+i)) {
			t.Fatalf("refresh %d token = %q", i, token)
		}
	}
}

func TestRejectedRefreshFlagsTheConnection(t *testing.T) {
	env := newTestEnv(t)
	connection := env.connect(t, 1)
	env.fake.rejectRefresh = true
	env.now = env.now.Add(2 * time.Hour)
	_, err := env.manager.AccessToken(context.Background(), 1, connection.ID)
	if !errors.Is(err, ErrReauthRequired) {
		t.Fatalf("error = %v, want reauth required", err)
	}
	if strings.Contains(err.Error(), "secret detail") {
		t.Fatalf("error quotes Microsoft's description: %v", err)
	}
	stored, _ := env.store.MicrosoftConnection(context.Background(), 1, connection.ID)
	if !stored.NeedsReauth() {
		t.Fatal("connection was not flagged for reauthorization")
	}
	if _, err := env.manager.AccessToken(context.Background(), 1, connection.ID); !errors.Is(err, ErrReauthRequired) {
		t.Fatalf("flagged connection served a token: %v", err)
	}
}

func TestConfigDefaultsToTheCommonTenant(t *testing.T) {
	cfg := New("id", "secret", "", nil, nil)
	if cfg.Tenant != DefaultTenant || !strings.Contains(cfg.TokenEndpoint, "/common/oauth2/v2.0/token") {
		t.Fatalf("config = %+v", cfg)
	}
	cfg = New("id", "secret", "contoso.onmicrosoft.com", nil, nil)
	if !strings.Contains(cfg.AuthorizationEndpoint, "/contoso.onmicrosoft.com/oauth2/v2.0/authorize") {
		t.Fatalf("tenant endpoint = %s", cfg.AuthorizationEndpoint)
	}
}
