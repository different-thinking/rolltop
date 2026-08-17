package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"rolltop/backend/store"
)

func TestBackupDatabaseRequiresOutputDirectory(t *testing.T) {
	err := runCommand(context.Background(), []string{"backup-db"}, &bytes.Buffer{}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "--output") {
		t.Fatalf("backup-db error = %v", err)
	}
}

func TestBackupDatabaseCopiesInstallationAndUserDatabases(t *testing.T) {
	ctx := context.Background()
	userID := writeMaintenanceFixture(t, 100)
	output := filepath.Join(t.TempDir(), "backup")

	var stdout bytes.Buffer
	if err := runCommand(ctx, []string{"backup-db", "--output", output}, &stdout, &bytes.Buffer{}); err != nil {
		t.Fatalf("backup-db: %v\n%s", err, stdout.String())
	}

	systemCopy := filepath.Join(output, "rolltop.db")
	userCopy := filepath.Join(output, "users", strconv.FormatInt(userID, 10), "rolltop.db")
	for _, path := range []string{systemCopy, userCopy} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("backup %s missing: %v", path, err)
		}
		if info.Size() == 0 {
			t.Fatalf("backup %s is empty", path)
		}
		problems, err := store.CheckDatabaseFile(ctx, path)
		if err != nil || len(problems) != 0 {
			t.Fatalf("backup %s is not a sound database: %v %v", path, problems, err)
		}
	}
	if got := countMaintenanceMessages(t, userCopy); got != 100 {
		t.Fatalf("backup holds %d messages, want 100", got)
	}
	// A backup taken with VACUUM INTO is already checkpointed, so it must not
	// depend on WAL sidecars that the backup does not contain.
	for _, suffix := range []string{"-wal", "-shm"} {
		if _, err := os.Stat(userCopy + suffix); err == nil {
			t.Fatalf("backup left a %s sidecar behind", suffix)
		}
	}
}

func TestBackupDatabaseRefusesToOverwriteExistingCopy(t *testing.T) {
	ctx := context.Background()
	writeMaintenanceFixture(t, 10)
	output := filepath.Join(t.TempDir(), "backup")

	if err := runCommand(ctx, []string{"backup-db", "--output", output}, &bytes.Buffer{}, &bytes.Buffer{}); err != nil {
		t.Fatalf("first backup-db: %v", err)
	}
	err := runCommand(ctx, []string{"backup-db", "--output", output}, &bytes.Buffer{}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("second backup-db error = %v, want a refusal to overwrite", err)
	}
}

func TestBackupDatabaseRunsWhileTheServerHoldsTheDataDirectory(t *testing.T) {
	ctx := context.Background()
	userID := writeMaintenanceFixture(t, 50)
	dataDir := os.Getenv("ROLLTOP_DATA_DIR")

	// The running server owns both the instance lock and open handles.
	lock, err := acquireInstanceLock(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	db, err := store.OpenServer(filepath.Join(dataDir, "rolltop.db"), dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.UserStore(ctx, userID); err != nil {
		t.Fatal(err)
	}

	output := filepath.Join(t.TempDir(), "backup")
	if err := runCommand(ctx, []string{"backup-db", "--output", output}, &bytes.Buffer{}, &bytes.Buffer{}); err != nil {
		t.Fatalf("online backup-db: %v", err)
	}
	if got := countMaintenanceMessages(t, filepath.Join(output, "users", strconv.FormatInt(userID, 10), "rolltop.db")); got != 50 {
		t.Fatalf("online backup holds %d messages, want 50", got)
	}
}
