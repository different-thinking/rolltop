// The companion guard to palette_test.go. Contrast rules only hold if every
// colour the app paints comes from a token, so component stylesheets are not
// allowed to name a colour themselves. A literal there is invisible to the
// palette tests and is how the dark theme grew its unreadable panels.

package theme

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Theme files are the one place a colour may be written down. Everything else
// consumes tokens.
var themeStylesheets = map[string]bool{
	"../../frontend/src/styles/mixins/_classic-theme.scss":      true,
	"../../frontend/src/styles/mixins/_classic-dark-theme.scss": true,
	"../../plugins/matrix_theme/themes/matrix/theme.scss":       true,
}

var (
	colourLiteralRE = regexp.MustCompile(`#[0-9a-fA-F]{3,8}\b|\brgba?\(\s*[0-9]`)
	// SVG strokes and the like are colours of an asset, not of the interface.
	literalExemptions = []string{"stroke: #fff"}
)

func componentStylesheets(t *testing.T) []string {
	t.Helper()
	var out []string
	patterns := []string{
		"../../frontend/src/styles/*.scss",
		"../../frontend/src/styles/mixins/*.scss",
		"../../plugins/*/frontend/styles.css",
		"../../plugins/*/frontend/styles/*.scss",
		"../../plugins/*/themes/*/theme.scss",
	}
	for _, pattern := range patterns {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			t.Fatalf("glob %s: %v", pattern, err)
		}
		out = append(out, matches...)
	}
	if len(out) < 15 {
		t.Fatalf("only found %d stylesheets (%v); the globs no longer match the tree", len(out), out)
	}
	return out
}

// Custom properties the app sets from TypeScript rather than in a stylesheet.
// They are still definitions, so a reference to them is not dangling.
var scriptDefinedProperties = []string{
	"--compose-viewport-height",
	"--compose-viewport-top",
	"--pull-distance",
	"--swipe-action-content-opacity",
	"--swipe-action-end-shift",
	"--swipe-action-icon-scale",
	"--swipe-action-label-opacity",
	"--swipe-action-start-shift",
	"--swipe-row-height",
}

var (
	definitionRE = regexp.MustCompile(`(--[a-z0-9-]+)\s*:`)
	referenceRE  = regexp.MustCompile(`var\(\s*(--[a-z0-9-]+)\s*\)`)
)

// TestStylesheetsReferenceOnlyDefinedTokens catches the failure the literal scan
// cannot see: var(--something) where nothing defines --something. It resolves to
// nothing, so the property is dropped and the element inherits or goes
// transparent — silently, and only in whichever theme forgot the token.
func TestStylesheetsReferenceOnlyDefinedTokens(t *testing.T) {
	sheets := componentStylesheets(t)

	defined := map[string]bool{}
	for _, name := range scriptDefinedProperties {
		defined[name] = true
	}
	contents := map[string]string{}
	for _, path := range sheets {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		contents[path] = string(raw)
		for _, match := range definitionRE.FindAllStringSubmatch(contents[path], -1) {
			defined[match[1]] = true
		}
	}

	for _, path := range sheets {
		for number, line := range strings.Split(contents[path], "\n") {
			for _, match := range referenceRE.FindAllStringSubmatch(line, -1) {
				if defined[match[1]] {
					continue
				}
				t.Errorf("%s:%d references %s, which no stylesheet defines: %s",
					filepath.ToSlash(path), number+1, match[1], strings.TrimSpace(line))
			}
		}
	}
}

func TestComponentStylesheetsOnlyUseTokens(t *testing.T) {
	sheets := componentStylesheets(t)
	scannedComponents := 0
	scannedThemes := 0

	for _, path := range sheets {
		if themeStylesheets[filepath.ToSlash(path)] {
			scannedThemes++
			continue
		}
		scannedComponents++
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		for number, line := range strings.Split(string(raw), "\n") {
			if !colourLiteralRE.MatchString(line) {
				continue
			}
			exempt := false
			for _, allowed := range literalExemptions {
				if strings.Contains(line, allowed) {
					exempt = true
					break
				}
			}
			if exempt {
				continue
			}
			t.Errorf("%s:%d writes a colour instead of using a token: %s",
				filepath.ToSlash(path), number+1, strings.TrimSpace(line))
		}
	}

	if scannedThemes != len(themeStylesheets) {
		t.Errorf("scanned %d theme stylesheets, expected %d; the exemption list is stale", scannedThemes, len(themeStylesheets))
	}
	if scannedComponents == 0 {
		t.Error("no component stylesheets were scanned")
	}
}

// TestMessageBodyThemeHasOneSource keeps the message-body iframe CSS from being
// copied again: it lived in both the Go renderer and the PGP plugin, and the two
// copies had already drifted to different colours.
func TestMessageBodyThemeHasOneSource(t *testing.T) {
	owners := []string{"../../frontend/src/lib/emailDocumentTheme.ts"}
	copies := []string{
		"../../backend/web/email_document.go",
		"../../plugins/client_side_pgp/frontend/crypto/pgp.ts",
	}

	for _, path := range owners {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		if !strings.Contains(string(raw), "data-rolltop-theme") {
			t.Errorf("%s no longer owns the message-body theme rules", path)
		}
	}
	for _, path := range copies {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		if strings.Contains(string(raw), "data-rolltop-theme") {
			t.Errorf("%s carries its own copy of the message-body theme rules; they belong in %s", path, owners[0])
		}
	}
}
