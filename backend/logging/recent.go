// File overview: In-memory tail of the process log. A hosted installation is
// often the one place where nobody can read the container log: the operator has
// the admin account and nothing else. Without this, "internal server error"
// is the whole story a browser ever gets, and the line that names the actual
// failure is written somewhere unreachable. The recorder tees every log line
// into a bounded ring so the admin database page can show the newest ones.

package logging

import (
	"io"
	"strings"
	"sync"
	"time"
)

// recentCapacity bounds the tail. It is a diagnostic aid, not an audit trail:
// enough lines to cover the failure an operator just reproduced, few enough
// that the buffer stays negligible next to the rest of the process.
const recentCapacity = 500

// maxRecentLineBytes truncates a single record. Multi-line messages (a SQLite
// integrity report, a remote server reply) would otherwise let one line crowd
// out the entire history.
const maxRecentLineBytes = 4000

// Record is one captured log line with the time the process recorded it.
type Record struct {
	Time    time.Time
	Message string
}

type recorder struct {
	mu      sync.Mutex
	records [recentCapacity]Record
	next    int
	filled  bool
}

var recent recorder

// Recorder returns the writer that keeps the newest log lines in memory. The
// binary tees the standard logger into it, so every package that logs through
// the log package is captured without changing its call sites.
func Recorder() io.Writer { return &recent }

func (r *recorder) Write(p []byte) (int, error) {
	r.add(time.Now().UTC(), string(p))
	return len(p), nil
}

func (r *recorder) add(at time.Time, line string) {
	message := trimStandardPrefix(strings.TrimRight(line, "\r\n"))
	if strings.TrimSpace(message) == "" {
		return
	}
	if len(message) > maxRecentLineBytes {
		message = message[:maxRecentLineBytes] + "..."
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.records[r.next] = Record{Time: at, Message: message}
	r.next++
	if r.next == recentCapacity {
		r.next = 0
		r.filled = true
	}
}

// Recent returns up to limit of the newest captured lines, oldest first, so the
// caller can render them in the order they happened.
func Recent(limit int) []Record {
	return recent.snapshot(limit)
}

func (r *recorder) snapshot(limit int) []Record {
	if limit <= 0 {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	stored := r.next
	if r.filled {
		stored = recentCapacity
	}
	if limit > stored {
		limit = stored
	}
	out := make([]Record, 0, limit)
	// Walk back from the newest record so a small limit returns the tail rather
	// than the oldest lines the ring still happens to hold.
	for offset := limit; offset > 0; offset-- {
		index := (r.next - offset + recentCapacity) % recentCapacity
		out = append(out, r.records[index])
	}
	return out
}

// trimStandardPrefix drops the date and time the standard logger already wrote,
// because every record carries its own timestamp. A line logged with different
// flags keeps whatever it starts with rather than losing real content to a
// guess.
func trimStandardPrefix(line string) string {
	const prefixLength = len("2006/01/02 15:04:05 ")
	if len(line) < prefixLength {
		return line
	}
	if _, err := time.Parse("2006/01/02 15:04:05", line[:prefixLength-1]); err != nil {
		return line
	}
	return line[prefixLength:]
}
