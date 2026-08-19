// File overview: Persistent crash reporting. Runtime crash dumps (panics, fatal
// runtime errors) and orderly fatal errors are appended to <data-dir>/crash.log
// so they survive container recreation even when stderr output is lost. A state
// file records how far the log had grown when the run started and whether the
// run ended cleanly, which lets the next start tell three cases apart: a clean
// exit, a run that reported why it died, and a run killed before it could write
// anything (SIGKILL, kernel OOM kill).
//
// Growth of the crash log is what identifies a reported failure, not the clean
// shutdown flag. An unrecovered panic runs deferred functions first and only
// then writes its dump, so by the time the runtime reports, this process has
// already recorded whatever it believed about its own exit. Comparing sizes
// catches the dump that arrives afterwards; recovering the panic to correct the
// flag would rewrite the stack trace this file exists to preserve.

package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"os"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"time"
)

const (
	crashLogName     = "crash.log"
	crashLogPrevName = "crash.log.prev"
	crashStateName   = "crash-state.json"

	// crashLogMaxBytes bounds the append-only log. Only failures are written,
	// so this holds many incidents before the oldest are rotated away.
	crashLogMaxBytes = 1 << 20

	// crashLogBytesUnknown marks a baseline that could not be read back. State
	// that will not parse says nothing about where the previous run's own
	// output began, so nothing already in the crash log may be attributed to
	// it - the reports there belong to runs that were reported long ago.
	crashLogBytesUnknown = math.MaxInt64
)

// runState describes the run that wrote it. CrashLogBytes is the crash log size
// once the run had announced itself, so any later growth is a report that run
// produced. CleanShutdown records whether it got to end on its own terms. The
// file is rewritten rather than deleted: the size baseline has to outlive a
// clean shutdown, or a panic dump written after the process signed off would
// look like part of an already-known report.
type runState struct {
	StartedAt     string `json:"started_at"`
	PID           int    `json:"pid"`
	CrashLogBytes int64  `json:"crash_log_bytes"`
	CleanShutdown bool   `json:"clean_shutdown"`
	// StopSignal names the signal that asked the run to stop, and the remaining
	// fields how far the shutdown it started had got. They are what separates a
	// process killed out of nowhere - a kernel OOM kill, a `docker kill` - from
	// one that was shutting down as asked and ran out of grace period. Both
	// leave CleanShutdown false and no crash report, so without them the next
	// start can only name the first, which is the wrong thing to go and check.
	StopSignal      string `json:"stop_signal,omitempty"`
	StopSignalAt    string `json:"stop_signal_at,omitempty"`
	ShutdownPhase   string `json:"shutdown_phase,omitempty"`
	ShutdownPhaseAt string `json:"shutdown_phase_at,omitempty"`
}

// crashReporter persists crash evidence in the data directory. The crash log is
// opened for appending and is never truncated: a report is the only record of
// why a run died, so destroying one to make room defeats the whole mechanism.
type crashReporter struct {
	dataDir string
	file    *os.File
	// owned tracks whether beginRun claimed the state file. Without it a failure
	// before the instance lock was acquired could overwrite the state of a run
	// that is still going.
	owned    bool
	finished bool
	// baseline is the crash log size once this run had announced itself.
	baseline int64
	// baselineTaken keeps every later state write from moving the baseline. A
	// shutdown rewrites the state several times, and a crash dump landing
	// between two of those writes would end up behind a baseline that had moved
	// past it - unreported, which is the one outcome this file may not produce.
	baselineTaken bool
	// startedAt is when this run announced itself, carried across the rewrites
	// so the field keeps meaning the start of the run rather than the last time
	// anything about it was recorded.
	startedAt string
	// stopSignal and the fields below track the shutdown in progress, so the
	// state file says how far it got if the process does not survive it.
	stopSignal   string
	stopSignalAt time.Time
	phase        string
	phaseAt      time.Time
}

// armCrashOutput routes runtime crash dumps into the crash log. It runs before
// the instance lock is held so that a port conflict or a bad configuration -
// exactly the failures that crash-loop a container - still leave a trace. Only
// creates and appends happen here; nothing a concurrently starting process owns
// is renamed, truncated, or removed.
func armCrashOutput(dataDir string) *crashReporter {
	c := &crashReporter{dataDir: dataDir}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		// Not fatal on its own: the instance lock reports the same directory
		// failure with the context an operator needs.
		log.Printf("crash reporting disabled: create data directory %s: %v", dataDir, err)
		return c
	}
	c.open()
	return c
}

