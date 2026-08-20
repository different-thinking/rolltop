// File overview: Message language and attachment indexing metadata persistence.

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// MarkSearchVisibleMessagesPendingIndex schedules a complete, tenant-scoped
// search rebuild without changing message content, IMAP state, or blob rows.
func (s *Store) MarkSearchVisibleMessagesPendingIndex(ctx context.Context, userID int64) (int64, error) {
	if userID <= 0 {
		return 0, fmt.Errorf("user id must be positive")
	}
	db, err := s.dataDB(ctx, userID)
	if err != nil {
		return 0, err
	}
	return markSearchMessagesPendingIndex(ctx, db, userID, searchPendingScope{})
}

// MarkSearchMessagesPendingIndexRange is the targeted form: it queues only the
// messages an abandoned Bleve batch owned, identified by the inclusive row-ID
// range the recovery marker recorded. Everything else in the index is already
// published and is deliberately left alone.
func (s *Store) MarkSearchMessagesPendingIndexRange(ctx context.Context, userID, firstID, lastID int64) (int64, error) {
	if userID <= 0 {
		return 0, fmt.Errorf("user id must be positive")
	}
	if firstID <= 0 || lastID < firstID {
		return 0, fmt.Errorf("message id range %d-%d is not usable", firstID, lastID)
	}
	db, err := s.dataDB(ctx, userID)
	if err != nil {
		return 0, err
	}
	return markSearchMessagesPendingIndex(ctx, db, userID, searchPendingScope{firstID: firstID, lastID: lastID})
}

// searchPendingScope selects which messages a pending-index write covers. The
// zero value means every search-visible message, which is the full rebuild; a
// row-ID range means the messages one abandoned Bleve batch owned.
//
// The two are separate cases rather than a sentinel the statement builder
// re-derives, because getting that derivation wrong in the narrow direction is
// invisible and in the wide direction rewrites every row in the tenant.
type searchPendingScope struct {
	firstID int64
	lastID  int64
}

func (s searchPendingScope) ranged() bool { return s.firstID != 0 || s.lastID != 0 }

func (s searchPendingScope) valid() bool {
	return !s.ranged() || (s.firstID > 0 && s.lastID >= s.firstID)
}

// filter returns the mailbox restriction and row bound for this scope.
//
// A full rebuild covers mailboxes the user has search enabled for. A targeted
// repair instead matches what the indexing worker will actually pick up, which
// is every mailbox not yet purged from the index: a batch can stall on rows in a
// mailbox whose include_in_search was switched off moments earlier, and marking
// zero rows for it while clearing the marker would drop them silently.
func (s searchPendingScope) filter(userID int64) (string, []any) {
	if !s.ranged() {
		return `AND mailbox_id IN (
					SELECT id FROM mailboxes WHERE user_id = ? AND include_in_search = 1
				)`, []any{userID}
	}
	return `AND mailbox_id NOT IN (
					SELECT id FROM mailboxes WHERE user_id = ? AND search_index_purged = 1
				)
				AND id BETWEEN ? AND ?`, []any{userID, s.firstID, s.lastID}
}

