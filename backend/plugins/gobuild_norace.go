//go:build !race

package plugins

// raceEnabled reports that this binary is not instrumented by the race
// detector, which is the ordinary build and what production ships.
const raceEnabled = false
