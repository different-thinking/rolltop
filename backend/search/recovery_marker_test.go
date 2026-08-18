package search

import (
	"os"
	"path/filepath"
	"strconv"
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