// markSearchMessagesPendingIndex records the recovery prerequisite: the rows
// the caller is about to drop from the search index have to be marked pending
// *before* the index is quarantined, or a crash in between leaves messages that
// are in neither the index nor the rebuild queue.
//
// Under SQLite this needed a dedicated connection, synchronous=FULL, and a full
// WAL checkpoint to guarantee the commit had reached the disk before the
// quarantine rename. None of that survives the move: a committed PostgreSQL
// transaction is durable by definition, which is the whole barrier this
// function used to construct by hand.
func markSearchMessagesPendingIndex(ctx context.Context, db *sql.DB, userID int64, scope searchPendingScope) (int64, error) {
	if !scope.valid() {
		return 0, fmt.Errorf("message id range %d-%d is not usable", scope.firstID, scope.lastID)
	}
	filter, filterArguments := scope.filter(userID)
	result, err := db.ExecContext(ctx, `UPDATE messages
			SET attachment_indexed_at = 0
			WHERE user_id = ?
				`+filter, append([]any{userID}, filterArguments...)...)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

// ListMessagesNeedingAttachmentIndex returns messages whose raw bodies still need attachment text extraction.
func (s *Store) ListMessagesNeedingAttachmentIndex(ctx context.Context, userID int64, limit int) ([]MessageRecord, error) {
	messages, _, err := s.ListMessagesNeedingAttachmentIndexAfter(ctx, userID, 0, limit)
	return messages, err
}

// ListMessagesNeedingAttachmentIndexAfter returns a circular, tenant-scoped
// page after messageID. wrapped reports that the page crossed back to lower IDs.
// The cursor keeps one failed raw message from pinning every later message while
// still bounding each attachment-index turn.
func (s *Store) ListMessagesNeedingAttachmentIndexAfter(ctx context.Context, userID, messageID int64, limit int) ([]MessageRecord, bool, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	if messageID < 0 {
		messageID = 0
	}
	messages, err := s.listMessagesNeedingAttachmentIndexRange(ctx, userID, messageID, limit, false)
	if err != nil {
		return nil, false, err
	}
	if messageID == 0 || len(messages) == limit {
		return messages, false, nil
	}
	wrapped, err := s.listMessagesNeedingAttachmentIndexRange(ctx, userID, messageID, limit-len(messages), true)
	if err != nil {
		return nil, false, err
	}
	return append(messages, wrapped...), len(wrapped) > 0, nil
}

func (s *Store) listMessagesNeedingAttachmentIndexRange(ctx context.Context, userID, messageID int64, limit int, throughCursor bool) ([]MessageRecord, error) {
	query := `SELECT id, user_id, account_id, mailbox_id, blob_id, message_id_header, in_reply_to, references_header, thread_key, subject, language_code, from_addr, to_addr, cc_addr,
			date_unix, internal_date_unix, uid, size, blob_path, body_text, body_html, is_read, read_sync_pending, is_starred, star_sync_pending, has_attachments, is_encrypted, is_signed, attachment_indexed_at, created_at, updated_at, category
		FROM messages WHERE user_id = ? AND attachment_indexed_at = 0
			AND mailbox_id NOT IN (
				SELECT id FROM mailboxes WHERE user_id = ? AND search_index_purged = 1
			)
			AND id > ? ORDER BY id LIMIT ?`
	if throughCursor {
		query = `SELECT id, user_id, account_id, mailbox_id, blob_id, message_id_header, in_reply_to, references_header, thread_key, subject, language_code, from_addr, to_addr, cc_addr,
			date_unix, internal_date_unix, uid, size, blob_path, body_text, body_html, is_read, read_sync_pending, is_starred, star_sync_pending, has_attachments, is_encrypted, is_signed, attachment_indexed_at, created_at, updated_at, category
		FROM messages WHERE user_id = ? AND attachment_indexed_at = 0
			AND mailbox_id NOT IN (
				SELECT id FROM mailboxes WHERE user_id = ? AND search_index_purged = 1
			)
			AND id <= ? ORDER BY id LIMIT ?`
	}
	rows, err := s.mustDataDB(ctx, userID).QueryContext(ctx, query, userID, userID, messageID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanMessages(rows)
}

// ListMessagesWithReadSyncPending returns locally changed read-state rows waiting for IMAP sync.
func (s *Store) ListMessagesWithReadSyncPending(ctx context.Context, userID int64, limit int) ([]MessageRecord, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.mustDataDB(ctx, userID).QueryContext(ctx, `SELECT id, user_id, account_id, mailbox_id, blob_id, message_id_header, in_reply_to, references_header, thread_key, subject, language_code, from_addr, to_addr, cc_addr,
			date_unix, internal_date_unix, uid, size, blob_path, body_text, body_html, is_read, read_sync_pending, is_starred, star_sync_pending, has_attachments, is_encrypted, is_signed, attachment_indexed_at, created_at, updated_at, category
		FROM messages WHERE user_id = ? AND read_sync_pending = 1 ORDER BY updated_at LIMIT ?`, userID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanMessages(rows)
}

// ListMessagesWithStarSyncPending returns locally changed star-state rows waiting for IMAP sync.
func (s *Store) ListMessagesWithStarSyncPending(ctx context.Context, userID int64, limit int) ([]MessageRecord, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.mustDataDB(ctx, userID).QueryContext(ctx, `SELECT id, user_id, account_id, mailbox_id, blob_id, message_id_header, in_reply_to, references_header, thread_key, subject, language_code, from_addr, to_addr, cc_addr,
			date_unix, internal_date_unix, uid, size, blob_path, body_text, body_html, is_read, read_sync_pending, is_starred, star_sync_pending, has_attachments, is_encrypted, is_signed, attachment_indexed_at, created_at, updated_at, category
		FROM messages WHERE user_id = ? AND star_sync_pending = 1 ORDER BY updated_at LIMIT ?`, userID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanMessages(rows)
}

// MarkMessageAttachmentIndexed records that attachment text extraction ran for a message.
func (s *Store) MarkMessageAttachmentIndexed(ctx context.Context, userID, messageID int64, hasAttachments bool) error {
	result, err := s.mustDataDB(ctx, userID).ExecContext(ctx, `UPDATE messages SET has_attachments = ?, attachment_indexed_at = ?, updated_at = ?
		WHERE user_id = ? AND id = ?`, boolInt(hasAttachments), nowUnix(), nowUnix(), userID, messageID)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return ErrNotFound
	}
	return nil
}

// MessageAttachmentIndexUpdate is one durable post-Bleve marker update.
type MessageAttachmentIndexUpdate struct {
	MessageID      int64
	HasAttachments bool
}

// MarkMessagesAttachmentIndexed records one already-committed Bleve batch in a
// single SQLite transaction. Rebuilds must not turn a 250-document Bleve batch
// into 250 separate SQLite commits.
func (s *Store) MarkMessagesAttachmentIndexed(ctx context.Context, userID int64, updates []MessageAttachmentIndexUpdate) error {
	if userID <= 0 {
		return fmt.Errorf("user id must be positive")
	}
	if len(updates) == 0 {
		return nil
	}
	tx, err := s.mustDataDB(ctx, userID).BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	statement, err := tx.PrepareContext(ctx, `UPDATE messages SET has_attachments = ?, attachment_indexed_at = ?, updated_at = ?
		WHERE user_id = ? AND id = ?`)
	if err != nil {
		return err
	}
	defer statement.Close()
	now := nowUnix()
	for _, update := range updates {
		if update.MessageID <= 0 {
			return ErrNotFound
		}
		result, err := statement.ExecContext(ctx, boolInt(update.HasAttachments), now, now, userID, update.MessageID)
		if err != nil {
			return err
		}
		rows, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if rows == 0 {
			return ErrNotFound
		}
	}
	return tx.Commit()
}

// ListSearchVisibleMessageIDsAfter pages the ids of a tenant's search-visible
// mail in id order. It is what the background sweep walks to find messages the
// index does not hold; ids alone are enough, because the sweep asks the index
// about them before it loads anything.
func (s *Store) ListSearchVisibleMessageIDsAfter(ctx context.Context, userID, afterID int64, limit int) ([]int64, error) {
	if userID <= 0 {
		return nil, fmt.Errorf("user id must be positive")
	}
	if limit <= 0 || limit > 1000 {
		limit = 500
	}
	if afterID < 0 {
		afterID = 0
	}
	db, err := s.dataDB(ctx, userID)
	if err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx, `SELECT m.id
		FROM messages m
		JOIN mailboxes mb ON mb.id = m.mailbox_id AND mb.user_id = m.user_id
		WHERE m.user_id = ? AND mb.include_in_search = 1 AND mb.search_index_purged = 0 AND m.id > ?
		ORDER BY m.id LIMIT ?`, userID, afterID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]int64, 0, limit)
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// CountIndexableMessagesForUser counts the mail that should have a document
// right now: search-visible, and not in a folder whose index was purged and is
// waiting for its rebuild.
//
// It is deliberately not CountSearchEnabledMessagesForUser, which counts a
// purged folder's mail as well. That is the honest denominator for a page
// reporting coverage - the mail is missing from search either way - and the
// wrong one for a sweep that skips purged folders: comparing a total that
// includes them against an index that cannot contain them leaves a gap no walk
// can ever close, and a sweep that never settles.
func (s *Store) CountIndexableMessagesForUser(ctx context.Context, userID int64) (int, error) {
	db, err := s.dataDB(ctx, userID)
	if err != nil {
		return 0, err
	}
	var n int
	err = db.QueryRowContext(ctx, `SELECT COUNT(*)
		FROM messages m
		JOIN mailboxes mb ON mb.id = m.mailbox_id AND mb.user_id = m.user_id
		WHERE m.user_id = ? AND mb.include_in_search = 1 AND mb.search_index_purged = 0`, userID).Scan(&n)
	return n, err
}

// MarkMessagesAttachmentIndexPending puts messages back in the indexing queue.
// It is the repair for rows that left the queue without a document ever being
// written, which the row itself cannot distinguish from a finished one: the
// caller establishes that by asking the index.
func (s *Store) MarkMessagesAttachmentIndexPending(ctx context.Context, userID int64, messageIDs []int64) (int64, error) {
	if userID <= 0 {
		return 0, fmt.Errorf("user id must be positive")
	}
	if len(messageIDs) == 0 {
		return 0, nil
	}
	db, err := s.dataDB(ctx, userID)
	if err != nil {
		return 0, err
	}
	args := make([]any, 0, len(messageIDs)+2)
	args = append(args, nowUnix(), userID)
	for _, id := range messageIDs {
		args = append(args, id)
	}
	result, err := db.ExecContext(ctx, `UPDATE messages SET attachment_indexed_at = 0, updated_at = ?
		WHERE user_id = ? AND id IN (`+sqlPlaceholders(len(messageIDs))+`)`, args...)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

// MarkMessageAttachmentIndexPending keeps a fallback search document eligible
// for later raw-body and attachment enrichment.
func (s *Store) MarkMessageAttachmentIndexPending(ctx context.Context, userID, messageID int64) error {
	result, err := s.mustDataDB(ctx, userID).ExecContext(ctx, `UPDATE messages SET attachment_indexed_at = 0, updated_at = ?
		WHERE user_id = ? AND id = ?`, nowUnix(), userID, messageID)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return ErrNotFound
	}
	return nil
}

// MarkMailboxSearchIndexPurged records a completed explicit Bleve purge without
// scheduling ordinary attachment recovery. The next folder sync or explicit
// rebuild clears this state before repairing the mailbox.
func (s *Store) MarkMailboxSearchIndexPurged(ctx context.Context, userID, mailboxID int64) error {
	result, err := s.mustDataDB(ctx, userID).ExecContext(ctx, `UPDATE mailboxes
		SET search_index_purged = 1, search_index_state_known = 1
		WHERE user_id = ? AND id = ?`, userID, mailboxID)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return ErrNotFound
	}
	return nil
}

// PreparePurgedMailboxSearchIndexRepair atomically re-enables exact repair for
// an explicitly purged mailbox. Existing per-message commit markers are kept:
// repair diffs SQLite against Bleve directly, so surviving documents from a
// partial purge must not be needlessly re-indexed. The returned bool reports
// whether purge state was consumed.
func (s *Store) PreparePurgedMailboxSearchIndexRepair(ctx context.Context, userID, mailboxID int64) (bool, error) {
	tx, err := s.mustDataDB(ctx, userID).BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var purged bool
	if err := tx.QueryRowContext(ctx, `SELECT search_index_purged FROM mailboxes
		WHERE user_id = ? AND id = ?`, userID, mailboxID).Scan(&purged); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, ErrNotFound
		}
		return false, err
	}
	if !purged {
		return false, nil
	}
	if _, err := tx.ExecContext(ctx, `UPDATE mailboxes
		SET search_index_purged = 0, search_index_state_known = 0
		WHERE user_id = ? AND id = ?`, userID, mailboxID); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

