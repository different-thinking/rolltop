// File overview: The one entry point mail, contacts, and calendar use to get a
// usable Google access token. It owns consent, encrypted persistence, refresh
// with per-connection deduplication, and the reauth_required failure path.

package googleauth

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

// refreshSkew refreshes an access token slightly before it actually expires so
// a long-running sync does not fail mid-request on an edge-of-expiry token.
const refreshSkew = 5 * time.Minute

// refreshTimeout bounds a shared refresh. It replaces the claimant's own
// deadline, which other waiters have no reason to inherit.
const refreshTimeout = time.Minute

// ConnectionStore is the persistence surface the manager needs. It is an
// interface so tests can drive refresh behavior without a SQLite file.
type ConnectionStore interface {
	UpsertGoogleConnection(ctx context.Context, userID int64, in store.GoogleConnectionUpsert) (store.GoogleConnection, error)
	ListGoogleConnections(ctx context.Context, userID int64) ([]store.GoogleConnection, error)
	GoogleConnection(ctx context.Context, userID, connectionID int64) (store.GoogleConnection, error)
	UpdateGoogleAccessToken(ctx context.Context, userID, connectionID int64, encryptedAccessToken string, expiresAt time.Time, encryptedRefreshToken string) error
	MarkGoogleConnectionReauthRequired(ctx context.Context, userID, connectionID int64, detail string) error
	DeleteGoogleConnection(ctx context.Context, userID, connectionID int64) error
}

// Manager coordinates Google OAuth for every user of one Rolltop install.
type Manager struct {
	config    Config
	client    *Client
	store     ConnectionStore
	masterKey []byte
	flows     *flowStore
	now       func() time.Time

	refreshMu sync.Mutex
	refreshes map[refreshKey]*refreshCall

	// tokenMu guards the decrypted-token cache. Every IMAP connection asks for a
	// token, and a sync pass opens several per mailbox, so without this a single
	// pass over a large account performs hundreds of single-row reads and
	// AES-GCM decrypts of a row that has not changed. The database it would
	// hammer is the same one that reports "database is locked" under load.
	tokenMu sync.Mutex
	tokens  map[refreshKey]cachedToken
}

// cachedToken is a decrypted access token and the instant it stops being
// usable. Nothing here is written to disk; the row remains the durable copy.
type cachedToken struct {
	token     string
	expiresAt time.Time
}

type refreshKey struct {
	userID       int64
	connectionID int64
}

type refreshCall struct {
	done       chan struct{}
	token      string
	connection store.GoogleConnection
	err        error
}

// NewManager builds a manager. A nil store or a master key of the wrong length
// makes every operation fail rather than silently storing weak data.
func NewManager(cfg Config, connections ConnectionStore, masterKey []byte) *Manager {
	return &Manager{
		config:    cfg,
		client:    NewClient(cfg),
		store:     connections,
		masterKey: masterKey,
		flows:     newFlowStore(time.Now),
		refreshes: map[refreshKey]*refreshCall{},
		tokens:    map[refreshKey]cachedToken{},
	}
}

// Config exposes the configuration so routes can report whether Google is set up.
func (m *Manager) Config() Config { return m.config }

// Configured reports whether the operator supplied client credentials.
func (m *Manager) Configured() bool {
	return m != nil && m.config.Configured()
}

// Client exposes the HTTP client so tests can retarget endpoints and backoff.
func (m *Manager) Client() *Client { return m.client }

// SetNow overrides the clock so tests can force token expiry. It must be called
// before the manager starts serving requests: the manager's own clock and the
// client's are plain fields read without synchronization on every call.
func (m *Manager) SetNow(now func() time.Time) {
	m.now = now
	m.client.Now = now
	// The flow store is read under its own lock by concurrent connect and
	// callback requests, so its clock is swapped under that same lock.
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
		return errors.New("google connections are unavailable")
	}
	if len(m.masterKey) != 32 {
		return errors.New("google tokens cannot be encrypted without a 32-byte master key")
	}
	if !m.config.Configured() {
		return ErrNotConfigured
	}
	if len(m.config.RedirectURLs) == 0 {
		// config.Load enforces this for the normal startup path; repeating it
		// here means a manager built directly cannot start a flow whose
		// redirect URI would have to be invented.
		return ErrNoRedirectURI
	}
	return nil
}

