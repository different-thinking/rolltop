package imapclient

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap"

	"rolltop/backend/store"
	"rolltop/backend/syncer"
	"rolltop/backend/xoauth2"
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
	fetcher := &Fetcher{Tokens: &xoauth2.StubTokenSource{Tokens: []string{"good-token"}}}
	err := fetcher.FetchMailbox(context.Background(), account, "INBOX", 0,
		func(syncer.FetchedMessage) error { return nil })
	if err != nil {
		t.Fatalf("fetch mailbox: %v", err)
	}
	searches := server.searchCommands()
	if len(searches) != 1 {
		t.Fatalf("searches = %v, want exactly one", searches)
	}
	if !carriesCutoff(searches[0], "1-MAR-2024") {
		t.Fatalf("search %q does not carry the cutoff", searches[0])
	}
}

// Repair downloads whatever the snapshot reports as missing, so the snapshot
// has to hand it a list the cutoff already applies to. Without this the first
// "Sync now" pulls the entire pre-cutoff history the setting exists to skip.
func TestSnapshotReportsASeparateFetchableListUnderACutoff(t *testing.T) {
	server := startFakeIMAPServer(t, "good-token")
	server.setSearchResults([]uint32{1, 2, 3, 4}, []uint32{3, 4})
	account := server.account(t)
	account.SyncStartAt = time.Date(2024, time.March, 1, 0, 0, 0, 0, time.UTC)
	fetcher := &Fetcher{Tokens: &xoauth2.StubTokenSource{Tokens: []string{"good-token"}}}
	snapshot, err := fetcher.SnapshotMailboxUIDs(context.Background(), account, "INBOX")
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	// Reconciliation deletes local rows missing from this list, so it must stay
	// the server's whole mailbox.
	if len(snapshot.UIDs) != 4 {
		t.Fatalf("snapshot UIDs = %v, want all four", snapshot.UIDs)
	}
	if len(snapshot.Fetchable()) != 2 {
		t.Fatalf("fetchable UIDs = %v, want only the two after the cutoff", snapshot.Fetchable())
	}
	searches := server.searchCommands()
	if len(searches) != 2 {
		t.Fatalf("searches = %v, want one unfiltered and one limited", searches)
	}
	if carriesCutoff(searches[0], "1-MAR-2024") {
		t.Fatalf("the reconcile search carried the cutoff: %q", searches[0])
	}
	if !carriesCutoff(searches[1], "1-MAR-2024") {
		t.Fatalf("the fetchable search did not carry the cutoff: %q", searches[1])
	}
}

// Without a cutoff there is nothing to limit, and a second search would double
// the reconcile cost for every account that has been running since before the
// setting existed.
func TestSnapshotIssuesOneSearchWithoutACutoff(t *testing.T) {
	server := startFakeIMAPServer(t, "good-token")
	server.setSearchResults([]uint32{1, 2}, nil)
	fetcher := &Fetcher{Tokens: &xoauth2.StubTokenSource{Tokens: []string{"good-token"}}}
	snapshot, err := fetcher.SnapshotMailboxUIDs(context.Background(), server.account(t), "INBOX")
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if searches := server.searchCommands(); len(searches) != 1 {
		t.Fatalf("searches = %v, want exactly one", searches)
	}
	if snapshot.FetchableUIDs != nil {
		t.Fatalf("fetchable list = %v, want nil so callers fall back to the full list", snapshot.FetchableUIDs)
	}
	if len(snapshot.Fetchable()) != 2 {
		t.Fatalf("Fetchable() = %v, want the full list", snapshot.Fetchable())
	}
}

// Reconciliation removes local messages missing from the server's list and flag
// sync marks everything outside its result unread, so a cutoff on either would
// destroy mail that was mirrored before the cutoff was chosen. This asserts the
// searches stay unfiltered rather than trusting the call sites to stay that way.
func TestDeleteAndFlagSearchesNeverCarryTheCutoff(t *testing.T) {
	cutoff := time.Date(2024, time.March, 1, 0, 0, 0, 0, time.UTC)
	server := startFakeIMAPServer(t, "good-token")
	account := server.account(t)
	account.SyncStartAt = cutoff
	fetcher := &Fetcher{Tokens: &xoauth2.StubTokenSource{Tokens: []string{"good-token"}}}
	ctx := context.Background()
	if _, err := fetcher.UIDs(ctx, account, "INBOX"); err != nil {
		t.Fatalf("reconcile UIDs: %v", err)
	}
	if _, err := fetcher.SeenUIDs(ctx, account, "INBOX"); err != nil {
		t.Fatalf("seen UIDs: %v", err)
	}
	if _, err := fetcher.FlaggedUIDs(ctx, account, "INBOX"); err != nil {
		t.Fatalf("flagged UIDs: %v", err)
	}
	if _, _, err := fetcher.SeenUIDsWithUIDValidity(ctx, account, "INBOX", 1); err != nil {
		t.Fatalf("generation-bound seen UIDs: %v", err)
	}
	if _, _, err := fetcher.FlaggedUIDsWithUIDValidity(ctx, account, "INBOX", 1); err != nil {
		t.Fatalf("generation-bound flagged UIDs: %v", err)
	}
	searches := server.searchCommands()
	if len(searches) != 5 {
		t.Fatalf("searches = %v, want five", searches)
	}
	for _, search := range searches {
		if carriesCutoff(search, "1-MAR-2024") {
			t.Fatalf("a delete or flag search carried the cutoff: %q", search)
		}
	}
}

func carriesCutoff(command, date string) bool {
	return strings.Contains(strings.ToUpper(strings.ReplaceAll(command, `"`, "")), "SINCE "+date)
}
