// File overview: Tests for recovering a per-user index that no longer opens.

package search

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/blevesearch/bleve/v2"
	bolt "go.etcd.io/bbolt"
)

// corruptPerUserIndex truncates the scorch root file the way an incomplete
// volume copy does, which is what produced "invalid database" in production.
func corruptPerUserIndex(t *testing.T, root string, userID int64) string {
	t.Helper()
	path := filepath.Join(root, strconv.FormatInt(userID, 10), LiveIndexDirName)
	index, err := openIndex(path)
	if err != nil {
		t.Fatalf("create index to corrupt: %v", err)
	}
	if err := index.Close(); err != nil {
		t.Fatalf("close index to corrupt: %v", err)
	}
	// scorch keeps its root file inside the store directory, and bbolt only
	// rejects a file large enough to hold meta pages - it silently reinitialises
	// a shorter one, which is not the failure being reproduced here.
	rootBolt := filepath.Join(path, "store", "root.bolt")
	if err := os.WriteFile(rootBolt, bytes.Repeat([]byte{0xAB}, 16384), 0o600); err != nil {
		t.Fatalf("corrupt %s: %v", rootBolt, err)
	}
	return path
}

func TestIsIndexCorruptionErrorMatchesDamagedFiles(t *testing.T) {
	for _, err := range []error{bolt.ErrInvalid, bolt.ErrVersionMismatch, bolt.ErrChecksum,
		bleve.ErrorIndexMetaMissing, bleve.ErrorIndexMetaCorrupt} {
		if !IsIndexCorruptionError(err) {
			t.Fatalf("%v is not treated as corruption", err)
		}
		if !IsIndexCorruptionError(errors.Join(errors.New("open Bleve index"), err)) {
			t.Fatalf("wrapped %v is not treated as corruption", err)
		}
	}
}

func TestIsIndexCorruptionErrorLeavesTransientFailuresAlone(t *testing.T) {
	for _, err := range []error{
		nil,
		bolt.ErrTimeout, // another process holds the index lock
		os.ErrPermission,
		errors.New("too many open files"),
		bleve.ErrorIndexPathDoesNotExist,
	} {
		if IsIndexCorruptionError(err) {
			t.Fatalf("%v is treated as corruption", err)
		}
	}
}

func TestOpenCorruptIndexQuarantinesAndRebuilds(t *testing.T) {
	root := t.TempDir()
	path := corruptPerUserIndex(t, root, 7)

	service, err := OpenPerUser(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.Close() })
	var scheduled []int64
	service.SetCorruptIndexHandler(func(userID int64) error {
		scheduled = append(scheduled, userID)
		return nil
	})

	index, err := service.indexForUser(7)
	if err != nil {
		t.Fatalf("open corrupt index: %v", err)
	}
	if index == nil {
		t.Fatal("no replacement index")
	}
	if len(scheduled) != 1 || scheduled[0] != 7 {
		t.Fatalf("reindex scheduled for %v, want [7]", scheduled)
	}
	entries, err := os.ReadDir(filepath.Join(root, "7"))
	if err != nil {
		t.Fatal(err)
	}
	quarantined := 0
	live := false
	for _, entry := range entries {
		switch {
		case entry.Name() == LiveIndexDirName:
			live = true
		case len(entry.Name()) > len(quarantineDirPrefix()) && entry.Name()[:len(quarantineDirPrefix())] == quarantineDirPrefix():
			quarantined++
		}
	}
	if !live || quarantined != 1 {
		t.Fatalf("live index=%t quarantined=%d after repair", live, quarantined)
	}
	if _, err := os.Stat(filepath.Join(path, "store", "root.bolt")); err != nil {
		t.Fatalf("replacement index is not usable: %v", err)
	}
}

// Rebuilding without a handler would leave an empty index and rows still marked
// indexed, so the damaged index stays where it is and the error is reported.
func TestOpenCorruptIndexKeepsIndexWhenNoReindexIsPossible(t *testing.T) {
	root := t.TempDir()
	corruptPerUserIndex(t, root, 3)

	service, err := OpenPerUser(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.Close() })

	if _, err := service.indexForUser(3); err == nil {
		t.Fatal("corrupt index opened without a reindex handler")
	}
	entries, err := os.ReadDir(filepath.Join(root, "3"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != LiveIndexDirName {
		t.Fatalf("index directory = %v, want the damaged index untouched", entries)
	}
}

func TestOpenCorruptIndexKeepsIndexWhenReindexCannotBeScheduled(t *testing.T) {
	root := t.TempDir()
	corruptPerUserIndex(t, root, 4)

	service, err := OpenPerUser(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.Close() })
	service.SetCorruptIndexHandler(func(int64) error { return errors.New("database unavailable") })

	if _, err := service.indexForUser(4); err == nil {
		t.Fatal("corrupt index quarantined although no rows could be queued")
	}
	entries, err := os.ReadDir(filepath.Join(root, "4"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != LiveIndexDirName {
		t.Fatalf("index directory = %v, want the damaged index untouched", entries)
	}
}

// Two goroutines racing on the same damaged index must both end up with the
// one live handle. Opening it twice and closing the loser is how the surviving
// tenant's search dies with "index is closed" until the process restarts.
func TestConcurrentOpenOfCorruptIndexSharesOneHandle(t *testing.T) {
	root := t.TempDir()
	corruptPerUserIndex(t, root, 9)

	service, err := OpenPerUser(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.Close() })
	var repairs atomic.Int64
	service.SetCorruptIndexHandler(func(int64) error {
		repairs.Add(1)
		return nil
	})

	const racers = 8
	indexes := make([]bleve.Index, racers)
	errs := make([]error, racers)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range racers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			indexes[i], errs[i] = service.indexForUser(9)
		}()
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("racer %d failed to open the repaired index: %v", i, err)
		}
		if indexes[i] != indexes[0] {
			t.Fatalf("racer %d received a second handle for the same index", i)
		}
	}
	if got := repairs.Load(); got != 1 {
		t.Fatalf("repairs = %d, want exactly one", got)
	}
	// The surviving handle must still be usable: a closed one answers every
	// search with "index is closed" for as long as the process lives.
	if _, err := indexes[0].DocCount(); err != nil {
		t.Fatalf("shared index handle is not usable: %v", err)
	}
}

// The same race without corruption: an ordinary first open from several
// goroutines must also produce exactly one handle.
func TestConcurrentFirstOpenSharesOneHandle(t *testing.T) {
	service, err := OpenPerUser(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.Close() })

	const racers = 8
	indexes := make([]bleve.Index, racers)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range racers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			index, err := service.indexForUser(21)
			if err != nil {
				t.Errorf("racer %d: %v", i, err)
				return
			}
			indexes[i] = index
		}()
	}
	close(start)
	wg.Wait()

	for i := range indexes {
		if indexes[i] != indexes[0] {
			t.Fatalf("racer %d received a second handle for the same index", i)
		}
	}
	if _, err := indexes[0].DocCount(); err != nil {
		t.Fatalf("shared index handle is not usable: %v", err)
	}
}