func (c *crashReporter) crashLogPath() string { return filepath.Join(c.dataDir, crashLogName) }
func (c *crashReporter) statePath() string    { return filepath.Join(c.dataDir, crashStateName) }

// open points runtime crash output at the crash log. The runtime duplicates the
// descriptor, so it keeps writing to whichever file was armed last.
func (c *crashReporter) open() {
	file, err := os.OpenFile(c.crashLogPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		log.Printf("crash reporting disabled: open %s: %v", c.crashLogPath(), err)
		return
	}
	if err := debug.SetCrashOutput(file, debug.CrashOptions{}); err != nil {
		log.Printf("crash reporting disabled: arm %s: %v", c.crashLogPath(), err)
		_ = file.Close()
		return
	}
	c.file = file
}

// beginRun reports what the previous run left behind and arms detection for this
// one. It must run under the instance lock: this is the only part of crash
// reporting that rewrites shared state, so holding the lock keeps a second
// process from observing or clobbering a half-updated marker.
func (c *crashReporter) beginRun(version string) {
	previous, hadPreviousRun := readRunState(c.statePath())
	size := c.crashLogSize()
	reported, searched := false, int64(0)
	// An unknown baseline is not a baseline of zero: reading the log from the
	// start would find some earlier run's report and file it against a run that
	// left nothing, hiding the unclean shutdown that lost the state file in the
	// first place.
	if hadPreviousRun && previous.CrashLogBytes != crashLogBytesUnknown {
		reported, searched = c.crashReportSince(previous.CrashLogBytes)
	}
	switch {
	case hadPreviousRun && reported:
		// Checked before CleanShutdown: a panic dump lands after the process
		// has already recorded how it thought it was ending.
		log.Printf("previous run ended with a crash or fatal error; the report is in %s, among the %d bytes searched past the previous start",
			c.crashLogPath(), searched)
	case hadPreviousRun && !previous.CleanShutdown && previous.StopSignal != "":
		// It was stopping when it died, so the kernel log is the wrong place to
		// look: the shutdown was simply given less time than it needed.
		log.Printf("previous run was asked to stop with %s%s and was killed before its shutdown finished%s; "+
			"the container's stop grace period is shorter than the shutdown needs - raise it "+
			"(docker: stop_grace_period, Kubernetes: terminationGracePeriodSeconds), "+
			"or lower ROLLTOP_SHUTDOWN_TIMEOUT so the shutdown fits inside the period it does get",
			previous.StopSignal, atSuffix(previous.StopSignalAt), shutdownProgressDetail(previous))
	case hadPreviousRun && !previous.CleanShutdown:
		log.Printf("previous run was terminated without a clean shutdown and left no crash report; "+
			"no stop signal was recorded, so it was likely killed externally (kernel OOM kill or SIGKILL) "+
			"or could not write %s - check the container exit code and the kernel log", c.crashLogPath())
	}
	if size > crashLogMaxBytes {
		c.rotate()
	}
	c.appendf(runStartMarkerFormat, version, time.Now().UTC().Format(time.RFC3339), os.Getpid())
	c.owned = true
	c.startedAt = time.Now().UTC().Format(time.RFC3339)
	c.writeState(false)
}

// beginShutdown records that a stop signal arrived. It is written before any of
// the shutdown work starts, because the point of the record is to survive a
// process that does not reach the end of that work.
func (c *crashReporter) beginShutdown(signal string) {
	if c == nil {
		return
	}
	c.stopSignal = signal
	c.stopSignalAt = time.Now()
	c.phase, c.phaseAt = "", time.Time{}
	c.writeState(false)
}

// shutdownPhase records what the shutdown is doing now. Each phase is one small
// state write, so the next start can name the step that did not finish - which
// is the difference between an operator raising the grace period and one
// searching the kernel log for an OOM kill that never happened.
func (c *crashReporter) shutdownPhase(phase string) {
	if c == nil || c.stopSignal == "" {
		return
	}
	c.phase = phase
	c.phaseAt = time.Now()
	c.writeState(false)
}

