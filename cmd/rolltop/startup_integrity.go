// File overview: Unclean-shutdown detection and the startup integrity check it
// triggers. Rolltop writes a marker while it runs and removes it only after the
// databases are closed, so the next start knows whether SQLite had a chance to
// checkpoint its WAL. A start that follows a kill, a host reset, or a power cut
// verifies the tenant files before serving, because that is when damage is
// cheapest to find: at startup with the repair command in the log, instead of
// hours later inside a sync run.

package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"rolltop/backend/config"
	"rolltop/backend/store"
)

const runningMarkerFilename = ".rolltop-running"

// runningMarkerPath is the file whose presence at startup means the previous
// run did not reach its clean shutdown.
func runningMarkerPath(dataDir string) string {
	return filepath.Join(dataDir, runningMarkerFilename)
}

// claimRunningMarker reports whether the previous run ended uncleanly and
// leaves the marker in place for this run. A marker that cannot be written is
// not fatal: it only costs the next start its automatic verification.
func claimRunningMarker(dataDir string) bool {
	path := runningMarkerPath(dataDir)
	_, statErr := os.Lstat(path)
	unclean := statErr == nil
	if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		log.Printf("inspect shutdown marker %s: %v", path, statErr)
	}
	if err := os.WriteFile(path, []byte(fmt.Sprintf("%d\n", os.Getpid())), 0o600); err != nil {
		log.Printf("write shutdown marker %s: %v", path, err)
	}
	return unclean
}

// releaseRunningMarker records that this process closed its databases. It must
// run after the store is closed, never before.
func releaseRunningMarker(dataDir string) {
	if err := os.Remove(runningMarkerPath(dataDir)); err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Printf("clear shutdown marker: %v", err)
	}
}

// startupIntegrityCheckRequired applies the configured policy to the state the
// marker reported.
func startupIntegrityCheckRequired(mode string, uncleanShutdown bool) bool {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case config.IntegrityCheckAlways:
		return true
	case config.IntegrityCheckNever:
		return false
	default:
		return uncleanShutdown
	}
}

// verifyUserDatabases runs quick_check against every tenant database and
// latches the damaged ones so background work stops retrying them and the log
// carries the repair command. A damaged file is reported, never repaired: the
// recovery is an explicit offline decision.
func verifyUserDatabases(ctx context.Context, db *store.Store, dataDir string, progress func(done, total int, detail string)) ([]int64, error) {
	users, err := db.ListUsers(ctx)
	if err != nil {
		return nil, fmt.Errorf("list users for startup integrity check: %w", err)
	}
	var damaged []int64
	for i, user := range users {
		if err := ctx.Err(); err != nil {
			return damaged, err
		}
		if progress != nil {
			progress(i, len(users), fmt.Sprintf("checking user %d", user.ID))
		}
		path := store.UserDatabaseFilePath(dataDir, user.ID)
		if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
			continue
		}
		problems, err := store.CheckDatabaseFile(ctx, path)
		if err != nil {
			// An unreadable file is a finding, not a reason to refuse to start
			// for the tenants whose data is intact.
			log.Printf("startup integrity check user_id=%d: %v", user.ID, err)
			continue
		}
		if len(problems) == 0 {
			continue
		}
		damaged = append(damaged, user.ID)
		log.Printf("startup integrity check user_id=%d: %v", user.ID, db.MarkCorrupt(user.ID, problems[0]))
		for _, problem := range problems[1:min(len(problems), maxLoggedIntegrityProblems)] {
			log.Printf("startup integrity check user_id=%d detail: %s", user.ID, problem)
		}
	}
	if progress != nil {
		progress(len(users), len(users), "databases verified")
	}
	return damaged, nil
}

// maxLoggedIntegrityProblems bounds how much of a quick_check report reaches
// the log; a badly damaged file can report thousands of lines.
const maxLoggedIntegrityProblems = 10
