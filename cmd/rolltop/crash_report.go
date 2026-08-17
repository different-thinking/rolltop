// File overview: Persistent crash reporting. Runtime crash dumps (panics,
// fatal runtime errors) are duplicated into <data-dir>/crash.log so they
// survive container recreation even when stderr output is lost. A shutdown
// marker file detects silent terminations (SIGKILL, kernel OOM kills) that
// cannot write anything before the process dies.

package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime/debug"
	"time"
)

const (
	crashLogName              = "crash.log"
	crashLogPrevName          = "crash.log.prev"
	uncleanShutdownMarkerName = "rolltop.unclean-shutdown"
)

type crashReporter struct {
	file       *os.File
	markerPath string
	crashPath  string
}

// setupCrashReporting inspects evidence from the previous run, arms the
// runtime crash output, and drops the unclean-shutdown marker for the current
// run. Call it once the instance lock is held so concurrent invocations
// cannot disturb the running instance's state.
func setupCrashReporting(dataDir string) *crashReporter {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		log.Printf("crash reporting disabled: %v", err)
		return nil
	}
	crashPath := filepath.Join(dataDir, crashLogName)
	markerPath := filepath.Join(dataDir, uncleanShutdownMarkerName)

	if info, err := os.Stat(crashPath); err == nil && info.Size() > 0 {
		prevPath := filepath.Join(dataDir, crashLogPrevName)
		if err := os.Rename(crashPath, prevPath); err != nil {
			log.Printf("previous run left a crash report at %s (preserving failed: %v)", crashPath, err)
		} else {
			log.Printf("previous run ended with a crash or fatal error; report preserved at %s", prevPath)
		}
	} else if _, err := os.Stat(markerPath); err == nil {
		// Marker present but nothing was written: the process was killed
		// before it could report anything (SIGKILL, kernel OOM kill, power
		// loss). This is the only trace such terminations leave behind.
		log.Printf("previous run was terminated without a clean shutdown and left no crash report; likely killed externally (e.g. kernel OOM kill) - check 'docker inspect' exit code and kernel logs")
	}

	if err := os.WriteFile(markerPath, []byte(time.Now().UTC().Format(time.RFC3339)+"\n"), 0o600); err != nil {
		log.Printf("write unclean-shutdown marker: %v", err)
		markerPath = ""
	}

	reporter := &crashReporter{markerPath: markerPath, crashPath: crashPath}
	file, err := os.OpenFile(crashPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		log.Printf("open crash log %s: %v", crashPath, err)
		return reporter
	}
	if err := debug.SetCrashOutput(file, debug.CrashOptions{}); err != nil {
		log.Printf("arm crash log %s: %v", crashPath, err)
	}
	reporter.file = file
	return reporter
}

// recordFatal persists an orderly fatal error (startup failure, fatal runtime
// condition) in the crash log so it survives lost stderr output.
func (c *crashReporter) recordFatal(err error) {
	if c == nil || c.file == nil || err == nil {
		return
	}
	_, _ = fmt.Fprintf(c.file, "%s fatal: %v\n", time.Now().UTC().Format(time.RFC3339), err)
	_ = c.file.Sync()
}

// markCleanShutdown removes the unclean-shutdown marker and, when nothing was
// recorded, the empty crash log. Call it on every orderly exit.
func (c *crashReporter) markCleanShutdown() {
	if c == nil {
		return
	}
	if c.markerPath != "" {
		_ = os.Remove(c.markerPath)
	}
	if c.crashPath != "" {
		if info, err := os.Stat(c.crashPath); err == nil && info.Size() == 0 {
			_ = os.Remove(c.crashPath)
		}
	}
}
