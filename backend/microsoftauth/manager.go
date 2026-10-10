// File overview: The one entry point the Microsoft 365 calendar sync uses to
// get a usable Graph access token. It owns the sign-in, encrypted persistence,
// refresh with per-connection deduplication, and the reauth_required path --
// the same responsibilities googleauth.Manager has for Google, kept separate
// because the two identity platforms differ in every detail that matters
// (rotation of refresh tokens, how an account is identified, what revocation
// exists).

package microsoftauth

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"rolltop/backend/auth"
	"rolltop/backend/crypto"
	"rolltop/backend/store"
)

// refreshSkew refreshes a token slightly before it expires, so a long sync
// does not fail mid-request on an edge-of-expiry token.
const refreshSkew = 5 * time.Minute

// refreshTimeout bounds a shared refresh on its own budget rather than the
// claimant's, which other waiters have no reason to inherit.
const refreshTimeout = time.Minute

// ConnectionStore is the persistence surface the manager needs.
type ConnectionStore interface {
	UpsertMicrosoftConnection(ctx context.Context, userID int64, in store.MicrosoftConnectionUpsert) (store.MicrosoftConnection, error)
	ListMicrosoftConnections(ctx context.Context, userID int64) ([]store.MicrosoftConnection, error)
	MicrosoftConnection(ctx context.Context, userID, connectionID int64) (store.MicrosoftConnection, error)
	UpdateMicrosoftAccessToken(ctx context.Context, userID, connectionID int64, encryptedAccessToken string, expiresAt time.Time, encryptedRefreshToken string) error
	MarkMicrosoftConnectionReauthRequired(ctx context.Context, userID, connectionID int64, detail string) error
	DeleteMicrosoftConnection(ctx context.Context, userID, connectionID int64) error
}

// Manager coordinates Microsoft OAuth for every user of one Rolltop install.
type Manager struct {
	config    Config
	client    *Client
	store     ConnectionStore
	masterKey []byte
	flows     *flowStore
	now       func() time.Time

	refreshMu sync.Mutex
	refreshes map[connectionKey]*refreshCall

	tokenMu sync.Mutex
	tokens  map[connectionKey]cachedToken
}

type connectionKey struct {
	userID       int64
	connectionID int64
}

type cachedToken struct {
	token     string
	expiresAt time.Time
}

type refreshCall struct {
	done       chan struct{}
	token      string
	connection store.MicrosoftConnection
	err        error
}

// NewManager builds a manager. A nil store or a master key of the wrong length
// makes every operation fail rather than store weak data.
func NewManager(cfg Config, connections ConnectionStore, masterKey []byte) *Manager {
	return &Manager{
		config:    cfg,
		client:    NewClient(cfg),
		store:     connections,
		masterKey: masterKey,
		flows:     newFlowStore(),
		refreshes: map[connectionKey]*refreshCall{},
		tokens:    map[connectionKey]cachedToken{},
	}
}

// Config exposes the configuration so routes can report whether Microsoft is
// set up.
func (m *Manager) Config() Config { return m.config }

// Configured reports whether the operator supplied client credentials.
func (m *Manager) Configured() bool {
	return m != nil && m.config.Configured()
}

// Client exposes the HTTP client so tests can retarget endpoints.
func (m *Manager) Client() *Client { return m.client }

// SetNow overrides the clock. It must be called before the manager serves.
func (m *Manager) SetNow(now func() time.Time) {
	m.now = now
	m.client.Now = now
	m.flows.setNow(now)
}

func (m *Manager) timeNow() time.Time {
	if m.now != nil {
		return m.now()
	}
	return time.Now()
}

func (m *Manager) ready() error {
	if m == nil || m.store == nil {
		return errors.New("microsoft connections are unavailable")
	}
	if len(m.masterKey) != 32 {
		return errors.New("microsoft tokens cannot be encrypted without a 32-byte master key")
	}
	if !m.config.Configured() {
		return ErrNotConfigured
	}
	if len(m.config.RedirectURLs) == 0 {
		return ErrNoRedirectURI
	}
	return nil
}

