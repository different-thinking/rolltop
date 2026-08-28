// File overview: Bills the mail says are owed -- one row per invoice, the
// messages that talked about it, and the backfill's work queue.
//
// The shape is shipments.go's, because the question has the same shape. "What
// do I still have to pay" is a question about invoices, and the invoice, the
// reminder, the dunning letter and the payment confirmation are four pieces of
// evidence about one of them rather than four answers. So an invoice is the
// row, keyed by the sender's domain and the reference the document carried, and
// every message that named it is linked to it.
//
// Two rules differ from a parcel's, and both come from what an invoice is:
//
//   - The status is set by the newest message, the way a parcel's is, because
//     the newest message is the one that knows whether the money arrived. A
//     payment confirmation closes the row; a dunning letter re-opens it.
//   - The dunning level only rises. Being chased is not a state that can be
//     taken back by an older message turning up later, which is exactly what
//     the backfill does: it reads newest first, so the invoice is routinely
//     read after the dunning letter that chases it.

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"rolltop/backend/mailparse"
)

// InvoiceVersion is the extraction generation a message was read for bills by.
// The backfill re-reads messages an older generation read, which is what lets
// an improved rule reach mail that is already stored.
//
// Bump it whenever a change to mailparse's invoice rules should reach stored
// mail. It is more expensive than the delivery equivalent -- this pass decodes
// attachment bodies and runs pdftotext on the PDFs -- but it is also spread
// over far fewer messages, because only mail already filed as paperwork is
// selected at all.
const InvoiceVersion = 1

// InvoiceBackfillLimit bounds one backfill pass and is the batch size the
// worker uses. It is a quarter of the delivery one: each row here can mean
// reading a megabyte off disk and shelling out to a text extractor, where a
// delivery scan reads text and stops.
const InvoiceBackfillLimit = 50

// invoiceListLimit bounds what one read of the invoice list returns. A reader
// with more open bills than this has a different problem than a list.
const invoiceListLimit = 200

const (
	// invoicePaidHistoryDays is how long a settled invoice stays on the list.
	// Long enough to answer "did that go through?", short enough that the list
	// is about what is still owed.
	invoicePaidHistoryDays = 30
	// invoiceOpenHorizonDays is how far back an open invoice is still worth
	// showing, counted from its due date or, for one nobody dated, from the
	// message that announced it.
	//
	// It exists because of what an initial sync does: it parses whatever the
	// mailbox holds in one afternoon, and without a horizon every invoice the
	// reader ever received would arrive on the list as an open item. Half a
	// year is past any real payment term and past any plausible "did I pay
	// this?", and what falls off the end is still in Invoices & Contracts.
	invoiceOpenHorizonDays = 180
)

// Invoice is one bill, as everything the mail has said about it so far.
type Invoice struct {
	ID     int64
	Issuer string
	// Reference is what rows are merged on inside an issuer; Number is what the
	// document printed, and is empty where it printed nothing.
	Reference string
	Number    string
	// DueDate is "YYYY-MM-DD", or empty for a bill nobody dated. ManualDueDate
	// is the day the reader entered themselves, which outranks it.
	DueDate       string
	ManualDueDate string
	Amount        string
	Currency      string
	// Status is what the mail said. ManualStatus is what the reader said, empty
	// when they have said nothing; it outranks Status wherever the two
	// disagree, and Status keeps being updated underneath it so taking the
	// correction back returns the mail's own answer rather than a stale one.
	Status       string
	ManualStatus string
	Settlement   string
	DunningLevel int
	// ReportedAt is the date of the message the current answer came from. It is
	// what decides whether a message may overwrite it.
	ReportedAt int64
	UpdatedAt  int64
	// Messages are the mails that named this invoice, newest first.
	Messages []InvoiceMessage
}

