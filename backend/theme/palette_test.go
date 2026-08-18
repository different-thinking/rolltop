// Design-system guard rails. The dark theme used to be a hand-maintained list
// of per-component overrides, and every colour that was forgotten shipped as
// unreadable text. These tests hold the two properties that prevented it:
// themes define the same tokens, and the pairs the app actually paints on top of
// each other stay above the WCAG thresholds.

package theme

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

const (
	repoRelativeStyles = "../../frontend/src/styles"
	matrixThemePath    = "../../plugins/matrix_theme/themes/matrix/theme.scss"

	// WCAG 2.1 AA: body text needs 4.5:1, large text and UI parts need 3:1.
	textContrast = 4.5
	uiContrast   = 3.0

	// Sender-initial avatar hues, mirrored by senderAvatarHueCount in
	// frontend/src/features/mail/MailViews.tsx and the .avatar-hue-N rules in
	// frontend/src/styles/_message-list.scss.
	avatarHueCount = 8
)

var (
	tokenRE = regexp.MustCompile(`(?m)^\s*(--[a-z0-9-]+)\s*:\s*([^;]+);`)
	mixRE   = regexp.MustCompile(`^color-mix\(in srgb,\s*var\((--[a-z0-9-]+)\)\s*(\d+)%,\s*(.+)\)$`)
	varRE   = regexp.MustCompile(`^var\((--[a-z0-9-]+)\)$`)
)

type palette struct {
	name   string
	tokens map[string]string
}

func loadPalette(t *testing.T, path, name string) palette {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	tokens := map[string]string{}
	for _, match := range tokenRE.FindAllStringSubmatch(string(raw), -1) {
		tokens[match[1]] = strings.TrimSpace(match[2])
	}
	if len(tokens) == 0 {
		t.Fatalf("no tokens found in %s", path)
	}
	return palette{name: name, tokens: tokens}
}

func classicPalettes(t *testing.T) (light palette, dark palette) {
	t.Helper()
	return loadPalette(t, filepath.Join(repoRelativeStyles, "mixins", "_classic-theme.scss"), "classic"),
		loadPalette(t, filepath.Join(repoRelativeStyles, "mixins", "_classic-dark-theme.scss"), "classic_dark")
}

// resolve turns a token value into an opaque colour, following var() aliases and
// flattening the color-mix() and rgba() forms the palettes use. Anything it
// cannot reduce to a colour is reported so a test can skip it knowingly rather
// than silently pass.
func (p palette) resolve(value string, depth int) ([3]float64, bool) {
	if depth > 8 {
		return [3]float64{}, false
	}
	value = strings.TrimSpace(value)
	if match := varRE.FindStringSubmatch(value); match != nil {
		next, ok := p.tokens[match[1]]
		if !ok {
			return [3]float64{}, false
		}
		return p.resolve(next, depth+1)
	}
	if match := mixRE.FindStringSubmatch(value); match != nil {
		top, ok := p.resolve("var("+match[1]+")", depth+1)
		if !ok {
			return [3]float64{}, false
		}
		percent, err := strconv.ParseFloat(match[2], 64)
		if err != nil {
			return [3]float64{}, false
		}
		base := strings.TrimSpace(match[3])
		if base == "transparent" {
			return [3]float64{}, false
		}
		under, ok := p.resolve(base, depth+1)
		if !ok {
			return [3]float64{}, false
		}
		return over(top, percent/100, under), true
	}
	if strings.HasPrefix(value, "rgba(") || strings.HasPrefix(value, "rgb(") {
		return [3]float64{}, false
	}
	return parseHex(value)
}

func (p palette) mustResolve(t *testing.T, token string) [3]float64 {
	t.Helper()
	value, ok := p.tokens[token]
	if !ok {
		t.Fatalf("%s: token %s is not defined", p.name, token)
	}
	colour, ok := p.resolve(value, 0)
	if !ok {
		t.Fatalf("%s: token %s (%q) does not resolve to an opaque colour", p.name, token, value)
	}
	return colour
}

func parseHex(value string) ([3]float64, bool) {
	value = strings.TrimPrefix(strings.TrimSpace(value), "#")
	if len(value) == 3 {
		value = string([]byte{value[0], value[0], value[1], value[1], value[2], value[2]})
	}
	if len(value) != 6 {
		return [3]float64{}, false
	}
	var out [3]float64
	for i := 0; i < 3; i++ {
		component, err := strconv.ParseUint(value[i*2:i*2+2], 16, 8)
		if err != nil {
			return [3]float64{}, false
		}
		out[i] = float64(component)
	}
	return out, true
}

