package web

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const manifestShell = `<!doctype html><html><head><link rel="manifest" href="/manifest.webmanifest" crossorigin="use-credentials" /></head><body></body></html>`

func chdirToDist(t *testing.T, files map[string]string) {
	t.Helper()
	dir := t.TempDir()
	distDir := filepath.Join(dir, frontendDistDir)
	if err := os.MkdirAll(distDir, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(distDir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Chdir(dir)
}

// injectWebManifestLink rewrites the shell's mail link byte for byte, so a
// shell that spells it any other way would silently link the mail manifest on
// every calendar page.
func TestFrontendShellCarriesTheManifestLinkTheServerRewrites(t *testing.T) {
	raw, err := os.ReadFile("../../frontend/index.html")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, mailWebManifestLink) {
		t.Fatalf("frontend/index.html no longer carries %s, which injectWebManifestLink rewrites", mailWebManifestLink)
	}
}

func TestWebManifestPathForRouteInstallsTheCalendarFromItsOwnPages(t *testing.T) {
	cases := map[string]string{
		"/calendar":            calendarWebManifestPath,
		"/calendar/2026-10-12": calendarWebManifestPath,
		"/calendarx":           mailWebManifestPath,
		"/mail/relevant":       mailWebManifestPath,
		"/contacts":            mailWebManifestPath,
		"/settings/calendars":  mailWebManifestPath,
		"/":                    mailWebManifestPath,
	}
	for path, want := range cases {
		if got := webManifestPathForRoute(path); got != want {
			t.Fatalf("webManifestPathForRoute(%q) = %q, want %q", path, got, want)
		}
	}
}

func TestHandleAppLinksTheCalendarManifestOnCalendarPages(t *testing.T) {
	chdirToDist(t, map[string]string{"index.html": manifestShell})
	server := &Server{}
	for path, want := range map[string]string{
		"/calendar":            calendarWebManifestPath,
		"/calendar/2026-10-12": calendarWebManifestPath,
		"/contacts":            mailWebManifestPath,
		"/mail/relevant":       mailWebManifestPath,
	} {
		rec := httptest.NewRecorder()
		server.handleApp(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s status = %d body = %s", path, rec.Code, rec.Body.String())
		}
		body := rec.Body.String()
		if strings.Count(body, `rel="manifest"`) != 1 || !strings.Contains(body, `href="`+want+`"`) {
			t.Fatalf("%s links the wrong manifest, want %s: %s", path, want, body)
		}
	}
}

func TestHandlerServesBothManifestsAndNoOtherFileThroughThem(t *testing.T) {
	chdirToDist(t, map[string]string{
		"manifest.webmanifest": `{"id":"/mail/relevant"}`,
		"calendar.webmanifest": `{"id":"/calendar"}`,
		"contacts.webmanifest": `{"id":"/contacts"}`,
		"index.html":           manifestShell,
	})
	handler := (&Server{}).Handler()
	for path, want := range map[string]string{
		"/manifest.webmanifest": `"/mail/relevant"`,
		"/calendar.webmanifest": `"/calendar"`,
	} {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), want) {
			t.Fatalf("%s = %d %s", path, rec.Code, rec.Body.String())
		}
		if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/manifest+json") {
			t.Fatalf("%s content-type = %q", path, got)
		}
		if got := rec.Header().Get("Cache-Control"); got != "private, no-store" {
			t.Fatalf("%s cache-control = %q", path, got)
		}
	}
	rec := httptest.NewRecorder()
	(&Server{}).handleWebManifest(rec, httptest.NewRequest(http.MethodGet, "/contacts.webmanifest", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("an unlisted manifest was served: %d %s", rec.Code, rec.Body.String())
	}
}

// Browsers tell installed apps apart by manifest id, so the two apps must not
// share one, and the mail app's must stay the id browsers derived from its
// start_url before it was written down - otherwise every existing install would
// become a different app.
func TestShippedManifestsAreTwoAppsWithTheirOwnIcons(t *testing.T) {
	type icon struct {
		Src     string `json:"src"`
		Sizes   string `json:"sizes"`
		Type    string `json:"type"`
		Purpose string `json:"purpose"`
	}
	type manifest struct {
		ID        string `json:"id"`
		StartURL  string `json:"start_url"`
		Scope     string `json:"scope"`
		Icons     []icon `json:"icons"`
		Shortcuts []struct {
			URL   string `json:"url"`
			Icons []icon `json:"icons"`
		} `json:"shortcuts"`
	}
	read := func(name string) manifest {
		raw, err := os.ReadFile(filepath.Join("../../frontend/public", name))
		if err != nil {
			t.Fatal(err)
		}
		var m manifest
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		return m
	}
	mail := read("manifest.webmanifest")
	calendar := read("calendar.webmanifest")

	if mail.ID != mail.StartURL {
		t.Fatalf("mail manifest id %q must equal its start_url %q", mail.ID, mail.StartURL)
	}
	if calendar.ID == mail.ID {
		t.Fatalf("both manifests carry id %q", calendar.ID)
	}
	if webManifestPathForRoute(calendar.StartURL) != calendarWebManifestPath {
		t.Fatalf("calendar start_url %q does not link the calendar manifest", calendar.StartURL)
	}
	if webManifestPathForRoute(mail.StartURL) != mailWebManifestPath {
		t.Fatalf("mail start_url %q does not link the mail manifest", mail.StartURL)
	}
	for name, m := range map[string]manifest{"manifest.webmanifest": mail, "calendar.webmanifest": calendar} {
		var png512, maskable bool
		icons := append([]icon{}, m.Icons...)
		for _, shortcut := range m.Shortcuts {
			icons = append(icons, shortcut.Icons...)
		}
		for _, ic := range icons {
			if _, err := os.Stat(filepath.Join("../../frontend/public", ic.Src)); err != nil {
				t.Fatalf("%s names a missing icon %s", name, ic.Src)
			}
		}
		for _, ic := range m.Icons {
			if ic.Type == "image/png" && ic.Sizes == "512x512" && ic.Purpose == "any" {
				png512 = true
			}
			if ic.Purpose == "maskable" {
				maskable = true
			}
		}
		if !png512 || !maskable {
			t.Fatalf("%s needs a 512px PNG and a maskable icon to install everywhere", name)
		}
	}
	for _, ic := range calendar.Icons {
		if strings.Contains(ic.Src, "mail") {
			t.Fatalf("calendar app uses the mail icon: %s", ic.Src)
		}
	}
}
