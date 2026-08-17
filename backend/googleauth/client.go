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

// maxTokenResponseBytes caps how much of a token or userinfo response is read.
// Google's responses are well under a kilobyte; anything larger is a fault.
const maxTokenResponseBytes = 1 << 20

const defaultMaxAttempts = 4

// Token is the subset of an OAuth token response Rolltop stores or uses.
type Token struct {
	AccessToken  string
	RefreshToken string
	ExpiresAt    time.Time
	Scopes       []string
	IDToken      string
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

func (c *Client) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *Client) httpClient() *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	return http.DefaultClient
}

func (c *Client) retryDelay(attempt int) time.Duration {
	if c.RetryDelay != nil {
		return c.RetryDelay(attempt)
	}
	return defaultRetryDelay(attempt)
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
	token, err := c.postToken(ctx, form)
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
	return c.postToken(ctx, form)
}

// Revoke asks Google to invalidate a grant. Callers delete their local copy
// regardless of the outcome, so this reports the error without acting on it.
func (c *Client) Revoke(ctx context.Context, token string) error {
	if strings.TrimSpace(token) == "" {
		return nil
	}
	form := url.Values{}
	form.Set("token", token)
	_, err := c.postForm(ctx, c.Config.RevokeEndpoint, form)
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

func (c *Client) postToken(ctx context.Context, form url.Values) (Token, error) {
	body, err := c.postForm(ctx, c.Config.TokenEndpoint, form)
	if err != nil {
		return Token{}, err
	}
	var payload struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
		Scope        string `json:"scope"`
		IDToken      string `json:"id_token"`
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
		IDToken:      payload.IDToken,
	}
	if payload.ExpiresIn > 0 {
		token.ExpiresAt = c.now().Add(time.Duration(payload.ExpiresIn) * time.Second).UTC()
	}
	return token, nil
}

func (c *Client) postForm(ctx context.Context, endpoint string, form url.Values) ([]byte, error) {
	encoded := form.Encode()
	return c.do(ctx, func() (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(encoded))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Accept", "application/json")
		return req, nil
	})
}

// do runs a request with backoff on rate limits and transient server errors.
// The request is rebuilt per attempt because its body is consumed each time.
func (c *Client) do(ctx context.Context, build func() (*http.Request, error)) ([]byte, error) {
	var lastErr error
	for attempt := 0; attempt < defaultMaxAttempts; attempt++ {
		if attempt > 0 {
			if err := sleepContext(ctx, c.retryDelay(attempt-1)); err != nil {
				return nil, err
			}
		}
		req, err := build()
		if err != nil {
			return nil, err
		}
		resp, err := c.httpClient().Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxTokenResponseBytes))
		_ = resp.Body.Close()
		if readErr != nil {
			lastErr = readErr
			continue
		}
		if resp.StatusCode == http.StatusOK {
			return body, nil
		}
		if retryableStatus(resp.StatusCode) {
			lastErr = statusError(resp.StatusCode, body)
			continue
		}
		return nil, statusError(resp.StatusCode, body)
	}
	return nil, fmt.Errorf("google request failed after %d attempts: %w", defaultMaxAttempts, lastErr)
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
	if payload.Error != "" {
		return fmt.Errorf("google returned HTTP %d: %s", status, describeOAuthError(payload.Error, payload.ErrorDescription))
	}
	// The body of a non-JSON error can echo request parameters, so it is
	// summarized by status alone rather than included verbatim.
	return fmt.Errorf("google returned HTTP %d", status)
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
