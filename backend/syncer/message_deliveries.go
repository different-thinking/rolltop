// File overview: The catch-up pass that reads stored mail for parcels. Newly
// fetched messages are read while the parser still has them open; this is what
// reaches the mail that was already in the mailbox when the feature arrived,
// and what a change to the extraction rules re-reads.
//
// It can only ever reach part of a mailbox. Blob retention prunes raw messages
// after ROLLTOP_BLOB_RETENTION and clears blob_path with them, so on a default
// install this pass sees the last fortnight and nothing before it. That is the
// right amount for what it is for: a parcel older than that has arrived.

package syncer

import (
	"context"

	"rolltop/backend/mailparse"
	"rolltop/backend/store"
)

// ScanPendingDeliveriesForUser reads the next batch of stored messages for the
// parcels they name. It returns how many messages it read so the caller knows
// whether to come back.
//
// Every candidate leaves the pass stamped, including the ones whose stored
// message turned out to be unreadable. A row that stayed pending would be
// selected on every pass and the backfill would never finish.
func (s *Service) ScanPendingDeliveriesForUser(ctx context.Context, userID int64, limit int) (int, error) {
	if s == nil || s.Store == nil || userID <= 0 {
		return 0, nil
	}
	if limit <= 0 {
		limit = store.DeliveryBackfillLimit
	}
	candidates, err := s.Store.ListMessagesNeedingDeliveryScan(ctx, userID, limit)
	if err != nil || len(candidates) == 0 {
		return 0, err
	}
	read := 0
	unreadable := make([]int64, 0, len(candidates))
	for _, candidate := range candidates {
		if err := ctx.Err(); err != nil {
			break
		}
		notices, ok := s.deliveriesInStoredMessage(userID, candidate)
		if !ok {
			unreadable = append(unreadable, candidate.ID)
			continue
		}
		if err := s.Store.ReplaceMessageShipments(ctx, userID, candidate.ID, candidate.Date, notices); err != nil {
			return read, err
		}
		read++
	}
	if err := s.Store.MarkMessagesDeliveryScanned(ctx, userID, unreadable); err != nil {
		return read, err
	}
	return read + len(unreadable), ctx.Err()
}

// deliveriesInStoredMessage reads one candidate's stored message. The second
// return is false when there was nothing to read, which is a message to stamp
// rather than one to record an empty answer for.
//
// A scan that stopped at its budget is kept: unlike a category, a parcel is not
// an answer that can be wrong, only one that can be missing. Whatever the scan
// did reach is a number the message really named, and the numbers past the
// budget are the ones this pass could never have had.
func (s *Service) deliveriesInStoredMessage(userID int64, candidate store.ShipmentCandidate) ([]mailparse.DeliveryNotice, bool) {
	if s.Blobs == nil || candidate.BlobPath == "" {
		return nil, false
	}
	f, err := s.Blobs.OpenUserBlob(userID, candidate.BlobPath)
	if err != nil {
		return nil, false
	}
	defer f.Close()
	notices, _, err := mailparse.DeliveryNoticesReaderScan(f)
	if err != nil {
		return nil, false
	}
	return notices, true
}
