// File overview: What an account-wide search rebuild says when it cannot start.
//
// The rebuild reserves every search-visible folder at once, so it is refused by
// far more than "a sync is running": one busy folder out of thirty refuses the
// whole account, and the tenant-wide recovery gate refuses it while nothing is
// running at all. These tests pin the three answers apart, because the operator
// reading them has to choose between waiting a minute and looking somewhere
// else entirely.

package syncer

import (
	"context"
	"testing"

	"rolltop/backend/store"
)

func TestAccountSearchRebuildBlockReasonNamesTheFolderAndTheWorkHoldingIt(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runner := NewRunnerWithContext(ctx, &Service{})
	const userID, accountID = 3, 7
	// Every reason carries its own next step, so an idle runner has to say that
	// the cause has cleared rather than leave the caller to append "try again".
	const idleReason = "Whatever held it has since been released — press rebuild again."
	mailboxes := []store.Mailbox{
		{AccountID: accountID, Name: "INBOX"},
		{AccountID: accountID, Name: "[Gmail]/All Mail"},
	}

	if reason := runner.AccountSearchRebuildBlockReason(userID, accountID, mailboxes); reason != idleReason {
		t.Fatalf("idle runner reason = %q", reason)
	}

	reserveForTest(t, runner, userID, accountID, "[Gmail]/All Mail", runnerWorkMailboxSync)
	want := `Folder sync is already running for the folder "[Gmail]/All Mail". Follow it in Activity, then try again.`
	if reason := runner.AccountSearchRebuildBlockReason(userID, accountID, mailboxes); reason != want {
		t.Fatalf("reserved folder reason = %q, want %q", reason, want)
	}

	// The one folder another tenant's rebuild would have to name is not this
	// tenant's, so the reservation must not leak across the user id.
	if reason := runner.AccountSearchRebuildBlockReason(userID+1, accountID, mailboxes); reason != idleReason {
		t.Fatalf("other tenant reason = %q", reason)
	}

	releaseForTest(t, runner, userID, accountID, "[Gmail]/All Mail")
	reserveForTest(t, runner, userID, accountID, "INBOX", runnerWorkMailboxSearchMaintenance)
	want = `Search index rebuild is already running for the folder "INBOX". Follow it in Activity, then try again.`
	if reason := runner.AccountSearchRebuildBlockReason(userID, accountID, mailboxes); reason != want {
		t.Fatalf("rebuild-in-flight reason = %q, want %q", reason, want)
	}
}

// The recovery gate is the refusal an operator cannot wait out by watching
// Activity: nothing is reserved, nothing is listed, and every rebuild is
// refused until recovery clears. Reporting it as a running sync is the one
// answer that sends them to look at the wrong page.
func TestAccountSearchRebuildBlockReasonSeparatesRecoveryFromRunningWork(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runner := NewRunnerWithContext(ctx, &Service{})
	const userID, accountID = 11, 4
	mailboxes := []store.Mailbox{{AccountID: accountID, Name: "INBOX"}}

	runner.mu.Lock()
	runner.generationRecoveryUsers[userID] = true
	runner.mu.Unlock()

	// Keyed on the user, so it must not claim to be about one mail server: the
	// admin card repeats the reason once per server a tenant owns.
	want := "Folder recovery is still pending for this user, and it holds every mail server until it finishes."
	if reason := runner.AccountSearchRebuildBlockReason(userID, accountID, mailboxes); reason != want {
		t.Fatalf("gated reason = %q, want %q", reason, want)
	}

	// A stopping server refuses before it looks at any of that, and says so:
	// trying again is pointless until it is back.
	cancel()
	if reason := runner.AccountSearchRebuildBlockReason(userID, accountID, mailboxes); reason != "The server is shutting down." {
		t.Fatalf("stopping reason = %q", reason)
	}
}

func reserveForTest(t *testing.T, runner *Runner, userID, accountID int64, mailbox, kind string) {
	t.Helper()
	keys := accountMailboxKeys(userID, accountID, []string{mailbox})
	runner.mu.Lock()
	defer runner.mu.Unlock()
	for _, key := range keys {
		runner.mailboxRunning[key] = true
	}
	runner.startMailboxWorkActivitiesLocked(userID, accountID, []string{mailbox}, keys, kind)
}

func releaseForTest(t *testing.T, runner *Runner, userID, accountID int64, mailbox string) {
	t.Helper()
	keys := accountMailboxKeys(userID, accountID, []string{mailbox})
	runner.mu.Lock()
	defer runner.mu.Unlock()
	for _, key := range keys {
		delete(runner.mailboxRunning, key)
	}
	runner.finishMailboxWorkActivitiesLocked(keys)
}
