// File overview: Tests for Google client configuration and redirect URI selection.

package googleauth

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func requestTo(t *testing.T, target string, forwardedProto string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	if forwardedProto != "" {
		req.Header.Set("X-Forwarded-Proto", forwardedProto)
	}
	return req
}

func TestConfigFromEnvReadsMultipleRedirectURLs(t *testing.T) {
	t.Setenv("ROLLTOP_GOOGLE_CLIENT_ID", " client-id ")
	t.Setenv("ROLLTOP_GOOGLE_CLIENT_SECRET", "client-secret")
	t.Setenv("ROLLTOP_GOOGLE_REDIRECT_URLS",
		"https://rolltop.example.test/api/google/callback, http://localhost:8080/api/google/callback\nhttps://rolltop.example.test/api/google/callback")
	cfg := ConfigFromEnv()

	if !cfg.Configured() {
		t.Fatal("config with client credentials reported as unconfigured")
	}
	if cfg.ClientID != "client-id" {
		t.Fatalf("client id = %q, want trimmed", cfg.ClientID)
	}
	if len(cfg.RedirectURLs) != 2 {
		t.Fatalf("redirect URLs = %v, want two deduplicated entries", cfg.RedirectURLs)
	}
	if cfg.ScopeString() == "" || cfg.Scopes[len(cfg.Scopes)-1] != ScopeMail {
		t.Fatalf("default scopes = %v, want the mail scope included", cfg.Scopes)
	}
}

func TestConfigFromEnvHonoursExplicitScopes(t *testing.T) {
	t.Setenv("ROLLTOP_GOOGLE_CLIENT_ID", "id")
	t.Setenv("ROLLTOP_GOOGLE_CLIENT_SECRET", "secret")
	t.Setenv("ROLLTOP_GOOGLE_SCOPES", "openid email")
	cfg := ConfigFromEnv()
	if cfg.ScopeString() != "openid email" {
		t.Fatalf("scope string = %q, want the configured scopes", cfg.ScopeString())
	}
}

func TestRedirectURLMatchesRequestOrigin(t *testing.T) {
	cfg := Config{
		RedirectURLs: []string{
			"https://rolltop.example.test" + CallbackPath,
			"http://localhost:8080" + CallbackPath,
		},
	}
	// A development request on localhost must not be sent Google's way with the
	// production redirect URI, which would fail Google's exact-match check.
	got := cfg.RedirectURL(requestTo(t, "http://localhost:8080/api/google/connect", ""))
	if got != "http://localhost:8080"+CallbackPath {
		t.Fatalf("localhost redirect = %q", got)
	}
	// A proxied production request arrives over plain HTTP with a proto header.
	proxied := requestTo(t, "http://rolltop.example.test/api/google/connect", "https, http")
	if got := cfg.RedirectURL(proxied); got != "https://rolltop.example.test"+CallbackPath {
		t.Fatalf("proxied redirect = %q", got)
	}
	// An unrecognized origin falls back to the first configured entry rather
	// than inventing one that Google has never seen.
	unknown := requestTo(t, "https://other.example.test/api/google/connect", "https")
	if got := cfg.RedirectURL(unknown); got != "https://rolltop.example.test"+CallbackPath {
		t.Fatalf("unknown-origin redirect = %q", got)
	}
}

func TestRedirectURLFallsBackToRequestOriginWhenUnconfigured(t *testing.T) {
	cfg := Config{}
	got := cfg.RedirectURL(requestTo(t, "http://localhost:8080/api/google/connect", ""))
	if got != "http://localhost:8080"+CallbackPath {
		t.Fatalf("unconfigured redirect = %q, want the request origin", got)
	}
	if cfg.RedirectURL(nil) != "" {
		t.Fatal("nil request produced a redirect URI")
	}
}
