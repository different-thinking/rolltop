// File overview: Parcels the mail announced -- one row per shipment, the
// messages that mentioned it, and the backfill's work queue.
//
// The shape follows from what a reader asks. "What is coming today" is a
// question about parcels, and the four messages that talk about one parcel are
// evidence for it rather than four answers. So a shipment is the row, keyed by
// the number the carrier issued, and every message that named that number is
// linked to it. A shop's dispatch note and the carrier's own mail land on the
// same row without either knowing about the other.

package store

import (
	"context"
	"fmt"
	"strings"
	"time"

	"rolltop/backend/mailparse"
)

// DeliveryVersion is the extraction generation a message was read for parcels
// by. The backfill re-reads messages an older generation read, which is what
// lets an improved rule reach mail that is already stored.
//
// Bump it whenever a change to mailparse's delivery rules should reach stored
// mail. It costs one blob read per message, spread over the background worker's
// turns, and it only reaches what blob retention still holds.
const DeliveryVersion = 1

// DeliveryBackfillLimit bounds one backfill pass and is the batch size the
// worker uses. It matches the category backfill's, because the two do the same
// work per row: open one stored message and read it.
const DeliveryBackfillLimit = 200

// shipmentListLimit bounds what one read of the parcel list returns. A reader
// with more open parcels than this has a different problem than a list.
const shipmentListLimit = 200

// Shipment is one parcel, as everything the mail has said about it so far.
type Shipment struct {
	ID             int64
	Carrier        string
	TrackingNumber string
	// ExpectedDate is "YYYY-MM-DD", or empty for a parcel nobody has dated yet.
	ExpectedDate string
	WindowStart  string
	WindowEnd    string
	Status       string
	// ReportedAt is the date of the message the current answer came from. It is
	// what decides whether a message may overwrite it.
	ReportedAt int64
	UpdatedAt  int64
	// Messages are the mails that named this parcel, newest first.
	Messages []ShipmentMessage
}

// ShipmentMessage is one mail that mentioned a parcel, reduced to what a link
// back to it needs.
type ShipmentMessage struct {
	MessageID int64
	MailboxID int64
	Subject   string
	FromAddr  string
	Date      int64
}

// ShipmentCandidate is one message the delivery extractor still has to read.
type ShipmentCandidate struct {
	ID       int64
	BlobPath string
	Date     int64
}

