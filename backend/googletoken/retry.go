// File overview: The one place that turns a token source into an authenticated
// attempt. Both the IMAP and the SMTP client need the same recovery, and having
// written it twice once already, the policy lives here so a change to it cannot
// reach only one protocol.

package googletoken

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// refreshTimeout bounds the recovery step on its own budget. It deliberately
// does not share the caller's deadline: the first attempt has already spent an
// unknown amount of it on dialing and a full authentication exchange, so an
// inherited context is frequently expired exactly when the retry matters, and
// the recovery this whole function exists for would be skipped in silence.
const refreshTimeout = 30 * time.Second

// TokenSource is the slice of the Google manager that authentication needs.
type TokenSource interface {
	AccessToken(ctx context.Context, userID, connectionID int64) (string, error)
	ForceRefresh(ctx context.Context, userID, connectionID int64) (string, error)
}

// ErrNoTokenSource reports that an OAuth account was reached by a client that
// was never wired to the Google manager. That is a wiring mistake rather than a
// credential problem, so it is named instead of surfacing as a login failure.
var ErrNoTokenSource = errors.New("this client cannot mint Google access tokens")

// AuthError marks a failure at the credential exchange. Callers wrap their
// protocol's authentication error in it so a retry is not spent on a server
// that was merely unreachable or a recipient that was rejected.
type AuthError struct{ Err error }

func (e AuthError) Error() string { return e.Err.Error() }
func (e AuthError) Unwrap() error { return e.Err }

// WithFreshToken runs attempt with a valid access token and retries it once
// against a forcibly refreshed one when the server rejected the credentials.
//
// The retry exists because a token can be rejected while this side still
// believes in it — a revoked scope, a password change, plain clock skew — and
// one extra round trip is cheaper than failing a whole sync run or send. It
// happens at most once, and only when the refresh actually produced a different
// token: repeating the value the server just refused would double every failing
// login for nothing.
func WithFreshToken(ctx context.Context, tokens TokenSource, userID, connectionID int64, attempt func(token string) error) error {
	if tokens == nil {
		return fmt.Errorf("%w: connection %d", ErrNoTokenSource, connectionID)
	}
	token, err := tokens.AccessToken(ctx, userID, connectionID)
	if err != nil {
		return fmt.Errorf("obtain google access token: %w", err)
	}
	err = attempt(token)
	var authFailure AuthError
	if err == nil || !errors.As(err, &authFailure) {
		return err
	}
	refreshCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), refreshTimeout)
	defer cancel()
	refreshed, refreshErr := tokens.ForceRefresh(refreshCtx, userID, connectionID)
	if refreshErr != nil || refreshed == token {
		// The original rejection describes the account better than a refresh
		// failure that is only a symptom of it.
		return err
	}
	return attempt(refreshed)
}