// StartConnect begins a sign-in for one signed-in user and returns the URL to
// send the browser to.
func (m *Manager) StartConnect(userID int64, redirectURI, loginHint string) (string, error) {
	if err := m.ready(); err != nil {
		return "", err
	}
	if userID <= 0 {
		return "", errors.New("microsoft authorization requires a signed-in user")
	}
	if strings.TrimSpace(redirectURI) == "" {
		return "", ErrNoRedirectURI
	}
	state, err := auth.NewOpaqueToken()
	if err != nil {
		return "", err
	}
	verifier, err := auth.NewOpaqueToken()
	if err != nil {
		return "", err
	}
	m.flows.put(state, pendingFlow{
		userID:       userID,
		codeVerifier: verifier,
		redirectURI:  redirectURI,
		createdAt:    m.timeNow(),
	})
	return authorizationURL(m.config, redirectURI, state, verifier, loginHint)
}

// CompleteConnect finishes a sign-in: it validates the pending flow, exchanges
// the code, identifies the account through Graph and stores the tokens
// encrypted.
func (m *Manager) CompleteConnect(ctx context.Context, userID int64, state, code string) (store.MicrosoftConnection, error) {
	if err := m.ready(); err != nil {
		return store.MicrosoftConnection{}, err
	}
	flow, err := m.flows.take(state, userID)
	if err != nil {
		return store.MicrosoftConnection{}, err
	}
	token, err := m.client.ExchangeCode(ctx, code, flow.redirectURI, flow.codeVerifier)
	if err != nil {
		return store.MicrosoftConnection{}, err
	}
	me, err := m.client.Me(ctx, token.AccessToken)
	if err != nil {
		return store.MicrosoftConnection{}, err
	}
	if strings.TrimSpace(me.ID) == "" {
		return store.MicrosoftConnection{}, errors.New("microsoft did not report a stable account identifier")
	}
	email := me.Email()
	if email == "" {
		return store.MicrosoftConnection{}, errors.New("microsoft did not report an address for this account")
	}
	encryptedRefresh, err := crypto.EncryptString(m.masterKey, token.RefreshToken)
	if err != nil {
		return store.MicrosoftConnection{}, err
	}
	encryptedAccess, err := m.encrypt(token.AccessToken)
	if err != nil {
		return store.MicrosoftConnection{}, err
	}
	connection, err := m.store.UpsertMicrosoftConnection(ctx, userID, store.MicrosoftConnectionUpsert{
		Email:                 email,
		Subject:               me.ID,
		DisplayName:           me.DisplayName,
		EncryptedRefreshToken: encryptedRefresh,
		EncryptedAccessToken:  encryptedAccess,
		AccessTokenExpiresAt:  token.ExpiresAt,
		GrantedScopes:         token.Scopes,
	})
	if err != nil {
		return store.MicrosoftConnection{}, err
	}
	// A reconnect lands on the row it had, so a cached token issued under the
	// grant the user just came back to widen must not be served on.
	m.forgetToken(userID, connection.ID)
	return connection, nil
}

// AccessToken returns a valid access token, refreshing it when needed.
func (m *Manager) AccessToken(ctx context.Context, userID, connectionID int64) (string, error) {
	if err := m.ready(); err != nil {
		return "", err
	}
	if token, ok := m.cached(userID, connectionID); ok {
		return token, nil
	}
	token, connection, err := m.AccessTokenAndConnection(ctx, userID, connectionID)
	if err != nil {
		return "", err
	}
	m.cache(userID, connectionID, token, connection.AccessTokenExpiresAt)
	return token, nil
}

