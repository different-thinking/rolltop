// File overview: The two waiting conventions every test in this package
// follows, and why they are as long as they are.
//
// Each test here drives a real PostgreSQL database, a real search index and a
// runner with its own goroutines, so anything a test waits for sits behind work
// whose duration nobody controls: a connection from the pool, a sync run row, a
// batch of messages through the index. `go test -p 2` puts a second package's
// database load beside it, and CI runs the pair on four shared vCPUs.
//
// A wait for something that must happen is therefore generous. It costs nothing
// on the passing path - the event arrives in milliseconds and the wait ends -
// and the only thing a tighter bound buys is a red build on a busy machine,
// which is exactly what this package produced roughly every other run before
// these constants existed.
//
// The opposite wait - the one that asserts something does *not* happen inside a
// window - stays short and stays written out at its call site. Its duration is
// part of the assertion rather than patience, so there is no shared constant for
// it and it must not be widened to match the ones below.

package syncer

import "time"

const (
	// waitForEvent bounds a wait for something the test says must happen. Long
	// enough that database latency is never what ends it.
	waitForEvent = 30 * time.Second

	// waitForRecoveryPass is the same for waits that sit behind a whole mailbox
	// recovery pass or a multi-hundred-message batch, where the work itself
	// takes seconds before the event the test is waiting for can occur.
	waitForRecoveryPass = 60 * time.Second

	// boundedTurnBudget is what the tests about a bounded mailbox turn give that
	// turn. It is deliberately far larger than the millisecond the assertion
	// itself needs: the budget starts when the turn does, so it also has to
	// cover opening a sync run and reading the mailbox out of PostgreSQL before
	// the fetcher is reached. A test whose fetcher blocks until released still
	// sees the deadline expire mid-fetch, so nothing is weakened by the extra
	// room - while a budget tighter than that latency turns every busy machine
	// into a red build.
	boundedTurnBudget = 5 * time.Second
)
