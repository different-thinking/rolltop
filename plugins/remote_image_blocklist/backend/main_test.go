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

func TestReplaceRulesRejectsInvalidPattern(t *testing.T) {
	ctx := context.Background()
	db, err := storetest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// A valid rule first, so we can prove the failed replace left it untouched.
	if err := rules.ReplaceRules(ctx, db.DB(), []string{`tracker\.example\.test`}); err != nil {
		t.Fatal(err)
	}
	if err := rules.ReplaceRules(ctx, db.DB(), []string{`valid\.test`, `(unclosed`}); err == nil {
		t.Fatal("ReplaceRules accepted an uncompilable pattern, want error")
	}
	// The whole replace is one transaction, so the rejected batch must not have
	// deleted the existing rule.
	patterns, err := rules.ListPatterns(ctx, db.DB())
	if err != nil {
		t.Fatal(err)
	}
	if len(patterns) != 1 || patterns[0] != `tracker\.example\.test` {
		t.Fatalf("patterns = %v, want the original rule preserved", patterns)
	}
}