func over(top [3]float64, alpha float64, under [3]float64) [3]float64 {
	var out [3]float64
	for i := range out {
		out[i] = top[i]*alpha + under[i]*(1-alpha)
	}
	return out
}

func relativeLuminance(colour [3]float64) float64 {
	channel := func(v float64) float64 {
		v /= 255
		if v <= 0.04045 {
			return v / 12.92
		}
		return math.Pow((v+0.055)/1.055, 2.4)
	}
	return 0.2126*channel(colour[0]) + 0.7152*channel(colour[1]) + 0.0722*channel(colour[2])
}

func contrast(a, b [3]float64) float64 {
	high, low := relativeLuminance(a), relativeLuminance(b)
	if low > high {
		high, low = low, high
	}
	return (high + 0.05) / (low + 0.05)
}

// TestThemesDefineTheSameTokens is the guard that matters most: a token added to
// one theme and forgotten in another is exactly how the dark theme ended up with
// unreadable panels, because the component then fell back to the other theme's
// value or to nothing at all.
func TestThemesDefineTheSameTokens(t *testing.T) {
	light, dark := classicPalettes(t)
	matrix := loadPalette(t, matrixThemePath, "matrix")

	for _, other := range []palette{dark, matrix} {
		for token := range light.tokens {
			if _, ok := other.tokens[token]; !ok {
				t.Errorf("%s is missing token %s, which %s defines", other.name, token, light.name)
			}
		}
		for token := range other.tokens {
			if _, ok := light.tokens[token]; !ok {
				// A theme may add tokens of its own (matrix has --font-heading),
				// but only after the shared palette covers it everywhere.
				if strings.HasPrefix(token, "--font-") {
					continue
				}
				t.Errorf("%s defines token %s that %s does not", other.name, token, light.name)
			}
		}
	}
}

// TestGoChromeConstantsMatchTheStylesheets keeps the colours the server stamps
// into index.html in step with the palettes. If they drift, the browser paints
// its chrome one colour and the page another, with a visible seam between them.
func TestGoChromeConstantsMatchTheStylesheets(t *testing.T) {
	light, dark := classicPalettes(t)
	if got := light.tokens["--chrome"]; got != LightChrome {
		t.Errorf("classic --chrome is %q but theme.LightChrome is %q", got, LightChrome)
	}
	if got := dark.tokens["--chrome"]; got != DarkChrome {
		t.Errorf("classic_dark --chrome is %q but theme.DarkChrome is %q", got, DarkChrome)
	}
}

func TestPaletteContrastMeetsWCAG(t *testing.T) {
	light, dark := classicPalettes(t)

	type pair struct {
		what      string
		ink       string
		ground    string
		threshold float64
	}
	pairs := []pair{
		{"body text on a panel", "--text", "--surface", textContrast},
		{"body text on the page", "--text", "--bg", textContrast},
		{"body text in a field", "--text", "--field", textContrast},
		{"body text on a raised surface", "--text", "--surface-raised", textContrast},
		{"recessed text on a panel", "--text-soft", "--surface", textContrast},
		{"secondary text on a panel", "--muted", "--surface", textContrast},
		{"secondary text on a subtle surface", "--muted", "--surface-subtle", textContrast},
		{"secondary text on the page", "--muted", "--bg", textContrast},
		{"read row text", "--muted", "--row-read", textContrast},
		{"unread row text", "--text", "--row-unread", textContrast},
		{"button label on the accent", "--on-accent", "--accent", textContrast},
		{"label on the primary action", "--on-action", "--action", textContrast},
		{"label on an inked chip", "--on-ink", "--surface-ink", textContrast},
		{"the primary action against the sidebar", "--action", "--surface", uiContrast},
		{"success text on a panel", "--ok", "--surface", textContrast},
		{"info text on a panel", "--info", "--surface", textContrast},
		{"warning text on a panel", "--warning", "--surface", textContrast},
		{"danger text on a panel", "--danger", "--surface", textContrast},
		{"label on a success fill", "--on-ok", "--ok", textContrast},
		{"label on an info fill", "--on-info", "--info", textContrast},
		{"label on a warning fill", "--on-warning", "--warning", textContrast},
		{"label on a danger fill", "--on-danger", "--danger", textContrast},
		{"search highlight text", "--on-highlight", "--surface", textContrast},
		{"the focus ring against a panel", "--focus", "--surface", uiContrast},
		// The star is a UI graphic rather than text, so it answers to the 3:1
		// threshold — on both row grounds, because a list paints it on each.
		// Gmail's own yellow sits at 1.9:1 on a white row, which is why this
		// pair is worth holding.
		{"a starred message on an unread row", "--star", "--row-unread", uiContrast},
		{"a starred message on a read row", "--star", "--row-read", uiContrast},
	}
	// Avatar initials are small text on a coloured chip, so every hue carries
	// the text threshold. Generating the pairs is what catches a ninth hue
	// added to one theme and forgotten in another.
	for hue := 0; hue < avatarHueCount; hue++ {
		pairs = append(pairs, pair{
			what:      fmt.Sprintf("an avatar initial on hue %d", hue),
			ink:       "--on-avatar",
			ground:    fmt.Sprintf("--avatar-%d", hue),
			threshold: textContrast,
		})
	}

	for _, p := range []palette{light, dark} {
		for _, pr := range pairs {
			ink := p.mustResolve(t, pr.ink)
			ground := p.mustResolve(t, pr.ground)
			if ratio := contrast(ink, ground); ratio < pr.threshold {
				t.Errorf("%s: %s (%s on %s) is %.2f:1, want at least %.1f:1",
					p.name, pr.what, pr.ink, pr.ground, ratio, pr.threshold)
			}
		}
	}
}

