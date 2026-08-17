package theme

import "testing"

func TestNormalizeAcceptsKnownThemesAndRejectsOthers(t *testing.T) {
	cases := map[string]string{
		"system":        System,
		"  System  ":    System,
		"auto":          System,
		"classic":       Classic,
		"classic_dark":  ClassicDark,
		"classic-dark":  ClassicDark,
		"CLASSIC_DARK":  ClassicDark,
		"matrix":        "",
		"":              "",
		"not-a-theme-x": "",
	}
	for input, want := range cases {
		if got := Normalize(input); got != want {
			t.Fatalf("Normalize(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestDocumentMarkerIsEmptyForSystemSoTheStylesheetFollowsTheOS(t *testing.T) {
	if got := DocumentMarker(System); got != "" {
		t.Fatalf("DocumentMarker(System) = %q, want empty so :root:not([data-theme]) matches", got)
	}
	if got := DocumentMarker("matrix"); got != "" {
		t.Fatalf("DocumentMarker(matrix) = %q, want empty: the server cannot resolve plugin themes", got)
	}
	if got := DocumentMarker(ClassicDark); got != ClassicDark {
		t.Fatalf("DocumentMarker(ClassicDark) = %q, want %q", got, ClassicDark)
	}
}

func TestChromeColorsPairsBothPalettesOnlyForSystem(t *testing.T) {
	light, dark, systemDependent, ok := ChromeColors(System)
	if !ok || !systemDependent || light != LightChrome || dark != DarkChrome {
		t.Fatalf("ChromeColors(System) = %q, %q, %v, %v", light, dark, systemDependent, ok)
	}

	light, dark, systemDependent, ok = ChromeColors(ClassicDark)
	if !ok || systemDependent || light != DarkChrome || dark != DarkChrome {
		t.Fatalf("ChromeColors(ClassicDark) = %q, %q, %v, %v", light, dark, systemDependent, ok)
	}

	if _, _, _, ok = ChromeColors("matrix"); ok {
		t.Fatal("ChromeColors(matrix) reported a colour the server cannot know")
	}
}