// AccessTokenAndConnection is AccessToken for callers that also need the
// connection's current state.
func (m *Manager) AccessTokenAndConnection(ctx context.Context, userID, connectionID int64) (string, store.MicrosoftConnection, error) {
	if err := m.ready(); err != nil {
		return "", store.MicrosoftConnection{}, err
	}
	connection, err := m.store.MicrosoftConnection(ctx, userID, connectionID)
	if err != nil {
		return "", store.MicrosoftConnection{}, err
	}
	if connection.NeedsReauth() {
		return "", connection, fmt.Errorf("%w: %s", ErrReauthRequired, connection.Email)
	}
	if token, ok := m.usableStoredToken(connection); ok {
		return token, connection, nil
	}
	return m.refresh(ctx, userID, connectionID, false)
}

// ForceRefresh discards the token Graph just rejected and fetches a new one.
// It never adopts a refresh already in flight: that one may have produced the
// very token that was rejected.
func (m *Manager) ForceRefresh(ctx context.Context, userID, connectionID int64) (string, error) {
	if err := m.ready(); err != nil {
		return "", err
	}
	m.forgetToken(userID, connectionID)
	token, connection, err := m.refresh(ctx, userID, connectionID, true)
	if err != nil {
		return "", err
	}
	m.cache(userID, connectionID, token, connection.AccessTokenExpiresAt)
	return token, nil
}

// AbandonFlow releases a pending sign-in the user cancelled.
func (m *Manager) AbandonFlow(userID int64, state string) {
	if m == nil || strings.TrimSpace(state) == "" {
		return
	}
	_, _ = m.flows.take(state, userID)
}

func (m *Manager) cached(userID, connectionID int64) (string, bool) {
	m.tokenMu.Lock()
	defer m.tokenMu.Unlock()
	entry, ok := m.tokens[connectionKey{userID, connectionID}]
	if !ok || entry.token == "" || !m.timeNow().Add(refreshSkew).Before(entry.expiresAt) {
		return "", false
	}
	return entry.token, true
}

func (m *Manager) cache(userID, connectionID int64, token string, expiresAt time.Time) {
	if token == "" || expiresAt.IsZero() {
		return
	}
	m.tokenMu.Lock()
	defer m.tokenMu.Unlock()
	m.tokens[connectionKey{userID, connectionID}] = cachedToken{token: token, expiresAt: expiresAt}
}

func (m *Manager) forgetToken(userID, connectionID int64) {
	if m == nil {
		return
	}
	m.tokenMu.Lock()
	defer m.tokenMu.Unlock()
	delete(m.tokens, connectionKey{userID, connectionID})
}

func (m *Manager) usableStoredToken(connection store.MicrosoftConnection) (string, bool) {
	if strings.TrimSpace(connection.EncryptedAccessToken) == "" || connection.AccessTokenExpiresAt.IsZero() {
		return "", false
	}
	if !m.timeNow().Add(refreshSkew).Before(connection.AccessTokenExpiresAt) {
		return "", false
	}
	token, err := crypto.DecryptString(m.masterKey, connection.EncryptedAccessToken)
	if err != nil {
		return "", false
	}
	return token, true
}

// refresh performs one refresh per connection at a time. Microsoft rotates the
// refresh token on use, so two concurrent refreshes are not merely wasteful:
// the loser stores a refresh token the winner already spent.
func (m *Manager) refresh(ctx context.Context, userID, connectionID int64, force bool) (string, store.MicrosoftConnection, error) {
	key := connectionKey{userID, connectionID}
	for {
		m.refreshMu.Lock()
		inflight, running := m.refreshes[key]
		if !running {
			call := &refreshCall{done: make(chan struct{})}
			m.refreshes[key] = call
			m.refreshMu.Unlock()

			refreshCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), refreshTimeout)
			call.token, call.connection, call.err = m.doRefresh(refreshCtx, userID, connectionID, force)
			cancel()

			m.refreshMu.Lock()
			delete(m.refreshes, key)
			close(call.done)
			m.refreshMu.Unlock()
			return call.token, call.connection, call.err
		}
		m.refreshMu.Unlock()
		select {
		case <-ctx.Done():
			return "", store.MicrosoftConnection{}, ctx.Err()
		case <-inflight.done:
		}
		if !force {
			return inflight.token, inflight.connection, inflight.err
		}
	}
}

