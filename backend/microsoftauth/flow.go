// File overview: Authorization-code flow with PKCE for Microsoft sign-in.
// Pending flows are held in memory keyed by an unguessable state value, with
// the same per-user bound googleauth applies and for the same reason: starting
// a flow takes a slot, and one user must not be able to evict another's.

package microsoftauth

import (
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/url"
	"strings"
	"sync"
	"time"
)

// ErrUnknownFlow reports a callback whose state matches no pending sign-in
// started by this user. Expired flows look the same on purpose.
var ErrUnknownFlow = errors.New("microsoft authorization request is unknown or expired")

const (
	flowTTL                = 10 * time.Minute
	maxPendingFlowsPerUser = 8
	maxPendingFlows        = 256
)

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

func newFlowStore() *flowStore {
	return &flowStore{flows: map[string]pendingFlow{}, now: time.Now}
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
	for s.countLocked(&flow.userID) >= maxPendingFlowsPerUser {
		if !s.deleteOldestLocked(&flow.userID) {
			break
		}
	}
	for len(s.flows) >= maxPendingFlows {
		if !s.deleteOldestLocked(nil) {
			break
		}
	}
	s.flows[state] = flow
}

// take returns and removes a pending flow, so a state is single-use. A flow
// started by one user survives another user's attempt to finish it.
func (s *flowStore) take(state string, userID int64) (pendingFlow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.evictExpiredLocked()
	flow, ok := s.flows[state]
	if !ok || flow.userID != userID {
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

func (s *flowStore) countLocked(userID *int64) int {
	count := 0
	for _, flow := range s.flows {
		if userID == nil || flow.userID == *userID {
			count++
		}
	}
	return count
}

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

func codeChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// authorizationURL builds the sign-in URL. prompt=select_account lets a reader
// with several Microsoft accounts pick the right one instead of being signed
// in silently with whichever the browser used last; a login hint narrows it
// when an existing connection is re-authorized.
func authorizationURL(cfg Config, redirectURI, state, verifier, loginHint string) (string, error) {
	parsed, err := url.Parse(cfg.AuthorizationEndpoint)
	if err != nil {
		return "", err
	}
	query := parsed.Query()
	query.Set("client_id", cfg.ClientID)
	query.Set("redirect_uri", redirectURI)
	query.Set("response_type", "code")
	query.Set("response_mode", "query")
	query.Set("scope", cfg.ScopeString())
	query.Set("state", state)
	query.Set("code_challenge", codeChallenge(verifier))
	query.Set("code_challenge_method", "S256")
	if hint := strings.TrimSpace(loginHint); hint != "" {
		query.Set("login_hint", hint)
	} else {
		query.Set("prompt", "select_account")
	}
	parsed.RawQuery = query.Encode()
	return parsed.String(), nil
}
