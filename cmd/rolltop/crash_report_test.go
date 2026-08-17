// File overview: Tests for persistent crash reporting: telling a clean exit, a
// reported failure, and a silent kill apart across restarts, and never losing a
// report that was already written.

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"rolltop/internal/testlog"
)

// restart replays what a process boundary does: arm crash output, announce the
// run, then end it with err. It returns the log the started run produced.
func restart(t *testing.T, dataDir string, err error) string {
	t.Helper()
	logs := testlog.Capture(t)
	reporter := armCrashOutput(dataDir)
	reporter.beginRun("test")
	reporter.finish(err)
	return logs.String()
}

func TestFirstStartReportsNothing(t *testing.T) {
	dataDir := t.TempDir()

	logs := restart(t, dataDir, nil)

	if strings.Contains(logs, "previous run") {
		t.Fatalf("first start reported previous-run evidence: %q", logs)
	}
	if _, err := os.Stat(filepath.Join(dataDir, uncleanShutdownMarkerName)); !os.IsNotExist(err) {
		t.Fatalf("marker outlived a clean shutdown: %v", err)
	}
}

func TestCleanShutdownStaysQuietOnNextStart(t *testing.T) {
	dataDir := t.TempDir()
	restart(t, dataDir, nil)

	logs := restart(t, dataDir, nil)

	if strings.Contains(logs, "previous run") {
		t.Fatalf("clean shutdown reported as an incident: %q", logs)
	}
}

func TestFatalErrorIsReportedOnNextStart(t *testing.T) {
	dataDir := t.TempDir()
	restart(t, dataDir, errors.New("listen on :8080: address already in use"))

	logs := restart(t, dataDir, nil)

	if !strings.Contains(logs, "previous run ended with a crash or fatal error") {
		t.Fatalf("fatal exit not reported on the next start: %q", logs)
	}
	report, err := os.ReadFile(filepath.Join(dataDir, crashLogName))
	if err != nil {
		t.Fatalf("crash log missing: %v", err)
	}
	if !strings.Contains(string(report), "fatal: listen on :8080: address already in use") {
		t.Fatalf("crash log does not hold the fatal error: %q", report)
	}
}

// A panic kills the process before finish() runs, leaving the marker in place
// and a runtime dump appended by the Go runtime.
func TestPanicDumpIsReportedOnNextStart(t *testing.T) {
	dataDir := t.TempDir()
	func() {
		testlog.Capture(t)
		reporter := armCrashOutput(dataDir)
		reporter.beginRun("test")
		reporter.appendf("panic: simulated\n\ngoroutine 1 [running]:")
	}()

	logs := restart(t, dataDir, nil)

	if !strings.Contains(logs, "previous run ended with a crash or fatal error") {
		t.Fatalf("panic dump not reported on the next start: %q", logs)
	}
}

func TestSilentKillIsReportedOnNextStart(t *testing.T) {
	dataDir := t.TempDir()
	func() {
		testlog.Capture(t)
		reporter := armCrashOutput(dataDir)
		reporter.beginRun("test")
		// No finish and no output: the process was killed outright.
	}()

	logs := restart(t, dataDir, nil)

	if !strings.Contains(logs, "killed externally") {
		t.Fatalf("silent kill not reported on the next start: %q", logs)
	}
}

// The deliberate restart for search index recovery is an intended outcome, so it
// must not be filed as a crash or leave evidence behind.
func TestPlannedRestartIsNotACrash(t *testing.T) {
	dataDir := t.TempDir()
	restartErr := fmt.Errorf("search index writer stalled for user %d; %w", int64(1), errRestartForRecovery)
	restart(t, dataDir, restartErr)

	logs := restart(t, dataDir, nil)

	if strings.Contains(logs, "previous run") {
		t.Fatalf("planned restart reported as an incident: %q", logs)
	}
	if _, err := os.Stat(filepath.Join(dataDir, crashLogName)); err == nil {
		contents, _ := os.ReadFile(filepath.Join(dataDir, crashLogName))
		if strings.Contains(string(contents), "fatal:") {
			t.Fatalf("planned restart recorded as a fatal error: %q", contents)
		}
	}
}

