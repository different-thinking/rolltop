// File overview: Tests for corrupt user database recovery. The corrupt fixture
// is produced by overwriting pages in the middle of a real SQLite file, so the
// salvage path is exercised against errors SQLite actually raises rather than
// against a stub.

package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	sqlite3 "github.com/mattn/go-sqlite3"
)

func TestSalvageUserDatabaseCopiesIntactDatabase(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	source := filepath.Join(root, "rolltop.db")
	const messages = 200
	writeSalvageFixture(t, source, messages)

	destination := filepath.Join(root, "recovered.db")
	report, err := SalvageUserDatabase(ctx, source, destination, nil, nil)
	if err != nil {
		t.Fatalf("salvage intact database: %v", err)
	}
	if report.Incomplete() {
		t.Fatalf("intact database reported losses: %+v", report)
	}
	if got := countRows(t, destination, "messages"); got != messages {
		t.Fatalf("recovered messages = %d, want %d", got, messages)
	}
	for _, table := range []string{"users", "mail_accounts", "mailboxes"} {
		if got := countRows(t, destination, table); got != 1 {
			t.Fatalf("recovered %s rows = %d, want 1", table, got)
		}
	}
	var subject string
	recovered := openSalvagedDatabase(t, destination)
	defer recovered.Close()
	if err := recovered.QueryRowContext(ctx, `SELECT subject FROM messages WHERE uid = ?`, 7).Scan(&subject); err != nil {
		t.Fatal(err)
	}
	if subject != "subject 7" {
		t.Fatalf("recovered subject = %q, want %q", subject, "subject 7")
	}
	if problems := quickCheckPath(t, destination); len(problems) != 0 {
		t.Fatalf("recovered database is not intact: %v", problems)
	}
}

func TestSalvageUserDatabaseRecoversReadableRowsFromCorruptFile(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	source := filepath.Join(root, "rolltop.db")
	const messages = 4000
	writeSalvageFixture(t, source, messages)
	corruptDatabasePages(t, source)

	if problems := quickCheckPath(t, source); len(problems) == 0 {
		t.Skip("SQLite did not report the injected page damage on this platform")
	}

	destination := filepath.Join(root, "recovered.db")
	report, err := SalvageUserDatabase(ctx, source, destination, nil, nil)
	if err != nil {
		t.Fatalf("salvage corrupt database: %v", err)
	}
	if report.RowsCopied == 0 {
		t.Fatalf("salvage recovered no rows: %+v", report)
	}
	if !report.Incomplete() {
		t.Fatalf("salvage of a damaged file reported no losses: %+v", report)
	}
	recoveredMessages := countRows(t, destination, "messages")
	if recoveredMessages == 0 {
		t.Fatalf("salvage recovered no messages: %+v", report)
	}
	if recoveredMessages > messages {
		t.Fatalf("recovered %d messages, source held %d", recoveredMessages, messages)
	}
	// Damage covering a few pages must not cost the whole table: the scan has
	// to resume after the damaged range instead of stopping at it.
	if recoveredMessages < messages/2 {
		t.Fatalf("recovered only %d of %d messages: %+v", recoveredMessages, messages, report)
	}
	if problems := quickCheckPath(t, destination); len(problems) != 0 {
		t.Fatalf("recovered database is not intact: %v", problems)
	}
	// The salvage must never write to the file it is reading.
	if problems := quickCheckPath(t, source); len(problems) == 0 {
		t.Fatal("corrupt source database was modified by salvage")
	}
}

func TestSalvageUserDatabaseRejectsSameSourceAndDestination(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rolltop.db")
	writeSalvageFixture(t, path, 1)
	if _, err := SalvageUserDatabase(context.Background(), path, path, nil, nil); err == nil {
		t.Fatal("salvage into the corrupt file was allowed")
	}
}

func TestIsCorruptClassifiesDriverAndWrappedErrors(t *testing.T) {
	if IsCorrupt(nil) {
		t.Fatal("nil error classified as corruption")
	}
	if IsCorrupt(sql.ErrNoRows) {
		t.Fatal("missing row classified as corruption")
	}
	wrapped := fmt.Errorf("store message: %v", "database disk image is malformed")
	if !IsCorrupt(wrapped) {
		t.Fatal("driver message that lost its type was not classified as corruption")
	}
	if !IsCorrupt(newCorruptionError(1, "/data/users/1/rolltop.db", wrapped)) {
		t.Fatal("wrapped corruption error was not classified as corruption")
	}
}

