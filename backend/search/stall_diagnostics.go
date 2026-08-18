// File overview: A durable copy of writer-stall diagnostics inside the data
// directory. The stack that names the blocked Bleve frame is the one artifact
// that explains a stall, and it is written to stderr, where a container log
// pipeline keeps only the first line of a multi-line entry and an operator with
// a shell in the container cannot read the history at all. This file puts the
// same text on the volume, next to crash.log, where it survives the restart.

package search

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// stallLogName is a sibling of crash.log so the two live together.
const stallLogName = "search-stall.log"

// stallLogMaxBytes bounds the file. Reports are appended, so the current file is
// renamed aside rather than truncated: losing the report that explains an
// incident to make room for the next one would defeat the point.
const stallLogMaxBytes = 1 << 20

// SetStallDiagnosticsDir points writer-stall reports at a directory on the data
// volume. Without it the diagnostics only reach the process log.
func (s *Service) SetStallDiagnosticsDir(dataDir string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.stallDiagnosticsDir = strings.TrimSpace(dataDir)
	s.mu.Unlock()
}

func (s *Service) stallDiagnosticsPath() string {
	if s == nil {
		return ""
	}
	s.mu.Lock()
	dir := s.stallDiagnosticsDir
	s.mu.Unlock()
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, stallLogName)
}

// stallDiagnosticsMu serializes appends so two stalled writers cannot interleave
// their reports inside one file.
var stallDiagnosticsMu sync.Mutex

// recordStallDiagnostics appends one report. Every failure is returned rather
// than logged here, so the caller decides how loud a failed diagnostic write is
// during an incident that is already going badly.
func (s *Service) recordStallDiagnostics(at time.Time, summary, stack string) error {
	path := s.stallDiagnosticsPath()
	if path == "" {
		return nil
	}
	stallDiagnosticsMu.Lock()
	defer stallDiagnosticsMu.Unlock()
	if err := rotateStallLog(path); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer file.Close()
	report := fmt.Sprintf("=== rolltop search writer stall at %s ===\n%s\n",
		at.UTC().Format(time.RFC3339Nano), summary)
	if strings.TrimSpace(stack) != "" {
		report += stack + "\n"
	}
	if _, err := file.WriteString(report); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	// The process that wrote this is about to be restarted, and on the storage
	// this exists to diagnose an unsynced append is exactly what gets lost.
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync %s: %w", path, err)
	}
	return nil
}

func rotateStallLog(path string) error {
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect %s: %w", path, err)
	}
	if info.Size() < stallLogMaxBytes {
		return nil
	}
	if err := os.Rename(path, path+".prev"); err != nil {
		return fmt.Errorf("rotate %s: %w", path, err)
	}
	return nil
}