// The report of an earlier crash must survive every later start, including the
// rotation that bounds the log.
func TestEarlierReportsSurviveLaterStarts(t *testing.T) {
	dataDir := t.TempDir()
	restart(t, dataDir, errors.New("first failure"))
	restart(t, dataDir, errors.New("second failure"))

	contents, err := os.ReadFile(filepath.Join(dataDir, crashLogName))
	if err != nil {
		t.Fatalf("crash log missing: %v", err)
	}
	for _, want := range []string{"first failure", "second failure"} {
		if !strings.Contains(string(contents), want) {
			t.Fatalf("crash log lost %q: %s", want, contents)
		}
	}
}

func TestOversizedCrashLogRotatesInsteadOfTruncating(t *testing.T) {
	dataDir := t.TempDir()
	crashPath := filepath.Join(dataDir, crashLogName)
	if err := os.WriteFile(crashPath, append([]byte("panic: the original report\n"), make([]byte, crashLogMaxBytes)...), 0o600); err != nil {
		t.Fatal(err)
	}

	restart(t, dataDir, nil)

	preserved, err := os.ReadFile(filepath.Join(dataDir, crashLogPrevName))
	if err != nil {
		t.Fatalf("rotated crash log missing: %v", err)
	}
	if !strings.Contains(string(preserved), "panic: the original report") {
		t.Fatal("rotated crash log does not hold the original report")
	}
}

// Rotation is the only operation that can lose reports, so a rename it cannot
// perform must leave the existing log intact rather than start a fresh one.
func TestFailedRotationKeepsTheExistingReport(t *testing.T) {
	dataDir := t.TempDir()
	crashPath := filepath.Join(dataDir, crashLogName)
	if err := os.WriteFile(crashPath, append([]byte("panic: the original report\n"), make([]byte, crashLogMaxBytes)...), 0o600); err != nil {
		t.Fatal(err)
	}
	// A directory cannot be replaced by a rename of a regular file.
	if err := os.Mkdir(filepath.Join(dataDir, crashLogPrevName), 0o700); err != nil {
		t.Fatal(err)
	}

	restart(t, dataDir, nil)

	contents, err := os.ReadFile(crashPath)
	if err != nil {
		t.Fatalf("crash log missing after a failed rotation: %v", err)
	}
	if !strings.Contains(string(contents), "panic: the original report") {
		t.Fatalf("failed rotation destroyed the report it could not preserve: %q", truncateForMessage(contents))
	}
}

// With the crash log unwritable there is nothing to append a fatal error to, so
// the marker has to stay: it is the only remaining evidence the run failed.
func TestUnwritableCrashLogKeepsTheMarker(t *testing.T) {
	dataDir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dataDir, crashLogName), 0o700); err != nil {
		t.Fatal(err)
	}
	func() {
		testlog.Capture(t)
		reporter := armCrashOutput(dataDir)
		reporter.beginRun("test")
		reporter.finish(errors.New("store failed to open"))
	}()

	logs := restart(t, dataDir, nil)

	if !strings.Contains(logs, "without a clean shutdown") {
		t.Fatalf("unpersisted fatal left no trace for the next start: %q", logs)
	}
}

// A marker left by a still-running process must not be deleted by a run that
// failed before it acquired the instance lock.
func TestFailureBeforeBeginRunLeavesTheMarkerAlone(t *testing.T) {
	dataDir := t.TempDir()
	testlog.Capture(t)
	live := armCrashOutput(dataDir)
	live.beginRun("test")

	early := armCrashOutput(dataDir)
	early.finish(nil)

	if _, err := os.Stat(filepath.Join(dataDir, uncleanShutdownMarkerName)); err != nil {
		t.Fatalf("a run that never started removed another run's marker: %v", err)
	}
}

func TestFinishIsIdempotent(t *testing.T) {
	dataDir := t.TempDir()
	testlog.Capture(t)
	reporter := armCrashOutput(dataDir)
	reporter.beginRun("test")
	reporter.finish(errors.New("boom"))
	reporter.finish(errors.New("boom"))

	contents, err := os.ReadFile(filepath.Join(dataDir, crashLogName))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(contents), "fatal: boom"); got != 1 {
		t.Fatalf("fatal recorded %d times, want 1", got)
	}
}

func truncateForMessage(contents []byte) string {
	if len(contents) > 120 {
		return string(contents[:120]) + "..."
	}
	return string(contents)
}
