// File overview: Process-wide soft heap ceiling for the Go runtime. Rolltop is
// one container in which IMAP fetches, MIME parsing, SQLite writes, and Bleve
// commits share a single heap. Without a limit the collector aims at roughly
// twice the live heap, so a large initial sync is free to grow until the
// container's memory limit stops the process mid-write. A soft ceiling turns
// that into more collection instead of a kill.

package memlimit

import (
	"errors"
	"fmt"
	"math"
	"os"
	"runtime/debug"
	"strconv"
	"strings"
)

// DefaultPercent is the share of the detected container or machine limit the
// heap may use when no explicit value is configured. The remainder covers what
// Go heap accounting does not: goroutine stacks, the SQLite library's own
// allocations, and the mmapped Bleve segments a search touches.
//
// Those segments are page cache and share the container's limit with the heap,
// so this share only holds while the index fits in what it leaves over. Startup
// measures the index against that remainder and warns when it does not, because
// the failure mode is not an allocation error: it is eviction of the pages the
// next commit needs, and commits that then run long enough to look hung.
const DefaultPercent = 80

// MinimumBytes is the smallest ceiling worth installing. Below it the collector
// would run continuously without ever freeing enough to matter, which is slower
// than no limit and no safer.
const MinimumBytes = 64 * 1024 * 1024

// Sources for an applied limit.
const (
	SourceOff        = "off"
	SourceConfig     = "config"
	SourceEnvGoMem   = "GOMEMLIMIT"
	SourceCgroup     = "cgroup"
	SourceMemTotal   = "meminfo"
	sourceUndetected = "none"
)

// Request is the operator's configured ceiling, before the machine is
// inspected. At most one of Disabled, Bytes, or Percent is meaningful.
type Request struct {
	// Disabled leaves the runtime at its default of unlimited growth.
	Disabled bool
	// Bytes is an absolute ceiling.
	Bytes int64
	// Percent is a share of the detected container or machine limit.
	Percent int
}

// Applied reports what the process asked the runtime for, so startup can say so
// in the log where an operator diagnosing a crash loop will look.
type Applied struct {
	// Bytes is the soft ceiling now in effect, or 0 when none is.
	Bytes int64
	// Source names where the value came from.
	Source string
	// Detected is the container or machine limit this process may use, whether
	// or not the ceiling was derived from it as a percentage.
	Detected int64
	// Percent is the share taken of Detected, or 0 for an absolute limit.
	Percent int
}

// Description renders the startup log line.
func (a Applied) Description() string {
	switch a.Source {
	case SourceOff:
		return "memory limit off, the Go heap may grow until the operating system intervenes"
	case SourceEnvGoMem:
		return fmt.Sprintf("memory limit %s from GOMEMLIMIT", FormatBytes(a.Bytes))
	case SourceConfig:
		return fmt.Sprintf("memory limit %s from ROLLTOP_MEMORY_LIMIT", FormatBytes(a.Bytes))
	default:
		return fmt.Sprintf("memory limit %s (%d%% of the %s %s limit)",
			FormatBytes(a.Bytes), a.Percent, FormatBytes(a.Detected), a.Source)
	}
}

// DefaultRequest is what an unset ROLLTOP_MEMORY_LIMIT means.
func DefaultRequest() Request { return Request{Percent: DefaultPercent} }

// ParseRequest reads the operator-facing value. It accepts an absolute size
// ("768MiB", "2GB", "805306368"), a share of the detected limit ("75%"), or
// "off" to leave the runtime unbounded. An empty value selects the default
// share, which is what an operator who never heard of this setting gets.
func ParseRequest(raw string) (Request, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return DefaultRequest(), nil
	}
	switch strings.ToLower(value) {
	case "off", "none", "unlimited", "0":
		return Request{Disabled: true}, nil
	case "auto", "default":
		return DefaultRequest(), nil
	}
	if strings.HasSuffix(value, "%") {
		percent, err := strconv.Atoi(strings.TrimSpace(strings.TrimSuffix(value, "%")))
		if err != nil {
			return Request{}, fmt.Errorf("%q is not a percentage", value)
		}
		if percent < 10 || percent > 100 {
			return Request{}, fmt.Errorf("percentage %d%% is outside the supported 10%% to 100%% range", percent)
		}
		return Request{Percent: percent}, nil
	}
	bytes, err := ParseBytes(value)
	if err != nil {
		return Request{}, err
	}
	if bytes < MinimumBytes {
		return Request{}, fmt.Errorf("%q is below the %s minimum", value, FormatBytes(MinimumBytes))
	}
	return Request{Bytes: bytes}, nil
}

// Apply installs the resolved ceiling in the Go runtime and reports it. An
// operator-set GOMEMLIMIT wins: the runtime has already applied that value, and
// silently replacing an explicit setting is worse than deferring to it.
func Apply(request Request) Applied {
	detected, source := DetectLimit()
	applied := resolve(request, os.Getenv("GOMEMLIMIT"), currentRuntimeLimit(), detected, source)
	if applied.Bytes > 0 && applied.Source != SourceEnvGoMem {
		debug.SetMemoryLimit(applied.Bytes)
	}
	return applied
}

