package search

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"rolltop/backend/store"
)

func TestActiveWriterStallSurvivesCallerCancellation(t *testing.T) {
	root := filepath.Join(t.TempDir(), "users")
	svc, err := OpenPerUser(root)
	if err != nil {
		t.Fatal(err)
	}
	base, err := svc.indexForUser(17)
	if err != nil {
		t.Fatal(err)
	}
	blocking := &blockingBatchIndex{
		delegatedBleveIndex: base,
		started:             make(chan struct{}),
		release:             make(chan struct{}),
		finished:            make(chan struct{}),
	}
	svc.mu.Lock()
	svc.indexes[17] = blocking
	svc.mu.Unlock()
	t.Cleanup(func() {
		blocking.unblock()
		_ = svc.Close()
	})

	svc.activeStallAfter = 20 * time.Millisecond
	logs := &capturedBleveLogs{}
	svc.bleveErrorLog = logs.Printf
	stalledUsers := make(chan int64, 2)
	svc.SetActiveWriterStallHandler(func(userID int64) {
		stalledUsers <- userID
	})

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		result <- svc.IndexMessage(ctx, store.MessageRecord{
			ID: 912, UserID: 17, AccountID: 4, MailboxID: 34,
			Subject: "private stalled subject", BodyText: "private stalled body", Date: time.Now(),
		}, nil)
	}()
	select {
	case <-blocking.started:
	case <-time.After(time.Second):
		t.Fatal("Bleve batch did not start")
	}
	cancel()
	select {
	case err := <-result:
		if err != context.Canceled {
			t.Fatalf("IndexMessage error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("caller remained blocked after cancellation")
	}

	select {
	case userID := <-stalledUsers:
		if userID != 17 {
			t.Fatalf("stalled user = %d, want 17", userID)
		}
	case <-time.After(time.Second):
		t.Fatal("active writer watchdog did not signal")
	}
	// The restart is asked for before the marker is written, so the signal
	// above does not prove the marker exists yet -- the same reason the log
	// assertion below waits rather than reads once. It is still written; wait
	// for it instead of racing it.
	var required bool
	for deadline := time.Now().Add(time.Second); ; {
		var err error
		required, err = searchRecoveryRequired(svc, 17)
		if err != nil {
			t.Fatal(err)
		}
		if required {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("active writer watchdog did not persist a recovery marker")
		}
		time.Sleep(time.Millisecond)
	}
	if writer := svc.writerForUser(17); writer.TryLock() {
		writer.Unlock()
		t.Fatal("watchdog released the writer while Bleve Batch remained active")
	}
	svc.writeCoordinator.mu.Lock()
	coordinatorActive := svc.writeCoordinator.activeUsers[17]
	svc.writeCoordinator.mu.Unlock()
	if coordinatorActive != 1 {
		t.Fatalf("coordinator active leases for user 17 = %d, want 1 until Bleve returns", coordinatorActive)
	}

	// watchActiveWriter notifies the stall handler before it writes its
	// diagnostics, and the handler channel is buffered, so the watchdog can be
	// descheduled between the two. Waiting on stalledUsers above therefore does
	// not prove the log line exists yet. Wait for it instead of assuming it.
	const stallLine = `bleve active writer stalled operation="index-batch"`
	var output string
	for deadline := time.Now().Add(time.Second); ; {
		output = logs.String()
		if strings.Contains(output, stallLine) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("stall diagnostics missing %q: %q", stallLine, output)
		}
		time.Sleep(time.Millisecond)
	}
	// The remaining fields come from the same single Printf call as stallLine,
	// so one snapshot covers them all.
	for _, want := range []string{
		`user_id=17 account_id=4 mailbox_id=34 documents=1`,
		`first_document_id=912 last_document_id=912 document_ids=[912]`,
		`marker_written=true restart_required=true`,
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("stall diagnostics missing %q: %q", want, output)
		}
	}
	if strings.Contains(output, "batch_bytes=0") {
		t.Fatalf("stall diagnostics omitted projected batch size: %q", output)
	}
	for _, private := range []string{"private stalled subject", "private stalled body"} {
		if strings.Contains(output, private) {
			t.Fatalf("stall diagnostics exposed indexed content %q: %q", private, output)
		}
	}

	time.Sleep(3 * svc.activeStallAfter)
	select {
	case userID := <-stalledUsers:
		t.Fatalf("watchdog signaled more than once for user %d", userID)
	default:
	}
	blocking.unblock()
	select {
	case <-blocking.finished:
	case <-time.After(time.Second):
		t.Fatal("Bleve batch did not finish after release")
	}
	deadline := time.Now().Add(time.Second)
	for {
		svc.writeCoordinator.mu.Lock()
		coordinatorActive = svc.writeCoordinator.activeUsers[17]
		svc.writeCoordinator.mu.Unlock()
		if coordinatorActive == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("coordinator lease remained active after Bleve returned")
		}
		time.Sleep(time.Millisecond)
	}
	required, err = searchRecoveryRequired(svc, 17)
	if err != nil {
		t.Fatal(err)
	}
	if !required {
		t.Fatal("recovery marker was cleared when the stalled operation returned")
	}
}

