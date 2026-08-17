// File overview: Authorization-code flow with PKCE. Pending flows are held in
// memory keyed by an unguessable state value, so a planted cookie cannot make a
// signed-in session adopt an attacker's Google account.

package googleauth

import (
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/url"
	"strings"
	"sync"
	"time"
)

// ErrUnknownFlow reports a callback whose state does not match a pending
// authorization started by this user. Expired flows look the same on purpose.
var ErrUnknownFlow = errors.New("google authorization request is unknown or expired")

// flowTTL bounds how long a started authorization may sit unfinished. Ten
// minutes is long enough for consent including a password prompt and 2FA.
const flowTTL = 10 * time.Minute

// maxPendingFlowsPerUser bounds how many unfinished authorizations one user can
// hold. Starting a flow is a plain GET, so a page a user merely visits can fire
// off connect requests; without a per-user bound those would evict every other
// user's pending consent and a signed-in victim could be kept from ever
// completing one. Eight covers a person retrying in several tabs.
const maxPendingFlowsPerUser = 8

// maxPendingFlows is the whole-process backstop, only reachable with many
// distinct users mid-consent at once.
const maxPendingFlows = 256

type pendingFlow struct {
	userID       int64
	codeVerifier string
	redirectURI  string
	createdAt    time.Time
}

type flowStore struct {
	mu    sync.Mutex
	flows map[string]pendingFlow
	now   func() time.Time
}

func newFlowStore(now func() time.Time) *flowStore {
	if now == nil {
		now = time.Now
	}
	return &flowStore{flows: map[string]pendingFlow{}, now: now}
}

func (s *flowStore) setNow(now func() time.Time) {
	if now == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.now = now
}

func (s *flowStore) put(state string, flow pendingFlow) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.evictExpiredLocked()
	s.evictOverflowForUserLocked(flow.userID)
	s.evictGlobalOverflowLocked()
	s.flows[state] = flow
}

// take returns and removes a pending flow, so a state value is single-use and a
// replayed callback cannot mint a second connection.
func (s *flowStore) take(state string, userID int64) (pendingFlow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.evictExpiredLocked()
	flow, ok := s.flows[state]
	if !ok {
		return pendingFlow{}, ErrUnknownFlow
	}
	if flow.userID != userID {
		// A flow started by one signed-in user may not be finished by another,
		// and it must survive the attempt: deleting first would let a foreign
		// callback destroy the owner's pending consent and force them to start
		// over.
		return pendingFlow{}, ErrUnknownFlow
	}
	delete(s.flows, state)
	return flow, nil
}

func (s *flowStore) evictExpiredLocked() {
	cutoff := s.now().Add(-flowTTL)
	for state, flow := range s.flows {
		if flow.createdAt.Before(cutoff) {
			delete(s.flows, state)
		}
	}
}

// evictOverflowForUserLocked makes room for one more flow belonging to userID by
// dropping that user's own oldest entries. Overflow caused by one user is paid
// for by that user, never by anyone else's pending consent.
func (s *flowStore) evictOverflowForUserLocked(userID int64) {
	for s.countForUserLocked(userID) >= maxPendingFlowsPerUser {
		if !s.deleteOldestLocked(&userID) {
			return
		}
	}
}

func (s *flowStore) evictGlobalOverflowLocked() {
	for len(s.flows) >= maxPendingFlows {
		if !s.deleteOldestLocked(nil) {
			return
		}
	}
}

func (s *flowStore) countForUserLocked(userID int64) int {
	count := 0
	for _, flow := range s.flows {
		if flow.userID == userID {
			count++
		}
	}
	return count
}

// deleteOldestLocked removes the oldest flow, optionally restricted to one user.
// It reports whether anything was removed.
func (s *flowStore) deleteOldestLocked(userID *int64) bool {
	oldestState := ""
	var oldestAt time.Time
	for state, flow := range s.flows {
		if userID != nil && flow.userID != *userID {
			continue
		}
		if oldestState == "" || flow.createdAt.Before(oldestAt) {
			oldestState, oldestAt = state, flow.createdAt
		}
	}
	if oldestState == "" {
		return false
	}
	delete(s.flows, oldestState)
	return true
}

// codeChallenge derives the S256 PKCE challenge for a verifier.
func codeChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// authorizationURL builds the consent URL.
//
// access_type=offline plus prompt=consent is what makes Google return a refresh
// token; without prompt=consent it omits one on every authorization after the
// first, which would leave the connection unable to refresh. include_granted_scopes
// keeps previously granted scopes attached when a later phase asks for contacts
// or calendar access, so incremental authorization does not revoke mail access.
func authorizationURL(cfg Config, redirectURI, state, verifier, loginHint string) (string, error) {
	endpoint := cfg.AuthorizationEndpoint
	if strings.TrimSpace(endpoint) == "" {
		endpoint = DefaultAuthorizationEndpoint
	}
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return "", err
	}
	query := parsed.Query()
	query.Set("client_id", cfg.ClientID)
	query.Set("redirect_uri", redirectURI)
	query.Set("response_type", "code")
	query.Set("scope", cfg.ScopeString())
	query.Set("state", state)
	query.Set("code_challenge", codeChallenge(verifier))
	query.Set("code_challenge_method", "S256")
	query.Set("access_type", "offline")
	query.Set("prompt", "consent")
	query.Set("include_granted_scopes", "true")
	if hint := strings.TrimSpace(loginHint); hint != "" {
		// Re-authorizing an existing connection should land on that account
		// rather than whichever Google account the browser used last.
		query.Set("login_hint", hint)
	}
	parsed.RawQuery = query.Encode()
	return parsed.String(), nil
}
