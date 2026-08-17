// File overview: The repair itself, shared by "rolltop recover-db" and by the
// admin UI's scheduled repairs. Replacing a tenant's SQLite file requires that
// nothing holds a handle on it, which is true in two situations: the offline
// command, and startup before any user store is opened. Both call
// repairUserDatabase; runScheduledDatabaseRepairs is the startup half that
// consumes the markers the UI writes.

package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"rolltop/backend/plugins"
	"rolltop/backend/store"
)

// repairUserDatabase rebuilds one tenant database from the rows that are still
// readable. The damaged file is only moved aside once a recovered file exists,
// so a failure anywhere leaves the original in place.
func repairUserDatabase(ctx context.Context, dataDir string, userID int64, manifests []plugins.Manifest, now time.Time, progress func(string)) (store.RepairOutcome, error) {
	outcome := store.RepairOutcome{UserID: userID, StartedAt: now.UTC()}
	fail := func(err error) (store.RepairOutcome, error) {
		outcome.FinishedAt = time.Now().UTC()
		outcome.Error = err.Error()
		return outcome, err
	}

	databasePath := store.UserDatabaseFilePath(dataDir, userID)
	if _, err := os.Stat(databasePath); err != nil {
		return fail(fmt.Errorf("open user %d database %s: %w", userID, databasePath, err))
	}
	stamp := now.UTC().Format("20060102T150405.000000000Z")
	recoveredPath := databasePath + ".recovered-" + stamp
	quarantinePath := databasePath + ".corrupt-" + stamp
	for _, path := range []string{recoveredPath, quarantinePath} {
		if _, err := os.Lstat(path); err == nil {
			return fail(fmt.Errorf("recovery path already exists: %s", path))
		} else if !errors.Is(err, os.ErrNotExist) {
			return fail(fmt.Errorf("inspect recovery path %s: %w", path, err))
		}
	}

	report, err := store.SalvageUserDatabase(ctx, databasePath, recoveredPath, manifests, progress)
	outcome.Report = report
	if err != nil {
		removeSQLiteFileSet(recoveredPath)
		return fail(fmt.Errorf("recover user %d database: %w", userID, err))
	}
	if report.RowsCopied == 0 {
		removeSQLiteFileSet(recoveredPath)
		return fail(fmt.Errorf("recovered no rows from %s; the damaged file was left untouched", databasePath))
	}

	if err := quarantineSQLiteFileSet(databasePath, quarantinePath); err != nil {
		removeSQLiteFileSet(recoveredPath)
		return fail(err)
	}
	if err := os.Rename(recoveredPath, databasePath); err != nil {
		// Put the original back so no exit path leaves the tenant without a
		// database at the expected location.
		restoreErr := quarantineSQLiteFileSet(quarantinePath, databasePath)
		return fail(errors.Join(fmt.Errorf("install recovered user %d database: %w", userID, err), restoreErr))
	}
	outcome.QuarantinePath = quarantinePath

	// The recovered file is only kept if it verifies. Salvaging onto the same
	// failing storage can produce a damaged copy, and installing that while
	// telling the operator the original was preserved would be the worst of
	// both: the live database is the bad one and nothing says so.
	problems, err := store.CheckDatabaseFile(ctx, databasePath)
	if err != nil {
		return fail(restoreQuarantinedDatabase(quarantinePath, databasePath,
			fmt.Errorf("verify recovered user %d database: %w", userID, err), &outcome))
	}
	if len(problems) > 0 {
		return fail(restoreQuarantinedDatabase(quarantinePath, databasePath,
			fmt.Errorf("recovered user %d database is still damaged: %s", userID, problems[0]), &outcome))
	}
	outcome.FinishedAt = time.Now().UTC()
	outcome.Succeeded = true
	return outcome, nil
}