// TestSwipeActionContrastMeetsWCAG covers the one place where a component builds
// its own surface: each swipe action washes its hue over the row at 20% and puts
// the same hue on top as its label.
func TestSwipeActionContrastMeetsWCAG(t *testing.T) {
	light, dark := classicPalettes(t)
	actions := []string{"--swipe-read", "--swipe-unread", "--swipe-snooze", "--swipe-archive", "--swipe-trash"}

	for _, p := range []palette{light, dark} {
		surface := p.mustResolve(t, "--surface")
		onSwipe := p.mustResolve(t, "--on-swipe")
		for _, action := range actions {
			hue := p.mustResolve(t, action)
			quiet := over(hue, 0.20, surface)
			if ratio := contrast(hue, quiet); ratio < textContrast {
				t.Errorf("%s: %s label on its quiet wash is %.2f:1, want at least %.1f:1", p.name, action, ratio, textContrast)
			}
			if ratio := contrast(onSwipe, hue); ratio < textContrast {
				t.Errorf("%s: %s armed label is %.2f:1, want at least %.1f:1", p.name, action, ratio, textContrast)
			}
		}
	}
}

// TestDarkTextAvoidsHalation keeps the dark theme's body text below the
// near-white range. Maximum contrast is not the goal: near-white on a dark
// ground makes glyphs bloom, which is what makes long reading sessions tiring.
func TestDarkTextAvoidsHalation(t *testing.T) {
	_, dark := classicPalettes(t)
	ratio := contrast(dark.mustResolve(t, "--text"), dark.mustResolve(t, "--surface"))
	if ratio > 13 {
		t.Errorf("dark body text is %.2f:1 against a panel, which is bright enough to halate; keep it near 12:1", ratio)
	}
	if ratio < 10 {
		t.Errorf("dark body text is only %.2f:1 against a panel; keep it near 12:1", ratio)
	}
}

func TestPaletteHelpersResolveTheFormsThePalettesUse(t *testing.T) {
	p := palette{name: "fixture", tokens: map[string]string{
		"--white": "#fff",
		"--ink":   "#000000",
		"--alias": "var(--ink)",
		"--wash":  "color-mix(in srgb, var(--ink) 50%, var(--white))",
	}}
	if got, ok := p.resolve("var(--alias)", 0); !ok || got != [3]float64{0, 0, 0} {
		t.Fatalf("alias resolved to %v (%v)", got, ok)
	}
	got, ok := p.resolve("var(--wash)", 0)
	if !ok {
		t.Fatal("color-mix did not resolve")
	}
	for _, component := range got {
		if math.Abs(component-127.5) > 0.5 {
			t.Fatalf("color-mix midpoint resolved to %v", got)
		}
	}
	if _, ok := p.resolve("rgba(1, 2, 3, .5)", 0); ok {
		t.Fatal("translucent values must be reported as unresolvable, not guessed")
	}
	if ratio := contrast([3]float64{255, 255, 255}, [3]float64{0, 0, 0}); fmt.Sprintf("%.0f", ratio) != "21" {
		t.Fatalf("white on black is %.2f:1, want 21:1", ratio)
	}
}
