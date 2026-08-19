package web

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"rolltop/backend/store"
	"rolltop/backend/store/storetest"
	"rolltop/backend/theme"
)

const themeShell = `<!doctype html><html lang="en"><head><meta name="rolltop-theme-color" /><meta name="rolltop-startup" /></head><body></body></html>`

func TestInjectStartupThemeStampsAnExplicitThemeBeforeFirstPaint(t *testing.T) {
	out := string(injectStartupTheme([]byte(themeShell), theme.ClassicDark))
	if !strings.Contains(out, `<html lang="en" data-theme="classic_dark">`) {
		t.Fatalf("dark theme was not stamped onto the shell: %s", out)
	}
	if !strings.Contains(out, `<meta name="theme-color" content="`+theme.DarkChrome+`" />`) {
		t.Fatalf("missing dark chrome colour: %s", out)
	}
	if strings.Contains(out, "prefers-color-scheme") {
		t.Fatalf("an explicit theme must not defer to the system: %s", out)
	}
	if strings.Contains(out, "rolltop-theme-color") {
		t.Fatalf("theme colour marker was left in the shell: %s", out)
	}
}

func TestInjectStartupThemeLeavesSystemUnmarkedAndPublishesBothChromeColours(t *testing.T) {
	out := string(injectStartupTheme([]byte(themeShell), theme.System))
	// An absent data-theme is what makes the stylesheet fall through to
	// prefers-color-scheme, so stamping "system" here would break the System theme.
	if strings.Contains(out, "data-theme") {
		t.Fatalf("system theme must not stamp a marker: %s", out)
	}
	if !strings.Contains(out, `<meta name="theme-color" media="(prefers-color-scheme: light)" content="`+theme.LightChrome+`" />`) {
		t.Fatalf("missing light chrome colour: %s", out)
	}
	if !strings.Contains(out, `<meta name="theme-color" media="(prefers-color-scheme: dark)" content="`+theme.DarkChrome+`" />`) {
		t.Fatalf("missing dark chrome colour: %s", out)
	}
}

func TestInjectStartupThemeLeavesPluginThemesToTheClient(t *testing.T) {
	out := string(injectStartupTheme([]byte(themeShell), "matrix"))
	if strings.Contains(out, "data-theme") {
		t.Fatalf("plugin theme must not be stamped by the server: %s", out)
	}
	if strings.Contains(out, "theme-color") {
		t.Fatalf("plugin chrome colour must be left to the client: %s", out)
	}
}

func TestInjectStartupThemeToleratesAShellWithoutMarkers(t *testing.T) {
	bare := `<!doctype html><html><body></body></html>`
	out := string(injectStartupTheme([]byte(bare), theme.ClassicDark))
	if !strings.Contains(out, `<html data-theme="classic_dark">`) {
		t.Fatalf("bare shell was not stamped: %s", out)
	}
}

// The injection is a no-op without its marker, so the shell has to keep it or
// the theme silently stops reaching the first paint.
func TestFrontendShellKeepsTheThemeColorMarker(t *testing.T) {
	raw, err := os.ReadFile("../../frontend/index.html")
	if err != nil {
		t.Fatalf("read frontend shell: %v", err)
	}
	if !bytes.Contains(raw, startupThemeColorMarker) {
		t.Fatalf("frontend/index.html is missing %s", startupThemeColorMarker)
	}
	if bytes.Contains(raw, []byte(`name="theme-color"`)) {
		t.Fatal("frontend/index.html hard-codes a theme colour; the server publishes it per theme")
	}
}

func TestCurrentThemeIDFallsBackToSystemForSignedOutShells(t *testing.T) {
	server := &Server{}
	anonymous := httptest.NewRequest(http.MethodGet, "/login", nil)
	if got := server.currentThemeID(anonymous); got != theme.System {
		t.Fatalf("currentThemeID(signed out) = %q, want %q", got, theme.System)
	}

	blank := httptest.NewRequest(http.MethodGet, "/mail", nil)
	blank = blank.WithContext(context.WithValue(blank.Context(), userContextKey, currentUser{User: store.User{Theme: "  "}}))
	if got := server.currentThemeID(blank); got != theme.System {
		t.Fatalf("currentThemeID(no stored theme) = %q, want %q", got, theme.System)
	}

	signedIn := httptest.NewRequest(http.MethodGet, "/mail", nil)
	signedIn = signedIn.WithContext(context.WithValue(signedIn.Context(), userContextKey, currentUser{User: store.User{Theme: theme.ClassicDark}}))
	if got := server.currentThemeID(signedIn); got != theme.ClassicDark {
		t.Fatalf("currentThemeID(dark reader) = %q, want %q", got, theme.ClassicDark)
	}
}