// atSuffix renders a recorded timestamp for a log line, or nothing at all when
// the state did not carry one.
func atSuffix(stamp string) string {
	if strings.TrimSpace(stamp) == "" {
		return ""
	}
	return " at " + stamp
}

// shutdownProgressDetail says how far the previous run's shutdown got. The
// elapsed time is only offered when both stamps parse: a phase name on its own
// still names the step to look at, while a made-up duration would send an
// operator after the wrong one.
func shutdownProgressDetail(state runState) string {
	if state.ShutdownPhase == "" {
		return ""
	}
	signalAt, signalErr := time.Parse(time.RFC3339, state.StopSignalAt)
	phaseAt, phaseErr := time.Parse(time.RFC3339, state.ShutdownPhaseAt)
	if signalErr != nil || phaseErr != nil || phaseAt.Before(signalAt) {
		return fmt.Sprintf(", while %s", state.ShutdownPhase)
	}
	return fmt.Sprintf(", while %s, reached %s after the signal",
		state.ShutdownPhase, phaseAt.Sub(signalAt).Round(time.Second))
}

// finish records how this run ended. It must run while the instance lock is
// still held, otherwise the next process can already have written its own marker
// and crash log by the time this one cleans up.
func (c *crashReporter) finish(err error) {
	if c.finished {
		return
	}
	c.finished = true
	if err == nil || isPlannedRestart(err) {
		// An orderly exit, including the deliberate restart for search index
		// recovery. The size baseline stays as it was, so a panic dump written
		// after this point is still recognized as new on the next start.
		c.writeState(true)
		c.close()
		return
	}
	// Leave the state marked unclean. Together with the size baseline it holds,
	// it tells the next start whether the reason below could be persisted.
	if !c.appendf("%s fatal: %v", time.Now().UTC().Format(time.RFC3339), err) {
		log.Printf("could not persist the fatal error to %s; the next start will report an unclean shutdown instead", c.crashLogPath())
	}
	c.close()
}

// rotate bounds the crash log. The current file is renamed rather than truncated
// so the reports it holds stay readable, and crash output is re-armed on the
// fresh file. A failed rename leaves everything in place and keeps appending: an
// oversized log is a far smaller problem than a destroyed one.
func (c *crashReporter) rotate() {
	previous := filepath.Join(c.dataDir, crashLogPrevName)
	if err := os.Rename(c.crashLogPath(), previous); err != nil {
		log.Printf("rotate oversized crash log %s: %v; continuing to append to it", c.crashLogPath(), err)
		return
	}
	log.Printf("crash log exceeded %d bytes; earlier reports moved to %s", crashLogMaxBytes, previous)
	c.close()
	c.open()
}

// runStartMarkerFormat is the line every run appends to announce itself.
// isRunStartMarker below has to recognize exactly this shape, so the writer and
// the reader are defined together: a marker the reader stops matching would be
// read as a crash report, and every start would announce a failure that never
// happened.
const runStartMarkerFormat = "=== rolltop %s started at %s pid=%d ==="

// isRunStartMarker reports whether a crash log line is a run announcement
// rather than part of a report.
//
// Every field is checked, not just the shape. A line this returns true for is
// skipped when the log is searched, so anything less than a marker it can fully
// account for has to count as evidence: hiding a report costs an operator the
// explanation for an incident, while treating a mangled line as one costs a log
// message that says to go and read the file.
func isRunStartMarker(line string) bool {
	const prefix = "=== rolltop "
	const suffix = " ==="
	if !strings.HasPrefix(line, prefix) || !strings.HasSuffix(line, suffix) {
		return false
	}
	body := line[len(prefix) : len(line)-len(suffix)]
	_, startedAt, found := strings.Cut(body, " started at ")
	if !found {
		return false
	}
	stamp, pid, found := strings.Cut(startedAt, " pid=")
	if !found {
		return false
	}
	if _, err := time.Parse(time.RFC3339, stamp); err != nil {
		return false
	}
	_, err := strconv.Atoi(pid)
	return err == nil
}

