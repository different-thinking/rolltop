package store

import "flag"

// updateBaseline rewrites the generated PostgreSQL baseline instead of
// failing when it differs.
var updateBaseline = flag.Bool("update", false, "rewrite generated golden files")

func updateGolden() bool { return updateBaseline != nil && *updateBaseline }
