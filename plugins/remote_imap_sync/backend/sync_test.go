package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"rolltop/backend/store"
	"rolltop/backend/syncer"
)

func TestRemoteSyncRunStatusLogsSanitizedLifecycle(t *testing.T) {
	var lines []string
	logf := func(format string, args ...any) {
		lines = append(lines, fmt.Sprintf(format, args...))
	}
	status := newRemoteSyncRunStatusWithLogger(context.Background(), 7, 11, 13, "manual",
		0, logf)
	status.SetTotal(40)
	status.Start()
	status.Update(3, 2, 1, 1234)
	status.logHeartbeat()
	status.Finish("failed", errors.New("dial secret.example for private-user: private-password rejected"))

	want := []string{
		"remote imap sync run started user_id=7 routine_id=11 run_id=13 trigger=manual pending_uid_total=40",
		"remote imap sync run heartbeat user_id=7 routine_id=11 run_id=13 trigger=manual scanned=3 transferred=2 skipped=1 current_uid=1234 total=40",
		`remote imap sync run failed user_id=7 routine_id=11 run_id=13 trigger=manual scanned=3 transferred=2 skipped=1 current_uid=1234 total=40 reason="The IMAP server could not complete the request."`,
	}
	if len(lines) != len(want) {
		t.Fatalf("log lines = %#v, want %#v", lines, want)
	}
	for i := range want {
		if lines[i] != want[i] {
			t.Fatalf("log line %d = %q, want %q", i, lines[i], want[i])
		}
	}
	joined := strings.Join(lines, "\n")
	for _, secret := range []string{"secret.example", "private-user", "private-password"} {
		if strings.Contains(joined, secret) {
			t.Fatalf("remote sync status logs exposed %q: %s", secret, joined)
		}
	}
}

func TestRemoteSyncRunStatusUpdatesDoNotLogPerMessage(t *testing.T) {
	var lines []string
	status := newRemoteSyncRunStatusWithLogger(context.Background(), 1, 2, 3, "unexpected\ntrigger",
		0, func(format string, args ...any) {
			lines = append(lines, fmt.Sprintf(format, args...))
		})
	status.SetTotal(100)
	status.Start()
	for scanned := int64(1); scanned <= 100; scanned++ {
		status.Update(scanned, scanned, 0, uint32(scanned))
	}
	status.Finish("completed", nil)

	if remoteSyncHeartbeatInterval != 30*time.Second {
		t.Fatalf("heartbeat interval = %s, want 30s", remoteSyncHeartbeatInterval)
	}
	if len(lines) != 2 {
		t.Fatalf("100 progress updates emitted %d lines, want only start and completion: %#v", len(lines), lines)
	}
	if !strings.Contains(lines[0], "trigger=other") || !strings.Contains(lines[1], "scanned=100 transferred=100") {
		t.Fatalf("unexpected lifecycle logs: %#v", lines)
	}
}

func TestRemoteSyncRunStatusIdentifiesRecoveryDeferral(t *testing.T) {
	var lines []string
	status := newRemoteSyncRunStatusWithLogger(context.Background(), 4, 5, 6, "recovery",
		0, func(format string, args ...any) {
			lines = append(lines, fmt.Sprintf(format, args...))
		})
	status.SetTotal(75)
	status.Update(25, 20, 5, 900)
	status.Finish("deferred", nil)

	want := "remote imap sync run deferred user_id=4 routine_id=5 run_id=6 trigger=recovery scanned=25 transferred=20 skipped=5 current_uid=900 total=75 reason=mailbox_generation_recovery"
	if len(lines) != 1 || lines[0] != want {
		t.Fatalf("deferred status logs = %#v, want %q", lines, want)
	}
}

