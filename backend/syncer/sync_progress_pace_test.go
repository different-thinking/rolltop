// File overview: What a run spends on reporting the work, as opposed to doing
// it. A folder being mirrored or repaired walks thousands of messages, and a
// report per message is a row write and a broadcast per message.

package syncer

import (
	"context"
	"testing"

	"rolltop/backend/blob"
	"rolltop/backend/store/storetest"
)

// A sync reports its tally on a pace rather than once per message. The tally
// itself is still per message, so nothing is lost: what changes is how often the
// run stops to write it down and tell every connected tab about it.
func TestSyncPacesItsProgressReportsRatherThanReportingPerMessage(t *testing.T) {
	ctx := context.Background()
	db, err := storetest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	user, account, mailbox := createRunnerMailboxFixture(t, ctx, db, "progress-pace@example.test")
	const remoteCount = 300
	if err := db.UpdateMailboxRemoteStatus(ctx, user.ID, mailbox.ID, 0, 0, remoteCount+1, 7); err != nil {
		t.Fatal(err)
	}
	fetcher := newBackfillFetcher(remoteCount)
	reports := 0
	service := &Service{
		Store: db, Blobs: blob.New(t.TempDir()), Fetcher: fetcher,
		NotifyProgress: func(int64) { reports++ },
	}

	if _, err := service.syncUserAccountMailboxes(freshTurnContext(t, ctx), user.ID, account.ID,
		[]string{mailbox.Name}, syncAccountOptions{}); err != nil {
		t.Fatal(err)
	}

	stored, err := db.CountMessagesForMailbox(ctx, user.ID, mailbox.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored != remoteCount {
		t.Fatalf("mirrored %d of %d messages", stored, remoteCount)
	}
	// The run still says what it did — the finished row carries the full tally —
	// and it says so a handful of times rather than three hundred.
	if reports == 0 {
		t.Fatal("the run never reported its progress")
	}
	if reports >= remoteCount {
		t.Fatalf("progress reports = %d for %d messages, want them paced rather than one each", reports, remoteCount)
	}
	run := latestSyncRun(t, ctx, db, user.ID)
	if run.Status != "ok" || run.MessagesStored != remoteCount {
		t.Fatalf("finished run status=%q stored=%d, want ok with the full tally", run.Status, run.MessagesStored)
	}
}

// Pacing must not lose a folder's tally: a turn that ends commits what it
// mirrored alongside the checkpoint that proves it, whatever the pace said.
func TestSyncCommitsAFolderTallyWhenItsTurnEnds(t *testing.T) {
	ctx := context.Background()
	db, err := storetest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	user, account, mailbox := createRunnerMailboxFixture(t, ctx, db, "progress-commit@example.test")
	const remoteCount = 3
	if err := db.UpdateMailboxRemoteStatus(ctx, user.ID, mailbox.ID, 0, 0, remoteCount+1, 7); err != nil {
		t.Fatal(err)
	}
	fetcher := newBackfillFetcher(remoteCount)
	service := &Service{Store: db, Blobs: blob.New(t.TempDir()), Fetcher: fetcher}

	if _, err := service.syncUserAccountMailboxes(freshTurnContext(t, ctx), user.ID, account.ID,
		[]string{mailbox.Name}, syncAccountOptions{}); err != nil {
		t.Fatal(err)
	}

	// Three messages arrive well inside one pacing interval, so nothing but the
	// folder boundary commits them.
	run := latestSyncRun(t, ctx, db, user.ID)
	if run.MessagesStored != remoteCount || run.MailboxesDone != 1 {
		t.Fatalf("run stored=%d mailboxes_done=%d, want %d/1", run.MessagesStored, run.MailboxesDone, remoteCount)
	}
	if run.MessagesSeen < remoteCount {
		t.Fatalf("run seen=%d, want at least the %d messages it walked", run.MessagesSeen, remoteCount)
	}
}
