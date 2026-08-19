package gravatar

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"rolltop/backend/store/storetest"
)

func TestGetImageMetaReturnsScopedMetadata(t *testing.T) {
	ctx := context.Background()
	store, err := storetest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	db := store.DB()
	// The plugin's tables are part of the baseline the test database carries, so
	// its migrations are not re-run here. What the fixture still needs is the
	// user its rows reference.
	user, err := store.CreateUser(ctx, "plugin@example.test", "Plugin", "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	hash := Hash("sender@example.com")
	if err := UpsertImage(ctx, db, Image{
		UserID:      user.ID,
		EmailHash:   hash,
		ContentType: "image/png",
		Image:       []byte{1, 2, 3, 4},
		Status:      "ok",
		FetchedAt:   now,
		ExpiresAt:   now.Add(time.Hour),
		UpdatedAt:   now,
	}); err != nil {
		t.Fatal(err)
	}
	meta, err := GetImageMeta(ctx, db, user.ID, hash)
	if err != nil {
		t.Fatal(err)
	}
	if meta.EmailHash != hash || meta.ContentType != "image/png" || meta.Status != "ok" || !meta.HasImage {
		t.Fatalf("meta = %+v", meta)
	}
	if _, err := GetImageMeta(ctx, db, user.ID+1, hash); err != sql.ErrNoRows {
		t.Fatalf("other user err = %v, want %v", err, sql.ErrNoRows)
	}
}
