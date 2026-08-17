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