// Manual statuses a reader can put on an invoice. Dismissed is for a row that
// was never a bill; paid is for one they settled without the sender ever
// writing to confirm it, which is most of what an old row on this list is.
const (
	InvoiceManualNone      = ""
	InvoiceManualPaid      = "paid"
	InvoiceManualDismissed = "dismissed"
)

// ValidInvoiceManualStatus keeps anything the column would refuse out of the
// query. It arrives from a request, so it is checked rather than trusted.
func ValidInvoiceManualStatus(value string) bool {
	switch value {
	case InvoiceManualNone, InvoiceManualPaid, InvoiceManualDismissed:
		return true
	default:
		return false
	}
}

// EffectiveStatus is what the invoice counts as: the reader's answer when they
// gave one, the mail's otherwise. Everything that groups, counts or hides an
// invoice reads this rather than either column.
func (i Invoice) EffectiveStatus() string {
	if i.ManualStatus == InvoiceManualPaid {
		return mailparse.InvoicePaid
	}
	return i.Status
}

// EffectiveDueDate is the day the invoice is due: the one the reader entered
// when they entered one, the extracted one otherwise. An invoice that stated no
// deadline is the commonest reason to enter one by hand.
func (i Invoice) EffectiveDueDate() string {
	if i.ManualDueDate != "" {
		return i.ManualDueDate
	}
	return i.DueDate
}

// Dismissed reports an invoice the reader said was never one.
func (i Invoice) Dismissed() bool {
	return i.ManualStatus == InvoiceManualDismissed
}

// Chased reports an invoice somebody has written about more than once.
func (i Invoice) Chased() bool {
	return i.DunningLevel > mailparse.InvoiceDunningNone
}

// InvoiceMessage is one mail that mentioned an invoice, reduced to what a link
// back to it needs.
type InvoiceMessage struct {
	MessageID int64
	MailboxID int64
	Subject   string
	FromAddr  string
	Date      int64
}

// InvoiceCandidate is one message the invoice extractor still has to read.
type InvoiceCandidate struct {
	ID       int64
	BlobPath string
	Date     int64
}