func TestActiveWriterWatchdogIgnoresCompletedOperation(t *testing.T) {
	root := filepath.Join(t.TempDir(), "users")
	svc, err := OpenPerUser(root)
	if err != nil {
		t.Fatal(err)
	}
	base, err := svc.indexForUser(23)
	if err != nil {
		t.Fatal(err)
	}
	blocking := &blockingBatchIndex{
		delegatedBleveIndex: base,
		started:             make(chan struct{}),
		release:             make(chan struct{}),
		finished:            make(chan struct{}),
	}
	svc.mu.Lock()
	svc.indexes[23] = blocking
	svc.mu.Unlock()
	t.Cleanup(func() {
		blocking.unblock()
		_ = svc.Close()
	})

	svc.activeStallAfter = 100 * time.Millisecond
	logs := &capturedBleveLogs{}
	svc.bleveErrorLog = logs.Printf
	stalledUsers := make(chan int64, 1)
	svc.SetActiveWriterStallHandler(func(userID int64) { stalledUsers <- userID })
	result := make(chan error, 1)
	go func() {
		result <- svc.IndexMessage(context.Background(), store.MessageRecord{
			ID: 1, UserID: 23, AccountID: 2, MailboxID: 3, Subject: "fast batch", Date: time.Now(),
		}, nil)
	}()
	select {
	case <-blocking.started:
		blocking.unblock()
	case <-time.After(time.Second):
		t.Fatal("Bleve batch did not start")
	}
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("completed Bleve batch did not return")
	}
	time.Sleep(2 * svc.activeStallAfter)
	select {
	case userID := <-stalledUsers:
		t.Fatalf("completed operation triggered watchdog for user %d", userID)
	default:
	}
	if required, err := searchRecoveryRequired(svc, 23); err != nil {
		t.Fatal(err)
	} else if required {
		t.Fatal("completed operation left a recovery marker")
	}
	if strings.Contains(logs.String(), "active writer stalled") {
		t.Fatalf("completed operation produced stall diagnostics: %q", logs.String())
	}
}

func TestActiveWriterStallSignalsBeforeBlockedDiagnostics(t *testing.T) {
	root := filepath.Join(t.TempDir(), "users")
	svc, err := OpenPerUser(root)
	if err != nil {
		t.Fatal(err)
	}
	base, err := svc.indexForUser(29)
	if err != nil {
		t.Fatal(err)
	}
	blocking := &blockingBatchIndex{
		delegatedBleveIndex: base,
		started:             make(chan struct{}),
		release:             make(chan struct{}),
		finished:            make(chan struct{}),
	}
	svc.mu.Lock()
	svc.indexes[29] = blocking
	svc.mu.Unlock()
	t.Cleanup(func() {
		blocking.unblock()
		_ = svc.Close()
	})

	svc.activeStallAfter = 20 * time.Millisecond
	diagnosticStarted := make(chan struct{})
	releaseDiagnostic := make(chan struct{})
	var diagnosticOnce sync.Once
	var releaseDiagnosticOnce sync.Once
	releaseBlockedDiagnostic := func() { releaseDiagnosticOnce.Do(func() { close(releaseDiagnostic) }) }
	svc.bleveErrorLog = func(string, ...any) {
		diagnosticOnce.Do(func() {
			close(diagnosticStarted)
			<-releaseDiagnostic
		})
	}
	stalledUsers := make(chan int64, 1)
	svc.SetActiveWriterStallHandler(func(userID int64) { stalledUsers <- userID })
	t.Cleanup(releaseBlockedDiagnostic)
	result := make(chan error, 1)
	go func() {
		result <- svc.IndexMessage(context.Background(), store.MessageRecord{
			ID: 1, UserID: 29, AccountID: 2, MailboxID: 3, Subject: "blocked diagnostics", Date: time.Now(),
		}, nil)
	}()
	select {
	case <-blocking.started:
	case <-time.After(time.Second):
		t.Fatal("Bleve batch did not start")
	}

	select {
	case userID := <-stalledUsers:
		if userID != 29 {
			t.Fatalf("stalled user = %d, want 29", userID)
		}
	case <-time.After(time.Second):
		t.Fatal("restart signal was blocked by diagnostics")
	}
	select {
	case <-diagnosticStarted:
	case <-time.After(time.Second):
		t.Fatal("stall diagnostics did not start after restart signal")
	}
	if required, markerErr := searchRecoveryRequired(svc, 29); markerErr != nil || !required {
		t.Fatalf("recovery marker required=%t err=%v, want true, nil", required, markerErr)
	}

	releaseBlockedDiagnostic()
	blocking.unblock()
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Bleve batch did not finish")
	}
}