// MarkMailboxSearchIndexRepairRequired records that a mailbox's full-text
// coverage can no longer be trusted, which is what an index write that had to
// be dropped leaves behind.
//
// It clears only the "state known" flag. A deliberate purge stays purged: the
// two answer different questions, and folding them together would quietly put
// a folder the reader excluded from search back into the rebuild.
func (s *Store) MarkMailboxSearchIndexRepairRequired(ctx context.Context, userID, mailboxID int64) error {
	if userID <= 0 || mailboxID <= 0 {
		return fmt.Errorf("user and mailbox ids must be positive")
	}
	_, err := s.mustDataDB(ctx, userID).ExecContext(ctx, `UPDATE mailboxes
		SET search_index_state_known = 0
		WHERE user_id = ? AND id = ? AND include_in_search = 1`, userID, mailboxID)
	return err
}

// MarkUserSearchIndexRepairRequired does the same for every search-visible
// mailbox a tenant has. It is what a quarantined index leaves behind: the
// replacement is empty, so no folder's coverage is known any more.
func (s *Store) MarkUserSearchIndexRepairRequired(ctx context.Context, userID int64) (int64, error) {
	if userID <= 0 {
		return 0, fmt.Errorf("user id must be positive")
	}
	db, err := s.dataDB(ctx, userID)
	if err != nil {
		return 0, err
	}
	result, err := db.ExecContext(ctx, `UPDATE mailboxes
		SET search_index_state_known = 0
		WHERE user_id = ? AND include_in_search = 1 AND search_index_state_known = 1`, userID)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

// CountMailboxesNeedingSearchIndexRepair reports how many of a tenant's
// search-visible folders have coverage nothing has verified. The admin page
// reports it because it is the number a rebuild acts on.
func (s *Store) CountMailboxesNeedingSearchIndexRepair(ctx context.Context, userID int64) (int64, error) {
	if userID <= 0 {
		return 0, fmt.Errorf("user id must be positive")
	}
	db, err := s.dataDB(ctx, userID)
	if err != nil {
		return 0, err
	}
	var pending int64
	err = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM mailboxes
		WHERE user_id = ? AND include_in_search = 1 AND search_index_state_known = 0`, userID).Scan(&pending)
	return pending, err
}

// CountMailboxesWithPurgedSearchIndexForUser reports how many search-visible
// folders had their documents purged and have not been rebuilt since.
//
// A purged folder is the one shortfall no background work closes: the indexing
// queue and the backlog sweep both skip it, because the purge is a deliberate
// state that waits for its rebuild. A page showing the shortfall without this
// number shows one that never moves and blames a worker that was told not to
// touch it.
func (s *Store) CountMailboxesWithPurgedSearchIndexForUser(ctx context.Context, userID int64) (int64, error) {
	if userID <= 0 {
		return 0, fmt.Errorf("user id must be positive")
	}
	db, err := s.dataDB(ctx, userID)
	if err != nil {
		return 0, err
	}
	var purged int64
	err = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM mailboxes
		WHERE user_id = ? AND include_in_search = 1 AND search_index_purged = 1`, userID).Scan(&purged)
	return purged, err
}

