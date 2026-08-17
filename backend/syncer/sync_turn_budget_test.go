// File overview: Tests for bounded sync turns that pause and resume instead of
// failing in the middle of a large mailbox backfill.

package syncer

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"rolltop/backend/blob"
	"rolltop/backend/store"
)

// backfillFetcher mirrors a folder that holds more history than one bounded turn
// can fetch. Turns are counted so tests can assert how a paused turn resumes.
type backfillFetcher struct {
	*moveTestFetcher
	count       uint32
	uidValidity uint32
	turns       atomic.Int32
	mu          sync.Mutex
	afterUIDs   []uint32
	// stopAfter ends a fetch with the cooperative pause sentinel once this many
	// messages have been emitted in one turn, standing in for a spent wall-clock
	// budget. Zero streams the whole folder.
	stopAfter int
	// stall spends the whole turn without emitting a message, the way an IMAP
	// server that stops answering does.
	stall bool
}

func newBackfillFetcher(count uint32) *backfillFetcher {
	return &backfillFetcher{moveTestFetcher: &moveTestFetcher{}, count: count, uidValidity: 7}
}

func (f *backfillFetcher) ListMailboxes(context.Context, store.MailAccount) ([]MailboxInfo, error) {
	return []MailboxInfo{{Name: "INBOX"}}, nil
}

func (f *backfillFetcher) MailboxStatus(context.Context, store.MailAccount, string) (MailboxStatus, error) {
	return MailboxStatus{Messages: f.count, UIDNext: f.count + 1, UIDValidity: f.uidValidity}, nil
}

func (f *backfillFetcher) UIDs(context.Context, store.MailAccount, string) ([]uint32, error) {
	uids := make([]uint32, 0, f.count)
	for uid := uint32(1); uid <= f.count; uid++ {
		uids = append(uids, uid)
	}
	return uids, nil
}

func (f *backfillFetcher) SeenUIDs(context.Context, store.MailAccount, string) ([]uint32, error) {
	return nil, nil
}

func (f *backfillFetcher) FlaggedUIDs(context.Context, store.MailAccount, string) ([]uint32, error) {
	return nil, nil
}

func (f *backfillFetcher) message(mailbox string, uid uint32) FetchedMessage {
	return FetchedMessage{
		Mailbox: mailbox, UID: uid, UIDValidity: f.uidValidity, InternalDate: time.Now().UTC(),
		Raw: []byte(fmt.Sprintf("Message-ID: <backfill-%d@example.test>\r\n"+
			"From: sender@example.test\r\nTo: owner@example.test\r\n"+
			"Subject: Backfill %d\r\n\r\nbackfilltoken\r\n", uid, uid)),
	}
}

func (f *backfillFetcher) FetchMailbox(_ context.Context, _ store.MailAccount, mailbox string,
	afterUID uint32, handle func(FetchedMessage) error,
) error {
	f.turns.Add(1)
	f.mu.Lock()
	f.afterUIDs = append(f.afterUIDs, afterUID)
	f.mu.Unlock()
	if f.stall {
		return fmt.Errorf("fetch mailbox %q UID batch %d: %w", mailbox, afterUID+1, errSyncTurnBudgetSpent)
	}
	emitted := 0
	for uid := afterUID + 1; uid <= f.count; uid++ {
		if err := handle(f.message(mailbox, uid)); err != nil {
			return fmt.Errorf("store message mailbox %q UID %d: %w", mailbox, uid, err)
		}
		emitted++
		if f.stopAfter > 0 && emitted >= f.stopAfter {
			return fmt.Errorf("fetch mailbox %q UID batch %d: %w", mailbox, uid, errSyncTurnBudgetSpent)
		}
	}
	return nil
}

func (f *backfillFetcher) FetchMailboxWithUIDValidity(ctx context.Context, account store.MailAccount,
	mailbox string, afterUID, expectedUIDValidity uint32, handle func(FetchedMessage) error,
) error {
	if expectedUIDValidity != f.uidValidity {
		return store.ErrMailboxGenerationChanged
	}
	return f.FetchMailbox(ctx, account, mailbox, afterUID, handle)
}

