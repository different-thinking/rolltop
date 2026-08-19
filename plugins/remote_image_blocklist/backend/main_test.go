package main

import (
	"context"
	"testing"

	"rolltop/backend/plugins"
	"rolltop/backend/store/storetest"
	"rolltop/plugins/remote_image_blocklist/rules"
)

func TestAllowRemoteImageFetchDeniesMatchingRule(t *testing.T) {
	ctx := context.Background()
	db, err := storetest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := rules.ReplaceRules(ctx, db.DB(), []string{`tracker\.example\.test`}); err != nil {
		t.Fatal(err)
	}

	decision, err := (remoteImageBlocklistHook{}).AllowRemoteImageFetch(ctx, db.DB(), plugins.RemoteImageFetchRequest{
		URL: "https://tracker.example.test/pixel.png",
	})
	if err != nil {
		t.Fatal(err)
	}
	if decision.Allow {
		t.Fatalf("decision = %+v, want denied", decision)
	}
}