func TestRemoteSyncRecoveryWaitLogsPauseHeartbeatAndResume(t *testing.T) {
	var lines []string
	checks := 0
	err := waitForRemoteSyncRecoveryWithStatus(context.Background(), 7, 11, time.Millisecond, time.Millisecond,
		func(context.Context, int64) (bool, error) {
			checks++
			return checks < 3, nil
		}, func(format string, args ...any) {
			lines = append(lines, fmt.Sprintf(format, args...))
		})
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 3 || !strings.Contains(lines[0], "remote imap sync paused user_id=7 routine_id=11") ||
		!strings.Contains(lines[1], "remote imap sync pause heartbeat user_id=7 routine_id=11") ||
		!strings.Contains(lines[2], "remote imap sync resumed user_id=7 routine_id=11") {
		t.Fatalf("recovery wait logs=%q", lines)
	}
}

func TestShouldWriteRunProgressCadence(t *testing.T) {
	tests := []struct {
		name    string
		scanned int64
		want    bool
	}{
		{name: "empty checkpoint", scanned: 0, want: false},
		{name: "first checkpoint", scanned: 1, want: true},
		{name: "second checkpoint", scanned: 2, want: false},
		{name: "before interval", scanned: 24, want: false},
		{name: "interval checkpoint", scanned: 25, want: true},
		{name: "after interval", scanned: 26, want: false},
		{name: "second interval checkpoint", scanned: 50, want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shouldWriteRunProgress(tt.scanned); got != tt.want {
				t.Fatalf("shouldWriteRunProgress(%d) = %v, want %v", tt.scanned, got, tt.want)
			}
		})
	}
}

func TestRunProgressCadenceKeepsFinalPosition(t *testing.T) {
	for total := int64(1); total <= 100; total++ {
		lastWritten := int64(0)
		writes := 0
		for scanned := int64(1); scanned <= total; scanned++ {
			if shouldWriteRunProgress(scanned) {
				lastWritten = scanned
				writes++
			}
		}
		if runProgressNeedsFlush(total, lastWritten) {
			lastWritten = total
			writes++
		}
		if lastWritten != total {
			t.Fatalf("total %d left last persisted position at %d", total, lastWritten)
		}
		wantMaxWrites := int(total/runProgressWriteEvery) + 2
		if writes > wantMaxWrites {
			t.Fatalf("total %d produced %d writes, want at most %d", total, writes, wantMaxWrites)
		}
	}
}

func TestRunProgressFailureFlushesSinceLastCheckpoint(t *testing.T) {
	tests := []struct {
		name      string
		scanned   int64
		persisted int64
		want      bool
	}{
		{name: "no messages", scanned: 0, persisted: 0, want: false},
		{name: "first checkpoint persisted", scanned: 1, persisted: 1, want: false},
		{name: "between checkpoints", scanned: 24, persisted: 1, want: true},
		{name: "checkpoint write failed", scanned: 25, persisted: 1, want: true},
		{name: "checkpoint persisted", scanned: 25, persisted: 25, want: false},
		{name: "later partial batch", scanned: 49, persisted: 25, want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := runProgressNeedsFlush(tt.scanned, tt.persisted); got != tt.want {
				t.Fatalf("runProgressNeedsFlush(%d, %d) = %v, want %v", tt.scanned, tt.persisted, got, tt.want)
			}
		})
	}
}

func TestShouldYieldRemoteSyncCadence(t *testing.T) {
	tests := []struct {
		name           string
		scanned, total int64
		want           bool
	}{
		{name: "empty", scanned: 0, total: 100, want: false},
		{name: "before first chunk", scanned: 24, total: 100, want: false},
		{name: "first full chunk with more", scanned: 25, total: 100, want: true},
		{name: "between chunks", scanned: 49, total: 100, want: false},
		{name: "second full chunk with more", scanned: 50, total: 100, want: true},
		{name: "exact final chunk", scanned: 25, total: 25, want: false},
		{name: "final chunk", scanned: 100, total: 100, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shouldYieldRemoteSync(tt.scanned, tt.total); got != tt.want {
				t.Fatalf("shouldYieldRemoteSync(%d, %d) = %v, want %v", tt.scanned, tt.total, got, tt.want)
			}
		})
	}
}

func TestWaitForRemoteSyncChunkHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	started := time.Now()
	err := waitForRemoteSyncChunk(ctx, time.Minute)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("waitForRemoteSyncChunk() error = %v, want context canceled", err)
	}
	if elapsed := time.Since(started); elapsed > 100*time.Millisecond {
		t.Fatalf("canceled wait took %s", elapsed)
	}
}

func TestWaitForRemoteSyncChunkCompletesDelay(t *testing.T) {
	if err := waitForRemoteSyncChunk(context.Background(), time.Millisecond); err != nil {
		t.Fatalf("waitForRemoteSyncChunk() error = %v", err)
	}
}

type fakeSeenClearer struct {
	calls   []fakeSeenClearerCall
	applied bool
	err     error
}

type fakeSeenClearerCall struct {
	mailbox     string
	uid         uint32
	seen        bool
	uidValidity uint32
}

func (f *fakeSeenClearer) SetSeenWithUIDValidity(ctx context.Context, account store.MailAccount, mailbox string,
	uid uint32, seen bool, expectedUIDValidity uint32,
) (bool, error) {
	f.calls = append(f.calls, fakeSeenClearerCall{mailbox: mailbox, uid: uid, seen: seen, uidValidity: expectedUIDValidity})
	return f.applied, f.err
}

func TestClearDestinationSeenFlagKeepsAnUnreadCopyUnread(t *testing.T) {
	clearer := &fakeSeenClearer{applied: true}
	appended := syncer.FetchedMessage{UID: 42, Flags: []string{"\\Seen", "\\Recent"}}
	if err := clearDestinationSeenFlag(context.Background(), clearer, store.MailAccount{}, "INBOX",
		nil, appended, 9); err != nil {
		t.Fatal(err)
	}
	want := []fakeSeenClearerCall{{mailbox: "INBOX", uid: 42, seen: false, uidValidity: 9}}
	if len(clearer.calls) != 1 || clearer.calls[0] != want[0] {
		t.Fatalf("clearer calls = %#v, want %#v", clearer.calls, want)
	}
}

func TestClearDestinationSeenFlagLeavesMatchingReadStateAlone(t *testing.T) {
	for _, tc := range []struct {
		name        string
		sourceFlags []string
		appended    syncer.FetchedMessage
	}{
		{name: "read source stays read", sourceFlags: []string{"\\seen"}, appended: syncer.FetchedMessage{UID: 42, Flags: []string{"\\Seen"}}},
		{name: "unread copy needs no repair", sourceFlags: nil, appended: syncer.FetchedMessage{UID: 42, Flags: []string{"\\Recent"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearer := &fakeSeenClearer{}
			if err := clearDestinationSeenFlag(context.Background(), clearer, store.MailAccount{}, "INBOX",
				tc.sourceFlags, tc.appended, 9); err != nil {
				t.Fatal(err)
			}
			if len(clearer.calls) != 0 {
				t.Fatalf("clearer calls = %#v, want none", clearer.calls)
			}
		})
	}
}

func TestClearDestinationSeenFlagFailsWithoutADestinationGeneration(t *testing.T) {
	clearer := &fakeSeenClearer{applied: true}
	appended := syncer.FetchedMessage{UID: 42, Flags: []string{"\\Seen"}}
	if err := clearDestinationSeenFlag(context.Background(), clearer, store.MailAccount{}, "INBOX",
		nil, appended, 0); err == nil {
		t.Fatal("expected a missing UIDVALIDITY to fail the repair")
	}
	if len(clearer.calls) != 0 {
		t.Fatalf("clearer calls = %#v, want none", clearer.calls)
	}
}

func TestClearDestinationSeenFlagReportsAChangedDestinationGeneration(t *testing.T) {
	clearer := &fakeSeenClearer{applied: false}
	appended := syncer.FetchedMessage{UID: 42, Flags: []string{"\\Seen"}}
	if err := clearDestinationSeenFlag(context.Background(), clearer, store.MailAccount{}, "INBOX",
		nil, appended, 9); err == nil {
		t.Fatal("expected a refused flag write to fail the repair")
	}
}
