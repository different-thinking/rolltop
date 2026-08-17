// File overview: Minimal leveled logging on top of the standard log package.
// Debugf lines are suppressed unless the debug level is selected, so production
// logs carry only operational messages while development keeps the verbose
// plugin and unsubscribe traces.
//
// This package owns what a log level means. Nothing here reads the environment:
// config.Load validates the operator's setting through ParseLevel and the
// binary applies it once with SetLevel, so there is exactly one interpretation
// of the value rather than one here and another in the config package.

package logging

import (
	"fmt"
	"log"
	"strings"
	"sync/atomic"
)

// Levels Rolltop understands.
const (
	LevelInfo  = "info"
	LevelDebug = "debug"
)

// debug is false until the binary applies the configured level, so a Debugf
// that runs before then stays quiet rather than latching an unvalidated guess.
var debug atomic.Bool

// ParseLevel validates an operator-supplied log level. Callers that read
// configuration use it so an unknown value is rejected at startup instead of
// silently meaning "not debug".
func ParseLevel(value string) (string, error) {
	switch level := strings.ToLower(strings.TrimSpace(value)); level {
	case "":
		return LevelInfo, nil
	case LevelInfo, LevelDebug:
		return level, nil
	default:
		return "", fmt.Errorf("log level must be %q or %q, got %q", LevelInfo, LevelDebug, value)
	}
}

// DebugEnabled reports whether debug-level lines are being written. Hot paths
// use it to skip building expensive arguments.
func DebugEnabled() bool {
	return debug.Load()
}

// SetLevel applies a level returned by ParseLevel.
func SetLevel(level string) {
	debug.Store(level == LevelDebug)
}

// SetDebug forces debug logging on or off. Tests use it to exercise both levels
// without mutating process environment state.
func SetDebug(enabled bool) {
	debug.Store(enabled)
}

// Debugf logs like log.Printf with a "debug " prefix, but only when debug
// logging is enabled. Line separators in the rendered message are escaped so
// values taken from mail content or URLs cannot forge extra log records.
// Arguments are evaluated eagerly even when debug is off, so keep expensive
// arguments behind a DebugEnabled() guard on hot paths.
func Debugf(format string, args ...any) {
	if !DebugEnabled() {
		return
	}
	message := fmt.Sprintf(format, args...)
	if strings.ContainsAny(message, "\r\n") {
		message = strings.NewReplacer("\r", `\r`, "\n", `\n`).Replace(message)
	}
	log.Printf("debug %s", message)
}
