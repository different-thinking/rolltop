// File overview: A blob record and its physical .eml file must never outlive
// whatever they were saved for -- storeSentMessage's cleanup helper is what
// reclaims both once nothing local references them any more.

package web

import (
	"context"
	"testing"

	"rolltop/backend/blob"
	"rolltop/backend/store"
	"rolltop/backend/store/storetest"
)

func TestCleanupOrphanedBlobRemovesRowAndFile(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db, err := storetest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	user, err := db.CreateUser(ctx, "me@example.test", "Me", "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	blobStore := blob.New(dir)
	saved, err := blobStore.SaveRawMessage(user.ID, 1, "Sent", 1, []byte("raw message"))
	if err != nil {
		t.Fatal(err)
	}
	blobRec, err := db.CreateBlob(ctx, store.BlobRecord{
		UserID: user.ID, Kind: "message", Path: saved.Path, SHA256: saved.SHA256, Size: saved.Size,
	})
	if err != nil {
		t.Fatal(err)
	}

	server := &Server{store: db, blobs: blobStore}
	if err := server.cleanupOrphanedBlob(ctx, user.ID, blobRec.ID, saved.Path); err != nil {
		t.Fatal(err)
	}

	if _, err := db.GetBlobForUser(ctx, user.ID, blobRec.ID); !store.IsNotFound(err) {
		t.Fatalf("blob row lookup err = %v, want not found after cleanup", err)
	}
	if _, err := blobStore.OpenUserBlob(user.ID, saved.Path); err == nil {
		t.Fatal("raw .eml file still exists after cleanup")
	}
}

// A second cleanup for the same blob must not error: the CreateMessage
// failure path joins this into an already-real error, and a cleanup that
// panics or errors on an already-gone row would obscure the original failure
// it is trying to report alongside.
func TestCleanupOrphanedBlobIsSafeToRetry(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db, err := storetest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	user, err := db.CreateUser(ctx, "me@example.test", "Me", "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	blobStore := blob.New(dir)
	saved, err := blobStore.SaveRawMessage(user.ID, 1, "Sent", 1, []byte("raw message"))
	if err != nil {
		t.Fatal(err)
	}
	blobRec, err := db.CreateBlob(ctx, store.BlobRecord{
		UserID: user.ID, Kind: "message", Path: saved.Path, SHA256: saved.SHA256, Size: saved.Size,
	})
	if err != nil {
		t.Fatal(err)
	}

	server := &Server{store: db, blobs: blobStore}
	if err := server.cleanupOrphanedBlob(ctx, user.ID, blobRec.ID, saved.Path); err != nil {
		t.Fatal(err)
	}
	if err := server.cleanupOrphanedBlob(ctx, user.ID, blobRec.ID, saved.Path); err != nil {
		t.Fatalf("second cleanup err = %v, want nil", err)
	}
}
