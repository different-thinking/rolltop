package search

import (
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
)

func openMarkerService(t *testing.T) (*Service, string) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "users")
	service, err := OpenPerUser(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.Close() })
	return service, root
}

func TestSearchIndexRecoveryMarkerRoundTripsDocumentRange(t *testing.T) {
	service, _ := openMarkerService(t)
	if err := service.MarkSearchIndexRecoveryRequiredForDocuments(4, 162614, 162620); err != nil {
		t.Fatal(err)
	}
	recovery, err := service.SearchIndexRecoveryPlan(4)
	if err != nil {
		t.Fatal(err)
	}
	if !recovery.Targeted() || recovery.FirstDocumentID != 162614 || recovery.LastDocumentID != 162620 {
		t.Fatalf("recovery = %+v, want the recorded 162614-162620 range", recovery)
	}
	if scope := recovery.Scope(); scope != "documents:162614-162620" {
		t.Fatalf("scope = %q, want the document range", scope)
	}
}

// A batch that names no documents is the only thing that still costs a rebuild.
func TestSearchIndexRecoveryMarkerWithoutRangeAsksForFullRebuild(t *testing.T) {
	service, _ := openMarkerService(t)
	if err := service.MarkSearchIndexRecoveryRequiredForDocuments(5, 0, 0); err != nil {
		t.Fatal(err)
	}
	recovery, err := service.SearchIndexRecoveryPlan(5)
	if err != nil {
		t.Fatal(err)
	}
	if !recovery.Required || recovery.Targeted() {
		t.Fatalf("recovery = %+v, want a required full rebuild", recovery)
	}
	if scope := recovery.Scope(); scope != "full-rebuild" {
		t.Fatalf("scope = %q, want full-rebuild", scope)
	}
}

// Two stalls in one process must not let the second shrink what the first
// recorded, or the messages of the earlier batch are never reindexed.
func TestSearchIndexRecoveryMarkerWidensAndNeverNarrows(t *testing.T) {
	service, _ := openMarkerService(t)
	if err := service.MarkSearchIndexRecoveryRequiredForDocuments(6, 500, 600); err != nil {
		t.Fatal(err)
	}
	if err := service.MarkSearchIndexRecoveryRequiredForDocuments(6, 200, 300); err != nil {
		t.Fatal(err)
	}
	recovery, err := service.SearchIndexRecoveryPlan(6)
	if err != nil {
		t.Fatal(err)
	}
	if recovery.FirstDocumentID != 200 || recovery.LastDocumentID != 600 {
		t.Fatalf("recovery = %+v, want the union 200-600", recovery)
	}
	// A full rebuild outranks any range, and nothing may take it back.
	if err := service.MarkSearchIndexRecoveryRequired(6); err != nil {
		t.Fatal(err)
	}
	if recovery, err = service.SearchIndexRecoveryPlan(6); err != nil {
		t.Fatal(err)
	} else if recovery.Targeted() {
		t.Fatalf("recovery = %+v, want the full rebuild to survive", recovery)
	}
	if err := service.MarkSearchIndexRecoveryRequiredForDocuments(6, 200, 300); err != nil {
		t.Fatal(err)
	}
	if recovery, err = service.SearchIndexRecoveryPlan(6); err != nil {
		t.Fatal(err)
	} else if recovery.Targeted() {
		t.Fatalf("recovery = %+v, want a range not to replace a full rebuild", recovery)
	}
}

