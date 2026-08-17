package imapclient

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap"

	"rolltop/backend/store"
	"rolltop/backend/syncer"
)

// The cutoff has to reach the server. Filtering locally would still download
// every body, which is the cost the setting exists to avoid.
func TestSyncStartLimitsTheSearchToMessagesAfterTheCutoff(t *testing.T) {
	cutoff := time.Date(2024, time.March, 1, 12, 30, 0, 0, time.UTC)
	criteria := limitToSyncStart(imap.NewSearchCriteria(), store.MailAccount{SyncStartAt: cutoff})
	if !criteria.Since.Equal(cutoff) {
		t.Fatalf("search SINCE = %s, want %s", criteria.Since, cutoff)
	}
}

// Every account that existed before this field carries the zero time, and they
// have already paid for their full initial sync.
func TestSyncStartLeavesAccountsWithoutACutoffUnfiltered(t *testing.T) {
	criteria := limitToSyncStart(imap.NewSearchCriteria(), store.MailAccount{})
	if !criteria.Since.IsZero() {
		t.Fatalf("search SINCE = %s, want no date filter", criteria.Since)
	}
}

// The helper is only useful if the fetch path actually calls it, so this drives
// a real FetchMailbox and reads the SEARCH command off the wire.
func TestFetchMailboxSendsTheCutoffToTheServer(t *testing.T) {
	server := startFakeIMAPServer(t, "good-token")
	account := server.account(t)
	account.SyncStartAt = time.Date(2024, time.March, 1, 0, 0, 0, 0, time.UTC)
	fetcher := &Fetcher{Tokens: &stubTokens{tokens: []string{"good-token"}}}
	err := fetcher.FetchMailbox(context.Background(), account, "INBOX", 0,
		func(syncer.FetchedMessage) error { return nil })
	if err != nil {
		t.Fatalf("fetch mailbox: %v", err)
	}
	server.mu.Lock()
	searches := append([]string(nil), server.searches...)
	server.mu.Unlock()
	if len(searches) != 1 {
		t.Fatalf("searches = %v, want exactly one", searches)
	}
	sent := strings.ToUpper(strings.ReplaceAll(searches[0], `"`, ""))
	if !strings.Contains(sent, "SINCE 1-MAR-2024") {
		t.Fatalf("search %q does not carry the cutoff", searches[0])
	}
}

// A cutoff applied to the body fetch but not to reconciliation would leave the
// mailbox looking permanently incomplete, and repair would re-request the same
// pre-cutoff UIDs on every run.
func TestSyncStartAppliesToReconciliationAndFlagSearchesToo(t *testing.T) {
	cutoff := time.Date(2024, time.March, 1, 0, 0, 0, 0, time.UTC)
	account := store.MailAccount{SyncStartAt: cutoff}
	for name, criteria := range map[string]*imap.SearchCriteria{
		"reconcile": func() *imap.SearchCriteria {
			c := imap.NewSearchCriteria()
			c.Uid = new(imap.SeqSet)
			c.Uid.AddRange(1, 0)
			return c
		}(),
		"seen": func() *imap.SearchCriteria {
			c := imap.NewSearchCriteria()
			c.WithFlags = []string{imap.SeenFlag}
			return c
		}(),
		"flagged": func() *imap.SearchCriteria {
			c := imap.NewSearchCriteria()
			c.WithFlags = []string{imap.FlaggedFlag}
			return c
		}(),
	} {
		if limited := limitToSyncStart(criteria, account); !limited.Since.Equal(cutoff) {
			t.Fatalf("%s search SINCE = %s, want %s", name, limited.Since, cutoff)
		}
	}
}
