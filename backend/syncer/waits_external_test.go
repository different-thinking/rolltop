// File overview: The same two waiting conventions as waits_test.go, for the
// tests that live in the external test package. A test file cannot import
// another package's test file, so the constants are declared once per package
// rather than shared; waits_test.go carries the reasoning behind their length.

package syncer_test

import "time"

const (
	// waitForEvent bounds a wait for something the test says must happen.
	waitForEvent = 30 * time.Second

	// waitForRecoveryPass is the same for waits that sit behind a whole mailbox
	// recovery pass or a multi-hundred-message batch.
	waitForRecoveryPass = 60 * time.Second
)
