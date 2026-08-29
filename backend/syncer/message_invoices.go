// File overview: The catch-up pass that reads stored mail for bills. Newly
// fetched messages are read while the parser still has them open; this is what
// reaches the mail that was already in the mailbox when the feature arrived,
// and what a change to the extraction rules re-reads.
//
// It is the most expensive backfill in this package, and the only one that
// opens attachment bodies -- a PDF invoice is read by an external text
// extractor here exactly as it is on the fetch path. Two things pay for that.
// Only mail the category has already called paperwork is selected at all, which
// is a small fraction of a mailbox; and the batch is a quarter of the delivery
// pass's, so one turn's worth of work stays comparable.
//
// Like the delivery pass it can only ever reach part of a mailbox. Blob
// retention prunes raw messages after ROLLTOP_BLOB_RETENTION and clears
// blob_path with them, so on a default install this pass sees the last
// fortnight and nothing before it.

package syncer

import (
	"context"

	"rolltop/backend/mailparse"
	"rolltop/backend/store"
)

// ScanPendingInvoicesForUser reads the next batch of stored paperwork for the
// bills it names. It returns how many messages it read so the caller knows
// whether to come back.
//
// Every candidate leaves the pass stamped, including the ones whose stored
// message turned out to be unreadable. A row that stayed pending would be
// selected on every pass and the backfill would never finish.
func (s *Service) ScanPendingInvoicesForUser(ctx context.Context, userID int64, limit int) (int, error) {
	if s == nil || s.Store == nil || userID <= 0 {
		return 0, nil
	}
	if limit <= 0 {
		limit = store.InvoiceBackfillLimit
	}
	candidates, err := s.Store.ListMessagesNeedingInvoiceScan(ctx, userID, limit)
	if err != nil || len(candidates) == 0 {
		return 0, err
	}
	read := 0
	// The three outcomes are kept apart because they cost different things. A
	// message that named a bill is written one at a time, because the upsert is
	// per invoice; the other two are the majority and are stamped in one
	// statement each at the end of the batch.
	unreadable := make([]int64, 0, len(candidates))
	empty := make([]int64, 0, len(candidates))
	for _, candidate := range candidates {
		if err := ctx.Err(); err != nil {
			break
		}
		notice, ok := s.invoiceInStoredMessage(userID, candidate)
		switch {
		case !ok:
			unreadable = append(unreadable, candidate.ID)
		case notice == nil:
			empty = append(empty, candidate.ID)
		default:
			if err := s.Store.ReplaceMessageInvoice(ctx, userID, candidate.ID, candidate.Date, notice); err != nil {
				return read, err
			}
			read++
		}
	}
	if err := s.Store.ClearMessageInvoices(ctx, userID, empty); err != nil {
		return read, err
	}
	if err := s.Store.MarkMessagesInvoiceScanned(ctx, userID, unreadable); err != nil {
		return read, err
	}
	return read + len(empty) + len(unreadable), ctx.Err()
}

// invoiceInStoredMessage reads one candidate's stored message. The second
// return is false when there was nothing to read, which is a message to stamp
// rather than one to record an empty answer for.
func (s *Service) invoiceInStoredMessage(userID int64, candidate store.InvoiceCandidate) (*mailparse.InvoiceNotice, bool) {
	if s.Blobs == nil || candidate.BlobPath == "" {
		return nil, false
	}
	f, err := s.Blobs.OpenUserBlob(userID, candidate.BlobPath)
	if err != nil {
		return nil, false
	}
	defer f.Close()
	notice, complete, err := mailparse.InvoiceNoticeReaderScan(f)
	if err != nil {
		return nil, false
	}
	return invoiceFromScan(notice, complete), true
}

// invoiceFromScan decides what a scan's answer is worth once it is known
// whether the scan reached the end of the message.
//
// This is where the invoice pass parts company with the parcel one, and the
// difference is the whole point of the feature. A parcel is an answer that can
// only be missing, never wrong, so a truncated scan's findings are kept. A bill
// can be wrong in a way that costs the reader something: the part the scan did
// not reach may be the very PDF saying the money was already taken by direct
// debit, and recording an open invoice on that evidence puts a reminder in the
// header for money nobody owes.
//
// So a truncated scan may report a settled bill -- which raises nothing and can
// only close a row that is already there -- and never an open one. What that
// drops is an invoice in a message too large to read to the end, which the
// fetch path had the whole of in hand anyway; what it avoids is the one failure
// this feature must not have.
func invoiceFromScan(notice *mailparse.InvoiceNotice, complete bool) *mailparse.InvoiceNotice {
	if notice == nil || complete || notice.Status == mailparse.InvoicePaid {
		return notice
	}
	return nil
}
