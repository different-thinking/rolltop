// File overview: The backup-db command. Unlike the other maintenance commands
// this one is meant to run while Rolltop serves traffic, so it deliberately
// does not take the data directory lock and never writes to the live files. It
// exists because copying a running SQLite database with cp, rsync, or a volume
// snapshot is one of the few reliable ways to end up with a corrupt file.

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"rolltop/backend/config"
	"rolltop/backend/store"
)

const backupDatabaseUsage = `Usage:
  rolltop backup-db --output DIR [--user-id ID]

Writes a consistent copy of the installation database and every user database
into DIR, mirroring the data directory layout. Safe to run while Rolltop is
serving: each copy is produced with SQLite's VACUUM INTO from a single read
transaction, so it holds every transaction committed when the copy started.

Message blobs and the search index are not copied. Blobs are re-fetchable from
IMAP and the search index is rebuilt from the database.
`

func runBackupDatabase(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("backup-db", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() { _, _ = io.WriteString(stderr, backupDatabaseUsage) }
	output := flags.String("output", "", "directory to write the backup into")
	userID := flags.Int64("user-id", 0, "back up only this numeric local user ID")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("backup-db does not accept positional arguments")
	}
	if strings.TrimSpace(*output) == "" {
		return fmt.Errorf("--output must name a directory to write the backup into")
	}
	// A negative id matches neither the all-databases nor the single-tenant
	// branch below, so without this it would silently back up everything.
	if *userID < 0 {
		return fmt.Errorf("--user-id must be a positive numeric local user ID")
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(*output, 0o700); err != nil {
		return fmt.Errorf("create backup directory %s: %w", *output, err)
	}

	type backupTarget struct {
		label  string
		source string
		dest   string
	}
	var targets []backupTarget
	if *userID == 0 {
		targets = append(targets, backupTarget{
			label:  "installation database",
			source: cfg.DatabasePath,
			dest:   filepath.Join(*output, filepath.Base(cfg.DatabasePath)),
		})
	}
	users, err := backupUserIDs(cfg.DataDir, *userID)
	if err != nil {
		return err
	}
	for _, id := range users {
		name := strconv.FormatInt(id, 10)
		targets = append(targets, backupTarget{
			label:  fmt.Sprintf("user %d database", id),
			source: store.UserDatabaseFilePath(cfg.DataDir, id),
			dest:   filepath.Join(*output, "users", name, "rolltop.db"),
		})
	}
	if len(targets) == 0 {
		return fmt.Errorf("no databases found under %s", cfg.DataDir)
	}

	// On a filesystem where SQLite cannot use its shared WAL index, a serving
	// Rolltop holds each database exclusively and no second process can read it.
	// Saying so up front beats a row of "database is locked" failures.
	if access, filesystem := store.ResolveAccessMode(cfg.SQLiteAccess, cfg.DataDir); !access.SharesFiles() {
		lock, free, lockErr := tryInstanceLock(cfg.DataDir)
		switch {
		case lockErr != nil:
			// Whether anything is serving could not be established. The backup
			// itself reports the truth either way.
		case free:
			// Nothing is serving, so the databases can be read from here. The
			// lock is released again: this command is not the owner.
			_ = lock.Close()
		default:
			return fmt.Errorf("rolltop is serving this data directory and %s makes it hold every database exclusively; "+
				"back up from the admin Database page instead, or stop the server first", filesystem.Name)
		}
	}

	var total int64
	var failures []string
	for _, target := range targets {
		size, err := store.BackupDatabaseFile(ctx, target.source, target.dest)
		if err != nil {
			// One damaged tenant must not cost the healthy ones their backup.
			failures = append(failures, fmt.Sprintf("%s: %v", target.label, err))
			fmt.Fprintf(stdout, "%s: FAILED (%v)\n", target.label, err)
			continue
		}
		total += size
		fmt.Fprintf(stdout, "%s -> %s (%d bytes)\n", target.label, target.dest, size)
	}
	fmt.Fprintf(stdout, "Backed up %d of %d database(s), %d bytes total.\n", len(targets)-len(failures), len(targets), total)
	fmt.Fprintln(stdout, "Message blobs and the search index are not included.")
	if len(failures) > 0 {
		return fmt.Errorf("%d database(s) could not be backed up:\n  %s", len(failures), strings.Join(failures, "\n  "))
	}
	return nil
}

// backupUserIDs reads the tenant list from the data directory rather than from
// the installation database, so backing up never opens a writer connection
// against a database the running server owns.
func backupUserIDs(dataDir string, userID int64) ([]int64, error) {
	if userID > 0 {
		path := store.UserDatabaseFilePath(dataDir, userID)
		if _, err := os.Stat(path); err != nil {
			return nil, fmt.Errorf("open user %d database %s: %w", userID, path, err)
		}
		return []int64{userID}, nil
	}
	matches, err := filepath.Glob(filepath.Join(dataDir, "users", "*", "rolltop.db"))
	if err != nil {
		return nil, err
	}
	var ids []int64
	for _, match := range matches {
		id, err := strconv.ParseInt(filepath.Base(filepath.Dir(match)), 10, 64)
		if err != nil || id <= 0 {
			continue
		}
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids, nil
}
