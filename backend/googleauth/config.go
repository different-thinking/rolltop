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

// ErrNoRedirectURI reports that no configured redirect URI matches the origin
// the browser used, so the flow would fail at Google with redirect_uri_mismatch.
var ErrNoRedirectURI = errors.New("no configured google redirect URI matches this server's address")

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

// RedirectURL picks the configured redirect URI matching how the browser
// reached this server. Google compares the value byte for byte against the URIs
// registered in the Cloud console, so guessing wrong is not a degraded
// experience, it is a flow that can never complete.
//
// With a single configured URI there is nothing to choose and it is used as-is,
// which keeps a normal install working even behind a proxy that rewrites the
// scheme or host. With several, the request origin decides. Host-only matching
// is the second pass because a TLS-terminating proxy that forwards without
// X-Forwarded-Proto makes an https deployment look like http here, and picking
// some other entry over the one whose host actually matches would be worse than
// a scheme mismatch the operator can see and fix.
//
// When nothing matches, this returns the empty string rather than an arbitrary
// entry: an unresolvable origin is a configuration problem, and failing at the
// connect button with a clear message beats bouncing the user off Google with
// redirect_uri_mismatch.
func (c Config) RedirectURL(r *http.Request) string {
	if len(c.RedirectURLs) == 1 {
		return c.RedirectURLs[0]
	}
	origin := requestOrigin(r)
	if len(c.RedirectURLs) == 0 {
		// Nothing configured: development against the origin the browser used.
		if origin == "" {
			return ""
		}
		return origin + CallbackPath
	}
	for _, candidate := range c.RedirectURLs {
		if sameOrigin(candidate, origin) {
			return candidate
		}
	}
	for _, candidate := range c.RedirectURLs {
		if sameHost(candidate, origin) {
			return candidate
		}
	}
	return ""
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

// sameHost compares only the host, which is what survives a proxy that
// terminates TLS without announcing it.
func sameHost(rawURL, origin string) bool {
	if rawURL == "" || origin == "" {
		return false
	}
	candidate, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	reached, err := url.Parse(origin)
	if err != nil {
		return false
	}
	return candidate.Host != "" && strings.EqualFold(candidate.Host, reached.Host)
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