func resolve(request Request, gomemlimit string, runtimeLimit, detected int64, detectedSource string) Applied {
	// Detected is reported for every source, not only for a percentage. It is
	// what the heap ceiling has to be judged against - how much of the container
	// is left for the mapped search index - and an operator who set an explicit
	// ceiling is exactly the one who needs that comparison made.
	if strings.TrimSpace(gomemlimit) != "" {
		// The runtime parses and applies GOMEMLIMIT itself, "off" included.
		return Applied{Bytes: runtimeLimit, Source: SourceEnvGoMem, Detected: detected}
	}
	if request.Disabled {
		return Applied{Source: SourceOff, Detected: detected}
	}
	if request.Bytes > 0 {
		return Applied{Bytes: request.Bytes, Source: SourceConfig, Detected: detected}
	}
	if detected <= 0 {
		// Nothing to take a share of. Unlimited growth is what happens today,
		// so report it plainly instead of guessing a ceiling.
		return Applied{Source: SourceOff}
	}
	percent := request.Percent
	if percent <= 0 {
		percent = DefaultPercent
	}
	// A detected limit is a byte count from the kernel, far below the range
	// where multiplying by a percentage could overflow.
	limit := detected * int64(percent) / 100
	if limit < MinimumBytes {
		// A container this small cannot run a sync inside the ceiling anyway.
		// Leaving the runtime unbounded keeps it slow rather than stuck.
		return Applied{Source: SourceOff, Detected: detected}
	}
	return Applied{Bytes: limit, Source: detectedSource, Detected: detected, Percent: percent}
}

// currentRuntimeLimit reads the runtime's ceiling without changing it.
func currentRuntimeLimit() int64 {
	current := debug.SetMemoryLimit(-1)
	if current == math.MaxInt64 {
		return 0
	}
	return current
}

// DetectLimit reports the memory this process may use: the cgroup limit when
// the container has one, otherwise the machine's total memory. A self-hoster
// who runs Rolltop without a container limit still benefits, because the
// alternative is growing into the host's swap and OOM killer.
func DetectLimit() (int64, string) {
	for _, path := range cgroupLimitPaths {
		if limit, ok := readCgroupLimitFile(path); ok {
			return limit, SourceCgroup
		}
	}
	if total, ok := readMemTotal(memInfoPath); ok {
		return total, SourceMemTotal
	}
	return 0, sourceUndetected
}

// cgroupLimitPaths are the v2 and v1 files, in that order. A container mounts
// its own cgroup namespace at these paths, so the process reads its own limit
// rather than the host's.
var cgroupLimitPaths = []string{
	"/sys/fs/cgroup/memory.max",
	"/sys/fs/cgroup/memory/memory.limit_in_bytes",
}

var memInfoPath = "/proc/meminfo"

func readCgroupLimitFile(path string) (int64, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	value := strings.TrimSpace(string(raw))
	if value == "" || strings.EqualFold(value, "max") {
		return 0, false
	}
	limit, err := strconv.ParseInt(value, 10, 64)
	if err != nil || limit <= 0 {
		return 0, false
	}
	// cgroup v1 spells "no limit" as a page-aligned value near the top of the
	// int64 range rather than a word, so anything implausibly large is no limit.
	if limit >= 1<<62 {
		return 0, false
	}
	return limit, true
}

func readMemTotal(path string) (int64, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	for _, line := range strings.Split(string(raw), "\n") {
		rest, found := strings.CutPrefix(line, "MemTotal:")
		if !found {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			return 0, false
		}
		kb, err := strconv.ParseInt(fields[0], 10, 64)
		if err != nil || kb <= 0 {
			return 0, false
		}
		return kb * 1024, true
	}
	return 0, false
}

var errUnknownSizeUnit = errors.New("size must be a plain byte count or end in K, M, G, or T")

// ParseBytes reads a byte size written the way container limits usually are.
// Binary and decimal suffixes are treated alike: this value bounds a heap, and
// pretending to distinguish 1000 from 1024 there would only invite typos.
func ParseBytes(value string) (int64, error) {
	trimmed := strings.TrimSpace(value)
	trimmed = strings.TrimSuffix(strings.TrimSuffix(trimmed, "B"), "b")
	trimmed = strings.TrimSuffix(strings.TrimSuffix(trimmed, "i"), "I")
	if trimmed == "" {
		return 0, fmt.Errorf("%q is not a size", value)
	}
	multiplier := int64(1)
	switch unit := trimmed[len(trimmed)-1]; unit {
	case 'K', 'k':
		multiplier = 1 << 10
	case 'M', 'm':
		multiplier = 1 << 20
	case 'G', 'g':
		multiplier = 1 << 30
	case 'T', 't':
		multiplier = 1 << 40
	case '0', '1', '2', '3', '4', '5', '6', '7', '8', '9':
	default:
		return 0, fmt.Errorf("%q: %w", value, errUnknownSizeUnit)
	}
	if multiplier > 1 {
		trimmed = trimmed[:len(trimmed)-1]
	}
	number, err := strconv.ParseInt(strings.TrimSpace(trimmed), 10, 64)
	if err != nil || number <= 0 {
		return 0, fmt.Errorf("%q is not a size", value)
	}
	if number > math.MaxInt64/multiplier {
		return 0, fmt.Errorf("%q is too large", value)
	}
	return number * multiplier, nil
}

// FormatBytes renders a size for logs and error messages.
func FormatBytes(value int64) string {
	if value <= 0 {
		return "unlimited"
	}
	units := []struct {
		suffix string
		scale  int64
	}{
		{"TiB", 1 << 40},
		{"GiB", 1 << 30},
		{"MiB", 1 << 20},
		{"KiB", 1 << 10},
	}
	for _, unit := range units {
		if value >= unit.scale {
			return fmt.Sprintf("%.1f%s", float64(value)/float64(unit.scale), unit.suffix)
		}
	}
	return fmt.Sprintf("%dB", value)
}
