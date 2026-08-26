package main

import (
	"context"
	"database/sql"
	"log"
	"regexp"
	"sync"

	"rolltop/backend/plugins"
	"rolltop/plugins/remote_image_blocklist/rules"
)

// compiledPatternCache memoizes regexp.Compile across image fetches. The
// blocklist changes only on an admin edit, but AllowRemoteImageFetch runs on
// every remote image, so recompiling every stored pattern on each fetch was
// pure repeated work. Entries are keyed by the pattern text; a nil value marks
// a pattern that failed to compile so a broken row is logged and skipped once
// rather than on every fetch. ReplaceRules now rejects uncompilable patterns at
// write time, so a nil entry should only ever come from a row predating that
// check. The set is bounded by the admin-maintained rule count, so it does not
// grow without bound.
var (
	compiledPatternMu    sync.RWMutex
	compiledPatternCache = map[string]*regexp.Regexp{}
)

func compiledPattern(pattern string) *regexp.Regexp {
	compiledPatternMu.RLock()
	re, ok := compiledPatternCache[pattern]
	compiledPatternMu.RUnlock()
	if ok {
		return re
	}
	compiledPatternMu.Lock()
	defer compiledPatternMu.Unlock()
	if re, ok := compiledPatternCache[pattern]; ok {
		return re
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		log.Printf("remote_image_blocklist: skipping invalid pattern %q: %v", pattern, err)
		re = nil
	}
	compiledPatternCache[pattern] = re
	return re
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
	for _, pattern := range patterns {
		re := compiledPattern(pattern)
		if re == nil {
			continue
		}
		if re.MatchString(req.URL) {
			return plugins.RemoteImageFetchDecision{Allow: false, Reason: "remote image blocklist"}, nil
		}
	}
	return plugins.RemoteImageFetchDecision{Allow: true}, nil
}
