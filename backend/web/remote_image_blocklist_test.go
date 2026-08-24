// File overview: Blocked remote images stay addressed by the sender's URL, so
// the block rules can still recognise them.

package web

import (
	"context"
	"strings"
	"testing"
	"time"

	"rolltop/backend/blob"
	"rolltop/backend/remoteimages"
	"rolltop/backend/store"
	"rolltop/backend/store/storetest"
)

func TestCachedRemoteImageURLsLeavesBlockedImagesAddressedBySender(t *testing.T) {
	ctx := context.Background()
	db, err := storetest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	user, err := db.CreateUser(ctx, "blocklist@example.test", "Blocklist", "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{store: db, blobs: blob.New(t.TempDir())}
	tracker := "https://cdn.example.test/open.php?id=42"
	hero := "https://cdn.example.test/hero.png"
	cache := func(rawURL string) {
		t.Helper()
		if _, err := db.UpsertRemoteImageCache(ctx, store.RemoteImageCache{
			UserID: user.ID, URLHash: remoteimages.Hash(rawURL), URL: rawURL,
			BlobPath: "users/blobs/remote/" + remoteimages.Hash(rawURL), ContentType: "image/png",
			Size: 64, Status: store.RemoteImageStatusOK, FetchedAt: time.Now(),
			ExpiresAt: time.Now().Add(24 * time.Hour),
		}); err != nil {
			t.Fatal(err)
		}
	}
	cache(tracker)
	cache(hero)

	body := `<img src="` + tracker + `"><img src="` + hero + `">`
	msg := store.MessageRecord{UserID: user.ID, MessageIDHeader: "<blocklist@example.test>", FromAddr: "sender@example.test"}

	// A rule added after both images were cached still has to reach the
	// tracker: it names the URL the sender wrote, which is only in the body for
	// as long as the image is not rewritten onto Rolltop's cache route.
	rules := []string{`(?i)/open\.php`}
	urls := server.cachedRemoteImageURLs(ctx, user.ID, msg, body, rules)
	if _, ok := urls[remoteimages.Hash(tracker)]; ok {
		t.Fatalf("blocked image was rewritten onto the cache route: %v", urls)
	}
	if urls[remoteimages.Hash(hero)] != remoteimages.CachedURL(remoteimages.Hash(hero)) {
		t.Fatalf("cached image was not served from the cache: %v", urls)
	}

	doc := emailDocumentWithInlineAttachments(remoteimages.ReplaceCached(body, urls), "", true, rules, nil)
	if strings.Contains(doc, "open.php") {
		t.Fatalf("blocked image survived the document: %s", doc)
	}
	if !strings.Contains(doc, remoteimages.CachedURL(remoteimages.Hash(hero))) {
		t.Fatalf("cached image was dropped: %s", doc)
	}

	// With no rules, both are served from the cache as before.
	all := server.cachedRemoteImageURLs(ctx, user.ID, msg, body, nil)
	if len(all) != 2 {
		t.Fatalf("cached URLs without rules = %v, want both", all)
	}
}
