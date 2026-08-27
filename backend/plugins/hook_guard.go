// File overview: Panic and timeout guards around in-process backend plugin
// hook calls. A backend plugin is a Go plugin loaded into this process, so a
// hook that panics takes the process with it and a hook that never returns
// holds whatever called it -- a sync turn, or an HTTP request drawing a
// message. Neither is a failure the caller can attribute to a plugin without
// this.

package plugins

import (
	"errors"
	"fmt"
	"time"
)

// ErrHookTimeout reports that a plugin hook did not answer inside its budget.
// An in-process call cannot be preempted, so the hook goes on running and only
// its result is abandoned: the caller stops waiting, and the goroutine ends
// whenever the plugin finally returns.
var ErrHookTimeout = errors.New("plugin hook timed out")

// ErrHookPanic reports that a plugin hook panicked. The panic value itself is
// deliberately not carried into the error: a panic value can hold content
// derived from the message being processed, and these errors reach the log.
var ErrHookPanic = errors.New("plugin hook panicked")

// IsHookGuardFailure reports whether an error came from the guard rather than
// from the plugin's own logic. It is the difference between "this plugin could
// not answer" -- skip it and carry on with the rest -- and "this plugin
// answered with an error", which is the caller's to interpret.
func IsHookGuardFailure(err error) bool {
	return errors.Is(err, ErrHookTimeout) || errors.Is(err, ErrHookPanic)
}

// CallHook runs one plugin invocation under a panic guard and a wall-clock
// budget. An error the plugin returned is passed back unchanged; a guard
// failure wraps ErrHookPanic or ErrHookTimeout so IsHookGuardFailure can tell
// them apart.
//
// The channel is buffered, so a hook that returns after the budget has expired
// still delivers into it and its goroutine ends rather than blocking forever on
// a receiver that has gone.
func CallHook[T any](timeout time.Duration, call func() (T, error)) (T, error) {
	type outcome struct {
		value T
		err   error
	}
	results := make(chan outcome, 1)
	go func() {
		var zero T
		defer func() {
			if recovered := recover(); recovered != nil {
				results <- outcome{value: zero, err: fmt.Errorf("%w (%T)", ErrHookPanic, recovered)}
			}
		}()
		value, err := call()
		results <- outcome{value: value, err: err}
	}()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case result := <-results:
		return result.value, result.err
	case <-timer.C:
		var zero T
		return zero, fmt.Errorf("%w after %s", ErrHookTimeout, timeout)
	}
}