// ReplaceMessageShipments records what one message said about parcels, and
// stamps the message as read by this generation either way -- a message that
// named none must not be selected forever.
//
// The upsert is what makes several messages one parcel. A message may only
// overwrite the day and the status when it is at least as recent as the message
// the stored answer came from, so mail read out of order -- which the backfill
// does by definition -- cannot undo a delivery report with last week's
// announcement.
func (s *Store) ReplaceMessageShipments(ctx context.Context, userID, messageID int64, messageDate int64, notices []mailparse.DeliveryNotice) error {
	if userID <= 0 || messageID <= 0 {
		return fmt.Errorf("shipments: user_id and message_id are required")
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
	// a message under a new generation cannot leave it attached to a parcel its
	// text no longer names.
	if _, err := tx.ExecContext(ctx, `DELETE FROM shipment_messages WHERE user_id = ? AND message_id = ?`, userID, messageID); err != nil {
		return err
	}
	for _, notice := range notices {
		carrier, number, status, ok := validShipment(notice)
		if !ok {
			continue
		}
		var shipmentID int64
		err := tx.QueryRowContext(ctx, `INSERT INTO shipments
				(user_id, carrier, tracking_number, expected_date, window_start, window_end, status, reported_at, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (user_id, carrier, tracking_number) DO UPDATE SET
				expected_date = CASE WHEN EXCLUDED.reported_at >= shipments.reported_at AND EXCLUDED.expected_date <> ''
					THEN EXCLUDED.expected_date ELSE shipments.expected_date END,
				window_start = CASE WHEN EXCLUDED.reported_at >= shipments.reported_at AND EXCLUDED.expected_date <> ''
					THEN EXCLUDED.window_start ELSE shipments.window_start END,
				window_end = CASE WHEN EXCLUDED.reported_at >= shipments.reported_at AND EXCLUDED.expected_date <> ''
					THEN EXCLUDED.window_end ELSE shipments.window_end END,
				status = CASE WHEN EXCLUDED.reported_at >= shipments.reported_at
					THEN EXCLUDED.status ELSE shipments.status END,
				reported_at = GREATEST(EXCLUDED.reported_at, shipments.reported_at),
				updated_at = EXCLUDED.updated_at
			RETURNING id`,
			userID, carrier, number, notice.ExpectedDate, notice.WindowStart, notice.WindowEnd, status, messageDate, now, now).Scan(&shipmentID)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO shipment_messages (shipment_id, message_id, user_id, created_at)
			VALUES (?, ?, ?, ?)
			ON CONFLICT (shipment_id, message_id) DO NOTHING`, shipmentID, messageID, userID, now); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE messages SET delivery_version = ? WHERE user_id = ? AND id = ?`,
		DeliveryVersion, userID, messageID); err != nil {
		return err
	}
	return tx.Commit()
}

// validShipment rejects what must never reach a row: an unknown carrier key, an
// empty number, and a status this build does not know. The extractor produces
// none of these; the check is here because the column has a constraint and a
// failed insert would fail the sync that carried it.
func validShipment(notice mailparse.DeliveryNotice) (string, string, string, bool) {
	number := strings.TrimSpace(notice.TrackingNumber)
	if number == "" || !mailparse.ValidDeliveryCarrier(notice.Carrier) {
		return "", "", "", false
	}
	switch notice.Status {
	case mailparse.DeliveryAnnounced, mailparse.DeliveryOutForDelivery, mailparse.DeliveryDelivered:
		return notice.Carrier, number, notice.Status, true
	default:
		return "", "", "", false
	}
}

// ListShipments returns the parcels worth showing on the given day: everything
// not yet delivered, and what was delivered recently enough to still be the
// answer to "did it come?".
//
// today is the reader's own day, "YYYY-MM-DD". The caller supplies it because
// the server has no timezone for a reader, and a day is exactly the thing that
// differs between them.
//
// A parcel nobody dated is kept while it is recent: a shop's "your order has
// shipped" is a real parcel with no day attached, and dropping it would lose
// the only notice of it. One that has gone quiet for two months is not news.
func (s *Store) ListShipments(ctx context.Context, userID int64, today string) ([]Shipment, error) {
	day, err := parsePlainDate(today)
	if err != nil {
		return nil, err
	}
	db, err := s.dataDB(ctx, userID)
	if err != nil {
		return nil, err
	}
	recent := day.AddDate(0, 0, -shipmentHistoryDays).Format(plainDateLayout)
	quiet := day.AddDate(0, 0, -shipmentQuietDays).Unix()
	// An undated parcel is aged by reported_at -- the date of the mail that
	// announced it -- and not by updated_at, which is when the row was written.
	// The two part company exactly where it matters: an initial sync parses
	// years of old dispatch mail in one afternoon, and by the row's age every
	// parcel a reader ever received would be "recently shipped, no date yet"
	// for two months, renewed every time the backfill re-read the message.
	rows, err := db.QueryContext(ctx, `SELECT id, carrier, tracking_number, expected_date, window_start, window_end, status, reported_at, updated_at
		FROM shipments
		WHERE user_id = ?
			AND (expected_date >= ? OR (expected_date = '' AND status <> 'delivered' AND reported_at >= ?))
		ORDER BY (expected_date = '') ASC, expected_date ASC, id ASC
		LIMIT ?`, userID, recent, quiet, shipmentListLimit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Shipment, 0, 16)
	index := make(map[int64]int, 16)
	for rows.Next() {
		var item Shipment
		if err := rows.Scan(&item.ID, &item.Carrier, &item.TrackingNumber, &item.ExpectedDate,
			&item.WindowStart, &item.WindowEnd, &item.Status, &item.ReportedAt, &item.UpdatedAt); err != nil {
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
	if err := s.attachShipmentMessages(ctx, userID, out, index); err != nil {
		return nil, err
	}
	// A shipment whose every message has been deleted is a parcel with no
	// evidence left. It is not shown, because there is nothing to open from it.
	kept := out[:0]
	for _, item := range out {
		if len(item.Messages) > 0 {
			kept = append(kept, item)
		}
	}
	return kept, nil
}

const (
	// shipmentHistoryDays is how long a delivered parcel stays on the list. Long
	// enough to answer "when did that arrive?", short enough that the list is
	// about what is coming.
	shipmentHistoryDays = 14
	// shipmentQuietDays is how long an undated parcel is kept waiting, counted
	// from the mail that announced it. A shop that said "shipped" and never
	// followed up is not news after two months.
	shipmentQuietDays = 60
	plainDateLayout   = "2006-01-02"
)

// attachShipmentMessages fills in the mail behind each parcel in one query
// rather than one per row.
func (s *Store) attachShipmentMessages(ctx context.Context, userID int64, shipments []Shipment, index map[int64]int) error {
	db, err := s.dataDB(ctx, userID)
	if err != nil {
		return err
	}
	ids := make([]any, 0, len(shipments)+1)
	ids = append(ids, userID)
	placeholders := make([]string, 0, len(shipments))
	for _, item := range shipments {
		ids = append(ids, item.ID)
		placeholders = append(placeholders, "?")
	}
	rows, err := db.QueryContext(ctx, `SELECT sm.shipment_id, m.id, m.mailbox_id, m.subject, m.from_addr, m.date_unix
		FROM shipment_messages sm
		JOIN messages m ON m.id = sm.message_id AND m.user_id = sm.user_id
		WHERE sm.user_id = ? AND sm.shipment_id IN (`+strings.Join(placeholders, ", ")+`)
		ORDER BY m.date_unix DESC, m.id DESC`, ids...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var shipmentID int64
		var item ShipmentMessage
		if err := rows.Scan(&shipmentID, &item.MessageID, &item.MailboxID, &item.Subject, &item.FromAddr, &item.Date); err != nil {
			return err
		}
		if at, ok := index[shipmentID]; ok {
			shipments[at].Messages = append(shipments[at].Messages, item)
		}
	}
	return rows.Err()
}

// ExpectedShipments is the header's whole answer: how many parcels are due
// today, and which carrier when there is exactly one.
type ExpectedShipments struct {
	Count int
	// Carrier is the single due parcel's carrier key, empty when there is not
	// exactly one. A reader waiting on one parcel is told which; a reader
	// waiting on three is told there are three.
	Carrier string
}

// maxCountedExpectedShipments bounds the header's query. It is a chip, not a
// list: past this the answer is "several" however many there are, and the bound
// is what keeps a query the whole app carries on every page from ever growing
// with the mailbox.
const maxCountedExpectedShipments = 50

// ShipmentsExpectedOn answers "is a parcel coming today". It is deliberately
// the narrowest question -- not "open parcels", which is a list -- because it is
// read on every page load and again whenever a sync stores mail, and the parcel
// list itself is the expensive read that only the parcel page should pay for.
func (s *Store) ShipmentsExpectedOn(ctx context.Context, userID int64, today string) (ExpectedShipments, error) {
	if _, err := parsePlainDate(today); err != nil {
		return ExpectedShipments{}, err
	}
	db, err := s.dataDB(ctx, userID)
	if err != nil {
		return ExpectedShipments{}, err
	}
	rows, err := db.QueryContext(ctx, `SELECT s.carrier
		FROM shipments s
		WHERE s.user_id = ? AND s.expected_date = ? AND s.status <> 'delivered'
			AND EXISTS (SELECT 1 FROM shipment_messages sm WHERE sm.user_id = s.user_id AND sm.shipment_id = s.id)
		ORDER BY s.id
		LIMIT ?`, userID, today, maxCountedExpectedShipments)
	if err != nil {
		return ExpectedShipments{}, err
	}
	defer rows.Close()
	out := ExpectedShipments{}
	first := ""
	for rows.Next() {
		var carrier string
		if err := rows.Scan(&carrier); err != nil {
			return ExpectedShipments{}, err
		}
		if out.Count == 0 {
			first = carrier
		}
		out.Count++
	}
	if err := rows.Err(); err != nil {
		return ExpectedShipments{}, err
	}
	if out.Count == 1 {
		out.Carrier = first
	}
	return out, nil
}

// parsePlainDate is the one place a day arrives from outside. It is a request
// parameter, so it is validated rather than interpolated.
func parsePlainDate(value string) (time.Time, error) {
	day, err := time.Parse(plainDateLayout, strings.TrimSpace(value))
	if err != nil {
		return time.Time{}, fmt.Errorf("shipments: %q is not a YYYY-MM-DD day", value)
	}
	return day, nil
}

// ListMessagesNeedingDeliveryScan returns the next batch of messages to read for
// parcels: the ones no generation has read, then the ones an older generation
// read. Only messages whose raw copy is still on disk are selected -- blob
// retention has thrown the rest away, and there is nothing left to read them
// from.
//
// Newest first, which is the opposite of the category backfill's order and for
// the opposite reason. A category is wanted for every message ever received; a
// parcel that was delivered a year ago is not news, and the newest mail is
// where the parcels that have not arrived yet are.
func (s *Store) ListMessagesNeedingDeliveryScan(ctx context.Context, userID int64, limit int) ([]ShipmentCandidate, error) {
	if limit <= 0 || limit > DeliveryBackfillLimit {
		limit = DeliveryBackfillLimit
	}
	db, err := s.dataDB(ctx, userID)
	if err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx, `SELECT id, blob_path, date_unix
		FROM messages
		WHERE user_id = ? AND delivery_version < ? AND blob_path <> ''
		ORDER BY date_unix DESC, id DESC
		LIMIT ?`, userID, DeliveryVersion, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]ShipmentCandidate, 0, limit)
	for rows.Next() {
		var item ShipmentCandidate
		if err := rows.Scan(&item.ID, &item.BlobPath, &item.Date); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

// MarkMessagesDeliveryScanned stamps messages the pass could not read, so an
// unreadable one is not selected forever. Their shipment links are deliberately
// left alone: a message whose stored copy has gone is not a message that has
// stopped naming a parcel, and clearing what an earlier reading found would
// throw the parcel away on the first pass after retention pruned the mail.
func (s *Store) MarkMessagesDeliveryScanned(ctx context.Context, userID int64, ids []int64) error {
	return s.stampMessagesDeliveryScanned(ctx, userID, ids, false)
}

// ClearMessageShipments stamps messages that were read and named no parcel, and
// detaches whatever an older generation attached to them.
//
// It takes the whole batch because that is nearly the whole batch: almost no
// mail names a parcel, and running the per-message upsert for each of them --
// one transaction, a DELETE and an UPDATE, to record that there was nothing to
// record -- is the same cost the fetch path had removed from it.
func (s *Store) ClearMessageShipments(ctx context.Context, userID int64, ids []int64) error {
	return s.stampMessagesDeliveryScanned(ctx, userID, ids, true)
}

func (s *Store) stampMessagesDeliveryScanned(ctx context.Context, userID int64, ids []int64, detach bool) error {
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
		args := append([]any{DeliveryVersion, userID}, idArgs...)
		_, err = db.ExecContext(ctx, `UPDATE messages SET delivery_version = ?
			WHERE user_id = ? AND id IN (`+list+`)`, args...)
		return err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	deleteArgs := append([]any{userID}, idArgs...)
	if _, err := tx.ExecContext(ctx, `DELETE FROM shipment_messages
		WHERE user_id = ? AND message_id IN (`+list+`)`, deleteArgs...); err != nil {
		return err
	}
	updateArgs := append([]any{DeliveryVersion, userID}, idArgs...)
	if _, err := tx.ExecContext(ctx, `UPDATE messages SET delivery_version = ?
		WHERE user_id = ? AND id IN (`+list+`)`, updateArgs...); err != nil {
		return err
	}
	return tx.Commit()
}

// MessageShipment is the parcel one message is about, as a message list shows
// it. It is a summary rather than the shipment itself: a row has space for one
// day and one carrier, and Count is what says the message named more.
type MessageShipment struct {
	ShipmentID     int64
	Carrier        string
	TrackingNumber string
	ExpectedDate   string
	Status         string
	// Count is how many parcels this message named. A dispatch mail for a large
	// order names one per parcel, and a row that showed only the first would be
	// quietly wrong about what the message said.
	Count int
}

// ShipmentsForMessages answers, for a batch of messages, which parcel each one
// is about. It is one indexed query for a whole list rather than one per row.
//
// Where a message names several, the one shown is the one that matters first:
// still coming before already delivered, and the nearest day before a later
// one. That is the same order the list itself is read in.
func (s *Store) ShipmentsForMessages(ctx context.Context, userID int64, messageIDs []int64) (map[int64]MessageShipment, error) {
	out := map[int64]MessageShipment{}
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
	// Undated parcels sort last within their status: a day nobody stated cannot
	// be nearer than one that was.
	rows, err := db.QueryContext(ctx, `SELECT sm.message_id, s.id, s.carrier, s.tracking_number, s.expected_date, s.status
		FROM shipment_messages sm
		JOIN shipments s ON s.id = sm.shipment_id AND s.user_id = sm.user_id
		WHERE sm.user_id = ? AND sm.message_id IN (`+strings.Join(placeholders, ", ")+`)
		ORDER BY sm.message_id,
			CASE WHEN s.status = 'delivered' THEN 1 ELSE 0 END,
			(s.expected_date = '') ASC, s.expected_date ASC, s.id ASC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var messageID int64
		var item MessageShipment
		if err := rows.Scan(&messageID, &item.ShipmentID, &item.Carrier, &item.TrackingNumber,
			&item.ExpectedDate, &item.Status); err != nil {
			return nil, err
		}
		if existing, ok := out[messageID]; ok {
			// The first row for a message already won the ordering above; the
			// rest only raise the count.
			existing.Count++
			out[messageID] = existing
			continue
		}
		item.Count = 1
		out[messageID] = item
	}
	return out, rows.Err()
}
