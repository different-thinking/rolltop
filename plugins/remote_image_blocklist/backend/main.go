package main

import (
	"context"
	"database/sql"
	"log"
	"regexp"
	"strings"
	"sync"

	"rolltop/backend/plugins"
	"rolltop/plugins/remote_image_blocklist/rules"
)

// compiledBlocklist memoizes the compiled form of the whole blocklist across
// image fetches. The blocklist changes only on an admin edit, but
// AllowRemoteImageFetch runs on every remote image, so recompiling every stored
// pattern on each fetch was pure repeated work. It caches exactly one snapshot —
// the currently-stored set, keyed by the joined pattern text — and recompiles
// only when that set actually changes, so a rule edited or deleted replaces the
// snapshot rather than leaving a dead entry behind (the reason a per-pattern map
// was the wrong shape: it grew by every distinct pattern ever seen). Patterns
// that fail to compile are logged and dropped when the snapshot is built, not on
// every fetch; ReplaceRules rejects uncompilable patterns at write time, so that
// only happens for a row predating that check.
var (
	compiledBlocklistMu  sync.Mutex
	compiledBlocklistKey string
	compiledBlocklistSet []*regexp.Regexp
	compiledBlocklistOK  bool
)

func compiledBlockers(patterns []string) []*regexp.Regexp {
	key := strings.Join(patterns, "\n")
	compiledBlocklistMu.Lock()
	defer compiledBlocklistMu.Unlock()
	if compiledBlocklistOK && key == compiledBlocklistKey {
		return compiledBlocklistSet
	}
	set := make([]*regexp.Regexp, 0, len(patterns))
	for _, pattern := range patterns {
		re, err := regexp.Compile(pattern)
		if err != nil {
			log.Printf("remote_image_blocklist: skipping invalid pattern %q: %v", pattern, err)
			continue
		}
		set = append(set, re)
	}
	compiledBlocklistKey = key
	compiledBlocklistSet = set
	compiledBlocklistOK = true
	return set
}

// RolltopPlugin is the symbol loaded by plugin.Open.
func RolltopPlugin() plugins.BackendPlugin {
	return plugins.NoopBackendPlugin{PluginID: plugins.RemoteImageBlocklist}
}

type remoteImageBlocklistHook struct{}

// Compile-time proof that the hook satisfies the host interface. Without it a
// missing method (as SeedRemoteImageRules once was) compiles fine and only fails
// the runtime type assertion in the host — silently disabling the whole plugin
// (fail-open: every tracker URL allowed) with no log line.
var _ plugins.RemoteImageBlocklistHook = remoteImageBlocklistHook{}

func (remoteImageBlocklistHook) SeedRemoteImageRules(ctx context.Context, db *sql.DB) error {
	return rules.SeedRules(ctx, db)
}

func (remoteImageBlocklistHook) ListRemoteImageRules(ctx context.Context, db *sql.DB) ([]plugins.RemoteImageRule, error) {
	rows, err := rules.ListRules(ctx, db)
	if err != nil {
		return nil, err
	}
	out := make([]plugins.RemoteImageRule, 0, len(rows))
	for _, row := range rows {
		out = append(out, plugins.RemoteImageRule{Pattern: row.Pattern, Enabled: row.Enabled})
	}
	return out, nil
}

func (remoteImageBlocklistHook) ListRemoteImagePatterns(ctx context.Context, db *sql.DB) ([]string, error) {
	return rules.ListPatterns(ctx, db)
}

func (remoteImageBlocklistHook) ReplaceRemoteImageRules(ctx context.Context, db *sql.DB, patterns []string) error {
	return rules.ReplaceRules(ctx, db, patterns)
}

func (remoteImageBlocklistHook) AllowRemoteImageFetch(ctx context.Context, db *sql.DB, req plugins.RemoteImageFetchRequest) (plugins.RemoteImageFetchDecision, error) {
	patterns, err := rules.ListPatterns(ctx, db)
	if err != nil {
		return plugins.RemoteImageFetchDecision{}, err
	}
	for _, re := range compiledBlockers(patterns) {
		if re.MatchString(req.URL) {
			return plugins.RemoteImageFetchDecision{Allow: false, Reason: "remote image blocklist"}, nil
		}
	}
	return plugins.RemoteImageFetchDecision{Allow: true}, nil
}
