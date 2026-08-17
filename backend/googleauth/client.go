// File overview: Direct HTTP calls against Google's OAuth endpoints, with the
// shared retry/backoff policy every Google API client in Rolltop reuses.
// Tokens and authorization codes must never reach a log line from here.

package googleauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ErrReauthRequired reports that Google rejected the stored refresh token. The
// user has to walk through consent again; retrying cannot fix it.
var ErrReauthRequired = errors.New("google connection requires re-authorization")

// ErrUpstream marks a failure that came from talking to Google, as opposed to a
// local one. Callers use it to decide between "Google could not be reached" and
// an internal error; without it a busy SQLite read would be reported to the
// user as a Google outage.
var ErrUpstream = errors.New("google request failed")

// ErrUnauthorized reports that Google rejected the access token itself. It is
// recoverable by refreshing, unlike ErrReauthRequired.
var ErrUnauthorized = errors.New("google rejected the access token")

// maxTokenResponseBytes caps how much of a token or userinfo response is read.
// Google's responses are well under a kilobyte; anything larger is a fault.
const maxTokenResponseBytes = 1 << 20

const defaultMaxAttempts = 4

// totalAttemptBudget caps a retried call end to end, so a slow upstream cannot
// hold an interactive request open for the sum of every attempt.
const totalAttemptBudget = 45 * time.Second

// Token is the subset of an OAuth token response Rolltop stores or uses.
type Token struct {
	AccessToken  string
	RefreshToken string
	ExpiresAt    time.Time
	Scopes       []string
}

// Userinfo identifies the Google account behind a connection.
type Userinfo struct {
	Subject       string `json:"sub"`
	Email         string `json:"email"`
	EmailVerified bool   `json:"email_verified"`
}

// Client performs the OAuth HTTP calls. The zero value is unusable; construct
// it through NewClient so retry behavior and timeouts are set.
type Client struct {
	Config     Config
	HTTPClient *http.Client
	// RetryDelay maps a zero-based attempt number to a wait. Tests override it
	// to keep backoff assertions instant.
	RetryDelay func(attempt int) time.Duration
	// Now exists so token expiry is computable in tests.
	Now func() time.Time
}

// NewClient builds a client with Rolltop's default timeout and backoff.
func NewClient(cfg Config) *Client {
	return &Client{
		Config:     cfg,
		HTTPClient: &http.Client{Timeout: 30 * time.Second},
		RetryDelay: defaultRetryDelay,
		Now:        time.Now,
	}
}

func defaultRetryDelay(attempt int) time.Duration {
	return time.Duration(1<<attempt) * 500 * time.Millisecond
}

// ExchangeCode trades an authorization code for tokens. A missing refresh token
// is treated as a failure because a connection without one cannot survive the
// first access-token expiry.
func (c *Client) ExchangeCode(ctx context.Context, code, redirectURI, codeVerifier string) (Token, error) {
	if !c.Config.Configured() {
		return Token{}, ErrNotConfigured
	}
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", redirectURI)
	form.Set("code_verifier", codeVerifier)
	form.Set("client_id", c.Config.ClientID)
	form.Set("client_secret", c.Config.ClientSecret)
	// One attempt only: the code is single-use, so if Google consumes it and
	// then fails the response, a retry comes back invalid_grant and would be
	// reported as "needs re-authorization" for what was a transient fault.
	token, err := c.token(ctx, form, c.doOnce)
	if err != nil {
		return Token{}, err
	}
	if token.RefreshToken == "" {
		return Token{}, errors.New("google did not return a refresh token; the connection would expire within an hour")
	}
	return token, nil
}

// RefreshToken exchanges a refresh token for a new access token. Google only
// returns a replacement refresh token when it rotates one, so an empty
// RefreshToken on the result means "keep the stored one".
func (c *Client) RefreshToken(ctx context.Context, refreshToken string) (Token, error) {
	if !c.Config.Configured() {
		return Token{}, ErrNotConfigured
	}
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", refreshToken)
	form.Set("client_id", c.Config.ClientID)
	form.Set("client_secret", c.Config.ClientSecret)
	return c.token(ctx, form, c.do)
}

// revokeTimeout bounds the whole revocation. Callers delete their local copy
// regardless of the outcome and only surface a warning, so an unreachable
// Google must not hold the user's disconnect request open for minutes.
const revokeTimeout = 5 * time.Second

// Revoke asks Google to invalidate a grant. A token Google has already
// invalidated counts as success: the grant is gone, which is what the caller
// wanted. Because the result is best effort, this runs a single attempt under a
// short deadline rather than the shared retry policy — retrying a result nobody
// acts on only makes the user wait.
func (c *Client) Revoke(ctx context.Context, token string) error {
	if strings.TrimSpace(token) == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, revokeTimeout)
	defer cancel()
	form := url.Values{}
	form.Set("token", token)
	_, err := c.doOnce(ctx, func() (*http.Request, error) {
		return formRequest(ctx, c.Config.RevokeEndpoint, form)
	})
	if err != nil && revokedTokenError(err) {
		return nil
	}
	return err
}

