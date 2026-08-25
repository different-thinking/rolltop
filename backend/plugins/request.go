// File overview: One shared, host-side derivation of the external base URL a
// backend plugin should use to build absolute links back to itself. The oidc and
// mail_mcp plugins each used to carry their own copy that trusted the
// X-Forwarded-Host / X-Forwarded-Proto headers unconditionally; the two drifted,
// and both let a client-supplied host redirect an OAuth/OIDC flow at an attacker
// origin. Centralising it here also lets a configured ROLLTOP_PUBLIC_URL be
// authoritative, closing that hole wherever it is set.

package plugins

import (
	"net/http"
	"net/url"
	"os"
	"strings"
)

// RequestBaseURL returns the external origin (scheme://host) for links a plugin
// generates back to itself. A configured ROLLTOP_PUBLIC_URL is authoritative and
// header-independent -- the reason it exists is to stop a spoofable Host /
// X-Forwarded-Host from steering a redirect or a discovery document at an
// attacker. Only when it is unset does the helper fall back to the request's
// forwarded scheme and host, which keeps single-host deployments working.
func RequestBaseURL(r *http.Request) string {
	if base := canonicalPublicBase(); base != "" {
		return base
	}
	scheme := "http"
	if RequestIsHTTPS(r) {
		scheme = "https"
	}
	host := strings.TrimSpace(r.Header.Get("X-Forwarded-Host"))
	if host == "" {
		host = r.Host
	}
	return scheme + "://" + host
}

// RequestIsHTTPS reports whether the browser reached the server over TLS, either
// directly or through a proxy that terminated it and forwarded the scheme.
func RequestIsHTTPS(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")), "https")
}

// canonicalPublicBase reads ROLLTOP_PUBLIC_URL and returns its scheme+host
// origin, or "" when it is unset or not an absolute http(s) URL with a host.
// Plugins run in the host process, so the same environment the host validated on
// startup is visible here.
func canonicalPublicBase() string {
	raw := strings.TrimSpace(os.Getenv("ROLLTOP_PUBLIC_URL"))
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host
}
