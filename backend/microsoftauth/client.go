// File overview: Direct HTTP calls against the Microsoft identity platform and
// Graph's /me. Tokens, codes and error descriptions that quote them must never
// reach a log line from here.

package microsoftauth

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

// ErrReauthRequired reports that Microsoft no longer honours the stored refresh
// token: consent was revoked, the token expired after its inactivity window,
// or a policy now demands an interactive sign-in. Retrying cannot fix it.
var ErrReauthRequired = errors.New("microsoft connection requires re-authorization")

// ErrUpstream marks a failure talking to Microsoft, as opposed to a local one.
var ErrUpstream = errors.New("microsoft request failed")

// ErrUnauthorized reports that Graph rejected an access token; refreshing it
// is the recovery, unlike ErrReauthRequired.
var ErrUnauthorized = errors.New("microsoft rejected the access token")

const (
	maxTokenResponseBytes = 1 << 20
	defaultMaxAttempts    = 4
	totalAttemptBudget    = 45 * time.Second
)

// Token is the subset of a token response Rolltop stores or uses.
type Token struct {
	AccessToken  string
	RefreshToken string
	ExpiresAt    time.Time
	Scopes       []string
}

// Me identifies the account behind an access token.
type Me struct {
	ID                string `json:"id"`
	Mail              string `json:"mail"`
	UserPrincipalName string `json:"userPrincipalName"`
	DisplayName       string `json:"displayName"`
}

// Email is the account's mail address, falling back to its sign-in name for
// accounts whose directory entry records no mailbox address (personal accounts
// commonly).
func (m Me) Email() string {
	if mail := strings.TrimSpace(m.Mail); mail != "" {
		return mail
	}
	return strings.TrimSpace(m.UserPrincipalName)
}

// Client performs the OAuth and identity calls.
type Client struct {
	Config     Config
	HTTPClient *http.Client
	RetryDelay func(attempt int) time.Duration
	Now        func() time.Time
}

// NewClient builds a client with Rolltop's default timeout and backoff.
func NewClient(cfg Config) *Client {
	return &Client{
		Config:     cfg,
		HTTPClient: &http.Client{Timeout: 30 * time.Second},
		RetryDelay: func(attempt int) time.Duration { return time.Duration(1<<attempt) * 500 * time.Millisecond },
		Now:        time.Now,
	}
}

// ExchangeCode trades an authorization code for tokens. It is attempted once:
// a code is single-use, and a retry after Microsoft consumed it would come
// back invalid_grant and be reported as a dead connection.
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
	form.Set("scope", c.Config.ScopeString())
	token, err := c.token(ctx, form, c.doOnce)
	if err != nil {
		return Token{}, err
	}
	if token.RefreshToken == "" {
		return Token{}, errors.New("microsoft did not return a refresh token; offline_access was not granted and the connection would expire within the hour")
	}
	return token, nil
}

// RefreshToken redeems a refresh token. Microsoft rotates refresh tokens, so
// the answer usually carries a new one, which the caller must store.
func (c *Client) RefreshToken(ctx context.Context, refreshToken string) (Token, error) {
	if !c.Config.Configured() {
		return Token{}, ErrNotConfigured
	}
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", refreshToken)
	form.Set("client_id", c.Config.ClientID)
	form.Set("client_secret", c.Config.ClientSecret)
	form.Set("scope", c.Config.ScopeString())
	return c.token(ctx, form, c.do)
}

// Me asks Graph who an access token belongs to.
func (c *Client) Me(ctx context.Context, accessToken string) (Me, error) {
	endpoint := strings.TrimRight(c.Config.GraphEndpoint, "/") + "/me?$select=id,mail,userPrincipalName,displayName"
	body, err := c.do(ctx, func() (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+accessToken)
		req.Header.Set("Accept", "application/json")
		return req, nil
	})
	if err != nil {
		return Me{}, err
	}
	var me Me
	if err := json.Unmarshal(body, &me); err != nil {
		return Me{}, fmt.Errorf("%w: decode /me: %v", ErrUpstream, err)
	}
	return me, nil
}

func (c *Client) token(ctx context.Context, form url.Values,
	send func(context.Context, func() (*http.Request, error)) ([]byte, error)) (Token, error) {
	body, err := send(ctx, func() (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Config.TokenEndpoint, strings.NewReader(form.Encode()))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Accept", "application/json")
		return req, nil
	})
	if err != nil {
		return Token{}, err
	}
	var payload struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
		Scope        string `json:"scope"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return Token{}, fmt.Errorf("%w: decode token response: %v", ErrUpstream, err)
	}
	if strings.TrimSpace(payload.AccessToken) == "" {
		return Token{}, fmt.Errorf("%w: token response contained no access token", ErrUpstream)
	}
	token := Token{
		AccessToken:  payload.AccessToken,
		RefreshToken: payload.RefreshToken,
		Scopes:       strings.Fields(payload.Scope),
	}
	if payload.ExpiresIn > 0 {
		token.ExpiresAt = c.now().Add(time.Duration(payload.ExpiresIn) * time.Second).UTC()
	}
	return token, nil
}

func (c *Client) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *Client) do(ctx context.Context, build func() (*http.Request, error)) ([]byte, error) {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, totalAttemptBudget)
		defer cancel()
	}
	var lastErr error
	for attempt := 0; attempt < defaultMaxAttempts; attempt++ {
		if attempt > 0 {
			delay := 500 * time.Millisecond
			if c.RetryDelay != nil {
				delay = c.RetryDelay(attempt - 1)
			}
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return nil, ctx.Err()
			case <-timer.C:
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

func (c *Client) doOnce(_ context.Context, build func() (*http.Request, error)) ([]byte, error) {
	body, _, err := c.attempt(build)
	return body, err
}

func (c *Client) attempt(build func() (*http.Request, error)) ([]byte, bool, error) {
	req, err := build()
	if err != nil {
		return nil, false, err
	}
	client := c.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
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
	retryable := resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= http.StatusInternalServerError
	return nil, retryable, statusError(resp.StatusCode, body)
}

// statusError classifies a failed response. Only the error code travels: the
// identity platform's error_description carries trace and correlation ids and
// can quote request parameters, and Graph's message can quote the request.
func statusError(status int, body []byte) error {
	var payload struct {
		Error            json.RawMessage `json:"error"`
		ErrorCodes       []int           `json:"error_codes"`
		ErrorDescription string          `json:"error_description"`
	}
	_ = json.Unmarshal(body, &payload)
	code := ""
	var asString string
	if json.Unmarshal(payload.Error, &asString) == nil {
		code = asString
	} else {
		var graph struct {
			Code string `json:"code"`
		}
		if json.Unmarshal(payload.Error, &graph) == nil {
			code = graph.Code
		}
	}
	switch code {
	case "invalid_grant", "interaction_required", "consent_required", "login_required":
		return fmt.Errorf("%w: %s", ErrReauthRequired, code)
	}
	if status == http.StatusUnauthorized {
		return fmt.Errorf("%w: %s", ErrUnauthorized, orUnknown(code))
	}
	return fmt.Errorf("%w: HTTP %d %s", ErrUpstream, status, orUnknown(code))
}

func orUnknown(code string) string {
	if strings.TrimSpace(code) == "" {
		return "unspecified"
	}
	return code
}