// Userinfo identifies the account an access token belongs to.
func (c *Client) Userinfo(ctx context.Context, accessToken string) (Userinfo, error) {
	body, err := c.do(ctx, func() (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.Config.UserinfoEndpoint, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+accessToken)
		req.Header.Set("Accept", "application/json")
		return req, nil
	})
	if err != nil {
		return Userinfo{}, err
	}
	var info Userinfo
	if err := json.Unmarshal(body, &info); err != nil {
		return Userinfo{}, fmt.Errorf("decode google userinfo response: %w", err)
	}
	return info, nil
}

// token posts to the token endpoint through the supplied attempt policy.
func (c *Client) token(ctx context.Context, form url.Values,
	send func(context.Context, func() (*http.Request, error)) ([]byte, error)) (Token, error) {
	body, err := send(ctx, func() (*http.Request, error) {
		return formRequest(ctx, c.Config.TokenEndpoint, form)
	})
	if err != nil {
		return Token{}, err
	}
	var payload struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
		Scope        string `json:"scope"`
		TokenType    string `json:"token_type"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return Token{}, fmt.Errorf("decode google token response: %w", err)
	}
	if strings.TrimSpace(payload.AccessToken) == "" {
		return Token{}, errors.New("google token response contained no access token")
	}
	token := Token{
		AccessToken:  payload.AccessToken,
		RefreshToken: payload.RefreshToken,
		Scopes:       strings.Fields(payload.Scope),
	}
	if payload.ExpiresIn > 0 {
		token.ExpiresAt = c.Now().Add(time.Duration(payload.ExpiresIn) * time.Second).UTC()
	}
	return token, nil
}

func formRequest(ctx context.Context, endpoint string, form url.Values) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	return req, nil
}

// do runs a request with backoff on rate limits and transient server errors.
// The request is rebuilt per attempt because its body is consumed each time.
func (c *Client) do(ctx context.Context, build func() (*http.Request, error)) ([]byte, error) {
	// Four attempts at a 30s client timeout plus backoff is about two minutes,
	// and ExchangeCode and Userinfo run inside a request the browser is waiting
	// on. Bound the whole budget unless the caller already set a deadline.
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, totalAttemptBudget)
		defer cancel()
	}
	var lastErr error
	for attempt := 0; attempt < defaultMaxAttempts; attempt++ {
		if attempt > 0 {
			if err := sleepContext(ctx, c.RetryDelay(attempt-1)); err != nil {
				return nil, err
			}
		}
		body, retryable, err := c.attempt(build)
		if err == nil {
			return body, nil
		}
		if !retryable {
			return nil, err
		}
		lastErr = err
	}
	return nil, fmt.Errorf("%w after %d attempts: %w", ErrUpstream, defaultMaxAttempts, lastErr)
}

// doOnce runs a single attempt, for callers whose result nobody retries on.
func (c *Client) doOnce(ctx context.Context, build func() (*http.Request, error)) ([]byte, error) {
	body, _, err := c.attempt(build)
	return body, err
}

// attempt performs one request and reports whether a retry could help.
func (c *Client) attempt(build func() (*http.Request, error)) (body []byte, retryable bool, err error) {
	req, err := build()
	if err != nil {
		return nil, false, err
	}
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, true, fmt.Errorf("%w: %v", ErrUpstream, err)
	}
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxTokenResponseBytes))
	_ = resp.Body.Close()
	if readErr != nil {
		return nil, true, fmt.Errorf("%w: %v", ErrUpstream, readErr)
	}
	if resp.StatusCode == http.StatusOK {
		return body, false, nil
	}
	return nil, retryableStatus(resp.StatusCode), statusError(resp.StatusCode, body)
}

func retryableStatus(status int) bool {
	return status == http.StatusTooManyRequests || status >= http.StatusInternalServerError
}

// statusError classifies a failed response. Google reports a revoked or
// otherwise dead grant as invalid_grant, which is the one error that must reach
// callers as a distinct type so they can flag the connection for re-consent.
func statusError(status int, body []byte) error {
	var payload struct {
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
	}
	_ = json.Unmarshal(body, &payload)
	if payload.Error == "invalid_grant" {
		return fmt.Errorf("%w: %s", ErrReauthRequired, describeOAuthError(payload.Error, payload.ErrorDescription))
	}
	if status == http.StatusUnauthorized {
		return fmt.Errorf("%w: %s", ErrUnauthorized, describeOAuthError(payload.Error, payload.ErrorDescription))
	}
	if payload.Error != "" {
		return fmt.Errorf("%w: HTTP %d: %s", ErrUpstream, status, describeOAuthError(payload.Error, payload.ErrorDescription))
	}
	// The body of a non-JSON error can echo request parameters, so it is
	// summarized by status alone rather than included verbatim.
	return fmt.Errorf("%w: HTTP %d", ErrUpstream, status)
}

// revokedTokenError reports whether Google's rejection means the grant is
// already gone. Revoking a token that Google has already invalidated answers
// HTTP 400 invalid_token, which is the desired end state, not a failure.
func revokedTokenError(err error) bool {
	return errors.Is(err, ErrReauthRequired) || strings.Contains(err.Error(), "invalid_token")
}

func describeOAuthError(code, description string) string {
	description = strings.TrimSpace(description)
	if description == "" {
		return code
	}
	return code + ": " + description
}

func sleepContext(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