// crashReportSince reports whether a crash report was appended past offset, and
// how many bytes it searched to decide. The two differ when the log no longer
// shares its bytes with the baseline and the whole file has to be read, so the
// count is what was searched rather than what the previous run appended.
//
// Growth on its own does not mean a crash. Every start appends its own marker,
// so comparing sizes announces a failure whenever the recorded baseline drifts
// by as little as one line - and a baseline can drift, because it comes from a
// stat that a FUSE-backed volume may answer from cache. Reading what was
// actually appended costs one pass over a file bounded at 1 MiB and cannot
// produce that false alarm.
func (c *crashReporter) crashReportSince(offset int64) (bool, int64) {
	file, err := os.Open(c.crashLogPath())
	if err != nil {
		return false, 0
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return false, 0
	}
	if !info.Mode().IsRegular() {
		// Not a log at all - a directory in its place, say. There is nothing to
		// have been reported, and claiming otherwise would hide the unclean
		// shutdown that a log this broken could not record.
		return false, 0
	}
	if offset < 0 || offset > info.Size() {
		// A rotated or replaced log shares no bytes with the baseline, so an
		// offset into it means nothing. Read what is there instead.
		offset = 0
	}
	if _, err := file.Seek(offset, io.SeekStart); err != nil {
		return false, 0
	}
	searched := info.Size() - offset
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), crashLogMaxBytes)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || isRunStartMarker(line) {
			continue
		}
		return true, searched
	}
	if scanner.Err() != nil {
		// A readable log whose contents will not parse still has something in
		// it. Reporting that keeps a real crash visible, where the alternative
		// hides one behind a read error.
		return true, searched
	}
	return false, searched
}

// appendf writes one line to the crash log and reports whether it landed.
func (c *crashReporter) appendf(format string, args ...any) bool {
	if c.file == nil {
		return false
	}
	if _, err := fmt.Fprintln(c.file, fmt.Sprintf(format, args...)); err != nil {
		log.Printf("write %s: %v", c.crashLogPath(), err)
		return false
	}
	return c.file.Sync() == nil
}

// crashLogSize reports how large the crash log is.
//
// The append handle's own offset is preferred whenever it is ahead of the stat.
// A stat taken right after a write can still report a cached size on a
// FUSE-backed volume, and a baseline recorded from that stat sits one line
// behind the file for the rest of its life. The offset is zero until this
// process writes, so the stat still answers the call made before a run appends
// its marker.
func (c *crashReporter) crashLogSize() int64 {
	size := int64(0)
	if info, err := os.Stat(c.crashLogPath()); err == nil {
		size = info.Size()
	}
	if c.file != nil {
		if offset, err := c.file.Seek(0, io.SeekCurrent); err == nil && offset > size {
			size = offset
		}
	}
	return size
}

// writeState records this run. The size baseline is captured on the first write
// and reused afterwards, so marking a clean shutdown cannot hide a report that
// arrives between the two writes.
func (c *crashReporter) writeState(cleanShutdown bool) {
	if !c.owned {
		return
	}
	if !cleanShutdown && !c.baselineTaken {
		c.baseline = c.crashLogSize()
		c.baselineTaken = true
	}
	state, err := json.Marshal(runState{
		StartedAt:       c.startedAt,
		PID:             os.Getpid(),
		CrashLogBytes:   c.baseline,
		CleanShutdown:   cleanShutdown,
		StopSignal:      c.stopSignal,
		StopSignalAt:    stampOrEmpty(c.stopSignalAt),
		ShutdownPhase:   c.phase,
		ShutdownPhaseAt: stampOrEmpty(c.phaseAt),
	})
	if err != nil {
		return
	}
	if err := os.WriteFile(c.statePath(), append(state, '\n'), 0o600); err != nil {
		log.Printf("write crash state %s: %v", c.statePath(), err)
	}
}

// stampOrEmpty formats a time for the state file, leaving the field out when
// the event it records has not happened.
func stampOrEmpty(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

func (c *crashReporter) close() {
	if c.file == nil {
		return
	}
	_ = c.file.Close()
	c.file = nil
}

// readRunState reports what the previous run recorded, and whether there was a
// previous run at all. State that cannot be parsed still counts as evidence of
// an unclean shutdown; it reports crashLogBytesUnknown so a crash log left from
// before is not misread as a fresh report.
func readRunState(path string) (runState, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return runState{}, false
	}
	var state runState
	if err := json.Unmarshal(data, &state); err != nil {
		return runState{CrashLogBytes: crashLogBytesUnknown}, true
	}
	return state, true
}
