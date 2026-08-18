package search

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeQuarantine(t *testing.T, root string, userID int64, stamp string, size int) string {
	t.Helper()
	path := filepath.Join(root, "1", "bleve.quarantine-"+stamp)
	if userID != 1 {
		path = filepath.Join(root, "2", "bleve.quarantine-"+stamp)
	}
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "segment.zap"), make([]byte, size), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// The production incident left two quarantines holding two thirds of the used
// volume. Retention keeps the newest one and reclaims the rest.
func TestPruneIndexQuarantinesKeepsNewestAndReclaimsTheRest(t *testing.T) {
	root := filepath.Join(t.TempDir(), "users")
	older := writeQuarantine(t, root, 1, "20260818T012701.929388599Z", 16)
	newer := writeQuarantine(t, root, 1, "20260818T132847.734346217Z", 32)
	live := filepath.Join(root, "1", "bleve")
	if err := os.MkdirAll(live, 0o700); err != nil {
		t.Fatal(err)
	}

	now := time.Date(2026, 8, 18, 18, 0, 0, 0, time.UTC)
	pruned, err := PruneIndexQuarantines(root, DefaultIndexQuarantineKeep, DefaultIndexQuarantineMaxAge, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(pruned) != 1 || pruned[0].Path != older {
		t.Fatalf("pruned = %+v, want only %s", pruned, older)
	}
	if pruned[0].UserID != 1 || pruned[0].Bytes < 16 {
		t.Fatalf("pruned record = %+v, want user 1 and its bytes", pruned[0])
	}
	if _, err := os.Stat(older); !os.IsNotExist(err) {
		t.Fatalf("older quarantine survived: %v", err)
	}
	if _, err := os.Stat(newer); err != nil {
		t.Fatalf("newest quarantine was removed: %v", err)
	}
	if _, err := os.Stat(live); err != nil {
		t.Fatalf("live index was removed: %v", err)
	}
}

// Age bounds even the one quarantine that would otherwise be kept.
func TestPruneIndexQuarantinesRemovesEverythingPastMaxAge(t *testing.T) {
	root := filepath.Join(t.TempDir(), "users")
	writeQuarantine(t, root, 1, "20260810T012701.929388599Z", 8)
	writeQuarantine(t, root, 1, "20260811T012701.929388599Z", 8)

	now := time.Date(2026, 8, 18, 18, 0, 0, 0, time.UTC)
	pruned, err := PruneIndexQuarantines(root, DefaultIndexQuarantineKeep, DefaultIndexQuarantineMaxAge, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(pruned) != 2 {
		t.Fatalf("pruned = %+v, want both week-old quarantines", pruned)
	}
	remaining, err := filepath.Glob(filepath.Join(root, "1", "bleve.quarantine-*"))
	if err != nil || len(remaining) != 0 {
		t.Fatalf("remaining quarantines = %v, %v", remaining, err)
	}
}

// A directory that is not a tenant must not be walked, let alone deleted.
func TestPruneIndexQuarantinesIgnoresNonTenantDirectories(t *testing.T) {
	root := filepath.Join(t.TempDir(), "users")
	stray := filepath.Join(root, "backups", "bleve.quarantine-20260101T000000.000000000Z")
	if err := os.MkdirAll(stray, 0o700); err != nil {
		t.Fatal(err)
	}
	pruned, err := PruneIndexQuarantines(root, 0, time.Nanosecond, time.Date(2026, 8, 18, 18, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if len(pruned) != 0 {
		t.Fatalf("pruned = %+v, want nothing outside a tenant directory", pruned)
	}
	if _, err := os.Stat(stray); err != nil {
		t.Fatalf("directory outside the tenant layout was removed: %v", err)
	}
}

// The footprint drives a memory warning, so it must measure what Scorch maps —
// the live index — and not the quarantines, which nothing maps.
func TestMeasureIndexFootprintCountsLiveIndexesOnly(t *testing.T) {
	root := filepath.Join(t.TempDir(), "users")
	writeQuarantine(t, root, 1, "20260818T012701.929388599Z", 4096)
	live := filepath.Join(root, "1", "bleve")
	if err := os.MkdirAll(live, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(live, "segment.zap"), make([]byte, 512), 0o600); err != nil {
		t.Fatal(err)
	}

	footprint, err := MeasureIndexFootprint(root)
	if err != nil {
		t.Fatal(err)
	}
	if footprint.Tenants != 1 || footprint.Bytes != 512 {
		t.Fatalf("footprint = %+v, want one tenant of 512 bytes", footprint)
	}
}

func TestRecordStallDiagnosticsAppendsAndRotates(t *testing.T) {
	dataDir := t.TempDir()
	service, _ := openMarkerService(t)
	service.SetStallDiagnosticsDir(dataDir)

	at := time.Date(2026, 8, 18, 13, 28, 43, 0, time.UTC)
	if err := service.recordStallDiagnostics(at, "stall summary", "frame one\nframe two"); err != nil {
		t.Fatal(err)
	}
	if err := service.recordStallDiagnostics(at, "second summary", ""); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dataDir, stallLogName))
	if err != nil {
		t.Fatal(err)
	}
	report := string(raw)
	for _, want := range []string{"stall summary", "frame one\nframe two", "second summary", "2026-08-18T13:28:43Z"} {
		if !strings.Contains(report, want) {
			t.Fatalf("stall report missing %q: %q", want, report)
		}
	}
	if strings.Count(report, "=== rolltop search writer stall") != 2 {
		t.Fatalf("stall report did not append both incidents: %q", report)
	}

	// Past its bound the current file is renamed aside, never truncated: the
	// report that explains an incident must not be lost to make room.
	if err := os.WriteFile(filepath.Join(dataDir, stallLogName), make([]byte, stallLogMaxBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := service.recordStallDiagnostics(at, "after rotation", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dataDir, stallLogName+".prev")); err != nil {
		t.Fatalf("oversized stall log was not rotated: %v", err)
	}
	rotated, err := os.ReadFile(filepath.Join(dataDir, stallLogName))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(rotated), "after rotation") {
		t.Fatalf("post-rotation report missing: %q", rotated)
	}
}

// Without a configured directory the diagnostics stay in the process log rather
// than failing the stall path.
func TestRecordStallDiagnosticsIsInertWithoutADirectory(t *testing.T) {
	service, _ := openMarkerService(t)
	if err := service.recordStallDiagnostics(time.Now(), "summary", "frame"); err != nil {
		t.Fatalf("unconfigured stall diagnostics returned %v, want nil", err)
	}
}

// Housekeeping that stops at the first unreadable tenant leaves every later one
// unpruned on this run and every run after it, which is how quarantines filled a
// volume to begin with.
func TestPruneIndexQuarantinesContinuesPastAnUnreadableTenant(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores the permission bits this test makes a tenant unreadable with")
	}
	root := filepath.Join(t.TempDir(), "users")
	blocked := filepath.Join(root, "1")
	if err := os.MkdirAll(blocked, 0o700); err != nil {
		t.Fatal(err)
	}
	expired := writeQuarantine(t, root, 2, "20260810T012701.929388599Z", 8)
	if err := os.Chmod(blocked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(blocked, 0o700) })

	now := time.Date(2026, 8, 18, 18, 0, 0, 0, time.UTC)
	pruned, err := PruneIndexQuarantines(root, DefaultIndexQuarantineKeep, DefaultIndexQuarantineMaxAge, now)
	if err == nil {
		t.Fatal("unreadable tenant was not reported")
	}
	if len(pruned) != 1 || pruned[0].Path != expired {
		t.Fatalf("pruned = %+v, want the reachable tenant pruned anyway", pruned)
	}
	if _, statErr := os.Stat(expired); !os.IsNotExist(statErr) {
		t.Fatalf("later tenant was skipped because of an earlier failure: %v", statErr)
	}
}

// The volume that refuses the rename is the volume whose incident this report
// explains, so the report is appended rather than dropped.
func TestRecordStallDiagnosticsAppendsWhenRotationFails(t *testing.T) {
	dataDir := t.TempDir()
	service, _ := openMarkerService(t)
	service.SetStallDiagnosticsDir(dataDir)
	logged := make([]string, 0, 2)
	service.bleveErrorLog = func(format string, args ...any) {
		logged = append(logged, fmt.Sprintf(format, args...))
	}

	path := filepath.Join(dataDir, stallLogName)
	if err := os.WriteFile(path, make([]byte, stallLogMaxBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	// A directory at the rotation target makes the rename fail without making
	// the append impossible.
	if err := os.Mkdir(path+".prev", 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path+".prev", "occupied"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := service.recordStallDiagnostics(time.Now(), "summary that must survive", "frame"); err != nil {
		t.Fatalf("report was dropped when rotation failed: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "summary that must survive") {
		t.Fatal("stall report was not appended after the failed rotation")
	}
	if len(logged) == 0 || !strings.Contains(logged[0], "continuing to append") {
		t.Fatalf("failed rotation was not reported: %v", logged)
	}
}
