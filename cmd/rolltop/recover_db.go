// File overview: Offline SQLite maintenance commands. "check-db" reports which
// database files SQLite considers damaged, and "recover-db" rebuilds one
// tenant's database from the rows that are still readable. Both refuse to run
// while a Rolltop server holds the data directory lock, because reading a
// corrupt file while the syncer writes to it produces a worse copy.

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"time"

	"rolltop/backend/config"
	"rolltop/backend/plugins"
	"rolltop/backend/store"
)

const databaseMaintenanceUsage = `Usage:
  rolltop check-db [--user-id ID] --confirm-offline
  rolltop recover-db --user-id ID --confirm-offline

The Rolltop server must be stopped. Rolltop also verifies these files by itself
during a startup that follows an unclean shutdown; see
ROLLTOP_STARTUP_INTEGRITY_CHECK.

check-db runs SQLite's quick_check against the installation database and every
user database, or only the selected user, and changes nothing.

recover-db copies every readable row of one corrupt user database into a fresh
database, quarantines the damaged file next to it as rolltop.db.corrupt-<stamp>,
and installs the recovered file in its place. Rows on damaged pages cannot be
recovered; the command reports what was lost per table. Mail is re-downloadable
from IMAP, but locally created state in those rows is not.
`

func runCheckDatabase(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("check-db", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() { _, _ = io.WriteString(stderr, databaseMaintenanceUsage) }
	userID := flags.Int64("user-id", 0, "check only this numeric local user ID")
	confirmOffline := flags.Bool("confirm-offline", false, "confirm that every Rolltop server using this data volume is stopped")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("check-db does not accept positional arguments")
	}
	if !*confirmOffline {
		return fmt.Errorf("refusing online integrity check: stop every Rolltop server using this data volume, then pass --confirm-offline")
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	lock, err := acquireInstanceLock(cfg.DataDir)
	if err != nil {
		return err
	}
	defer lock.Close()

	manifests, err := plugins.LoadManifests(cfg.PluginDir)
	if err != nil {
		return err
	}

	// The installation database is verified before any store opens it: opening
	// applies migrations, and this command promises to change nothing. A
	// damaged installation database also stops the run, because the tenant list
	// would come from exactly that file.
	damaged := 0
	if *userID == 0 {
		problems, err := store.CheckDatabaseFile(ctx, cfg.DatabasePath)
		if err != nil {
			return fmt.Errorf("check installation database %s: %w", cfg.DatabasePath, err)
		}
		if reportIntegrity(stdout, "installation database", cfg.DatabasePath, problems) {
			return fmt.Errorf("the installation database is damaged; repair it before checking tenant databases")
		}
	}
	users, err := listMaintenanceUsers(ctx, cfg, manifests, *userID)
	if err != nil {
		return err
	}
	for _, id := range users {
		path := store.UserDatabaseFilePath(cfg.DataDir, id)
		if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
			fmt.Fprintf(stdout, "user %d: no database at %s\n", id, path)
			continue
		} else if err != nil {
			return fmt.Errorf("inspect user %d database: %w", id, err)
		}
		problems, err := store.CheckDatabaseFile(ctx, path)
		if err != nil {
			return fmt.Errorf("check user %d database %s: %w", id, path, err)
		}
		if reportIntegrity(stdout, fmt.Sprintf("user %d database", id), path, problems) {
			damaged++
			fmt.Fprintf(stdout, "  repair with: rolltop recover-db --user-id %d --confirm-offline\n", id)
		}
	}
	if damaged > 0 {
		return fmt.Errorf("%d database file(s) are damaged", damaged)
	}
	return nil
}

func reportIntegrity(stdout io.Writer, label, path string, problems []string) bool {
	if len(problems) == 0 {
		fmt.Fprintf(stdout, "%s %s: ok\n", label, path)
		return false
	}
	fmt.Fprintf(stdout, "%s %s: %d problem(s)\n", label, path, len(problems))
	for i, problem := range problems {
		if i == maxReportedIntegrityProblems {
			fmt.Fprintf(stdout, "  ... %d more\n", len(problems)-i)
			break
		}
		fmt.Fprintf(stdout, "  %s\n", problem)
	}
	return true
}

const maxReportedIntegrityProblems = 20

