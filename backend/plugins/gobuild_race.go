//go:build race

package plugins

// raceEnabled reports that this binary is instrumented by the race detector,
// which the toolchain signals through the "race" build tag.
const raceEnabled = true
