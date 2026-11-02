// File overview: The catch-up pass that reads stored mail for tickets. Newly
// fetched messages are read while the parser still has them open; this is what
// reaches the mail that was already in the mailbox when the feature arrived,
// and what a change to the ticket readers re-reads.
//
// It opens attachment bodies -- a Wallet pass, a DB ticket PDF through the same
// text extractor search uses -- so its batch is a quarter of the parcel pass's.
// Like every backfill here it only reaches what blob retention still holds.

package syncer

import (
	"context"

	"rolltop/backend/mailparse"
	"rolltop/backend/store"
)

// ScanPendingTravelForUser reads the next batch of stored messages for the
// tickets they carry. It returns how many messages it read so the caller knows
// whether to come back. Every candidate leaves the pass stamped, the unreadable
// ones included, or the backfill would never finish.
//
// Trips it records are never announced: this is old mail by definition, and a
// push for every ticket the mailbox already held would be the opposite of news.
func (s *Service) ScanPendingTravelForUser(ctx context.Context, userID int64, limit int) (int, error) {
	if s == nil || s.Store == nil || userID <= 0 {
		return 0, nil
	}
	if limit <= 0 {
		limit = store.TravelBackfillLimit
	}
	candidates, err := s.Store.ListMessagesNeedingTravelScan(ctx, userID, limit)
	if err != nil || len(candidates) == 0 {
		return 0, err
	}
	read := 0
	unreadable := make([]int64, 0, len(candidates))
	empty := make([]int64, 0, len(candidates))
	for _, candidate := range candidates {
		if err := ctx.Err(); err != nil {
			break
		}
		bookings, ok := s.travelInStoredMessage(userID, candidate)
		switch {
		case !ok:
			unreadable = append(unreadable, candidate.ID)
		case len(bookings) == 0:
			empty = append(empty, candidate.ID)
		default:
			if err := s.Store.ReplaceMessageTrips(ctx, userID, candidate.ID, candidate.Date, bookings, false); err != nil {
				return read, err
			}
			read++
		}
	}
	if err := s.Store.ClearMessageTrips(ctx, userID, empty); err != nil {
		return read, err
	}
	if err := s.Store.MarkMessagesTravelScanned(ctx, userID, unreadable); err != nil {
		return read, err
	}
	return read + len(empty) + len(unreadable), ctx.Err()
}

// travelInStoredMessage reads one candidate's stored message. The second return
// is false when there was nothing to read.
func (s *Service) travelInStoredMessage(userID int64, candidate store.TripCandidate) ([]mailparse.TravelBooking, bool) {
	if s.Blobs == nil || candidate.BlobPath == "" {
		return nil, false
	}
	f, err := s.Blobs.OpenUserBlob(userID, candidate.BlobPath)
	if err != nil {
		return nil, false
	}
	defer f.Close()
	bookings, err := mailparse.TravelBookingsReaderScan(f)
	if err != nil {
		return nil, false
	}
	return bookings, true
}
