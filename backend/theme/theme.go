// File overview: Theme identity shared by the user store, the bootstrap API and
// the SPA shell. The server knows the built-in themes well enough to paint the
// first frame correctly; anything a plugin contributes is corrected by the
// client once it has the stylesheet.

package theme

import "strings"

// Built-in theme identifiers. System is not a palette of its own: it means the
// document carries no theme marker, so the stylesheet follows the reader's
// operating system through prefers-color-scheme.
const (
	System      = "system"
	Classic     = "classic"
	ClassicDark = "classic_dark"
)

// Chrome colours mirror --chrome in the matching theme mixin under
// frontend/src/styles/mixins/. Browsers tint their own surrounding UI with
// them, so a mismatch shows up as a seam above the page.
const (
	LightChrome = "#f2f0eb"
	DarkChrome  = "#10161f"
)

// Normalize maps a stored or submitted theme identifier onto a known one.
// Unknown identifiers are left to the caller to validate against the plugin
// themes that are actually installed.
func Normalize(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case System, "auto":
		return System
	case Classic:
		return Classic
	case ClassicDark, "classic-dark":
		return ClassicDark
	default:
		return ""
	}
}

// DocumentMarker is the value for the document's data-theme attribute. It is
// empty for System and for plugin themes the server cannot resolve, and an
// absent attribute is exactly what makes the stylesheet follow the system.
func DocumentMarker(id string) string {
	switch id {
	case Classic, ClassicDark:
		return id
	default:
		return ""
	}
}

// ChromeColors returns the colour a browser should tint its chrome with for
// this theme. A System theme has no single answer, so both are returned and
// the caller is expected to publish them behind prefers-color-scheme. Themes
// the server does not know return ok=false: the client sets the colour from
// the stylesheet once it has loaded.
func ChromeColors(id string) (light string, dark string, systemDependent bool, ok bool) {
	switch id {
	case Classic:
		return LightChrome, LightChrome, false, true
	case ClassicDark:
		return DarkChrome, DarkChrome, false, true
	case System, "":
		return LightChrome, DarkChrome, true, true
	default:
		return "", "", false, false
	}
}
