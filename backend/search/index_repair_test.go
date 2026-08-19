// File overview: Tests for recovering a per-user index that no longer opens.

package search

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
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

func TestRebuildPerUserIndexMovesLiveIndexAside(t *testing.T) {
	root := t.TempDir()
	service, err := OpenPerUser(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.Close() })
	if _, err := service.indexForUser(11); err != nil {
		t.Fatal(err)
	}

	quarantine, err := service.RebuildPerUserIndex(context.Background(), 11)
	if err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	if quarantine.QuarantinePath == "" {
		t.Fatal("rebuild reported no quarantine")
	}
	if _, err := os.Stat(quarantine.QuarantinePath); err != nil {
		t.Fatalf("quarantined index is missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "11", LiveIndexDirName)); !os.IsNotExist(err) {
		t.Fatalf("live index still present after rebuild: %v", err)
	}
	// The next write opens a fresh index in the place the old one left.
	if _, err := service.indexForUser(11); err != nil {
		t.Fatalf("open index after rebuild: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "11", LiveIndexDirName)); err != nil {
		t.Fatalf("replacement index was not created: %v", err)
	}
}

func TestRebuildPerUserIndexWithoutIndexIsNotAnError(t *testing.T) {
	root := t.TempDir()
	service, err := OpenPerUser(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.Close() })

	quarantine, err := service.RebuildPerUserIndex(context.Background(), 12)
	if err != nil {
		t.Fatalf("rebuild without an index: %v", err)
	}
	if quarantine.QuarantinePath != "" {
		t.Fatalf("quarantine = %q, want none", quarantine.QuarantinePath)
	}
}