func (m *Manager) doRefresh(ctx context.Context, userID, connectionID int64, force bool) (string, store.MicrosoftConnection, error) {
	connection, err := m.store.MicrosoftConnection(ctx, userID, connectionID)
	if err != nil {
		return "", store.MicrosoftConnection{}, err
	}
	if !force {
		if token, ok := m.usableStoredToken(connection); ok {
			return token, connection, nil
		}
	}
	refreshToken, err := crypto.DecryptString(m.masterKey, connection.EncryptedRefreshToken)
	if err != nil {
		m.forgetToken(userID, connectionID)
		if markErr := m.store.MarkMicrosoftConnectionReauthRequired(ctx, userID, connectionID, "stored token could not be decrypted"); markErr != nil {
			log.Printf("microsoft connection user_id=%d connection_id=%d could not be flagged for reauthorization: %v", userID, connectionID, markErr)
		}
		return "", connection, fmt.Errorf("%w: stored token could not be decrypted", ErrReauthRequired)
	}
	token, err := m.client.RefreshToken(ctx, refreshToken)
	if err != nil {
		if errors.Is(err, ErrReauthRequired) {
			m.forgetToken(userID, connectionID)
			if markErr := m.store.MarkMicrosoftConnectionReauthRequired(ctx, userID, connectionID, "Microsoft rejected the stored authorization."); markErr != nil {
				log.Printf("microsoft connection user_id=%d connection_id=%d could not be flagged for reauthorization: %v", userID, connectionID, markErr)
			}
			connection.Status = store.MicrosoftConnectionStatusReauthRequired
		}
		return "", connection, err
	}
	encryptedAccess, err := m.encrypt(token.AccessToken)
	if err != nil {
		return "", connection, err
	}
	encryptedRefresh := ""
	if token.RefreshToken != "" {
		if encryptedRefresh, err = crypto.EncryptString(m.masterKey, token.RefreshToken); err != nil {
			return "", connection, err
		}
	}
	if err := m.store.UpdateMicrosoftAccessToken(ctx, userID, connectionID, encryptedAccess, token.ExpiresAt, encryptedRefresh); err != nil {
		return "", connection, err
	}
	connection.EncryptedAccessToken = encryptedAccess
	connection.AccessTokenExpiresAt = token.ExpiresAt
	if encryptedRefresh != "" {
		connection.EncryptedRefreshToken = encryptedRefresh
	}
	connection.Status = store.MicrosoftConnectionStatusOK
	connection.StatusDetail = ""
	return token.AccessToken, connection, nil
}

func (m *Manager) encrypt(token string) (string, error) {
	if strings.TrimSpace(token) == "" {
		return "", nil
	}
	return crypto.EncryptString(m.masterKey, token)
}

// List returns the user's connections for display.
func (m *Manager) List(ctx context.Context, userID int64) ([]store.MicrosoftConnection, error) {
	if m == nil || m.store == nil {
		return nil, errors.New("microsoft connections are unavailable")
	}
	return m.store.ListMicrosoftConnections(ctx, userID)
}

// Get returns one connection owned by the user.
func (m *Manager) Get(ctx context.Context, userID, connectionID int64) (store.MicrosoftConnection, error) {
	if m == nil || m.store == nil {
		return store.MicrosoftConnection{}, errors.New("microsoft connections are unavailable")
	}
	return m.store.MicrosoftConnection(ctx, userID, connectionID)
}

// Disconnect removes the local connection and the calendars mirrored through
// it. The identity platform offers no endpoint that revokes one app's grant
// for one user -- the closest, revoking every sign-in session, would sign the
// reader out of everything else too -- so the grant itself stays listed in the
// account's app permissions until the reader removes it there.
func (m *Manager) Disconnect(ctx context.Context, userID, connectionID int64) error {
	if m == nil || m.store == nil {
		return errors.New("microsoft connections are unavailable")
	}
	if err := m.store.DeleteMicrosoftConnection(ctx, userID, connectionID); err != nil {
		return err
	}
	m.forgetToken(userID, connectionID)
	return nil
}
