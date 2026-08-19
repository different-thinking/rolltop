package sqlident

import "testing"

func TestQuote(t *testing.T) {
	cases := map[string]string{
		"messages":            `"messages"`,
		"order":               `"order"`,
		`weird"name`:          `"weird""name"`,
		"rolltop_test_x_1_2":  `"rolltop_test_x_1_2"`,
		`""`:                  `""""""`,
		"":                    `""`,
		"MixedCase":           `"MixedCase"`,
		"with space":          `"with space"`,
		"trailing_underscore": `"trailing_underscore"`,
	}
	for name, want := range cases {
		if got := Quote(name); got != want {
			t.Errorf("Quote(%q) = %s, want %s", name, got, want)
		}
	}
}