func TestNoteErrorNamesTenantDatabaseAndLatchesHealth(t *testing.T) {
	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	db, err := OpenServer(filepath.Join(dataDir, "rolltop.db"), dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if got := db.NoteError(1, sql.ErrNoRows); got != sql.ErrNoRows {
		t.Fatalf("NoteError rewrote an ordinary error: %v", got)
	}
	if db.DatabaseCorrupt(1) {
		t.Fatal("ordinary error latched corruption")
	}

	noted := db.NoteError(1, fmt.Errorf("store message: %w", ErrCorrupt))
	if !IsCorrupt(noted) {
		t.Fatalf("NoteError returned %v, want a corruption error", noted)
	}
	message := noted.Error()
	for _, want := range []string{
		filepath.Join(dataDir, "users", "1", "rolltop.db"),
		"rolltop recover-db --user-id 1 --confirm-offline",
	} {
		if !strings.Contains(message, want) {
			t.Fatalf("corruption message %q does not mention %q", message, want)
		}
	}
	if !db.DatabaseCorrupt(1) {
		t.Fatal("corruption was not latched for the tenant")
	}
	if db.DatabaseCorrupt(2) {
		t.Fatal("corruption latched for an unrelated tenant")
	}
	if records := db.CorruptDatabases(); len(records) != 1 || records[0].UserID != 1 {
		t.Fatalf("corrupt database records = %+v", records)
	}
}

// writeSalvageFixture builds a user-schema database holding one account, one
// mailbox, and messageCount messages with bodies large enough to spread the
// table across many SQLite pages.
func writeSalvageFixture(t *testing.T, path string, messageCount int) {
	t.Helper()
	ctx := context.Background()
	db, err := open(path, "", false, schemaUser, nil, defaultPluginCatalog())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
	}()
	now := nowUnix()
	if _, err := db.db.ExecContext(ctx, `INSERT INTO users (id, email, name, password_hash, created_at, updated_at)
		VALUES (1, 'owner@example.test', 'Owner', 'hash', ?, ?)`, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.ExecContext(ctx, `INSERT INTO mail_accounts (id, user_id, email, host, port, username, encrypted_password, created_at, updated_at)
		VALUES (1, 1, 'owner@example.test', 'imap.example.test', 993, 'owner', 'secret', ?, ?)`, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.ExecContext(ctx, `INSERT INTO mailboxes (id, user_id, account_id, name, created_at, updated_at)
		VALUES (1, 1, 1, 'INBOX', ?, ?)`, now, now); err != nil {
		t.Fatal(err)
	}
	tx, err := db.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	body := make([]byte, 2048)
	for i := range body {
		body[i] = byte('a' + i%26)
	}
	for i := 1; i <= messageCount; i++ {
		blobPath := fmt.Sprintf("users/1/blobs/%d.eml", i)
		if _, err := tx.ExecContext(ctx, `INSERT INTO blobs (id, user_id, kind, path, sha256, size, created_at)
			VALUES (?, 1, 'raw', ?, ?, ?, ?)`, i, blobPath, fmt.Sprintf("%064d", i), len(body), now); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO messages
				(id, user_id, account_id, mailbox_id, blob_id, subject, from_addr, uid, blob_path, body_text, created_at, updated_at)
			VALUES (?, 1, 1, 1, ?, ?, 'sender@example.test', ?, ?, ?, ?, ?)`,
			i, i, fmt.Sprintf("subject %d", i), i, blobPath, string(body), now, now); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

// corruptDatabasePages overwrites pages in the middle of the file, which is
// where the largest table's leaf pages live in this fixture.
func corruptDatabasePages(t *testing.T, path string) {
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
		t.Fatalf("fixture database is only %d bytes; the corruption test needs a multi-page table", info.Size())
	}
	damage := make([]byte, 8*pageSize)
	for i := range damage {
		damage[i] = 0xA5
	}
	offset := (info.Size() / 2 / pageSize) * pageSize
	if _, err := file.WriteAt(damage, offset); err != nil {
		t.Fatal(err)
	}
	if err := file.Sync(); err != nil {
		t.Fatal(err)
	}
}

func openSalvagedDatabase(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", path+"?_busy_timeout=5000")
	if err != nil {
		t.Fatal(err)
	}
	return db
}

func countRows(t *testing.T, path, table string) int {
	t.Helper()
	db := openSalvagedDatabase(t, path)
	defer db.Close()
	var count int
	if err := db.QueryRow(fmt.Sprintf(`SELECT COUNT(*) FROM %s`, quoteIdentifier(table))).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func quickCheckPath(t *testing.T, path string) []string {
	t.Helper()
	db := openSalvagedDatabase(t, path)
	defer db.Close()
	problems, err := quickCheck(context.Background(), db)
	if err != nil {
		// An unreadable page is itself a corruption report.
		return []string{err.Error()}
	}
	return problems
}

func TestIsCorruptTrustsTheDriverCodeOverMessageText(t *testing.T) {
	// A typed driver error must never be reclassified by the text fallback just
	// because a value it quotes contains one of the markers.
	busy := sqlite3.Error{Code: sqlite3.ErrBusy}
	if IsCorrupt(fmt.Errorf("store subject %q: %w", "database disk image is malformed", busy)) {
		t.Fatal("a busy error quoting the corruption text was classified as corruption")
	}
	corrupt := sqlite3.Error{Code: sqlite3.ErrCorrupt}
	if !IsCorrupt(fmt.Errorf("store message: %w", corrupt)) {
		t.Fatal("SQLITE_CORRUPT was not classified as corruption")
	}
}

func TestClearCorruptionReleasesALatchedTenant(t *testing.T) {
	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	db, err := OpenServer(filepath.Join(dataDir, "rolltop.db"), dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	db.MarkCorrupt(1, "quick_check finding")
	if !db.DatabaseCorrupt(1) {
		t.Fatal("tenant was not latched")
	}
	db.ClearCorruption(1)
	if db.DatabaseCorrupt(1) {
		t.Fatal("tenant stayed latched after a clean verification")
	}
}

func TestMustDataDBReturnsAFailingHandleInsteadOfPanicking(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	db, err := OpenServer(filepath.Join(dataDir, "rolltop.db"), dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	user, err := db.CreateUser(ctx, "latched@example.test", "Latched", "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	db.MarkCorrupt(user.ID, "quick_check finding")

	// A background goroutine reaching a latched tenant must get an error, not
	// take the whole process down with it.
	handle := db.mustDataDB(ctx, user.ID)
	if handle == nil {
		t.Fatal("mustDataDB returned no handle")
	}
	if err := handle.PingContext(ctx); err == nil {
		t.Fatal("the unavailable handle answered a ping")
	}
	if _, err := db.dataDB(ctx, user.ID); !IsCorrupt(err) {
		t.Fatalf("dataDB for a latched tenant = %v, want a corruption error", err)
	}
}

func TestPrepareUserStoresSkipsADamagedTenant(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	db, err := OpenServer(filepath.Join(dataDir, "rolltop.db"), dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	damaged, err := db.CreateUser(ctx, "damaged@example.test", "Damaged", "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	healthy, err := db.CreateUser(ctx, "healthy@example.test", "Healthy", "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	// Replace one tenant file with something SQLite cannot open at all.
	if _, err := db.UserStore(ctx, damaged.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(UserDatabaseFilePath(dataDir, damaged.ID), []byte("this is not a database"), 0o600); err != nil {
		t.Fatal(err)
	}

	reopened, err := OpenServer(filepath.Join(dataDir, "rolltop.db"), dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	// One damaged tenant must not keep the installation from starting.
	if err := reopened.PrepareUserStores(ctx, nil); err != nil {
		t.Fatalf("PrepareUserStores failed for one damaged tenant: %v", err)
	}
	if !reopened.DatabaseCorrupt(damaged.ID) {
		t.Fatal("the damaged tenant was not latched")
	}
	if reopened.DatabaseCorrupt(healthy.ID) {
		t.Fatal("the healthy tenant was latched")
	}
	if _, err := reopened.UserStore(ctx, healthy.ID); err != nil {
		t.Fatalf("healthy tenant is unusable: %v", err)
	}
}

func TestSalvageReportSerializesWithStableFieldNames(t *testing.T) {
	encoded, err := json.Marshal(SalvageReport{RowsCopied: 7, Tables: []TableSalvage{{Table: "messages", Copied: 7}}})
	if err != nil {
		t.Fatal(err)
	}
	// The admin UI and the persisted repair reports read these names, so they
	// must not follow Go field renames.
	for _, want := range []string{`"rows_copied":7`, `"table":"messages"`, `"copied":7`} {
		if !strings.Contains(string(encoded), want) {
			t.Fatalf("salvage report JSON %s does not contain %s", encoded, want)
		}
	}
}