func TestThemedWebManifestPaintsTheInstalledSplashInTheReadersTheme(t *testing.T) {
	shipped := []byte(`{"name":"rolltop","theme_color":"#f2f0eb","background_color":"#f2f0eb"}`)

	themed, err := themedWebManifest(shipped, theme.ClassicDark)
	if err != nil {
		t.Fatalf("themedWebManifest: %v", err)
	}
	var manifest map[string]any
	if err := json.Unmarshal(themed, &manifest); err != nil {
		t.Fatalf("unmarshal themed manifest: %v", err)
	}
	if manifest["theme_color"] != theme.DarkChrome || manifest["background_color"] != theme.DarkChrome {
		t.Fatalf("dark manifest = %v", manifest)
	}
	if manifest["name"] != "rolltop" {
		t.Fatalf("themed manifest dropped shipped fields: %v", manifest)
	}

	// A manifest has no media queries, so System has to keep what was shipped.
	for _, id := range []string{theme.System, "matrix"} {
		untouched, err := themedWebManifest(shipped, id)
		if err != nil {
			t.Fatalf("themedWebManifest(%q): %v", id, err)
		}
		if string(untouched) != string(shipped) {
			t.Fatalf("themedWebManifest(%q) rewrote the manifest: %s", id, untouched)
		}
	}

	if _, err := themedWebManifest([]byte("not json"), theme.ClassicDark); err == nil {
		t.Fatal("a corrupt manifest should be reported, not silently replaced")
	}
}

// The service worker keeps its static cache keyed by URL only, so a per-user
// manifest must not be in the list it precaches and serves to any session.
func TestServiceWorkerDoesNotCacheThePersonalisedManifest(t *testing.T) {
	raw, err := os.ReadFile("../../frontend/public/sw.js")
	if err != nil {
		t.Fatalf("read service worker: %v", err)
	}
	if bytes.Contains(raw, []byte("manifest.webmanifest")) {
		t.Fatal("sw.js still caches /manifest.webmanifest, which is now per-user")
	}
}

// Credentials are not sent for manifest requests unless the link asks for them,
// and without them the server cannot know whose theme to paint.
func TestFrontendShellRequestsTheManifestWithCredentials(t *testing.T) {
	raw, err := os.ReadFile("../../frontend/index.html")
	if err != nil {
		t.Fatalf("read frontend shell: %v", err)
	}
	if !bytes.Contains(raw, []byte(`rel="manifest"`)) || !bytes.Contains(raw, []byte(`crossorigin="use-credentials"`)) {
		t.Fatalf("frontend/index.html must request the manifest with credentials: %s", raw)
	}
}

func TestHandleWebManifestServesEachReaderTheirOwnThemeAndNeverCachesIt(t *testing.T) {
	ctx := context.Background()
	db, err := storetest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	darkReader, err := db.CreateUser(ctx, "dark-manifest@example.test", "Dark Reader", "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	darkReader, err = db.UpdateUserDisplayPreferences(ctx, darkReader.ID, darkReader.DateLocale, darkReader.DateFormat, theme.ClassicDark)
	if err != nil {
		t.Fatal(err)
	}
	if darkReader.Theme != theme.ClassicDark {
		t.Fatalf("stored theme = %q, want %q", darkReader.Theme, theme.ClassicDark)
	}

	dir := t.TempDir()
	distDir := filepath.Join(dir, frontendDistDir)
	if err := os.MkdirAll(distDir, 0o700); err != nil {
		t.Fatal(err)
	}
	shipped := `{"name":"rolltop","theme_color":"` + theme.LightChrome + `","background_color":"` + theme.LightChrome + `"}`
	if err := os.WriteFile(filepath.Join(distDir, "manifest.webmanifest"), []byte(shipped), 0o600); err != nil {
		t.Fatal(err)
	}
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(wd) })

	server := &Server{store: db, startedAt: time.Now()}

	req := httptest.NewRequest(http.MethodGet, "/manifest.webmanifest", nil)
	req = req.WithContext(context.WithValue(req.Context(), userContextKey, currentUser{User: darkReader}))
	rec := httptest.NewRecorder()
	server.handleWebManifest(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), theme.DarkChrome) {
		t.Fatalf("dark reader got a light manifest: %s", rec.Body.String())
	}
	if got := rec.Header().Get("Cache-Control"); got != "private, no-store" {
		t.Fatalf("cache-control = %q, want private, no-store", got)
	}
	// Vary:* also makes the service worker's Cache.put reject the response, so a
	// personalised manifest cannot linger in a cache keyed only by URL.
	if got := rec.Header().Get("Vary"); got != "*" {
		t.Fatalf("vary = %q, want *", got)
	}

	anon := httptest.NewRecorder()
	server.handleWebManifest(anon, httptest.NewRequest(http.MethodGet, "/manifest.webmanifest", nil))
	if strings.Contains(anon.Body.String(), theme.DarkChrome) {
		t.Fatalf("anonymous request received another reader's theme: %s", anon.Body.String())
	}
	if strings.Contains(anon.Body.String(), darkReader.Email) {
		t.Fatalf("manifest exposed a user: %s", anon.Body.String())
	}
}