// MarkMailboxSearchIndexActive records that exact mailbox repair completed.
// It is the only transition from migration-time unknown state to verified
// active state, so settings never guesses current Bleve coverage from markers.
func (s *Store) MarkMailboxSearchIndexActive(ctx context.Context, userID, mailboxID int64) error {
	result, err := s.mustDataDB(ctx, userID).ExecContext(ctx, `UPDATE mailboxes
		SET search_index_purged = 0, search_index_state_known = 1
		WHERE user_id = ? AND id = ?`, userID, mailboxID)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return ErrNotFound
	}
	return nil
}

// UpdateMessageLanguage stores plugin-detected language metadata for search filtering.
func (s *Store) UpdateMessageLanguage(ctx context.Context, userID, messageID int64, languageCode string) error {
	languageCode = strings.ToLower(strings.TrimSpace(languageCode))
	_, err := s.mustDataDB(ctx, userID).ExecContext(ctx, `UPDATE messages SET language_code = ?, updated_at = ?
		WHERE user_id = ? AND id = ?`, strings.ToLower(strings.TrimSpace(languageCode)), nowUnix(), userID, messageID)
	if err != nil {
		return err
	}
	return s.upsertPluginMessageLanguage(ctx, userID, messageID, languageCode)
}

func (s *Store) upsertPluginMessageLanguage(ctx context.Context, userID, messageID int64, languageCode string) error {
	languageCode = strings.ToLower(strings.TrimSpace(languageCode))
	if userID == 0 || messageID == 0 || languageCode == "" {
		return nil
	}
	_, err := s.mustDataDB(ctx, userID).ExecContext(ctx, `INSERT INTO plugin_language_messages
			(user_id, message_id, language_code, detected_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(user_id, message_id) DO UPDATE SET
			language_code = excluded.language_code,
			detected_at = excluded.detected_at`,
		userID, messageID, languageCode, nowUnix())
	return err
}

