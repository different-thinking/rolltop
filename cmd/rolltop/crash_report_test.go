// File overview: Tests for persistent crash reporting: preserving previous
// crash reports, detecting silent kills via the unclean-shutdown marker, and
// cleaning both up on orderly exits.

package main

import (
	"bytes"
	"errors"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func captureCrashReportLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var logs bytes.Buffer
	previousWriter := log.Writer()
	previousFlags := log.Flags()
	log.SetOutput(&logs)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(previousWriter)
		log.SetFlags(previousFlags)
	})
	return &logs
}

func TestSetupCrashReportingFreshStart(t *testing.T) {
	logs := captureCrashReportLogs(t)
	dataDir := t.TempDir()

	reporter := setupCrashReporting(dataDir)
	if reporter == nil {
		t.Fatal("reporter is nil")
	}
	if strings.Contains(logs.String(), "previous run") {
		t.Fatalf("fresh start logged previous-run evidence: %q", logs.String())
	}
	if _, err := os.Stat(filepath.Join(dataDir, uncleanShutdownMarkerName)); err != nil {
		t.Fatalf("unclean-shutdown marker missing: %v", err)
	}

	reporter.markCleanShutdown()
	if _, err := os.Stat(filepath.Join(dataDir, uncleanShutdownMarkerName)); !os.IsNotExist(err) {
		t.Fatalf("marker still present after clean shutdown: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dataDir, crashLogName)); !os.IsNotExist(err) {
		t.Fatalf("empty crash log not removed after clean shutdown: %v", err)
	}
}

func TestSetupCrashReportingPreservesPreviousCrashReport(t *testing.T) {
	logs := captureCrashReportLogs(t)
	dataDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dataDir, crashLogName), []byte("panic: boom\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	setupCrashReporting(dataDir)

	preserved, err := os.ReadFile(filepath.Join(dataDir, crashLogPrevName))
	if err != nil {
		t.Fatalf("preserved crash report missing: %v", err)
	}
	if !strings.Contains(string(preserved), "panic: boom") {
		t.Fatalf("preserved crash report content = %q", preserved)
	}
	if !strings.Contains(logs.String(), "report preserved at") {
		t.Fatalf("log output %q does not mention the preserved report", logs.String())
	}
}

func TestSetupCrashReportingDetectsSilentKill(t *testing.T) {
	logs := captureCrashReportLogs(t)
	dataDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dataDir, uncleanShutdownMarkerName), []byte("2026-01-01T00:00:00Z\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	setupCrashReporting(dataDir)

	if !strings.Contains(logs.String(), "killed externally") {
		t.Fatalf("log output %q does not report the silent kill", logs.String())
	}
}

func TestRecordFatalPersistsError(t *testing.T) {
	captureCrashReportLogs(t)
	dataDir := t.TempDir()

	reporter := setupCrashReporting(dataDir)
	reporter.recordFatal(errors.New("listen on :8080: address already in use"))
	reporter.markCleanShutdown()

	contents, err := os.ReadFile(filepath.Join(dataDir, crashLogName))
	if err != nil {
		t.Fatalf("crash log missing after recordFatal: %v", err)
	}
	if !strings.Contains(string(contents), "fatal: listen on :8080: address already in use") {
		t.Fatalf("crash log content = %q", contents)
	}
	if _, err := os.Stat(filepath.Join(dataDir, uncleanShutdownMarkerName)); !os.IsNotExist(err) {
		t.Fatal("marker still present after orderly fatal exit")
	}
}

func TestCrashReporterNilSafety(t *testing.T) {
	var reporter *crashReporter
	reporter.recordFatal(errors.New("boom"))
	reporter.markCleanShutdown()
}