// restoreQuarantinedDatabase puts the original back after a failed recovery and
// removes the rejected copy, so the tenant keeps the file the operator was told
// is preserved. A rollback that cannot complete is reported rather than hidden.
func restoreQuarantinedDatabase(quarantinePath, databasePath string, cause error, outcome *store.RepairOutcome) error {
	rejectedPath := databasePath + ".rejected-" + filepath.Base(quarantinePath)
	if err := quarantineSQLiteFileSet(databasePath, rejectedPath); err != nil {
		return errors.Join(cause, fmt.Errorf("set the rejected recovery aside: %w", err))
	}
	if err := quarantineSQLiteFileSet(quarantinePath, databasePath); err != nil {
		return errors.Join(cause, fmt.Errorf("restore the original database from %s: %w", quarantinePath, err))
	}
	// The original is back at its own path, so there is no quarantine to report.
	outcome.QuarantinePath = ""
	removeSQLiteFileSet(rejectedPath)
	return errors.Join(cause, fmt.Errorf("the original database was restored and left in place at %s", databasePath))
}

// runScheduledDatabaseRepairs consumes the repair markers written by the admin
// UI. It runs before user stores are opened, so the databases it replaces have
// no open handles. A marker is cleared whether the repair succeeded or failed:
// a failed repair leaves the original file untouched and reports itself through
// the persisted outcome, and retrying it automatically on every boot would turn
// one damaged tenant into a restart loop.
func runScheduledDatabaseRepairs(ctx context.Context, dataDir string, manifests []plugins.Manifest, now time.Time, progress func(done, total int, detail string)) ([]store.RepairOutcome, error) {
	users, err := scheduledRepairUserIDs(dataDir)
	if err != nil {
		return nil, err
	}
	if len(users) == 0 {
		return nil, nil
	}
	outcomes := make([]store.RepairOutcome, 0, len(users))
	for i, userID := range users {
		if err := ctx.Err(); err != nil {
			return outcomes, err
		}
		if progress != nil {
			progress(i, len(users), fmt.Sprintf("repairing user %d database", userID))
		}
		log.Printf("scheduled database repair user_id=%d starting", userID)
		outcome, repairErr := repairUserDatabase(ctx, dataDir, userID, manifests, now, func(message string) {
			log.Printf("scheduled database repair user_id=%d: %s", userID, message)
			if progress != nil {
				progress(i, len(users), fmt.Sprintf("user %d: %s", userID, message))
			}
		})
		outcomes = append(outcomes, outcome)
		if writeErr := store.WriteUserDatabaseRepairReport(dataDir, userID, outcome); writeErr != nil {
			log.Printf("scheduled database repair user_id=%d: persist report: %v", userID, writeErr)
		}
		if clearErr := store.ClearUserDatabaseRepair(dataDir, userID); clearErr != nil {
			// Leaving the marker would repeat the repair on every boot.
			return outcomes, fmt.Errorf("clear repair marker for user %d: %w", userID, clearErr)
		}
		if repairErr != nil {
			log.Printf("scheduled database repair user_id=%d failed: %v", userID, repairErr)
			continue
		}
		log.Printf("scheduled database repair user_id=%d recovered rows=%d unreadable=%d gaps=%d dropped=%d quarantine=%q",
			userID, outcome.Report.RowsCopied, outcome.Report.RowsSkipped, outcome.Report.Gaps, outcome.Report.RowsDropped, outcome.QuarantinePath)
	}
	if progress != nil {
		progress(len(users), len(users), "database repairs finished")
	}
	return outcomes, nil
}

// scheduledRepairUserIDs reads pending repairs from the data directory rather
// than from the installation database, so a repair can be scheduled for a
// tenant whose own database no longer opens.
func scheduledRepairUserIDs(dataDir string) ([]int64, error) {
	entries, err := os.ReadDir(filepath.Join(dataDir, "users"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var users []int64
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		userID, err := strconv.ParseInt(entry.Name(), 10, 64)
		if err != nil || userID <= 0 {
			continue
		}
		if _, found, err := store.UserDatabaseRepairRequest(dataDir, userID); err != nil {
			log.Printf("read repair marker user_id=%d: %v", userID, err)
			continue
		} else if !found {
			continue
		}
		users = append(users, userID)
	}
	return users, nil
}
