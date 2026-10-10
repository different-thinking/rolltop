// File overview: Microsoft identity platform OAuth client configuration. One
// app registration serves every Rolltop user; each user still signs in with
// their own work, school or personal Microsoft account against it. Endpoints
// are struct fields so tests can point the whole flow at a fake Microsoft.

package microsoftauth

import (
	"errors"
	"net/http"
	"net/url"
	"strings"

	"rolltop/backend/googleauth"
)

// ErrNotConfigured reports that the operator supplied no Microsoft client, so
// every Microsoft route answers "unavailable" instead of starting a flow that
// cannot complete.
var ErrNotConfigured = errors.New("microsoft oauth is not configured")

// ErrNoRedirectURI reports that no configured redirect URI matches the origin
// the browser used; Microsoft would answer with a redirect_uri mismatch.
var ErrNoRedirectURI = errors.New("no configured microsoft redirect URI matches this server's address")

// DefaultTenant lets both work or school accounts and personal Microsoft
// accounts sign in. An operator who wants only their own organisation sets
// ROLLTOP_MICROSOFT_TENANT to its tenant id or domain.
const DefaultTenant = "common"

// DefaultLoginHost is the identity platform's global host.
const DefaultLoginHost = "https://login.microsoftonline.com"

// DefaultGraphEndpoint is Microsoft Graph v1.0.
const DefaultGraphEndpoint = "https://graph.microsoft.com/v1.0"

// ScopeCalendars is the delegated read/write calendar permission. Rolltop
// creates, edits and deletes events, answers invitations and creates Teams
// meetings through the event itself, and all of that is this one permission.
const ScopeCalendars = "Calendars.ReadWrite"

// ScopeUserRead identifies the signed-in account through Graph's /me.
const ScopeUserRead = "User.Read"

// DefaultScopes identify the account, keep the grant alive without the user
// (offline_access is what yields a refresh token) and cover calendars.
var DefaultScopes = []string{"openid", "email", "profile", "offline_access", ScopeUserRead, ScopeCalendars}

// CallbackPath is the route Microsoft redirects back to after sign-in.
const CallbackPath = "/api/microsoft/callback"

// Config describes the app registration shared by all Rolltop users.
type Config struct {
	ClientID     string
	ClientSecret string
	Tenant       string
	// RedirectURLs is an allowlist chosen from by request origin, exactly as
	// for Google (googleauth.RedirectURLFor).
	RedirectURLs []string
	Scopes       []string

	AuthorizationEndpoint string
	TokenEndpoint         string
	GraphEndpoint         string
}

// New builds a configuration from the operator's validated settings.
func New(clientID, clientSecret, tenant string, redirectURLs, scopes []string) Config {
	tenant = strings.TrimSpace(tenant)
	if tenant == "" {
		tenant = DefaultTenant
	}
	cfg := Config{
		ClientID:              strings.TrimSpace(clientID),
		ClientSecret:          strings.TrimSpace(clientSecret),
		Tenant:                tenant,
		RedirectURLs:          append([]string(nil), redirectURLs...),
		Scopes:                append([]string(nil), scopes...),
		AuthorizationEndpoint: DefaultLoginHost + "/" + url.PathEscape(tenant) + "/oauth2/v2.0/authorize",
		TokenEndpoint:         DefaultLoginHost + "/" + url.PathEscape(tenant) + "/oauth2/v2.0/token",
		GraphEndpoint:         DefaultGraphEndpoint,
	}
	if len(cfg.Scopes) == 0 {
		cfg.Scopes = append([]string(nil), DefaultScopes...)
	}
	return cfg
}

// Configured reports whether a sign-in can be started at all.
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

// RedirectURL picks the configured redirect URI for how the browser reached
// this server.
func (c Config) RedirectURL(r *http.Request) string {
	return googleauth.RedirectURLFor(c.RedirectURLs, r)
}