// UpdateMessageSecurityState stores plugin-detected encrypted/signed metadata discovered while parsing raw messages.
func (s *Store) UpdateMessageSecurityState(ctx context.Context, userID, messageID int64, encrypted, signed bool) error {
	_, err := s.mustDataDB(ctx, userID).ExecContext(ctx, `UPDATE messages SET is_encrypted = ?, is_signed = ?, updated_at = ?
		WHERE user_id = ? AND id = ?`, boolInt(encrypted), boolInt(signed), nowUnix(), userID, messageID)
	return err
}

// MessageIDsInRange returns the message row IDs that still exist in an inclusive
// range. Targeted search recovery uses it to tell apart the two reasons a marked
// document can be missing from SQLite: it was never there, or its message was
// deleted while the stalled batch held the writer gate and the matching Bleve
// delete could not run. Only the second leaves a document behind in an index
// that recovery now keeps.
func (s *Store) MessageIDsInRange(ctx context.Context, userID, firstID, lastID int64) ([]int64, error) {
	if userID <= 0 {
		return nil, fmt.Errorf("user id must be positive")
	}
	if firstID <= 0 || lastID < firstID {
		return nil, fmt.Errorf("message id range %d-%d is not usable", firstID, lastID)
	}
	db, err := s.dataDB(ctx, userID)
	if err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx,
		`SELECT id FROM messages WHERE user_id = ? AND id BETWEEN ? AND ? ORDER BY id`,
		userID, firstID, lastID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := make([]int64, 0)
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
