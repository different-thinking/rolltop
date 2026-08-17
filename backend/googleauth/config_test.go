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

func TestNewFillsDefaultScopesAndEndpoints(t *testing.T) {
	cfg := New(" client-id ", "client-secret", []string{"https://rolltop.example.test" + CallbackPath}, nil)
	if !cfg.Configured() || cfg.ClientID != "client-id" {
		t.Fatalf("config = %+v, want trimmed and configured", cfg)
	}
	if cfg.ScopeString() == "" || cfg.Scopes[len(cfg.Scopes)-1] != ScopeMail {
		t.Fatalf("default scopes = %v, want the mail scope included", cfg.Scopes)
	}
	if cfg.TokenEndpoint != DefaultTokenEndpoint || cfg.UserinfoEndpoint != DefaultUserinfoEndpoint {
		t.Fatalf("endpoints not defaulted: %+v", cfg)
	}
	explicit := New("id", "secret", nil, []string{"openid", "email"})
	if explicit.ScopeString() != "openid email" {
		t.Fatalf("scope string = %q, want the configured scopes", explicit.ScopeString())
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
}

func TestRedirectURLPrefersHostMatchOverListOrder(t *testing.T) {
	// A TLS-terminating proxy that forwards without X-Forwarded-Proto makes an
	// https deployment look like plain http here. Picking the first list entry
	// would hand Google the localhost URI and make production unusable, so the
	// entry whose host actually matches wins.
	cfg := Config{
		RedirectURLs: []string{
			"http://localhost:8080" + CallbackPath,
			"https://rolltop.example.test" + CallbackPath,
		},
	}
	got := cfg.RedirectURL(requestTo(t, "http://rolltop.example.test/api/google/connect", ""))
	if got != "https://rolltop.example.test"+CallbackPath {
		t.Fatalf("host-matched redirect = %q, want the production entry", got)
	}
}

func TestRedirectURLRefusesToGuessForUnknownOrigin(t *testing.T) {
	cfg := Config{
		RedirectURLs: []string{
			"https://rolltop.example.test" + CallbackPath,
			"http://localhost:8080" + CallbackPath,
		},
	}
	// Sending an unrelated origin to Google guarantees redirect_uri_mismatch.
	// Reporting the misconfiguration up front is the honest outcome.
	unknown := requestTo(t, "https://other.example.test/api/google/connect", "https")
	if got := cfg.RedirectURL(unknown); got != "" {
		t.Fatalf("unknown-origin redirect = %q, want no guess", got)
	}
}

func TestRedirectURLUsesSoleConfiguredEntry(t *testing.T) {
	// With one URI there is nothing to choose, so a proxy that rewrites the
	// host or scheme must not be able to break the flow.
	cfg := Config{RedirectURLs: []string{"https://rolltop.example.test" + CallbackPath}}
	for _, request := range []*http.Request{
		requestTo(t, "http://rolltop.example.test/api/google/connect", ""),
		requestTo(t, "http://10.0.0.5:8080/api/google/connect", ""),
	} {
		if got := cfg.RedirectURL(request); got != "https://rolltop.example.test"+CallbackPath {
			t.Fatalf("sole-entry redirect = %q", got)
		}
	}
}

func TestRedirectURLNeverInventsOneFromRequestHeaders(t *testing.T) {
	// X-Forwarded-Host is attacker-controllable when no proxy strips it, so an
	// empty allowlist must fail rather than build a redirect URI from it.
	cfg := Config{}
	request := requestTo(t, "http://localhost:8080/api/google/connect", "")
	request.Header.Set("X-Forwarded-Host", "attacker.example.test")
	if got := cfg.RedirectURL(request); got != "" {
		t.Fatalf("unconfigured redirect = %q, want no guess", got)
	}
	if cfg.RedirectURL(nil) != "" {
		t.Fatal("nil request produced a redirect URI")
	}
}

func TestRedirectURLHonoursForwardedHost(t *testing.T) {
	// Behind a proxy that forwards to an internal upstream, r.Host is the
	// upstream address and only X-Forwarded-Host names what the browser used.
	cfg := Config{
		RedirectURLs: []string{
			"https://rolltop.example.test" + CallbackPath,
			"http://localhost:8080" + CallbackPath,
		},
	}
	request := requestTo(t, "http://10.0.0.5:8080/api/google/connect", "https")
	request.Header.Set("X-Forwarded-Host", "rolltop.example.test, internal.example.test")
	if got := cfg.RedirectURL(request); got != "https://rolltop.example.test"+CallbackPath {
		t.Fatalf("forwarded-host redirect = %q", got)
	}
}