// Markers written by an earlier build carry no payload and must keep meaning
// what they meant then: rebuild everything.
func TestSearchIndexRecoveryMarkerFromEarlierBuildAsksForFullRebuild(t *testing.T) {
	service, root := openMarkerService(t)
	userDir := filepath.Join(root, strconv.FormatInt(7, 10))
	if err := os.MkdirAll(userDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(userDir, searchIndexRecoveryMarker),
		[]byte(searchIndexRecoveryHeaderV1+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	recovery, err := service.SearchIndexRecoveryPlan(7)
	if err != nil {
		t.Fatal(err)
	}
	if !recovery.Required || recovery.Targeted() {
		t.Fatalf("recovery = %+v, want a required full rebuild", recovery)
	}
}

// Clearing a marker that could not be made durable has to restore the same
// recovery, not a broader one.
func TestClearSearchIndexRecoveryPreservesRangeWhenRestoring(t *testing.T) {
	service, _ := openMarkerService(t)
	if err := service.MarkSearchIndexRecoveryRequiredForDocuments(8, 900, 950); err != nil {
		t.Fatal(err)
	}
	err := service.clearSearchIndexRecoveryRequiredWithSync(8, func(string) error {
		return os.ErrPermission
	})
	if err == nil {
		t.Fatal("clear reported success despite a failed directory sync")
	}
	recovery, planErr := service.SearchIndexRecoveryPlan(8)
	if planErr != nil {
		t.Fatal(planErr)
	}
	if !recovery.Targeted() || recovery.FirstDocumentID != 900 || recovery.LastDocumentID != 950 {
		t.Fatalf("restored recovery = %+v, want the original 900-950 range", recovery)
	}
}

// An abandoned index close must schedule recovery only for the tenant whose
// writer is still inside Bleve. Marking every tenant queues a full reindex for
// accounts whose index was published in full — hours of work to repair nothing,
// and the load that produces the next slow commit.
func TestUnfinishedWriterRecoveriesNamesOnlyWritersInFlight(t *testing.T) {
	service, _ := openMarkerService(t)
	stalled := service.writerForUser(11)
	stalled.setActive(bleveErrorContext{
		Operation: "index-batch", UserID: 11, FirstDocumentID: 162614, LastDocumentID: 162620,
	})
	finished := service.writerForUser(12)
	finished.setActive(bleveErrorContext{Operation: "index-batch", UserID: 12})
	finished.clearActive()

	recoveries := service.UnfinishedWriterRecoveries()
	if len(recoveries) != 1 {
		t.Fatalf("recoveries = %+v, want only the writer in flight", recoveries)
	}
	recovery, ok := recoveries[11]
	if !ok || !recovery.Targeted() || recovery.FirstDocumentID != 162614 || recovery.LastDocumentID != 162620 {
		t.Fatalf("recovery for the stalled tenant = %+v (present=%t), want its 162614-162620 range", recovery, ok)
	}
	if _, ok := recoveries[12]; ok {
		t.Fatal("a tenant whose writer returned was scheduled for recovery")
	}
}

// searchRecoveryRequired keeps the two-value shape these tests were written
// around. The production accessor returns the whole plan, deliberately: a
// boolean form alongside it would let a caller drop the document range that
// decides between an in-place repair and a full rebuild.
func searchRecoveryRequired(service *Service, userID int64) (bool, error) {
	recovery, err := service.SearchIndexRecoveryPlan(userID)
	return recovery.Required, err
}

// A header this build does not know means a payload written under rules it does
// not know. Reading the document line anyway would run a repair against bounds
// that may no longer mean what they say here.
func TestSearchIndexRecoveryMarkerFromLaterBuildAsksForFullRebuild(t *testing.T) {
	service, root := openMarkerService(t)
	userDir := filepath.Join(root, "9")
	if err := os.MkdirAll(userDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(userDir, searchIndexRecoveryMarker),
		[]byte("rolltop-search-recovery-v3\ndocuments 100 200\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	recovery, err := service.SearchIndexRecoveryPlan(9)
	if err != nil {
		t.Fatal(err)
	}
	if !recovery.Required || recovery.Targeted() {
		t.Fatalf("recovery = %+v, want an unknown version to degrade to a full rebuild", recovery)
	}
}

// An index that is gone must not be "verified" by creating an empty one: the
// targeted repair would reindex its own range, clear the marker, and leave every
// other message flagged as indexed but absent.
func TestVerifyPerUserIndexOpensRejectsAMissingIndex(t *testing.T) {
	service, root := openMarkerService(t)
	if err := os.MkdirAll(filepath.Join(root, "13"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := service.VerifyPerUserIndexOpens(13); err == nil {
		t.Fatal("verification accepted a tenant with no live index")
	}
	if _, err := os.Stat(filepath.Join(root, "13", liveIndexDirName)); !os.IsNotExist(err) {
		t.Fatalf("verification created an index it was asked to check: %v", err)
	}
}

// The stall watchdog and the abandoned-close hook can publish a marker for the
// same tenant at once: the hook fires when Close timed out, which is the same
// situation that leaves a watchdog running. Without serialization the later
// rename drops the range the earlier one had just widened the marker with.
func TestConcurrentMarkerWritesKeepEveryWidenedRange(t *testing.T) {
	service, _ := openMarkerService(t)
	if err := service.MarkSearchIndexRecoveryRequiredForDocuments(21, 100, 200); err != nil {
		t.Fatal(err)
	}

	ranges := [][2]int64{{300, 400}, {500, 600}, {700, 800}, {900, 1000}}
	errs := make(chan error, len(ranges))
	start := make(chan struct{})
	var wait sync.WaitGroup
	for _, span := range ranges {
		wait.Add(1)
		go func(first, last int64) {
			defer wait.Done()
			<-start
			errs <- service.MarkSearchIndexRecoveryRequiredForDocuments(21, first, last)
		}(span[0], span[1])
	}
	close(start)
	wait.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}

	recovery, err := service.SearchIndexRecoveryPlan(21)
	if err != nil {
		t.Fatal(err)
	}
	if recovery.FirstDocumentID != 100 || recovery.LastDocumentID != 1000 {
		t.Fatalf("recovery = %+v, want every concurrent range merged into 100-1000", recovery)
	}
}
