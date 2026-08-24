package store

import "testing"

// Gmail's label views hold a copy of nearly every message. With one folder per
// message in this data model, syncing them mirrors the whole mailbox twice, so
// they have to start out excluded rather than merely "manual".
func TestGmailLabelViewsDefaultToNeverSyncing(t *testing.T) {
	for _, testCase := range []struct {
		name       string
		role       string
		attributes []string
		want       string
	}{
		{name: "INBOX", role: "inbox", want: "auto"},
		{name: "[Gmail]/All Mail", role: "all", want: "never"},
		{name: "[Gmail]/Important", want: "never"},
		{name: "[Gmail]/Starred", want: "never"},
		{name: "[Google Mail]/All Mail", want: "never"},
		// Localized installs report the attribute even when the name differs,
		// which is why attributes are checked before names.
		{name: "[Gmail]/Alle Nachrichten", role: "all", want: "never"},
		{name: "[Gmail]/Markiert", attributes: []string{"\\Flagged"}, want: "never"},
		{name: "[Gmail]/Wichtig", attributes: []string{"\\Important"}, want: "never"},
		{name: "[Gmail]/Alle Nachrichten", attributes: []string{"\\All"}, want: "never"},
		// A real folder that happens to carry an ordinary role keeps syncing.
		{name: "Rechnungen", attributes: []string{"\\Sent"}, role: "sent", want: "manual"},
		{name: "[Gmail]/Spam", role: "junk", want: "manual"},
		{name: "[Gmail]/Trash", role: "trash", want: "manual"},
		{name: "[Gmail]/Sent Mail", role: "sent", want: "manual"},
		{name: "Projects/2024", want: "manual"},
		// A user folder that merely looks like a label view must not be
		// silently excluded.
		{name: "Important", want: "manual"},
	} {
		if got := defaultMailboxSyncMode(testCase.name, testCase.role, testCase.attributes); got != testCase.want {
			t.Errorf("default sync mode for %q (role %q, attributes %v) = %q, want %q",
				testCase.name, testCase.role, testCase.attributes, got, testCase.want)
		}
	}
}

// resolveInheritedSyncMode is the in-memory twin of EffectiveMailboxSyncMode,
// and it exists so that reporting unsynced folders costs one query for the
// whole table rather than one per parent per candidate. It has to answer the
// way the query-per-parent version did.
func TestResolveInheritedSyncModeMatchesTheEffectiveMode(t *testing.T) {
	modes := map[string]string{
		"INBOX":                    "auto",
		"Archive":                  "manual",
		"Archive/2025":             "inherit",
		"Archive/2025/Rechnungen":  "inherit",
		"Projekte":                 "inherit",
		"Projekte/Kunde":           "inherit",
		"[Gmail]/Alle Nachrichten": "never",
		"Sent":                     "manual",
	}
	for _, c := range []struct {
		name string
		want string
	}{
		{"INBOX", "auto"},
		{"Sent", "manual"},
		{"[Gmail]/Alle Nachrichten", "never"},
		// The nearest parent with a mode of its own decides, however deep.
		{"Archive/2025", "manual"},
		{"Archive/2025/Rechnungen", "manual"},
		// A chain that names nothing lands on auto, exactly as walking it
		// through the store does when every lookup misses.
		{"Projekte", "auto"},
		{"Projekte/Kunde", "auto"},
		// A folder absent from the map is auto rather than a zero value that
		// would read as some other mode.
		{"Unbekannt", "auto"},
	} {
		if got := resolveInheritedSyncMode(c.name, modes); got != c.want {
			t.Errorf("resolveInheritedSyncMode(%q) = %q, want %q", c.name, got, c.want)
		}
	}
}