func runRecoverDatabase(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("recover-db", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() { _, _ = io.WriteString(stderr, databaseMaintenanceUsage) }
	userID := flags.Int64("user-id", 0, "numeric local user ID")
	confirmOffline := flags.Bool("confirm-offline", false, "confirm that every Rolltop server using this data volume is stopped")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("recover-db does not accept positional arguments")
	}
	if *userID <= 0 {
		return fmt.Errorf("--user-id must be a positive numeric local user ID")
	}
	if !*confirmOffline {
		return fmt.Errorf("refusing online database recovery: stop every Rolltop server using this data volume, then pass --confirm-offline")
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	lock, err := acquireInstanceLock(cfg.DataDir)
	if err != nil {
		return err
	}
	defer lock.Close()

	manifests, err := plugins.LoadManifests(cfg.PluginDir)
	if err != nil {
		return err
	}
	if _, err := listMaintenanceUsers(ctx, cfg, manifests, *userID); err != nil {
		return err
	}

	outcome, err := repairUserDatabase(ctx, cfg.DataDir, *userID, manifests, time.Now(), func(message string) {
		fmt.Fprintf(stdout, "  %s\n", message)
	})
	// The report is written for both outcomes, so a failed recovery leaves the
	// same record on the admin Database page that a scheduled repair would.
	if writeErr := store.WriteUserDatabaseRepairReport(cfg.DataDir, *userID, outcome); writeErr != nil {
		log.Printf("persist repair report for user %d: %v", *userID, writeErr)
	}
	if err != nil {
		return err
	}
	// A repair scheduled from the admin UI is now redundant.
	if clearErr := store.ClearUserDatabaseRepair(cfg.DataDir, *userID); clearErr != nil {
		log.Printf("clear repair marker for user %d: %v", *userID, clearErr)
	}
	writeSalvageReport(stdout, *userID, outcome.Report, store.UserDatabaseFilePath(cfg.DataDir, *userID), outcome.QuarantinePath)
	return nil
}

func writeSalvageReport(stdout io.Writer, userID int64, report store.SalvageReport, databasePath, quarantinePath string) {
	fmt.Fprintf(stdout, "Recovered user %d database:\n  %s\n", userID, databasePath)
	fmt.Fprintf(stdout, "Quarantined the damaged file at:\n  %s\n", quarantinePath)
	for _, table := range report.Tables {
		if table.Copied == 0 && table.Skipped == 0 && table.Dropped == 0 && table.Gaps == 0 && table.Failure == "" {
			continue
		}
		line := fmt.Sprintf("  %s: %d row(s) recovered", table.Table, table.Copied)
		if table.Skipped > 0 {
			line += fmt.Sprintf(", %d unreadable", table.Skipped)
		}
		if table.Gaps > 0 {
			line += fmt.Sprintf(", %d damaged range(s) skipped", table.Gaps)
		}
		if table.Dropped > 0 {
			line += fmt.Sprintf(", %d dropped", table.Dropped)
		}
		if table.Failure != "" {
			line += fmt.Sprintf(", scan stopped early: %s", table.Failure)
		}
		fmt.Fprintln(stdout, line)
	}
	for _, table := range report.MissingTables {
		fmt.Fprintf(stdout, "  %s: table is not part of the current schema and was not recovered\n", table)
	}
	fmt.Fprintf(stdout, "Total: %d row(s) recovered, %d unreadable, %d damaged range(s), %d dropped.\n",
		report.RowsCopied, report.RowsSkipped, report.Gaps, report.RowsDropped)
	if report.Incomplete() {
		fmt.Fprintln(stdout, "Some rows were lost, so the local search index no longer matches this database.")
		fmt.Fprintf(stdout, "Run: rolltop reset-search --user-id %d --confirm-offline\n", userID)
	}
	fmt.Fprintln(stdout, "Start Rolltop normally; the next sync re-downloads mail that IMAP still holds.")
}

// quarantineSQLiteFileSet moves a database and its WAL sidecars together. A
// stale -wal left beside a replacement database would be applied to it, so a
// sidecar that cannot be moved is deleted and, failing that, the whole move is
// rolled back — including the parts that already succeeded, because a database
// restored without the -wal that belongs to it has silently lost every
// transaction still held in that log.
func quarantineSQLiteFileSet(from, to string) error {
	if err := os.Rename(from, to); err != nil {
		return fmt.Errorf("quarantine %s: %w", from, err)
	}
	moved := []string{""}
	rollback := func(cause error) error {
		errs := []error{cause}
		for _, suffix := range moved {
			if err := os.Rename(to+suffix, from+suffix); err != nil {
				errs = append(errs, fmt.Errorf("restore %s: %w", from+suffix, err))
			}
		}
		return errors.Join(errs...)
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		source := from + suffix
		if _, err := os.Lstat(source); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return rollback(fmt.Errorf("inspect %s: %w", source, err))
		}
		if err := os.Rename(source, to+suffix); err == nil {
			moved = append(moved, suffix)
			continue
		}
		// The -shm file is rebuilt from the -wal on the next open, so deleting
		// it loses nothing; a -wal that can neither be moved nor removed does,
		// which is why the whole set goes back.
		if err := os.Remove(source); err != nil {
			return rollback(fmt.Errorf("remove stale %s: %w", source, err))
		}
	}
	return nil
}

func removeSQLiteFileSet(path string) {
	for _, suffix := range []string{"", "-wal", "-shm"} {
		_ = os.Remove(path + suffix)
	}
}

// listMaintenanceUsers resolves which tenants a maintenance command covers. The
// installation database is opened only to read the user list, so a damaged user
// database is never migrated on the way to being repaired.
func listMaintenanceUsers(ctx context.Context, cfg config.Config, manifests []plugins.Manifest, userID int64) ([]int64, error) {
	db, err := store.OpenServerWithPluginManifests(cfg.DatabasePath, cfg.DataDir, manifests, nil)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	if userID > 0 {
		if _, err := db.GetUserByID(ctx, userID); err != nil {
			if store.IsNotFound(err) {
				return nil, fmt.Errorf("local user %d does not exist", userID)
			}
			return nil, fmt.Errorf("load local user %d: %w", userID, err)
		}
		return []int64{userID}, nil
	}
	users, err := db.ListUsers(ctx)
	if err != nil {
		return nil, fmt.Errorf("list local users: %w", err)
	}
	ids := make([]int64, 0, len(users))
	for _, user := range users {
		ids = append(ids, user.ID)
	}
	return ids, nil
}
