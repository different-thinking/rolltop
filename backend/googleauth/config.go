// File overview: Google OAuth client configuration read from the environment.
// Endpoints are struct fields rather than constants so tests can point the whole
// flow at a fake Google without touching global state.

package googleauth

import (
	"errors"
	"net/http"
	"net/url"
	"os"
	"strings"
)

// ErrNotConfigured reports that the operator has not supplied Google OAuth
// client credentials, so every Google route must answer "unavailable" instead
// of starting a flow that cannot complete.
var ErrNotConfigured = errors.New("google oauth is not configured")

// Default endpoints for Google's OAuth 2.0 and OpenID Connect services.
const (
	DefaultAuthorizationEndpoint = "https://accounts.google.com/o/oauth2/v2/auth"
	DefaultTokenEndpoint         = "https://oauth2.googleapis.com/token"
	DefaultRevokeEndpoint        = "https://oauth2.googleapis.com/revoke"
	DefaultUserinfoEndpoint      = "https://openidconnect.googleapis.com/v1/userinfo"
)

// ScopeMail is the IMAP/SMTP scope. It is requested from the first connect so a
// later Gmail phase does not have to send every user back through consent.
const ScopeMail = "https://mail.google.com/"

// DefaultScopes covers identifying the account plus IMAP/SMTP access. Contacts
// and calendar scopes are added incrementally by their own phases.
var DefaultScopes = []string{"openid", "email", ScopeMail}

// Config describes the single OAuth client shared by all Rolltop users. Each
// user still authorizes their own Google accounts against it.
type Config struct {
	ClientID     string
	ClientSecret string
	// RedirectURLs is an allowlist. Google requires an exact match against the
	// URIs registered in the Cloud console, and an install may legitimately be
	// reachable under more than one origin (production host plus localhost for
	// development), so the request origin decides which one is used.
	RedirectURLs []string
	Scopes       []string

	AuthorizationEndpoint string
	TokenEndpoint         string
	RevokeEndpoint        string
	UserinfoEndpoint      string
}

// ConfigFromEnv reads the operator-supplied Google client configuration.
func ConfigFromEnv() Config {
	cfg := Config{
		ClientID:              strings.TrimSpace(os.Getenv("ROLLTOP_GOOGLE_CLIENT_ID")),
		ClientSecret:          strings.TrimSpace(os.Getenv("ROLLTOP_GOOGLE_CLIENT_SECRET")),
		RedirectURLs:          splitList(os.Getenv("ROLLTOP_GOOGLE_REDIRECT_URLS")),
		Scopes:                splitList(os.Getenv("ROLLTOP_GOOGLE_SCOPES")),
		AuthorizationEndpoint: DefaultAuthorizationEndpoint,
		TokenEndpoint:         DefaultTokenEndpoint,
		RevokeEndpoint:        DefaultRevokeEndpoint,
		UserinfoEndpoint:      DefaultUserinfoEndpoint,
	}
	if len(cfg.Scopes) == 0 {
		cfg.Scopes = append([]string(nil), DefaultScopes...)
	}
	return cfg
}

// Configured reports whether a connect flow can be started at all.
func (c Config) Configured() bool {
	return c.ClientID != "" && c.ClientSecret != ""
}

// ScopeString renders the requested scopes for an authorization URL.
func (c Config) ScopeString() string {
	if len(c.Scopes) == 0 {
		return strings.Join(DefaultScopes, " ")
	}
	return strings.Join(c.Scopes, " ")
}

// RedirectURL picks the configured redirect URI that matches how the browser
// reached this server. Falling back to the first entry keeps a single-origin
// install working without the operator having to think about ordering; falling
// back to the request origin keeps development usable before any redirect URI
// has been configured.
func (c Config) RedirectURL(r *http.Request) string {
	origin := requestOrigin(r)
	for _, candidate := range c.RedirectURLs {
		if sameOrigin(candidate, origin) {
			return candidate
		}
	}
	if len(c.RedirectURLs) > 0 {
		return c.RedirectURLs[0]
	}
	if origin == "" {
		return ""
	}
	return origin + CallbackPath
}

// CallbackPath is the route Google redirects back to after consent.
const CallbackPath = "/api/google/callback"

func sameOrigin(rawURL, origin string) bool {
	if rawURL == "" || origin == "" {
		return false
	}
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	return strings.EqualFold(parsed.Scheme+"://"+parsed.Host, origin)
}

func requestOrigin(r *http.Request) string {
	if r == nil {
		return ""
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if forwarded := strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")); forwarded != "" {
		// Only the first value matters when a chain of proxies appended theirs.
		if first, _, found := strings.Cut(forwarded, ","); found {
			forwarded = strings.TrimSpace(first)
		}
		if forwarded == "http" || forwarded == "https" {
			scheme = forwarded
		}
	}
	host := strings.TrimSpace(r.Host)
	if host == "" {
		return ""
	}
	return scheme + "://" + host
}

// splitList accepts comma, whitespace, or newline separated values so operators
// can format multi-value environment variables however reads best for them.
func splitList(raw string) []string {
	fields := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == '\n' || r == '\r' || r == '\t' || r == ' '
	})
	out := make([]string, 0, len(fields))
	seen := map[string]bool{}
	for _, field := range fields {
		field = strings.TrimSpace(field)
		if field == "" || seen[field] {
			continue
		}
		seen[field] = true
		out = append(out, field)
	}
	return out
}
