// File overview: Value helpers shared by the store's query layer.
//
// Timestamps are stored as unix seconds in BIGINT columns and booleans as 0/1
// integers, which is what the SQLite schema used and what the PostgreSQL
// baseline deliberately kept (WP2 in docs/postgres-migration-plan.md). Genuine
// BOOLEAN and TIMESTAMPTZ columns are a later cleanup, not part of the move.

package store

import "time"

func nowUnix() int64 {
	return time.Now().UTC().Unix()
}

func unixTime(v int64) time.Time {
	if v == 0 {
		return time.Time{}
	}
	return time.Unix(v, 0).UTC()
}

func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}