func (f *backfillFetcher) FetchUIDs(_ context.Context, _ store.MailAccount, mailbox string, uids []uint32,
	handle func(FetchedMessage) error,
) error {
	f.turns.Add(1)
	if f.stall {
		return fmt.Errorf("fetch mailbox %q UID batch %d: %w", mailbox, uids[0], errSyncTurnBudgetSpent)
	}
	emitted := 0
	for _, uid := range uids {
		if err := handle(f.message(mailbox, uid)); err != nil {
			return fmt.Errorf("store message mailbox %q UID %d: %w", mailbox, uid, err)
		}
		emitted++
		if f.stopAfter > 0 && emitted >= f.stopAfter {
			return fmt.Errorf("fetch mailbox %q UID batch %d: %w", mailbox, uid, errSyncTurnBudgetSpent)
		}
	}
	return nil
}

func (f *backfillFetcher) FetchUIDsWithUIDValidity(ctx context.Context, account store.MailAccount,
	mailbox string, uids []uint32, expectedUIDValidity uint32, handle func(FetchedMessage) error,
) error {
	if expectedUIDValidity != f.uidValidity {
		return store.ErrMailboxGenerationChanged
	}
	return f.FetchUIDs(ctx, account, mailbox, uids, handle)
}

// spentTurnContext bounds a turn whose pause point has already passed, so the
// sync stops after its first durable message without depending on wall clock.
func spentTurnContext(t *testing.T, parent context.Context) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(parent, time.Minute)
	t.Cleanup(cancel)
	return context.WithValue(ctx, syncTurnBudgetKey{},
		syncTurnBudget{pauseAt: time.Now().Add(-time.Millisecond)})
}

func freshTurnContext(t *testing.T, parent context.Context) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(parent, time.Minute)
	t.Cleanup(cancel)
	return withSyncTurnBudget(ctx)
}

func latestSyncRun(t *testing.T, ctx context.Context, db *store.Store, userID int64) store.SyncRun {
	t.Helper()
	runs, err := db.ListSyncRunsForUser(ctx, userID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) == 0 {
		t.Fatal("sync did not record a run")
	}
	return runs[0]
}

