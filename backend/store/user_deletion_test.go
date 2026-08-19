// File overview: What deleting a user has to take with it.

package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestDeleteUserRemovesTenantDirectory pins the half of a deletion that is not
// in the database. The rows cascade away on their own; the raw .eml blobs and
// the Bleve index over their text sit on the volume, and nothing revisits a
// directory whose user is gone — the retention loops iterate ServiceableUsers.
func TestDeleteUserRemovesTenantDirectory(t *testing.T) {
	ctx := context.Background()
	db := mustOpenTestStore(t)
	user, err := db.CreateUser(ctx, "deleted@example.test", "Deleted", "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	dir := db.UserDataDir(user.ID)
	if dir == "" {
		t.Fatal("the test store has no data directory")
	}
	if err := os.MkdirAll(filepath.Join(dir, "blobs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "blobs", "1.eml"), []byte("From: someone\r\n\r\nbody"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := db.DeleteUser(ctx, user.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("the tenant directory survived the deletion: stat %s = %v", dir, err)
	}
	if _, err := db.GetUserByID(ctx, user.ID); !IsNotFound(err) {
		t.Fatalf("GetUserByID after delete = %v, want not found", err)
	}
}

// TestDeleteUserReportsMissingUser keeps the not-found answer ahead of the
// directory removal: a wrong id must not delete anything on disk.
func TestDeleteUserReportsMissingUser(t *testing.T) {
	ctx := context.Background()
	db := mustOpenTestStore(t)
	if err := db.DeleteUser(ctx, 999_999); !IsNotFound(err) {
		t.Fatalf("DeleteUser(missing) = %v, want not found", err)
	}
}
