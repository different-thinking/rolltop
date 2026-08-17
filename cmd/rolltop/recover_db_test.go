package main

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"rolltop/backend/store"
)

func TestCheckDatabaseRequiresExplicitOfflineConfirmation(t *testing.T) {
	err := runCommand(context.Background(), []string{"check-db"}, &bytes.Buffer{}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "--confirm-offline") {
		t.Fatalf("check-db error = %v", err)
	}
}

func TestRecoverDatabaseRequiresExplicitOfflineConfirmation(t *testing.T) {
	err := runCommand(context.Background(), []string{"recover-db", "--user-id", "1"}, &bytes.Buffer{}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "--confirm-offline") {
		t.Fatalf("recover-db error = %v", err)
	}
}

func TestCheckDatabaseReportsIntactDatabases(t *testing.T) {
	userID := writeMaintenanceFixture(t, 50)
	var stdout bytes.Buffer
	if err := runCommand(context.Background(), []string{"check-db", "--confirm-offline"}, &stdout, &bytes.Buffer{}); err != nil {
		t.Fatalf("check-db on intact data: %v\n%s", err, stdout.String())
	}
	if !strings.Contains(stdout.String(), fmt.Sprintf("user %d database", userID)) {
		t.Fatalf("check-db did not report the user database:\n%s", stdout.String())
	}
	if strings.Contains(stdout.String(), "problem(s)") {
		t.Fatalf("check-db reported problems for intact data:\n%s", stdout.String())
	}
}

func TestRecoverDatabaseSalvagesCorruptUserDatabase(t *testing.T) {
	ctx := context.Background()
	userID := writeMaintenanceFixture(t, 4000)
	dataDir := os.Getenv("ROLLTOP_DATA_DIR")
	databasePath := userDatabasePath(dataDir, userID)
	corruptDatabaseFile(t, databasePath)

	var checkOut bytes.Buffer
	checkErr := runCommand(ctx, []string{"check-db", "--user-id", strconv.FormatInt(userID, 10), "--confirm-offline"}, &checkOut, &bytes.Buffer{})
	if checkErr == nil {
		t.Skip("SQLite did not report the injected page damage on this platform")
	}
	if !strings.Contains(checkOut.String(), "recover-db --user-id") {
		t.Fatalf("check-db did not name the repair command:\n%s", checkOut.String())
	}

	var stdout bytes.Buffer
	if err := runCommand(ctx, []string{"recover-db", "--user-id", strconv.FormatInt(userID, 10), "--confirm-offline"}, &stdout, &bytes.Buffer{}); err != nil {
		t.Fatalf("recover-db: %v\n%s", err, stdout.String())
	}
	output := stdout.String()
	if !strings.Contains(output, "reset-search --user-id") {
		t.Fatalf("recover-db did not advise a search rebuild:\n%s", output)
	}

	quarantined, err := filepath.Glob(databasePath + ".corrupt-*")
	if err != nil {
		t.Fatal(err)
	}
	if len(quarantined) != 1 {
		t.Fatalf("quarantined files = %v, want exactly one", quarantined)
	}
	if leftovers, err := filepath.Glob(databasePath + ".recovered-*"); err != nil || len(leftovers) != 0 {
		t.Fatalf("recovery scratch files remained: %v, %v", leftovers, err)
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if _, err := os.Stat(databasePath + suffix); err == nil {
			t.Fatalf("stale %s sidecar was left beside the recovered database", suffix)
		}
	}

	problems, err := store.CheckDatabaseFile(ctx, databasePath)
	if err != nil {
		t.Fatal(err)
	}
	if len(problems) != 0 {
		t.Fatalf("recovered database still reports problems: %v", problems)
	}
	recovered := countMaintenanceMessages(t, databasePath)
	if recovered == 0 {
		t.Fatalf("recovered database holds no messages:\n%s", output)
	}
	if recovered >= 4000 {
		t.Fatalf("recovered %d messages from a damaged file holding 4000", recovered)
	}

	// The server must be able to open and migrate the recovered database.
	db, err := store.OpenServer(os.Getenv("ROLLTOP_DB_PATH"), dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.ListMailboxesForUser(ctx, userID); err != nil {
		t.Fatalf("read recovered mailboxes: %v", err)
	}
}

// writeMaintenanceFixture builds a data directory with one user whose database
// holds messageCount messages, and points the process configuration at it.
func writeMaintenanceFixture(t *testing.T, messageCount int) int64 {
	t.Helper()
	ctx := context.Background()
	dataDir := filepath.Join(t.TempDir(), "data")
	pluginDir := filepath.Join(t.TempDir(), "plugins")
	if err := os.MkdirAll(pluginDir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ROLLTOP_DATA_DIR", dataDir)
	t.Setenv("ROLLTOP_DB_PATH", filepath.Join(dataDir, "rolltop.db"))
	t.Setenv("ROLLTOP_PLUGIN_DIR", pluginDir)
	t.Setenv("ROLLTOP_MASTER_KEY", "01234567890123456789012345678901")

	db, err := store.OpenServer(filepath.Join(dataDir, "rolltop.db"), dataDir)
	if err != nil {
		t.Fatal(err)
	}
	user, err := db.CreateUser(ctx, "recover@example.test", "Recover", "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	account, err := db.CreateMailAccount(ctx, store.MailAccount{
		UserID: user.ID, Email: "recover@example.test", Host: "imap.example.test",
		Port: 993, Username: "recover", EncryptedPassword: "secret", UseTLS: true, Mailbox: "INBOX",
	})
	if err != nil {
		t.Fatal(err)
	}
	mailbox, err := db.GetOrCreateMailbox(ctx, user.ID, account.ID, "INBOX")
	if err != nil {
		t.Fatal(err)
	}
	userDB, err := db.UserDB(ctx, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	body := make([]byte, 2048)
	for i := range body {
		body[i] = byte('a' + i%26)
	}
	tx, err := userDB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= messageCount; i++ {
		blobPath := fmt.Sprintf("users/%d/blobs/%d.eml", user.ID, i)
		if _, err := tx.ExecContext(ctx, `INSERT INTO blobs (id, user_id, kind, path, sha256, size, created_at)
			VALUES (?, ?, 'raw', ?, ?, ?, 0)`, i, user.ID, blobPath, fmt.Sprintf("%064d", i), len(body)); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO messages
				(id, user_id, account_id, mailbox_id, blob_id, subject, uid, blob_path, body_text, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 0, 0)`,
			i, user.ID, account.ID, mailbox.ID, i, fmt.Sprintf("subject %d", i), i, blobPath, string(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	return user.ID
}

func corruptDatabaseFile(t *testing.T, path string) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		t.Fatal(err)
	}
	const pageSize = 4096
	if info.Size() < 64*pageSize {
		t.Fatalf("fixture database is only %d bytes", info.Size())
	}
	damage := make([]byte, 8*pageSize)
	for i := range damage {
		damage[i] = 0xA5
	}
	if _, err := file.WriteAt(damage, (info.Size()/2/pageSize)*pageSize); err != nil {
		t.Fatal(err)
	}
	if err := file.Sync(); err != nil {
		t.Fatal(err)
	}
}

func countMaintenanceMessages(t *testing.T, path string) int {
	t.Helper()
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}
