package web

import (
	"bytes"
	"os"
	"strings"
	"testing"

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

func TestStartupThemeIDFallsBackToSystemForAnonymousShells(t *testing.T) {
	if got := startupThemeID(map[string]any{}); got != theme.System {
		t.Fatalf("startupThemeID(anonymous) = %q, want %q", got, theme.System)
	}
	if got := startupThemeID(map[string]any{"user": apiUser{Theme: theme.ClassicDark}}); got != theme.ClassicDark {
		t.Fatalf("startupThemeID(user) = %q, want %q", got, theme.ClassicDark)
	}
}
