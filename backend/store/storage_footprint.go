// File overview: What one tenant's mail occupies inside PostgreSQL, as a figure
// that belongs to that tenant rather than to the server.

package store

import (
	"context"
	"fmt"
)

// UserMailRowBytes sums the stored size of the rows one tenant's mail is made
// of: the message headers and previews, the attachment and location rows hanging
// off them, and the blob records that point at the raw files on the volume.
//
// The database reports one size for itself and nothing per tenant, and that
// total is an operator's number: it covers every tenant at once, plus indexes,
// plus the space autovacuum has not reclaimed. Summing pg_column_size over this
// tenant's own rows is the share a reader can be shown without describing
// somebody else's mailbox. It measures the data and not the indexes built on it,
// so it reads slightly under what the tenant costs the server - which is the
// direction an under-count belongs in for a figure sitting next to two measured
// directory sizes.
//
// The sum is a sequential scan of the tenant's rows, so callers cache it; the
// settings page holds every storage figure for five minutes.
func (s *Store) UserMailRowBytes(ctx context.Context, userID int64) (int64, error) {
	if userID <= 0 {
		return 0, fmt.Errorf("user id must be positive")
	}
	db, err := s.dataDB(ctx, userID)
	if err != nil {
		return 0, err
	}
	var bytes int64
	err = db.QueryRowContext(ctx, `SELECT
			(SELECT coalesce(sum(pg_column_size(m.*)), 0) FROM messages m WHERE m.user_id = ?)
			+ (SELECT coalesce(sum(pg_column_size(a.*)), 0) FROM attachments a WHERE a.user_id = ?)
			+ (SELECT coalesce(sum(pg_column_size(l.*)), 0) FROM locations l WHERE l.user_id = ?)
			+ (SELECT coalesce(sum(pg_column_size(b.*)), 0) FROM blobs b WHERE b.user_id = ?)`,
		userID, userID, userID, userID).Scan(&bytes)
	if err != nil {
		return 0, err
	}
	return bytes, nil
}
