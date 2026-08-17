// File overview: Static frontend and SPA fallback serving.

package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

const frontendDistDir = "frontend/dist"
const immutableFrontendAssetCacheControl = "public, max-age=31536000, immutable"

var startupBootstrapMarker = []byte(`<meta name="rolltop-startup" />`)

type androidLatestMetadata struct {
	VersionCode int    `json:"versionCode"`
	VersionName string `json:"versionName"`
	APKURL      string `json:"apkUrl"`
	SHA256      string `json:"sha256,omitempty"`
}

func (s *Server) handleApp(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	if r.URL.Path != "/" && !isAppRoute(r.URL.Path) {
		http.NotFound(w, r)
		return
	}
	index := filepath.Join(frontendDistDir, "index.html")
	contents, err := os.ReadFile(index)
	if err != nil {
		http.Error(w, "frontend has not been built; run npm run build", http.StatusServiceUnavailable)
		return
	}
	if s.store != nil {
		payload, payloadErr := s.bootstrapPayload(w, r)
		if errors.Is(payloadErr, errSessionUnavailable) && isPublicAuthRoute(r.URL.Path) {
			// The login and setup shells are the recovery path for a browser
			// whose session cookie cannot be resolved (for example a corrupt
			// session row): render them anonymously so the user can sign in
			// again and replace the broken cookie.
			payload, payloadErr = s.bootstrapPayload(w, r.WithContext(context.WithValue(r.Context(), sessionErrorContextKey, nil)))
		}
		if errors.Is(payloadErr, errSessionUnavailable) {
			sessionUnavailable(w)
			return
		}
		if payloadErr != nil {
			s.serverError(w, payloadErr)
			return
		}
		injected, injectErr := injectStartupBootstrap(contents, payload)
		if injectErr != nil {
			http.Error(w, "frontend startup marker is missing", http.StatusInternalServerError)
			return
		}
		contents = injected
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "private, no-store")
	// Vary:* also makes Cache.put reject this response while older service
	// workers are being replaced, so personalized startup JSON cannot linger.
	w.Header().Set("Vary", "*")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(contents)
}

func injectStartupBootstrap(index []byte, payload any) ([]byte, error) {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	if !bytes.Contains(index, startupBootstrapMarker) {
		return nil, errors.New("startup bootstrap marker is missing")
	}
	script := make([]byte, 0, len(startupBootstrapMarker)+len(encoded)+96)
	script = append(script, `<script id="rolltop-startup" type="application/json">`...)
	script = append(script, encoded...)
	script = append(script, `</script>`...)
	return bytes.Replace(index, startupBootstrapMarker, script, 1), nil
}

func (s *Server) handleFrontendAsset(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	clean := filepath.Clean(strings.TrimPrefix(r.URL.Path, "/"))
	if strings.HasPrefix(clean, "..") || filepath.IsAbs(clean) {
		http.NotFound(w, r)
		return
	}
	full := filepath.Join(frontendDistDir, clean)
	if _, err := os.Stat(full); err != nil {
		http.NotFound(w, r)
		return
	}
	if cacheControl := frontendAssetCacheControl(clean); cacheControl != "" {
		w.Header().Set("Cache-Control", cacheControl)
	}
	http.ServeFile(w, r, full)
}

func (s *Server) handleAndroidLatest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	full := filepath.Join(frontendDistDir, "android", "latest.json")
	data, err := os.ReadFile(full)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	var metadata androidLatestMetadata
	if err := json.Unmarshal(data, &metadata); err != nil {
		http.Error(w, "invalid android update metadata", http.StatusInternalServerError)
		return
	}
	metadata.APKURL = publicRequestBaseURL(r) + "/android/rolltop.apk"
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, metadata)
}

func (s *Server) handleAndroidAPK(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	full := filepath.Join(frontendDistDir, "android", "rolltop.apk")
	if _, err := os.Stat(full); err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/vnd.android.package-archive")
	w.Header().Set("Content-Disposition", `attachment; filename="rolltop.apk"`)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	http.ServeFile(w, r, full)
}

func publicRequestBaseURL(r *http.Request) string {
	scheme := r.Header.Get("X-Forwarded-Proto")
	if scheme == "" {
		if r.TLS != nil {
			scheme = "https"
		} else {
			scheme = "http"
		}
	}
	if forwardedHost := r.Header.Get("X-Forwarded-Host"); forwardedHost != "" {
		return scheme + "://" + forwardedHost
	}
	return scheme + "://" + r.Host
}

func isImmutableFrontendAsset(cleanPath string) bool {
	cleanPath = filepath.ToSlash(filepath.Clean(cleanPath))
	if !strings.HasPrefix(cleanPath, "assets/") {
		return false
	}
	switch strings.ToLower(filepath.Ext(cleanPath)) {
	case ".js", ".css":
		return true
	default:
		return false
	}
}

func frontendAssetCacheControl(cleanPath string) string {
	if isImmutableFrontendAsset(cleanPath) {
		return immutableFrontendAssetCacheControl
	}
	if filepath.ToSlash(filepath.Clean(cleanPath)) == "sw.js" {
		return "no-cache"
	}
	return ""
}

// spaRoute declares one client-side route. This table is the single source of
// truth: Handler() registers mux entries from it and both predicates below
// answer from it, so a new SPA page is declared once instead of being kept in
// step across three hand-maintained lists. Drifting between those lists is not
// cosmetic — a path missing from the mux 404s, and an auth-recovery page
// missing from the public set becomes unreachable during exactly the store
// outages it exists to recover from.
type spaRoute struct {
	path string
	// exact serves the path itself.
	exact bool
	// prefix serves everything below path + "/".
	prefix bool
	// public keeps the page reachable when the session cannot be resolved.
	public bool
	// ownPrefixHandler marks a subtree that is registered elsewhere because it
	// serves more than the app shell.
	ownPrefixHandler bool
}

var spaRoutes = []spaRoute{
	{path: "/setup", exact: true, public: true},
	{path: "/login", exact: true, public: true},
	// Password reset emails link here, so it must survive a broken session too.
	{path: "/reset-password", exact: true, public: true},
	{path: "/mail", exact: true, prefix: true},
	{path: "/snoozes", exact: true},
	{path: "/mailbox", prefix: true},
	{path: "/search", exact: true, prefix: true},
	{path: "/compose", exact: true},
	// /contacts/{id} doubles as a vCard download, so its subtree has its own
	// handler that chooses between the file and the app shell.
	{path: "/contacts", exact: true, prefix: true, ownPrefixHandler: true},
	{path: "/messages", prefix: true},
	{path: "/sync-runs", prefix: true},
	{path: "/settings/account", exact: true, prefix: true},
	{path: "/admin/users", exact: true},
}

// matchSPARoute finds the declaration serving a path.
func matchSPARoute(p string) (spaRoute, bool) {
	for _, route := range spaRoutes {
		if route.exact && p == route.path {
			return route, true
		}
		if route.prefix && strings.HasPrefix(p, route.path+"/") {
			return route, true
		}
	}
	return spaRoute{}, false
}

// isPublicAuthRoute names the SPA routes that must stay reachable without a
// resolvable session so a browser can re-authenticate.
func isPublicAuthRoute(p string) bool {
	route, ok := matchSPARoute(p)
	return ok && route.public
}

func isAppRoute(p string) bool {
	_, ok := matchSPARoute(p)
	return ok
}