func TestSearchIndexRecoveryMarkerRoundTripWithMalformedIndex(t *testing.T) {
	root := filepath.Join(t.TempDir(), "users")
	userDir := filepath.Join(root, "31")
	if err := os.MkdirAll(userDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(userDir, "bleve"), []byte("not an index directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	svc, err := OpenPerUser(root)
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	if err := svc.MarkSearchIndexRecoveryRequired(31); err != nil {
		t.Fatal(err)
	}
	if err := svc.MarkSearchIndexRecoveryRequired(31); err != nil {
		t.Fatalf("idempotent marker write: %v", err)
	}
	if required, err := searchRecoveryRequired(svc, 31); err != nil {
		t.Fatal(err)
	} else if !required {
		t.Fatal("recovery marker was not found")
	}
	if err := svc.ClearSearchIndexRecoveryRequired(31); err != nil {
		t.Fatal(err)
	}
	if required, err := searchRecoveryRequired(svc, 31); err != nil {
		t.Fatal(err)
	} else if required {
		t.Fatal("recovery marker remained after clear")
	}
}

func TestSearchIndexRecoveryMarkerIsRestoredWhenClearCannotBePersisted(t *testing.T) {
	root := filepath.Join(t.TempDir(), "users")
	svc, err := OpenPerUser(root)
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	if err := svc.MarkSearchIndexRecoveryRequired(37); err != nil {
		t.Fatal(err)
	}

	persistErr := errors.New("directory sync failed")
	syncCalls := 0
	err = svc.clearSearchIndexRecoveryRequiredWithSync(37, func(string) error {
		syncCalls++
		if syncCalls == 1 {
			return persistErr
		}
		return nil
	})
	if !errors.Is(err, persistErr) || !strings.Contains(err.Error(), "marker restored for retry") {
		t.Fatalf("clear error = %v, want persisted-clear failure with restored marker", err)
	}
	if syncCalls != 2 {
		t.Fatalf("directory sync calls = %d, want clear and restored-marker sync", syncCalls)
	}
	if required, requiredErr := searchRecoveryRequired(svc, 37); requiredErr != nil || !required {
		t.Fatalf("recovery marker required=%t err=%v, want true, nil", required, requiredErr)
	}
	if err := svc.ClearSearchIndexRecoveryRequired(37); err != nil {
		t.Fatal(err)
	}
}

func TestFilterBleveBatchStackReturnsOnlyTargetFrames(t *testing.T) {
	stack := []byte(`goroutine 7 [select]:
rolltop/backend/search.privateWorker("private message body")
	/home/rolltop/private.go:12 +0x111

goroutine 19 [semacquire]:
github.com/blevesearch/bleve/v2/index/scorch.(*Scorch).Batch(0xc000, 0xdeadbeef)
	/home/gxs/go/pkg/mod/github.com/blevesearch/bleve/v2/index/scorch/scorch.go:201 +0x123
rolltop/backend/search.(*Service).commitBatch.func1()
	/home/rolltop/backend/search/search.go:850 +0x99
`)
	filtered := filterBleveBatchStack(stack)
	for _, want := range []string{
		"github.com/blevesearch/bleve/v2/index/scorch.(*Scorch).Batch",
		"/index/scorch/scorch.go:201",
		"rolltop/backend/search.(*Service).commitBatch.func1",
	} {
		if !strings.Contains(filtered, want) {
			t.Fatalf("filtered stack missing %q: %q", want, filtered)
		}
	}
	for _, unwanted := range []string{"goroutine", "private message body", "0xdeadbeef", "+0x123"} {
		if strings.Contains(filtered, unwanted) {
			t.Fatalf("filtered stack retained %q: %q", unwanted, filtered)
		}
	}
	if len(filtered) > maxBleveStackBytes {
		t.Fatalf("filtered stack length = %d, want <= %d", len(filtered), maxBleveStackBytes)
	}
}

// The marker lands on the same volume the stalled writer is blocked on, so the
// two fail together: a full disk or a wedged mount takes the marker write down
// with the batch. Recovery therefore may not be conditional on the marker.
func TestActiveWriterStallRestartsWhenMarkerCannotBeWritten(t *testing.T) {
	root := filepath.Join(t.TempDir(), "users")
	svc, err := OpenPerUser(root)
	if err != nil {
		t.Fatal(err)
	}
	base, err := svc.indexForUser(23)
	if err != nil {
		t.Fatal(err)
	}
	blocking := &blockingBatchIndex{
		delegatedBleveIndex: base,
		started:             make(chan struct{}),
		release:             make(chan struct{}),
		finished:            make(chan struct{}),
	}
	svc.mu.Lock()
	svc.indexes[23] = blocking
	svc.mu.Unlock()
	t.Cleanup(func() {
		blocking.unblock()
		_ = svc.Close()
	})

	// A directory where the marker file belongs fails every write onto it, for
	// root as much as for anyone else -- which a permission bit would not.
	markerPath, _, err := svc.searchIndexRecoveryMarkerPath(23, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(markerPath, 0o700); err != nil {
		t.Fatal(err)
	}

	svc.activeStallAfter = 20 * time.Millisecond
	logs := &capturedBleveLogs{}
	svc.bleveErrorLog = logs.Printf
	stalledUsers := make(chan int64, 2)
	svc.SetActiveWriterStallHandler(func(userID int64) {
		stalledUsers <- userID
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		_ = svc.IndexMessage(ctx, store.MessageRecord{
			ID: 77, UserID: 23, AccountID: 1, MailboxID: 2,
			Subject: "stalled", BodyText: "stalled", Date: time.Now(),
		}, nil)
	}()
	select {
	case <-blocking.started:
	case <-time.After(time.Second):
		t.Fatal("Bleve batch did not start")
	}

	select {
	case userID := <-stalledUsers:
		if userID != 23 {
			t.Fatalf("stalled user = %d, want 23", userID)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("watchdog did not signal a restart after the marker write failed")
	}

	const stallLine = `bleve active writer stalled operation="index-batch"`
	var output string
	for deadline := time.Now().Add(2 * time.Second); ; {
		output = logs.String()
		if strings.Contains(output, stallLine) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("stall diagnostics missing %q: %q", stallLine, output)
		}
		time.Sleep(time.Millisecond)
	}
	// The operator has to be able to tell the two apart: the process is
	// restarting, and it is going to come back without a repair scope.
	for _, want := range []string{`marker_written=false`, `restart_required=true`} {
		if !strings.Contains(output, want) {
			t.Fatalf("stall diagnostics missing %q: %q", want, output)
		}
	}
}

// A volume in the state that stalls the writer does not return errors, it
// stops answering: a wedged mount blocks in CreateTemp, in the fsync, or in the
// rename. So the restart must be asked for before anything touches /data --
// holding the marker lock here is that write never returning.
func TestActiveWriterStallRestartsWhenTheMarkerWriteBlocks(t *testing.T) {
	root := filepath.Join(t.TempDir(), "users")
	svc, err := OpenPerUser(root)
	if err != nil {
		t.Fatal(err)
	}
	base, err := svc.indexForUser(31)
	if err != nil {
		t.Fatal(err)
	}
	blocking := &blockingBatchIndex{
		delegatedBleveIndex: base,
		started:             make(chan struct{}),
		release:             make(chan struct{}),
		finished:            make(chan struct{}),
	}
	svc.mu.Lock()
	svc.indexes[31] = blocking
	svc.mu.Unlock()
	t.Cleanup(func() {
		blocking.unblock()
		_ = svc.Close()
	})

	// Every marker write goes through this lock, so holding it is a marker
	// write that never comes back -- which is what a hung mount looks like from
	// in here, and what an error-returning fake could not reproduce.
	recoveryMarkerMu.Lock()
	markerReleased := false
	releaseMarker := func() {
		if !markerReleased {
			markerReleased = true
			recoveryMarkerMu.Unlock()
		}
	}
	t.Cleanup(releaseMarker)

	svc.activeStallAfter = 20 * time.Millisecond
	svc.bleveErrorLog = (&capturedBleveLogs{}).Printf
	stalledUsers := make(chan int64, 2)
	svc.SetActiveWriterStallHandler(func(userID int64) {
		stalledUsers <- userID
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		_ = svc.IndexMessage(ctx, store.MessageRecord{
			ID: 88, UserID: 31, AccountID: 1, MailboxID: 2,
			Subject: "stalled", BodyText: "stalled", Date: time.Now(),
		}, nil)
	}()
	select {
	case <-blocking.started:
	case <-time.After(time.Second):
		t.Fatal("Bleve batch did not start")
	}

	select {
	case userID := <-stalledUsers:
		if userID != 31 {
			t.Fatalf("stalled user = %d, want 31", userID)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the restart was never asked for while the marker write was blocked")
	}

	// Let the marker write finish so the watchdog goroutine is not left holding
	// the package lock for whatever test runs next.
	releaseMarker()
}
