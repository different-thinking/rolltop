// File overview: The build flags a Go plugin has to share with the binary that
// loads it. This lives beside the loader rather than in the tests that compile
// plugins, because it is the loading rule that dictates it.

package plugins

// GoBuildFlags returns the `go build` flags a plugin must be compiled with to
// be loadable by the running binary.
//
// plugin.Open refuses a plugin whose build fingerprint differs from the host's,
// and the race detector changes that fingerprint for every package it
// instruments. A test binary compiled with -race therefore cannot load a plugin
// compiled without it; the failure reads "plugin was built with a different
// version of package internal/runtime/sys", which names neither the race
// detector nor the plugin build. Every test that compiles a plugin passes these
// flags, so `go test -race ./...` works on this repository.
//
// The same class already cost this repository once: -coverprofile instruments
// the test binary the same way, which is why .github/workflows/ci.yml runs the
// suite without coverage.
func GoBuildFlags() []string {
	if raceEnabled {
		return []string{"-race"}
	}
	return nil
}