func TestBoundedTurnPausesIncrementalFetchWithoutFailingTheRun(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(filepath.Join(t.TempDir(), "rolltop.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	user, account, mailbox := createRunnerMailboxFixture(t, ctx, db, "paused-backfill@example.test")
	const remoteCount = 4
	if err := db.UpdateMailboxRemoteStatus(ctx, user.ID, mailbox.ID, 0, 0, remoteCount+1, 7); err != nil {
		t.Fatal(err)
	}
	fetcher := newBackfillFetcher(remoteCount)
	service := &Service{Store: db, Blobs: blob.New(t.TempDir()), Fetcher: fetcher}

	// An account-wide pass does not request a folder by name, so this exercises
	// the plain incremental fetch rather than the sparse repair path.
	_, err = service.syncUserWithOptions(spentTurnContext(t, ctx), user.ID, nil, syncAccountOptions{})
	if !errors.Is(err, ErrSyncTurnPaused) {
		t.Fatalf("bounded turn error = %v, want ErrSyncTurnPaused", err)
	}
	stored, err := db.CountMessagesForMailbox(ctx, user.ID, mailbox.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored != 1 {
		t.Fatalf("paused turn stored %d messages, want 1", stored)
	}
	lastUIDs, err := db.LastUIDs(ctx, user.ID, account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if lastUIDs[mailbox.Name] != 1 {
		t.Fatalf("paused turn checkpoint=%d, want 1", lastUIDs[mailbox.Name])
	}
	run := latestSyncRun(t, ctx, db, user.ID)
	if run.Status != "ok" || run.Error != "" {
		t.Fatalf("paused run status=%q error=%q, want ok without an error", run.Status, run.Error)
	}

	// The next turn resumes from the checkpoint instead of refetching the folder.
	if _, err := service.syncUserWithOptions(freshTurnContext(t, ctx), user.ID, nil, syncAccountOptions{}); err != nil {
		t.Fatal(err)
	}
	if got := fetcher.afterUIDs; len(got) != 2 || got[1] != 1 {
		t.Fatalf("fetch resume points = %v, want the second turn to start after UID 1", got)
	}
	stored, err = db.CountMessagesForMailbox(ctx, user.ID, mailbox.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored != remoteCount {
		t.Fatalf("resumed backfill stored %d messages, want %d", stored, remoteCount)
	}
	lastUIDs, err = db.LastUIDs(ctx, user.ID, account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if lastUIDs[mailbox.Name] != remoteCount {
		t.Fatalf("resumed checkpoint=%d, want %d", lastUIDs[mailbox.Name], remoteCount)
	}
}

func TestBoundedTurnPausesFolderRepairAndKeepsMirroredMessages(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(filepath.Join(t.TempDir(), "rolltop.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	user, account, mailbox := createRunnerMailboxFixture(t, ctx, db, "paused-repair@example.test")
	const remoteCount = 4
	if err := db.UpdateMailboxRemoteStatus(ctx, user.ID, mailbox.ID, 0, 0, remoteCount+1, 7); err != nil {
		t.Fatal(err)
	}
	fetcher := newBackfillFetcher(remoteCount)
	service := &Service{Store: db, Blobs: blob.New(t.TempDir()), Fetcher: fetcher}

	// Requesting the folder by name is what an Inbox poll does, and an empty local
	// folder with remote history takes the sparse repair path.
	_, err = service.syncUserAccountMailboxes(spentTurnContext(t, ctx), user.ID, account.ID,
		[]string{mailbox.Name}, syncAccountOptions{})
	if !errors.Is(err, ErrSyncTurnPaused) {
		t.Fatalf("bounded repair turn error = %v, want ErrSyncTurnPaused", err)
	}
	stored, err := db.CountMessagesForMailbox(ctx, user.ID, mailbox.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored != 1 {
		t.Fatalf("paused repair stored %d messages, want 1", stored)
	}
	if exists, err := db.MessageExistsByUIDForGeneration(ctx, user.ID, account.ID, mailbox.ID, 1, 7); err != nil {
		t.Fatal(err)
	} else if !exists {
		t.Fatal("paused repair left its mirrored message incomplete, so the next turn refetches it")
	}
	lastUIDs, err := db.LastUIDs(ctx, user.ID, account.ID)
	if err != nil {
		t.Fatal(err)
	}
	// The checkpoint is also the new-mail baseline, so an unfinished repair must
	// not advance it and turn the remaining history into arrivals.
	if lastUIDs[mailbox.Name] != 0 {
		t.Fatalf("paused repair advanced the checkpoint to %d", lastUIDs[mailbox.Name])
	}
	run := latestSyncRun(t, ctx, db, user.ID)
	if run.Status != "ok" || run.Error != "" {
		t.Fatalf("paused repair run status=%q error=%q, want ok without an error", run.Status, run.Error)
	}

	if _, err := service.syncUserAccountMailboxes(freshTurnContext(t, ctx), user.ID, account.ID,
		[]string{mailbox.Name}, syncAccountOptions{}); err != nil {
		t.Fatal(err)
	}
	stored, err = db.CountMessagesForMailbox(ctx, user.ID, mailbox.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored != remoteCount {
		t.Fatalf("resumed repair stored %d messages, want %d", stored, remoteCount)
	}
	lastUIDs, err = db.LastUIDs(ctx, user.ID, account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if lastUIDs[mailbox.Name] != remoteCount {
		t.Fatalf("resumed repair checkpoint=%d, want %d", lastUIDs[mailbox.Name], remoteCount)
	}
}

func TestPausedInboxTurnResumesWithoutWaitingForTheNextPoll(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	db, err := store.Open(filepath.Join(t.TempDir(), "rolltop.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	user, account, mailbox := createRunnerMailboxFixture(t, ctx, db, "requeued-backfill@example.test")
	const remoteCount = 6
	if err := db.UpdateMailboxRemoteStatus(ctx, user.ID, mailbox.ID, 0, 0, remoteCount+1, 7); err != nil {
		t.Fatal(err)
	}
	fetcher := newBackfillFetcher(remoteCount)
	fetcher.stopAfter = 2
	service := &Service{Store: db, Blobs: blob.New(t.TempDir()), Fetcher: fetcher}
	runner := NewRunnerWithContext(ctx, service)

	if !runner.StartAccountMailboxes(user.ID, account.ID, []string{mailbox.Name}) {
		t.Fatal("Inbox sync did not start")
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		stored, err := db.CountMessagesForMailbox(ctx, user.ID, mailbox.ID)
		if err != nil {
			t.Fatal(err)
		}
		if stored == remoteCount {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("paused Inbox turns stalled after %d of %d messages", stored, remoteCount)
		}
		time.Sleep(5 * time.Millisecond)
	}
	waitForRunnerUserIdle(t, runner, user.ID)

	if fetcher.turns.Load() < 2 {
		t.Fatalf("backfill finished in %d turns, want the paused turn to be resumed", fetcher.turns.Load())
	}
	runs, err := db.ListSyncRunsForUser(ctx, user.ID, 50)
	if err != nil {
		t.Fatal(err)
	}
	for _, run := range runs {
		if run.Status != "ok" {
			t.Fatalf("paused backfill recorded run status=%q error=%q", run.Status, run.Error)
		}
	}
	lastUIDs, err := db.LastUIDs(ctx, user.ID, account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if lastUIDs[mailbox.Name] != remoteCount {
		t.Fatalf("requeued backfill checkpoint=%d, want %d", lastUIDs[mailbox.Name], remoteCount)
	}
}

func TestBoundedTurnThatMirrorsNothingStillReportsFailure(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(filepath.Join(t.TempDir(), "rolltop.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	user, account, mailbox := createRunnerMailboxFixture(t, ctx, db, "stalled-turn@example.test")
	if err := db.UpdateMailboxRemoteStatus(ctx, user.ID, mailbox.ID, 0, 0, 5, 7); err != nil {
		t.Fatal(err)
	}
	fetcher := newBackfillFetcher(4)
	fetcher.stall = true
	service := &Service{Store: db, Blobs: blob.New(t.TempDir()), Fetcher: fetcher}

	// A turn that spends its budget without mirroring anything must not ask to be
	// rescheduled immediately: that would retry a stalled server at full speed.
	_, err = service.syncUserAccountMailboxes(freshTurnContext(t, ctx), user.ID, account.ID,
		[]string{mailbox.Name}, syncAccountOptions{})
	if err == nil || errors.Is(err, ErrSyncTurnPaused) {
		t.Fatalf("stalled turn error = %v, want a plain failure", err)
	}
	if run := latestSyncRun(t, ctx, db, user.ID); run.Status != "failed" {
		t.Fatalf("stalled turn run status=%q, want failed", run.Status)
	}
}

func TestWithSyncTurnBudgetKeepsMostOfTheTurnForFetching(t *testing.T) {
	if ctx := withSyncTurnBudget(context.Background()); syncTurnBudgeted(ctx) {
		t.Fatal("a context without a deadline was given a turn budget")
	}
	parent, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	ctx := withSyncTurnBudget(parent)
	budget, ok := ctx.Value(syncTurnBudgetKey{}).(syncTurnBudget)
	if !ok {
		t.Fatal("bounded turn did not receive a budget")
	}
	if remaining := time.Until(budget.pauseAt); remaining < 80*time.Second {
		t.Fatalf("pause point leaves only %s of a 90s turn for fetching", remaining)
	}
	if syncTurnBudgetSpent(ctx) {
		t.Fatal("a fresh 90s turn reported its budget as spent")
	}

	short, cancelShort := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancelShort()
	shortBudget, ok := withSyncTurnBudget(short).Value(syncTurnBudgetKey{}).(syncTurnBudget)
	if !ok {
		t.Fatal("short bounded turn did not receive a budget")
	}
	if remaining := time.Until(shortBudget.pauseAt); remaining <= 0 {
		t.Fatalf("short turn pause point is already spent: %s", remaining)
	}
}

func TestSyncTurnPausedSeparatesTurnBudgetFromRealFailures(t *testing.T) {
	unbounded := context.Background()
	if syncTurnPaused(unbounded, errSyncTurnBudgetSpent) {
		t.Fatal("an unbounded run treated a budget stop as a pause")
	}
	live, cancelLive := context.WithTimeout(context.Background(), time.Minute)
	defer cancelLive()
	budgeted := withSyncTurnBudget(live)
	if !syncTurnPaused(budgeted, fmt.Errorf("fetch mailbox: %w", errSyncTurnBudgetSpent)) {
		t.Fatal("a cooperative budget stop was not reported as a pause")
	}
	if syncTurnPaused(budgeted, nil) {
		t.Fatal("a successful turn was reported as paused")
	}
	// A per-command timeout that fires while the turn itself still has time left
	// is a stalled server, not a spent budget.
	if syncTurnPaused(budgeted, context.DeadlineExceeded) {
		t.Fatal("a command timeout inside a live turn was reported as a pause")
	}
	expired, cancelExpired := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancelExpired()
	expiredBudget := withSyncTurnBudget(expired)
	<-expired.Done()
	if !syncTurnPaused(expiredBudget, errors.New("imap: connection closed")) {
		t.Fatal("an error after the turn deadline was not reported as a pause")
	}
	shutdown, cancelShutdown := context.WithTimeout(context.Background(), time.Minute)
	shutdownBudget := withSyncTurnBudget(shutdown)
	cancelShutdown()
	if syncTurnPaused(shutdownBudget, context.Canceled) {
		t.Fatal("process shutdown was reported as a turn pause")
	}
}
