// File overview: What the admin volume card measures.

package web

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeFileOfSize(t *testing.T, path string, size int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, make([]byte, size), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestMeasureVolumeUsageSplitsBlobsFromIndexes is the whole point of the card.
// The per-tenant Bleve index lives under users/<id>/bleve, inside the same tree
// as the blobs, so measuring "users/" as blob bytes folds the index into the
// blob figure and leaves the index figure at zero — pointing an operator
// diagnosing a full volume at the wrong consumer.
func TestMeasureVolumeUsageSplitsBlobsFromIndexes(t *testing.T) {
	dir := t.TempDir()
	writeFileOfSize(t, filepath.Join(dir, "users", "1", "blobs", "a.eml"), 100)
	writeFileOfSize(t, filepath.Join(dir, "users", "1", "blobs", "nested", "b.eml"), 50)
	writeFileOfSize(t, filepath.Join(dir, "users", "1", "bleve", "store", "root.bolt"), 30)
	writeFileOfSize(t, filepath.Join(dir, "users", "2", "blobs", "c.eml"), 7)
	writeFileOfSize(t, filepath.Join(dir, "users", "2", "bleve", "001.zap"), 3)
	// A quarantined index is reclaimable space, so it is neither of the two.
	writeFileOfSize(t, filepath.Join(dir, "users", "2", "bleve.quarantine-20260101T000000.000000000Z", "old.zap"), 11)
	// Not a tenant directory: left out rather than guessed at.
	writeFileOfSize(t, filepath.Join(dir, "users", "notanid", "stray"), 999)

	usage, err := measureVolumeUsage(dir)
	if err != nil {
		t.Fatal(err)
	}
	if usage.BlobBytes != 157 {
		t.Errorf("blob bytes = %d, want 157", usage.BlobBytes)
	}
	if usage.IndexBytes != 33 {
		t.Errorf("index bytes = %d, want 33", usage.IndexBytes)
	}
	if usage.OtherBytes != 11 {
		t.Errorf("other bytes = %d, want 11 (the quarantined index)", usage.OtherBytes)
	}
	if usage.MeasuredAt.IsZero() {
		t.Error("the measurement carries no timestamp, so the page cannot say how old it is")
	}
}

// TestMeasureVolumeUsageTreatsAnUnsyncedInstallAsZero keeps a fresh install
// from reporting an error on a page whose job is to say what is wrong.
func TestMeasureVolumeUsageTreatsAnUnsyncedInstallAsZero(t *testing.T) {
	usage, err := measureVolumeUsage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if usage.BlobBytes != 0 || usage.IndexBytes != 0 || usage.OtherBytes != 0 {
		t.Errorf("an empty data directory measured %+v", usage)
	}
	if usage.MeasuredAt.IsZero() {
		t.Error("an empty measurement still has to count as measured")
	}
}

// TestCachedVolumeUsageDoesNotWalkOnTheRequestPath pins the reason the walk was
// moved off it: the admin page polls every 15 seconds inside a five-second
// budget it shares with the database status query, and a volume with a few
// hundred thousand blobs spent the whole budget here — reporting a perfectly
// healthy database as unreachable.
func TestCachedVolumeUsageDoesNotWalkOnTheRequestPath(t *testing.T) {
	dir := t.TempDir()
	writeFileOfSize(t, filepath.Join(dir, "users", "1", "blobs", "a.eml"), 64)
	server := &Server{dataDir: dir}

	// The first call returns the zero measurement and starts the walk.
	if first := server.cachedVolumeUsage(); !first.MeasuredAt.IsZero() {
		t.Fatalf("the first call blocked on a measurement: %+v", first)
	}

	deadline := time.Now().Add(5 * time.Second)
	var usage volumeUsage
	for time.Now().Before(deadline) {
		usage = server.cachedVolumeUsage()
		if !usage.MeasuredAt.IsZero() {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if usage.MeasuredAt.IsZero() {
		t.Fatal("the background measurement never finished")
	}
	if usage.BlobBytes != 64 {
		t.Errorf("blob bytes = %d, want 64", usage.BlobBytes)
	}

	// And a later call serves the cache rather than starting another walk.
	again := server.cachedVolumeUsage()
	if !again.MeasuredAt.Equal(usage.MeasuredAt) {
		t.Errorf("a fresh measurement was re-run: %s then %s", usage.MeasuredAt, again.MeasuredAt)
	}
}
