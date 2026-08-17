package mailparse

import (
	"net/mail"
	"strings"
	"testing"
)

func TestCategorizePrefersTheMoreSpecificHeaderClaim(t *testing.T) {
	cases := []struct {
		name   string
		header mail.Header
		from   string
		want   string
	}{
		{
			name:   "plain mail from a person",
			header: mail.Header{"From": []string{"Ada <ada@example.test>"}},
			from:   "ada@example.test",
			want:   CategoryRelevant,
		},
		{
			// A discussion list carries unsubscribe and automation headers too,
			// so List-Post has to win or every forum reads as a newsletter.
			name: "discussion list wins over its own unsubscribe header",
			header: mail.Header{
				"List-Id":          []string{"<golang-nuts.googlegroups.com>"},
				"List-Post":        []string{"<mailto:golang-nuts@googlegroups.com>"},
				"List-Unsubscribe": []string{"<mailto:leave@googlegroups.com>"},
				"Precedence":       []string{"list"},
			},
			from: "golang-nuts@googlegroups.com",
			want: CategoryForums,
		},
		{
			name:   "read-only list is a newsletter, not a forum",
			header: mail.Header{"List-Id": []string{"<news.example.test>"}, "List-Post": []string{"NO"}},
			from:   "news@example.test",
			want:   CategoryNewsletters,
		},
		{
			// RFC 2369's own example spells the read-only value with a trailing
			// comment, so the comment has to come off before comparing.
			name: "read-only list with the RFC's trailing comment is still a newsletter",
			header: mail.Header{
				"List-Id":   []string{"<news.example.test>"},
				"List-Post": []string{"NO (posting not allowed on this list)"},
			},
			from: "news@example.test",
			want: CategoryNewsletters,
		},
		{
			name:   "unsubscribe route alone is a newsletter",
			header: mail.Header{"List-Unsubscribe": []string{"<https://example.test/unsub>"}},
			from:   "marketing@example.test",
			want:   CategoryNewsletters,
		},
		{
			name:   "auto-submitted marks a notification",
			header: mail.Header{"Auto-Submitted": []string{"auto-generated"}},
			from:   "billing@example.test",
			want:   CategoryNotifications,
		},
		{
			// "no" is the sender stating a human wrote it, which is the one
			// Auto-Submitted value that must not trip the automation rule.
			name:   "auto-submitted no leaves mail relevant",
			header: mail.Header{"Auto-Submitted": []string{"no"}},
			from:   "ada@example.test",
			want:   CategoryRelevant,
		},
		{
			name:   "bulk precedence marks a notification",
			header: mail.Header{"Precedence": []string{"bulk"}},
			from:   "receipts@example.test",
			want:   CategoryNotifications,
		},
		{
			name:   "no-reply sender is a notification without any header help",
			header: mail.Header{},
			from:   "Shop <no-reply@example.test>",
			want:   CategoryNotifications,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Categorize(tc.header, tc.from); got != tc.want {
				t.Fatalf("Categorize() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestBareAddressIsTheOneKeyClassificationAndCorrectionsShare(t *testing.T) {
	// Both the classifier's address fallback and the per-sender correction key
	// on this, so a value either works for both or for neither.
	cases := map[string]string{
		`"Shop" <Offers@Example.test>`: "offers@example.test",
		"ada@example.test":             "ada@example.test",
		"  ADA@example.test  ":         "ada@example.test",
		"Two <a@x.test>, <b@x.test>":   "a@x.test",
		// An unterminated bracket is malformed but still names a sender;
		// rejecting it would leave that sender impossible to file.
		"Jane Doe <jane@x.test": "jane@x.test",
		"":                      "",
		"not-an-address":        "",
	}
	for input, want := range cases {
		if got := BareAddress(input); got != want {
			t.Fatalf("BareAddress(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestCategorizeAddressKeepsUnrecognizedSendersRelevant(t *testing.T) {
	cases := map[string]string{
		"ada@example.test":                 CategoryRelevant,
		"":                                 CategoryRelevant,
		"not-an-address":                   CategoryRelevant,
		"noreply@example.test":             CategoryNotifications,
		"do.not.reply@example.test":        CategoryNotifications,
		"MAILER-DAEMON@example.test":       CategoryNotifications,
		"Alerts <notifications@corp.test>": CategoryNotifications,
	}
	for from, want := range cases {
		if got := CategorizeAddress(from); got != want {
			t.Fatalf("CategorizeAddress(%q) = %q, want %q", from, got, want)
		}
	}
}

func TestCategorizeReaderFallsBackWhenHeadersCannotBeRead(t *testing.T) {
	raw := "List-Id: <news.example.test>\r\nFrom: news@example.test\r\n\r\nbody\r\n"
	if got := CategorizeReader(strings.NewReader(raw), "news@example.test"); got != CategoryNewsletters {
		t.Fatalf("CategorizeReader(list message) = %q, want %q", got, CategoryNewsletters)
	}
	// A truncated or absent body must still yield a category so the backfill
	// never selects the same row again.
	if got := CategorizeReader(strings.NewReader("not a message"), "no-reply@example.test"); got != CategoryNotifications {
		t.Fatalf("CategorizeReader(garbage) = %q, want %q", got, CategoryNotifications)
	}
	if got := CategorizeReader(nil, "ada@example.test"); got != CategoryRelevant {
		t.Fatalf("CategorizeReader(nil) = %q, want %q", got, CategoryRelevant)
	}
}

func TestCategoryRegistryIsTheOnlyPlaceTheSetIsDefined(t *testing.T) {
	registry := CategoryRegistry()
	names := Categories()
	if len(registry) != len(names) {
		t.Fatalf("registry has %d entries but Categories() has %d", len(registry), len(names))
	}
	for i, entry := range registry {
		if entry.Name != names[i] {
			t.Fatalf("registry entry %d = %q, Categories()[%d] = %q", i, entry.Name, i, names[i])
		}
		if entry.Label == "" || entry.Icon == "" {
			t.Fatalf("category %q has no display text: %+v", entry.Name, entry)
		}
		if !ValidCategory(entry.Name) {
			t.Fatalf("category %q is listed but rejected by ValidCategory", entry.Name)
		}
	}
	if ValidCategory("everything") || ValidCategory("") {
		t.Fatal("ValidCategory accepted a name that is not in the registry")
	}
}

func TestParseFillsTheCategoryFromTheMessageHeaders(t *testing.T) {
	raw := []byte("From: Newsletter <news@example.test>\r\n" +
		"Subject: Weekly\r\n" +
		"List-Unsubscribe: <https://example.test/unsub>\r\n" +
		"Content-Type: text/plain\r\n\r\nhello\r\n")
	parsed, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Category != CategoryNewsletters {
		t.Fatalf("parsed category = %q, want %q", parsed.Category, CategoryNewsletters)
	}
}