// StartConnect begins consent for one signed-in user and returns the Google URL
// to redirect the browser to. loginHint pre-selects an account when
// re-authorizing an existing connection.
func (m *Manager) StartConnect(userID int64, redirectURI, loginHint string) (string, error) {
	if err := m.ready(); err != nil {
		return "", err
	}
	if userID <= 0 {
		return "", errors.New("google authorization requires a signed-in user")
	}
	if strings.TrimSpace(redirectURI) == "" {
		return "", ErrNoRedirectURI
	}
	// The session and CSRF secrets come from the same generator, so hardening
	// token generation happens in one place rather than missing OAuth.
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

// CompleteConnect finishes consent: it validates the pending flow, exchanges the
// code, identifies the account, and stores the tokens encrypted.
func (m *Manager) CompleteConnect(ctx context.Context, userID int64, state, code string) (store.GoogleConnection, error) {
	if err := m.ready(); err != nil {
		return store.GoogleConnection{}, err
	}
	flow, err := m.flows.take(state, userID)
	if err != nil {
		return store.GoogleConnection{}, err
	}
	token, err := m.client.ExchangeCode(ctx, code, flow.redirectURI, flow.codeVerifier)
	if err != nil {
		return store.GoogleConnection{}, err
	}
	info, err := m.client.Userinfo(ctx, token.AccessToken)
	if err != nil {
		return store.GoogleConnection{}, err
	}
	email := strings.TrimSpace(info.Email)
	if email == "" {
		return store.GoogleConnection{}, errors.New("google did not report an email address for this account")
	}
	if !info.EmailVerified {
		// The address becomes this connection's display identity, its
		// reauthorization login hint, and the IMAP/SMTP username, so an
		// address Google itself does not vouch for must not be adopted.
		return store.GoogleConnection{}, errors.New("google reports this account's email address as unverified")
	}
	if strings.TrimSpace(info.Subject) == "" {
		return store.GoogleConnection{}, errors.New("google did not report a stable account identifier")
	}
	encryptedRefresh, err := crypto.EncryptString(m.masterKey, token.RefreshToken)
	if err != nil {
		return store.GoogleConnection{}, err
	}
	encryptedAccess, err := m.encryptAccessToken(token.AccessToken)
	if err != nil {
		return store.GoogleConnection{}, err
	}
	connection, err := m.store.UpsertGoogleConnection(ctx, userID, store.GoogleConnectionUpsert{
		GoogleEmail:           email,
		GoogleSubject:         info.Subject,
		EncryptedRefreshToken: encryptedRefresh,
		EncryptedAccessToken:  encryptedAccess,
		AccessTokenExpiresAt:  token.ExpiresAt,
		GrantedScopes:         token.Scopes,
	})
	if err != nil {
		return store.GoogleConnection{}, err
	}
	// A reconnect lands on the row it already had - the subject claim matches -
	// so this process can be holding a cached access token for it, and that
	// token was issued under the grant the user has just come back to widen.
	// Serving it on would refuse every contacts and calendar request for the
	// rest of its hour with "reconnect the account to grant access", which is
	// exactly what was just done.
	//
	// The cache is dropped rather than filled with the token this exchange
	// produced. Two consents for one account can be in flight at once, and
	// seeding the cache here would let the slower one install its older token
	// over the row the faster one already wrote - the same stale-grant failure,
	// reached by a narrower path. An empty entry cannot be stale: the next
	// caller reads the row, which is the copy the last writer won.
	m.forgetToken(userID, connection.ID)
	return connection, nil
}

// AccessToken returns a currently valid access token for a connection,
// refreshing it when it is expired or close to expiring.
func (m *Manager) AccessToken(ctx context.Context, userID, connectionID int64) (string, error) {
	if err := m.ready(); err != nil {
		return "", err
	}
	if token, ok := m.cachedToken(userID, connectionID); ok {
		return token, nil
	}
	token, connection, err := m.AccessTokenAndConnection(ctx, userID, connectionID)
	if err != nil {
		return "", err
	}
	m.cacheToken(userID, connectionID, token, connection.AccessTokenExpiresAt)
	return token, nil
}

// cachedToken returns a token held in memory while it is still comfortably
// valid. It applies the same expiry margin as the stored copy, so the cache can
// never keep a token alive past the point the database would have refreshed it.
func (m *Manager) cachedToken(userID, connectionID int64) (string, bool) {
	m.tokenMu.Lock()
	defer m.tokenMu.Unlock()
	entry, ok := m.tokens[refreshKey{userID: userID, connectionID: connectionID}]
	if !ok || entry.token == "" || entry.expiresAt.IsZero() {
		return "", false
	}
	if !m.timeNow().Add(refreshSkew).Before(entry.expiresAt) {
		return "", false
	}
	return entry.token, true
}

func (m *Manager) cacheToken(userID, connectionID int64, token string, expiresAt time.Time) {
	if token == "" || expiresAt.IsZero() {
		return
	}
	m.tokenMu.Lock()
	defer m.tokenMu.Unlock()
	m.tokens[refreshKey{userID: userID, connectionID: connectionID}] = cachedToken{token: token, expiresAt: expiresAt}
}

// forgetToken drops a cached token whose validity this process can no longer
// vouch for: the server rejected it, or the connection is gone.
func (m *Manager) forgetToken(userID, connectionID int64) {
	if m == nil {
		return
	}
	m.tokenMu.Lock()
	defer m.tokenMu.Unlock()
	delete(m.tokens, refreshKey{userID: userID, connectionID: connectionID})
}

// AccessTokenAndConnection is AccessToken for callers that also need the
// connection's current state. It hands back the row this call already loaded
// (updated in place when a refresh ran) so the caller does not have to read it
// again straight after.
func (m *Manager) AccessTokenAndConnection(ctx context.Context, userID, connectionID int64) (string, store.GoogleConnection, error) {
	if err := m.ready(); err != nil {
		return "", store.GoogleConnection{}, err
	}
	connection, err := m.store.GoogleConnection(ctx, userID, connectionID)
	if err != nil {
		return "", store.GoogleConnection{}, err
	}
	if connection.NeedsReauth() {
		return "", connection, fmt.Errorf("%w: %s", ErrReauthRequired, connection.GoogleEmail)
	}
	if token, ok := m.usableStoredToken(connection); ok {
		return token, connection, nil
	}
	return m.refresh(ctx, userID, connectionID, false)
}

// ForceRefresh discards the stored access token and fetches a new one. The IMAP
// and SMTP paths use it to retry once after an authentication failure, which
// covers a token Google invalidated before its stated expiry.
//
// Unlike AccessToken it never adopts the result of a refresh that was already
// running: that refresh may well have produced the very token the caller just
// had rejected, and handing it back would turn the retry into a no-op.
func (m *Manager) ForceRefresh(ctx context.Context, userID, connectionID int64) (string, error) {
	if err := m.ready(); err != nil {
		return "", err
	}
	// The caller reached this because a server rejected the token, so the
	// cached copy is exactly the value that must not be handed out again.
	m.forgetToken(userID, connectionID)
	token, connection, err := m.refresh(ctx, userID, connectionID, true)
	if err != nil {
		return "", err
	}
	m.cacheToken(userID, connectionID, token, connection.AccessTokenExpiresAt)
	return token, nil
}

// AbandonFlow releases a pending authorization the user will never finish, so a
// cancelled consent does not hold its slot until the TTL expires.
func (m *Manager) AbandonFlow(userID int64, state string) {
	if m == nil || strings.TrimSpace(state) == "" {
		return
	}
	_, _ = m.flows.take(state, userID)
}

// usableStoredToken reports the stored access token when it is still good.
// A token that cannot be decrypted is treated as absent rather than fatal,
// because a refresh recovers the connection without user action.
func (m *Manager) usableStoredToken(connection store.GoogleConnection) (string, bool) {
	if strings.TrimSpace(connection.EncryptedAccessToken) == "" {
		return "", false
	}
	if connection.AccessTokenExpiresAt.IsZero() {
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

// refresh performs one refresh per connection at a time. Sync workers for the
// same account run concurrently, and letting each of them refresh would race
// for which token gets persisted last.
//
// A forced refresh waits for any running refresh to finish and then performs
// its own, because its caller has already established that the token in flight
// is not good enough.
func (m *Manager) refresh(ctx context.Context, userID, connectionID int64, force bool) (string, store.GoogleConnection, error) {
	key := refreshKey{userID: userID, connectionID: connectionID}
	for {
		m.refreshMu.Lock()
		inflight, running := m.refreshes[key]
		if !running {
			call := &refreshCall{done: make(chan struct{})}
			m.refreshes[key] = call
			m.refreshMu.Unlock()

			// Waiters adopt this result, so it must not inherit the
			// claimant's deadline: a sync worker with a short per-request
			// context would otherwise cancel a refresh other callers are
			// waiting on. Waiter-side cancellation still works through the
			// select below.
			refreshCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), refreshTimeout)
			call.token, call.connection, call.err = m.doRefresh(refreshCtx, userID, connectionID, force)
			cancel()

			// Clearing the slot and publishing the result happen together.
			// Doing them separately leaves a window in which an arriving
			// caller finds no refresh in flight and starts a second one.
			m.refreshMu.Lock()
			delete(m.refreshes, key)
			close(call.done)
			m.refreshMu.Unlock()
			return call.token, call.connection, call.err
		}
		m.refreshMu.Unlock()
		select {
		case <-ctx.Done():
			return "", store.GoogleConnection{}, ctx.Err()
		case <-inflight.done:
		}
		if !force {
			return inflight.token, inflight.connection, inflight.err
		}
		// Claim the slot for a refresh of our own.
	}
}

// doRefresh runs with the refresh slot held. It re-reads the connection rather
// than trusting the row its caller saw: several callers can each read a stale
// row and only then queue up here, so without a re-check under the slot the
// second one would refresh a token the first already replaced. force skips the
// re-check because its caller has established the stored token is unusable
// whatever the row says.
func (m *Manager) doRefresh(ctx context.Context, userID, connectionID int64, force bool) (string, store.GoogleConnection, error) {
	connection, err := m.store.GoogleConnection(ctx, userID, connectionID)
	if err != nil {
		return "", store.GoogleConnection{}, err
	}
	if !force {
		if token, ok := m.usableStoredToken(connection); ok {
			return token, connection, nil
		}
	}
	refreshToken, err := crypto.DecryptString(m.masterKey, connection.EncryptedRefreshToken)
	if err != nil {
		// The master key no longer matches this ciphertext, so no amount of
		// retrying helps; the user has to reconnect. Both reauthorization
		// branches drop the cached token, so the invariant "a connection
		// flagged for reauthorization serves nothing" holds locally instead of
		// resting on the cache and the row happening to expire together.
		m.forgetToken(userID, connectionID)
		if markErr := m.store.MarkGoogleConnectionReauthRequired(ctx, userID, connectionID, "stored token could not be decrypted"); markErr != nil {
			log.Printf("google connection user_id=%d connection_id=%d could not be flagged for reauthorization: %v", userID, connectionID, markErr)
		}
		return "", connection, fmt.Errorf("%w: stored token could not be decrypted", ErrReauthRequired)
	}
	token, err := m.client.RefreshToken(ctx, refreshToken)
	if err != nil {
		if errors.Is(err, ErrReauthRequired) {
			// Flagging the row is best effort. If the write fails the caller
			// still has to learn that consent is gone, so the authorization
			// error wins over the storage error: reporting a generic upstream
			// failure here would leave the UI offering "Test connection"
			// instead of "Reauthorize".
			// A connection the user has to reauthorize must not keep serving a
			// cached token, which would hide the broken state until it expires.
			m.forgetToken(userID, connectionID)
			if markErr := m.store.MarkGoogleConnectionReauthRequired(ctx, userID, connectionID, "Google rejected the stored authorization."); markErr != nil {
				log.Printf("google connection user_id=%d connection_id=%d could not be flagged for reauthorization: %v", userID, connectionID, markErr)
			}
			connection.Status = store.GoogleConnectionStatusReauthRequired
		}
		return "", connection, err
	}
	encryptedAccess, err := m.encryptAccessToken(token.AccessToken)
	if err != nil {
		return "", connection, err
	}
	encryptedRefresh := ""
	if token.RefreshToken != "" {
		if encryptedRefresh, err = crypto.EncryptString(m.masterKey, token.RefreshToken); err != nil {
			return "", connection, err
		}
	}
	if err := m.store.UpdateGoogleAccessToken(ctx, userID, connectionID, encryptedAccess, token.ExpiresAt, encryptedRefresh); err != nil {
		return "", connection, err
	}
	// Mirror the write onto the copy handed back, so a caller that needs the
	// connection's current state does not have to re-read the row.
	connection.EncryptedAccessToken = encryptedAccess
	connection.AccessTokenExpiresAt = token.ExpiresAt
	if encryptedRefresh != "" {
		connection.EncryptedRefreshToken = encryptedRefresh
	}
	connection.Status = store.GoogleConnectionStatusOK
	connection.StatusDetail = ""
	return token.AccessToken, connection, nil
}

func (m *Manager) encryptAccessToken(token string) (string, error) {
	if strings.TrimSpace(token) == "" {
		return "", nil
	}
	return crypto.EncryptString(m.masterKey, token)
}

// List returns the user's connections for display.
func (m *Manager) List(ctx context.Context, userID int64) ([]store.GoogleConnection, error) {
	if m == nil || m.store == nil {
		return nil, errors.New("google connections are unavailable")
	}
	return m.store.ListGoogleConnections(ctx, userID)
}

// Get returns one connection owned by the user.
func (m *Manager) Get(ctx context.Context, userID, connectionID int64) (store.GoogleConnection, error) {
	if m == nil || m.store == nil {
		return store.GoogleConnection{}, errors.New("google connections are unavailable")
	}
	return m.store.GoogleConnection(ctx, userID, connectionID)
}

// Disconnect revokes the grant at Google and removes the local connection.
//
// The two failures mean opposite things to the user, so they are reported
// separately: revokeErr means Rolltop forgot the account but Google may still
// list the grant, while err means the local connection is still there and the
// user's click did not take effect. Collapsing them would let a failed delete
// be presented as a successful disconnect.
//
// The local row is deleted even when revocation fails, so an account Google has
// already forgotten about can still be detached.
func (m *Manager) Disconnect(ctx context.Context, userID, connectionID int64) (revokeErr error, err error) {
	if m == nil || m.store == nil {
		return nil, errors.New("google connections are unavailable")
	}
	connection, err := m.store.GoogleConnection(ctx, userID, connectionID)
	if err != nil {
		return nil, err
	}
	revokeErr = m.revoke(ctx, connection)
	if err := m.store.DeleteGoogleConnection(ctx, userID, connectionID); err != nil {
		return revokeErr, err
	}
	// The row is gone, so a token still held in memory would outlive the grant
	// it belongs to and keep a disconnected account working until it expires.
	m.forgetToken(userID, connectionID)
	return revokeErr, nil
}

// revoke asks Google to drop the grant. Revoking the refresh token invalidates
// the whole grant including outstanding access tokens.
func (m *Manager) revoke(ctx context.Context, connection store.GoogleConnection) error {
	if len(m.masterKey) != 32 || strings.TrimSpace(connection.EncryptedRefreshToken) == "" {
		return nil
	}
	refreshToken, err := crypto.DecryptString(m.masterKey, connection.EncryptedRefreshToken)
	if err != nil {
		// Nothing to revoke that we can read; deleting locally is still correct.
		return nil
	}
	return m.client.Revoke(ctx, refreshToken)
}