// ReplaceMessageInvoice records what one message said about a bill, and stamps
// the message as read by this generation either way -- a message that named
// none must not be selected forever.
//
// The upsert is what makes several messages one invoice. A message may only
// overwrite the day, the amount and the status when it is at least as recent as
// the message the stored answer came from, so mail read out of order -- which
// the backfill does by definition -- cannot undo a payment confirmation with
// last month's invoice. The dunning level is the exception and takes the
// highest either side has seen, for the reason in the file header.
func (s *Store) ReplaceMessageInvoice(ctx context.Context, userID, messageID int64, messageDate int64, notice *mailparse.InvoiceNotice) error {
	if userID <= 0 || messageID <= 0 {
		return fmt.Errorf("invoices: user_id and message_id are required")
	}
	issuer, reference, status, settlement, ok := validInvoice(notice)
	if !ok {
		return s.ClearMessageInvoices(ctx, userID, []int64{messageID})
	}
	db, err := s.dataDB(ctx, userID)
	if err != nil {
		return err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	now := nowUnix()
	// The message's own links are replaced rather than added to, so re-reading
	// a message under a new generation cannot leave it attached to an invoice
	// its text no longer names.
	if _, err := tx.ExecContext(ctx, `DELETE FROM invoice_messages WHERE user_id = ? AND message_id = ?`, userID, messageID); err != nil {
		return err
	}
	var invoiceID int64
	err = tx.QueryRowContext(ctx, `INSERT INTO invoices
			(user_id, issuer, reference, number, due_date, amount, currency, status, settlement,
				dunning_level, reported_at, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (user_id, issuer, reference) DO UPDATE SET
			number = CASE WHEN EXCLUDED.number <> '' THEN EXCLUDED.number ELSE invoices.number END,
			due_date = CASE WHEN EXCLUDED.reported_at >= invoices.reported_at AND EXCLUDED.due_date <> ''
				THEN EXCLUDED.due_date ELSE invoices.due_date END,
			amount = CASE WHEN EXCLUDED.reported_at >= invoices.reported_at AND EXCLUDED.amount <> ''
				THEN EXCLUDED.amount ELSE invoices.amount END,
			currency = CASE WHEN EXCLUDED.reported_at >= invoices.reported_at AND EXCLUDED.amount <> ''
				THEN EXCLUDED.currency ELSE invoices.currency END,
			status = CASE WHEN EXCLUDED.reported_at >= invoices.reported_at
				THEN EXCLUDED.status ELSE invoices.status END,
			settlement = CASE WHEN EXCLUDED.reported_at >= invoices.reported_at AND EXCLUDED.settlement <> ''
				THEN EXCLUDED.settlement ELSE invoices.settlement END,
			dunning_level = GREATEST(EXCLUDED.dunning_level, invoices.dunning_level),
			reported_at = GREATEST(EXCLUDED.reported_at, invoices.reported_at),
			updated_at = EXCLUDED.updated_at
		RETURNING id`,
		userID, issuer, reference, notice.Number, notice.DueDate, notice.Amount, notice.Currency,
		status, settlement, notice.DunningLevel, messageDate, now, now).Scan(&invoiceID)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO invoice_messages (invoice_id, message_id, user_id, created_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT (invoice_id, message_id) DO NOTHING`, invoiceID, messageID, userID, now); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE messages SET invoice_version = ? WHERE user_id = ? AND id = ?`,
		InvoiceVersion, userID, messageID); err != nil {
		return err
	}
	return tx.Commit()
}

// validInvoice rejects what must never reach a row: a missing issuer or
// reference, and a status, settlement or dunning level this build does not
// know. The extractor produces none of these; the check is here because the
// columns have constraints and a failed insert would fail the sync that
// carried it.
func validInvoice(notice *mailparse.InvoiceNotice) (string, string, string, string, bool) {
	if notice == nil {
		return "", "", "", "", false
	}
	issuer := strings.TrimSpace(notice.Issuer)
	reference := strings.TrimSpace(notice.Reference)
	if issuer == "" || reference == "" {
		return "", "", "", "", false
	}
	if !mailparse.ValidInvoiceSettlement(notice.Settlement) {
		return "", "", "", "", false
	}
	if notice.DunningLevel < mailparse.InvoiceDunningNone || notice.DunningLevel > mailparse.InvoiceDunningFinal {
		return "", "", "", "", false
	}
	switch notice.Status {
	case mailparse.InvoiceOpen, mailparse.InvoicePaid:
		return issuer, reference, notice.Status, notice.Settlement, true
	default:
		return "", "", "", "", false
	}
}

// ListInvoices returns the bills worth showing on the given day: everything not
// settled that is still inside the horizon, and what was settled recently
// enough to still answer "did that go through?".
//
// today is the reader's own day, "YYYY-MM-DD". The caller supplies it because
// the server has no timezone for a reader, and a day is exactly the thing that
// differs between them.
func (s *Store) ListInvoices(ctx context.Context, userID int64, today string) ([]Invoice, error) {
	day, err := parsePlainDate(today)
	if err != nil {
		return nil, err
	}
	db, err := s.dataDB(ctx, userID)
	if err != nil {
		return nil, err
	}
	horizon := day.AddDate(0, 0, -invoiceOpenHorizonDays).Format(plainDateLayout)
	paidSince := day.AddDate(0, 0, -invoicePaidHistoryDays).Unix()
	// A dismissed invoice is gone from every read: the reader said it was never
	// one, and a list that kept showing it would be asking them again.
	//
	// An invoice nobody dated is aged by reported_at -- the date of the mail
	// that announced it -- and not by updated_at, which is when the row was
	// written. The two part company exactly where it matters: an initial sync
	// reads years of billing mail in one afternoon, and by the row's age every
	// invoice the reader ever received would be "recently announced, no date
	// yet" for half a year, renewed every time the backfill re-read it.
	//
	// A chased invoice ignores the horizon entirely. Somebody is still writing
	// about it, so it is still owed however old the deadline is.
	rows, err := db.QueryContext(ctx, `SELECT id, issuer, reference, number, due_date, manual_due_date,
			amount, currency, status, manual_status, settlement, dunning_level, reported_at, updated_at
		FROM invoices
		WHERE user_id = ? AND manual_status <> 'dismissed'
			AND (
				(status = 'open' AND manual_status = '' AND (
					dunning_level > 0
					OR COALESCE(NULLIF(manual_due_date, ''), NULLIF(due_date, '')) >= ?
					OR (manual_due_date = '' AND due_date = '' AND reported_at >= ?)
				))
				OR ((status = 'paid' OR manual_status = 'paid') AND updated_at >= ?)
			)
		ORDER BY dunning_level DESC,
			(COALESCE(NULLIF(manual_due_date, ''), NULLIF(due_date, '')) IS NULL) ASC,
			COALESCE(NULLIF(manual_due_date, ''), NULLIF(due_date, '')) ASC,
			id ASC
		LIMIT ?`, userID, horizon, paidSince, paidSince, invoiceListLimit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Invoice, 0, 16)
	index := make(map[int64]int, 16)
	for rows.Next() {
		var item Invoice
		if err := rows.Scan(&item.ID, &item.Issuer, &item.Reference, &item.Number, &item.DueDate,
			&item.ManualDueDate, &item.Amount, &item.Currency, &item.Status, &item.ManualStatus,
			&item.Settlement, &item.DunningLevel, &item.ReportedAt, &item.UpdatedAt); err != nil {
			return nil, err
		}
		index[item.ID] = len(out)
		out = append(out, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, nil
	}
	if err := s.attachInvoiceMessages(ctx, userID, out, index); err != nil {
		return nil, err
	}
	// An invoice whose every message has been deleted is a bill with no
	// evidence left. It is not shown, because there is nothing to open from it.
	kept := out[:0]
	for _, item := range out {
		if len(item.Messages) > 0 {
			kept = append(kept, item)
		}
	}
	return kept, nil
}

// attachInvoiceMessages fills in the mail behind each invoice in one query
// rather than one per row.
func (s *Store) attachInvoiceMessages(ctx context.Context, userID int64, invoices []Invoice, index map[int64]int) error {
	db, err := s.dataDB(ctx, userID)
	if err != nil {
		return err
	}
	ids := make([]any, 0, len(invoices)+1)
	ids = append(ids, userID)
	placeholders := make([]string, 0, len(invoices))
	for _, item := range invoices {
		ids = append(ids, item.ID)
		placeholders = append(placeholders, "?")
	}
	rows, err := db.QueryContext(ctx, `SELECT im.invoice_id, m.id, m.mailbox_id, m.subject, m.from_addr, m.date_unix
		FROM invoice_messages im
		JOIN messages m ON m.id = im.message_id AND m.user_id = im.user_id
		WHERE im.user_id = ? AND im.invoice_id IN (`+strings.Join(placeholders, ", ")+`)
		ORDER BY m.date_unix DESC, m.id DESC`, ids...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var invoiceID int64
		var item InvoiceMessage
		if err := rows.Scan(&invoiceID, &item.MessageID, &item.MailboxID, &item.Subject, &item.FromAddr, &item.Date); err != nil {
			return err
		}
		if at, ok := index[invoiceID]; ok {
			invoices[at].Messages = append(invoices[at].Messages, item)
		}
	}
	return rows.Err()
}

// DueInvoices is the header chip's whole answer: how many bills want paying,
// how many of those are being chased, and which sender when there is one.
type DueInvoices struct {
	Count int
	// Chased is how many of them somebody has written about again. It is what
	// lets the chip say the one thing that changes a reader's afternoon.
	Chased int
	// Issuer is the single due invoice's sender, empty when there is not
	// exactly one. A reader owing one bill is told who to; a reader owing three
	// is told there are three.
	Issuer string
}

// maxCountedDueInvoices bounds the header's query. It is a chip, not a list:
// past this the answer is "several" however many there are, and the bound is
// what keeps a query the whole app carries on every page from ever growing with
// the mailbox.
const maxCountedDueInvoices = 50

// InvoicesDueOn answers "do I owe money today". It is deliberately the
// narrowest question -- not "open invoices", which is a list -- because it is
// read on every page load and again whenever a sync stores mail.
//
// It differs from the parcel chip's question in the one way that matters: a
// parcel stops being today's news the day after, while an unpaid invoice
// becomes *more* pressing. So the test is "due today or earlier", not "due
// today", and a chased invoice counts whatever its date says -- including none
// at all, because a dunning letter with no readable deadline is still a dunning
// letter.
func (s *Store) InvoicesDueOn(ctx context.Context, userID int64, today string) (DueInvoices, error) {
	if _, err := parsePlainDate(today); err != nil {
		return DueInvoices{}, err
	}
	db, err := s.dataDB(ctx, userID)
	if err != nil {
		return DueInvoices{}, err
	}
	rows, err := db.QueryContext(ctx, `SELECT i.issuer, i.dunning_level
		FROM invoices i
		WHERE i.user_id = ? AND i.status = 'open' AND i.manual_status = ''
			AND (
				i.dunning_level > 0
				OR (COALESCE(NULLIF(i.manual_due_date, ''), NULLIF(i.due_date, '')) <= ?
					AND COALESCE(NULLIF(i.manual_due_date, ''), NULLIF(i.due_date, '')) IS NOT NULL)
			)
			AND EXISTS (SELECT 1 FROM invoice_messages im WHERE im.user_id = i.user_id AND im.invoice_id = i.id)
		ORDER BY i.id
		LIMIT ?`, userID, today, maxCountedDueInvoices)
	if err != nil {
		return DueInvoices{}, err
	}
	defer rows.Close()
	out := DueInvoices{}
	first := ""
	for rows.Next() {
		var issuer string
		var dunning int
		if err := rows.Scan(&issuer, &dunning); err != nil {
			return DueInvoices{}, err
		}
		if out.Count == 0 {
			first = issuer
		}
		out.Count++
		if dunning > 0 {
			out.Chased++
		}
	}
	if err := rows.Err(); err != nil {
		return DueInvoices{}, err
	}
	if out.Count == 1 {
		out.Issuer = first
	}
	return out, nil
}

// ListMessagesNeedingInvoiceScan returns the next batch of messages to read for
// bills: the ones no generation has read, then the ones an older generation
// read. Only messages whose raw copy is still on disk are selected -- blob
// retention has thrown the rest away, and there is nothing left to read them
// from.
//
// The category filter is what makes this pass affordable. Reading a message for
// a bill means decoding its attachments and running a text extractor over the
// PDFs, which is far too much to do to a whole mailbox; the category has
// already decided which mail is paperwork, and that is a small fraction of it.
//
// Newest first, the same order and for the same reason as the delivery
// backfill: a bill settled a year ago is not news, and the newest mail is where
// the ones still owed are.
func (s *Store) ListMessagesNeedingInvoiceScan(ctx context.Context, userID int64, limit int) ([]InvoiceCandidate, error) {
	if limit <= 0 || limit > InvoiceBackfillLimit {
		limit = InvoiceBackfillLimit
	}
	db, err := s.dataDB(ctx, userID)
	if err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx, `SELECT id, blob_path, date_unix
		FROM messages
		WHERE user_id = ? AND category = ? AND invoice_version < ? AND blob_path <> ''
		ORDER BY date_unix DESC, id DESC
		LIMIT ?`, userID, mailparse.CategoryInvoices, InvoiceVersion, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]InvoiceCandidate, 0, limit)
	for rows.Next() {
		var item InvoiceCandidate
		if err := rows.Scan(&item.ID, &item.BlobPath, &item.Date); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

// MarkMessagesInvoiceScanned stamps messages the pass could not read, so an
// unreadable one is not selected forever. Their invoice links are deliberately
// left alone: a message whose stored copy has gone is not a message that has
// stopped naming a bill, and clearing what an earlier reading found would throw
// the invoice away on the first pass after retention pruned the mail.
func (s *Store) MarkMessagesInvoiceScanned(ctx context.Context, userID int64, ids []int64) error {
	return s.stampMessagesInvoiceScanned(ctx, userID, ids, false)
}

// ClearMessageInvoices stamps messages that were read and named no bill, and
// detaches whatever an older generation attached to them.
//
// It takes the whole batch because that is most of the batch: even inside
// Invoices & Contracts a good half of the mail is a contract, a receipt or a
// notice rather than money owed.
func (s *Store) ClearMessageInvoices(ctx context.Context, userID int64, ids []int64) error {
	return s.stampMessagesInvoiceScanned(ctx, userID, ids, true)
}

func (s *Store) stampMessagesInvoiceScanned(ctx context.Context, userID int64, ids []int64, detach bool) error {
	if len(ids) == 0 {
		return nil
	}
	db, err := s.dataDB(ctx, userID)
	if err != nil {
		return err
	}
	placeholders := make([]string, 0, len(ids))
	idArgs := make([]any, 0, len(ids))
	for _, id := range ids {
		idArgs = append(idArgs, id)
		placeholders = append(placeholders, "?")
	}
	list := strings.Join(placeholders, ", ")
	if !detach {
		args := append([]any{InvoiceVersion, userID}, idArgs...)
		_, err = db.ExecContext(ctx, `UPDATE messages SET invoice_version = ?
			WHERE user_id = ? AND id IN (`+list+`)`, args...)
		return err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	deleteArgs := append([]any{userID}, idArgs...)
	if _, err := tx.ExecContext(ctx, `DELETE FROM invoice_messages
		WHERE user_id = ? AND message_id IN (`+list+`)`, deleteArgs...); err != nil {
		return err
	}
	updateArgs := append([]any{InvoiceVersion, userID}, idArgs...)
	if _, err := tx.ExecContext(ctx, `UPDATE messages SET invoice_version = ?
		WHERE user_id = ? AND id IN (`+list+`)`, updateArgs...); err != nil {
		return err
	}
	return tx.Commit()
}

// MessageInvoice is the bill one message is about, as a message list shows it.
type MessageInvoice struct {
	InvoiceID    int64
	Issuer       string
	Number       string
	DueDate      string
	Amount       string
	Currency     string
	Status       string
	Settlement   string
	DunningLevel int
}

// InvoicesForMessages answers, for a batch of messages, which bill each one is
// about. It is one indexed query for a whole list rather than one per row.
//
// A message names at most one invoice, which is the difference from parcels: a
// dispatch note lists a number per parcel, while a billing mail is about one
// document. So there is no count to carry and no ordering to pick a winner
// with.
func (s *Store) InvoicesForMessages(ctx context.Context, userID int64, messageIDs []int64) (map[int64]MessageInvoice, error) {
	out := map[int64]MessageInvoice{}
	if userID <= 0 || len(messageIDs) == 0 {
		return out, nil
	}
	db, err := s.dataDB(ctx, userID)
	if err != nil {
		return nil, err
	}
	args := make([]any, 0, len(messageIDs)+1)
	args = append(args, userID)
	placeholders := make([]string, 0, len(messageIDs))
	seen := make(map[int64]bool, len(messageIDs))
	for _, id := range messageIDs {
		if id <= 0 || seen[id] {
			continue
		}
		seen[id] = true
		args = append(args, id)
		placeholders = append(placeholders, "?")
	}
	if len(placeholders) == 0 {
		return out, nil
	}
	rows, err := db.QueryContext(ctx, `SELECT im.message_id, i.id, i.issuer, i.number,
			COALESCE(NULLIF(i.manual_due_date, ''), i.due_date) AS effective_due,
			i.amount, i.currency,
			CASE WHEN i.manual_status = 'paid' THEN 'paid' ELSE i.status END AS effective_status,
			i.settlement, i.dunning_level
		FROM invoice_messages im
		JOIN invoices i ON i.id = im.invoice_id AND i.user_id = im.user_id
		WHERE im.user_id = ? AND i.manual_status <> 'dismissed'
			AND im.message_id IN (`+strings.Join(placeholders, ", ")+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var messageID int64
		var item MessageInvoice
		if err := rows.Scan(&messageID, &item.InvoiceID, &item.Issuer, &item.Number, &item.DueDate,
			&item.Amount, &item.Currency, &item.Status, &item.Settlement, &item.DunningLevel); err != nil {
			return nil, err
		}
		out[messageID] = item
	}
	return out, rows.Err()
}

// SetInvoiceManualStatus records what the reader said about one invoice, or
// clears it when value is empty. It returns the invoice as it now reads.
//
// The extracted status is left alone on purpose. A sender that confirms the
// payment a day later still updates its own column underneath, so taking the
// correction back does not leave the reader with the answer they were
// correcting.
func (s *Store) SetInvoiceManualStatus(ctx context.Context, userID, invoiceID int64, value string) (Invoice, error) {
	if !ValidInvoiceManualStatus(value) {
		return Invoice{}, fmt.Errorf("invoices: unsupported manual status %q", value)
	}
	now := nowUnix()
	at := now
	if value == InvoiceManualNone {
		at = 0
	}
	return s.updateInvoice(ctx, userID, invoiceID, `UPDATE invoices
		SET manual_status = ?, manual_status_at = ?, updated_at = ?
		WHERE user_id = ? AND id = ?`, value, at, now, userID, invoiceID)
}

// SetInvoiceDueDate records a deadline the reader entered, or clears it when
// day is empty. It is the counterpart to marking a parcel arrived: the
// commonest invoice with no date is one whose terms were only in a scan, and
// the reader is the only one who can read those.
func (s *Store) SetInvoiceDueDate(ctx context.Context, userID, invoiceID int64, day string) (Invoice, error) {
	day = strings.TrimSpace(day)
	if day != "" {
		if _, err := parsePlainDate(day); err != nil {
			return Invoice{}, err
		}
	}
	now := nowUnix()
	return s.updateInvoice(ctx, userID, invoiceID, `UPDATE invoices
		SET manual_due_date = ?, updated_at = ?
		WHERE user_id = ? AND id = ?`, day, now, userID, invoiceID)
}

// updateInvoice runs one of the reader's corrections and reads the row back.
// The two of them differ only in which column they set, and the returned
// projection has to stay identical to the list's, so it is written once.
func (s *Store) updateInvoice(ctx context.Context, userID, invoiceID int64, statement string, args ...any) (Invoice, error) {
	db, err := s.dataDB(ctx, userID)
	if err != nil {
		return Invoice{}, err
	}
	var item Invoice
	err = db.QueryRowContext(ctx, statement+` RETURNING id, issuer, reference, number, due_date, manual_due_date,
			amount, currency, status, manual_status, settlement, dunning_level, reported_at, updated_at`, args...).
		Scan(&item.ID, &item.Issuer, &item.Reference, &item.Number, &item.DueDate, &item.ManualDueDate,
			&item.Amount, &item.Currency, &item.Status, &item.ManualStatus, &item.Settlement,
			&item.DunningLevel, &item.ReportedAt, &item.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Invoice{}, ErrNotFound
	}
	return item, err
}
