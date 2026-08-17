// File overview: Mailbox, all-mail, search seed, sender stat, and date-window query helpers.

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ListMessagesForUser returns recent messages across visible mailboxes for one user.
func (s *Store) ListMessagesForUser(ctx context.Context, userID int64, limit, offset int) ([]MessageRecord, error) {
	db, err := s.dataDB(ctx, userID)
	if err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := db.QueryContext(ctx, `SELECT m.id, m.user_id, m.account_id, m.mailbox_id, m.blob_id, m.message_id_header, m.in_reply_to, m.references_header, m.thread_key, m.subject, m.language_code, m.from_addr, m.to_addr, m.cc_addr,
			m.date_unix, m.internal_date_unix, m.uid, m.size, m.blob_path, m.body_text, m.body_html, m.is_read, m.read_sync_pending, m.is_starred, m.star_sync_pending, m.has_attachments, m.is_encrypted, m.is_signed, m.attachment_indexed_at, m.created_at, m.updated_at
		FROM messages m
		LEFT JOIN message_snoozes sn ON sn.user_id = m.user_id
			AND sn.thread_key = COALESCE(NULLIF(m.thread_key, ''), 'id:' || m.id)
		WHERE m.user_id = ? AND m.duplicate_of_message_id = 0 AND (sn.id IS NULL OR sn.snoozed_until <= ?)
		ORDER BY CASE WHEN COALESCE(sn.snoozed_until, 0) > m.date_unix THEN sn.snoozed_until ELSE m.date_unix END DESC, m.id DESC
		LIMIT ? OFFSET ?`, userID, nowUnix(), limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanMessages(rows)
}

// ListMessagesForMailbox returns recent messages from one user-owned mailbox.
func (s *Store) ListMessagesForMailbox(ctx context.Context, userID, mailboxID int64, limit, offset int) ([]MessageRecord, error) {
	db, err := s.dataDB(ctx, userID)
	if err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := db.QueryContext(ctx, `SELECT m.id, m.user_id, m.account_id, m.mailbox_id, m.blob_id, m.message_id_header, m.in_reply_to, m.references_header, m.thread_key, m.subject, m.language_code, m.from_addr, m.to_addr, m.cc_addr,
			m.date_unix, m.internal_date_unix, m.uid, m.size, m.blob_path, m.body_text, m.body_html, m.is_read, m.read_sync_pending, m.is_starred, m.star_sync_pending, m.has_attachments, m.is_encrypted, m.is_signed, m.attachment_indexed_at, m.created_at, m.updated_at
		FROM messages m
		LEFT JOIN message_snoozes sn ON sn.user_id = m.user_id
			AND sn.thread_key = COALESCE(NULLIF(m.thread_key, ''), 'id:' || m.id)
		WHERE m.user_id = ? AND m.mailbox_id = ? AND m.duplicate_of_message_id = 0 AND (sn.id IS NULL OR sn.snoozed_until <= ?)
		ORDER BY CASE WHEN COALESCE(sn.snoozed_until, 0) > m.date_unix THEN sn.snoozed_until ELSE m.date_unix END DESC, m.id DESC
		LIMIT ? OFFSET ?`, userID, mailboxID, nowUnix(), limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanMessages(rows)
}

// CountMessagesForUser counts all local message header rows for one user.
// Storage reporting presents this as Message Headers because SQLite keeps the
// message envelope, flags, thread fields, and preview text even when the raw body
// has been pruned or never cached locally.
func (s *Store) CountMessagesForUser(ctx context.Context, userID int64) (int, error) {
	db, err := s.dataDB(ctx, userID)
	if err != nil {
		return 0, err
	}
	var n int
	err = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM messages WHERE user_id = ?`, userID).Scan(&n)
	return n, err
}

// CountCachedMessageBodiesForUser counts messages whose raw RFC822 body is
// currently held in the local blob store. Remote-only placeholder blobs are not
// counted because there is no local body file behind them.
func (s *Store) CountCachedMessageBodiesForUser(ctx context.Context, userID int64) (int, error) {
	db, err := s.dataDB(ctx, userID)
	if err != nil {
		return 0, err
	}
	var n int
	err = db.QueryRowContext(ctx, `SELECT COUNT(*)
		FROM messages m
		JOIN blobs b ON b.user_id = m.user_id AND b.id = m.blob_id
		WHERE m.user_id = ? AND m.blob_path != '' AND b.kind IN ('message', 'message-cache') AND b.size > 0`, userID).Scan(&n)
	return n, err
}

// CountMessagesForMailbox counts local mirrored messages in one user-owned mailbox.
func (s *Store) CountMessagesForMailbox(ctx context.Context, userID, mailboxID int64) (int, error) {
	db, err := s.dataDB(ctx, userID)
	if err != nil {
		return 0, err
	}
	var n int
	err = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM messages WHERE user_id = ? AND mailbox_id = ?`, userID, mailboxID).Scan(&n)
	return n, err
}

// ListSearchIndexedMessageIDsForMailbox pages current local rows whose search
// document commit marker is set. Callers can intersect these IDs with Bleve
// hits without letting stale documents substitute for missing current rows.
func (s *Store) ListSearchIndexedMessageIDsForMailbox(ctx context.Context, userID, mailboxID, afterID int64, limit int) ([]int64, error) {
	if limit <= 0 || limit > 1000 {
		limit = 500
	}
	db, err := s.dataDB(ctx, userID)
	if err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx, `SELECT id FROM messages
		WHERE user_id = ? AND mailbox_id = ? AND attachment_indexed_at > 0 AND id > ?
		ORDER BY id LIMIT ?`, userID, mailboxID, afterID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := make([]int64, 0, limit)
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// CountSearchIndexedMessagesByMailbox returns the durable post-Bleve-commit
// marker count for each of a user's mailboxes. Settings uses this inexpensive
// aggregate for progress; exact missing/stale document reconciliation remains
// part of search-index repair rather than a blocking browser request.
func (s *Store) CountSearchIndexedMessagesByMailbox(ctx context.Context, userID int64) (map[int64]int, error) {
	rows, err := s.mustDataDB(ctx, userID).QueryContext(ctx, `SELECT mailbox_id, COUNT(*)
		FROM messages
		WHERE user_id = ? AND attachment_indexed_at > 0
			AND mailbox_id NOT IN (
				SELECT id FROM mailboxes WHERE user_id = ? AND search_index_purged = 1
			)
		GROUP BY mailbox_id`, userID, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	counts := map[int64]int{}
	for rows.Next() {
		var mailboxID int64
		var count int
		if err := rows.Scan(&mailboxID, &count); err != nil {
			return nil, err
		}
		counts[mailboxID] = count
	}
	return counts, rows.Err()
}

// ListPurgedSearchIndexMailboxIDs returns tenant-owned mailboxes whose local
// full-text documents were deliberately purged and are waiting for a folder
// sync or explicit rebuild.
func (s *Store) ListPurgedSearchIndexMailboxIDs(ctx context.Context, userID int64) (map[int64]bool, error) {
	rows, err := s.mustDataDB(ctx, userID).QueryContext(ctx, `SELECT id
		FROM mailboxes
		WHERE user_id = ? AND search_index_purged = 1`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	purged := map[int64]bool{}
	for rows.Next() {
		var mailboxID int64
		if err := rows.Scan(&mailboxID); err != nil {
			return nil, err
		}
		purged[mailboxID] = true
	}
	return purged, rows.Err()
}

// CountSearchEnabledMessagesForUser counts local messages in folders included in
// full-text search. Settings storage uses this alongside Bleve's document count
// to show whether SQLite mail and the search index are in the same ballpark.
func (s *Store) CountSearchEnabledMessagesForUser(ctx context.Context, userID int64) (int, error) {
	db, err := s.dataDB(ctx, userID)
	if err != nil {
		return 0, err
	}
	var n int
	err = db.QueryRowContext(ctx, `SELECT COUNT(*)
		FROM messages m
		JOIN mailboxes mb ON mb.id = m.mailbox_id AND mb.user_id = m.user_id
		WHERE m.user_id = ? AND mb.include_in_search = 1`, userID).Scan(&n)
	return n, err
}

// ListRecentSearchEnabledMessagesForUser returns the newest local messages from
// folders that participate in full-text search. The web layer uses this as a
// bounded self-healing pass before search requests so a failed/interrupted Bleve
// commit does not leave today's mail undiscoverable until a manual repair.
func (s *Store) ListRecentSearchEnabledMessagesForUser(ctx context.Context, userID int64, limit int) ([]MessageRecord, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	db, err := s.dataDB(ctx, userID)
	if err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx, `SELECT m.id, m.user_id, m.account_id, m.mailbox_id, m.blob_id, m.message_id_header, m.in_reply_to, m.references_header, m.thread_key, m.subject, m.language_code, m.from_addr, m.to_addr, m.cc_addr,
			m.date_unix, m.internal_date_unix, m.uid, m.size, m.blob_path, m.body_text, m.body_html, m.is_read, m.read_sync_pending, m.is_starred, m.star_sync_pending, m.has_attachments, m.is_encrypted, m.is_signed, m.attachment_indexed_at, m.created_at, m.updated_at
		FROM messages m
		JOIN mailboxes mb ON mb.id = m.mailbox_id AND mb.user_id = m.user_id
		WHERE m.user_id = ? AND mb.include_in_search = 1
		ORDER BY m.date_unix DESC, m.id DESC LIMIT ?`, userID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanMessages(rows)
}

// ScopeMessage is one message a whole-view selection ("act on everything this
// filter matches") resolves to. Only routing fields are carried: the account
// decides which Trash folder applies, and the source mailbox decides which
// folders a move has to refresh afterwards.
type ScopeMessage struct {
	ID        int64
	AccountID int64
	MailboxID int64
}

// maxScopeMessages bounds one resolved selection so a filter over a very large
// mailbox cannot pin an unbounded ID set in memory. Callers pass their own
// smaller limit and report to the user when a pass was cut short.
const maxScopeMessages = 50000

// ScopeFilter narrows a whole-view selection past the list it mirrors. The zero
// value selects everything that list shows.
type ScopeFilter struct {
	// Before keeps only mail dated before this instant, which is what an
	// "archive everything older than" action selects.
	Before time.Time
}

// sql renders the filter as a WHERE fragment plus its arguments. The cutoff
// belongs in the query rather than in a pass over the resolved rows: a
// selection is capped, and lists are ordered newest first, so filtering
// afterwards would spend the whole cap on mail the caller did not ask for and
// never reach the old messages it did.
func (f ScopeFilter) sql() (string, []any) {
	if f.Before.IsZero() {
		return "", nil
	}
	return " AND m.date_unix < ?", []any{f.Before.UTC().Unix()}
}

// Matches reports whether one message record satisfies the filter. Scopes that
// are resolved through the search index rather than SQL apply it this way.
func (f ScopeFilter) Matches(msg MessageRecord) bool {
	return f.Before.IsZero() || msg.Date.Before(f.Before)
}

func scopeMessageLimit(limit int) int {
	if limit <= 0 || limit > maxScopeMessages {
		return maxScopeMessages
	}
	return limit
}

// archivedMailboxExclusion renders the SQL that keeps each account's effective
// Archive folder out of a list, along with its arguments. Requesting the
// exclusion when nothing is configured yields an empty fragment, so the Inbox
// list simply equals All Mail until an Archive folder exists.
func (s *Store) archivedMailboxExclusion(ctx context.Context, userID int64, exclude bool) (string, []any, error) {
	if !exclude {
		return "", nil, nil
	}
	ids, err := s.ArchiveMailboxIDsForUser(ctx, userID)
	if err != nil || len(ids) == 0 {
		return "", nil, err
	}
	placeholders, args := int64ListPlaceholders(ids)
	return " AND m.mailbox_id NOT IN (" + placeholders + ")", args, nil
}

// ListAllMailScopeMessagesForUser lists the messages an All Mail selection
// covers: every unsnoozed message in a folder All Mail shows. Rows the list
// hides (Trash, Junk, Drafts, snoozed threads) stay out of the selection.
func (s *Store) ListAllMailScopeMessagesForUser(ctx context.Context, userID int64, filter ScopeFilter, limit int) ([]ScopeMessage, error) {
	return s.listAllMailScopeMessagesForUser(ctx, userID, filter, limit, false)
}

// ListUnarchivedMailScopeMessagesForUser lists what an Inbox selection covers:
// the All Mail scope minus each account's Archive folder, so a whole-view
// delete from the Inbox list never reaches archived mail.
func (s *Store) ListUnarchivedMailScopeMessagesForUser(ctx context.Context, userID int64, filter ScopeFilter, limit int) ([]ScopeMessage, error) {
	return s.listAllMailScopeMessagesForUser(ctx, userID, filter, limit, true)
}

func (s *Store) listAllMailScopeMessagesForUser(ctx context.Context, userID int64, filter ScopeFilter, limit int, excludeArchived bool) ([]ScopeMessage, error) {
	db, err := s.dataDB(ctx, userID)
	if err != nil {
		return nil, err
	}
	exclusion, exclusionArgs, err := s.archivedMailboxExclusion(ctx, userID, excludeArchived)
	if err != nil {
		return nil, err
	}
	cutoff, cutoffArgs := filter.sql()
	args := make([]any, 0, len(exclusionArgs)+len(cutoffArgs)+3)
	args = append(args, userID)
	args = append(args, exclusionArgs...)
	args = append(args, cutoffArgs...)
	args = append(args, nowUnix(), scopeMessageLimit(limit))
	rows, err := db.QueryContext(ctx, `SELECT m.id, m.account_id, m.mailbox_id
		FROM messages m
		JOIN mailboxes mb ON mb.id = m.mailbox_id AND mb.user_id = m.user_id
		LEFT JOIN message_snoozes sn ON sn.user_id = m.user_id
			AND sn.thread_key = COALESCE(NULLIF(m.thread_key, ''), 'id:' || m.id)
		WHERE m.user_id = ? AND mb.show_in_all_mail = 1`+exclusion+cutoff+` AND m.duplicate_of_message_id = 0
			AND (sn.id IS NULL OR sn.snoozed_until <= ?)
		ORDER BY m.date_unix DESC, m.id DESC
		LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanScopeMessages(rows)
}

// ListMailboxScopeMessagesForUser lists the messages a single-folder selection
// covers. Unlike All Mail this includes folders hidden from All Mail, because
// the user is looking at that folder's own list.
func (s *Store) ListMailboxScopeMessagesForUser(ctx context.Context, userID, mailboxID int64, filter ScopeFilter, limit int) ([]ScopeMessage, error) {
	db, err := s.dataDB(ctx, userID)
	if err != nil {
		return nil, err
	}
	cutoff, cutoffArgs := filter.sql()
	args := make([]any, 0, len(cutoffArgs)+4)
	args = append(args, userID, mailboxID)
	args = append(args, cutoffArgs...)
	args = append(args, nowUnix(), scopeMessageLimit(limit))
	rows, err := db.QueryContext(ctx, `SELECT m.id, m.account_id, m.mailbox_id
		FROM messages m
		LEFT JOIN message_snoozes sn ON sn.user_id = m.user_id
			AND sn.thread_key = COALESCE(NULLIF(m.thread_key, ''), 'id:' || m.id)
		WHERE m.user_id = ? AND m.mailbox_id = ?`+cutoff+` AND m.duplicate_of_message_id = 0
			AND (sn.id IS NULL OR sn.snoozed_until <= ?)
		ORDER BY m.date_unix DESC, m.id DESC
		LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanScopeMessages(rows)
}

// RoleMailboxIDsForUser lists every folder carrying one mailbox role across all
// of a user's accounts. A role is unique per account and is set on the folder
// itself, so this is the configured answer to which folder holds an account's
// sent mail or drafts rather than a guess made per view.
func (s *Store) RoleMailboxIDsForUser(ctx context.Context, userID int64, role string) ([]int64, error) {
	normalized := normalizeMailboxRole(role)
	if normalized == "" {
		return nil, fmt.Errorf("list role mailboxes: unsupported role %q", role)
	}
	db, err := s.dataDB(ctx, userID)
	if err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx, `SELECT id FROM mailboxes
		WHERE user_id = ? AND role = ? ORDER BY id`, userID, normalized)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := make([]int64, 0, 4)
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// ListRoleMailScopeMessagesForUser lists what a whole-view selection over a
// role covers, so a delete from a role view reaches exactly the rows it shows.
func (s *Store) ListRoleMailScopeMessagesForUser(ctx context.Context, userID int64, role string, filter ScopeFilter, limit int) ([]ScopeMessage, error) {
	mailboxIDs, err := s.RoleMailboxIDsForUser(ctx, userID, role)
	if err != nil || len(mailboxIDs) == 0 {
		return nil, err
	}
	db, err := s.dataDB(ctx, userID)
	if err != nil {
		return nil, err
	}
	placeholders, idArgs := int64ListPlaceholders(mailboxIDs)
	cutoff, cutoffArgs := filter.sql()
	args := make([]any, 0, len(idArgs)+len(cutoffArgs)+3)
	args = append(args, userID)
	args = append(args, idArgs...)
	args = append(args, cutoffArgs...)
	args = append(args, nowUnix(), scopeMessageLimit(limit))
	rows, err := db.QueryContext(ctx, `SELECT m.id, m.account_id, m.mailbox_id
		FROM messages m
		LEFT JOIN message_snoozes sn ON sn.user_id = m.user_id
			AND sn.thread_key = COALESCE(NULLIF(m.thread_key, ''), 'id:' || m.id)
		WHERE m.user_id = ? AND m.mailbox_id IN (`+placeholders+`)`+cutoff+` AND m.duplicate_of_message_id = 0
			AND (sn.id IS NULL OR sn.snoozed_until <= ?)
		ORDER BY m.date_unix DESC, m.id DESC
		LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanScopeMessages(rows)
}

// int64ListPlaceholders renders an IN list for a caller-owned set of IDs. Only
// values the store itself resolved are ever passed here, never request input.
func int64ListPlaceholders(values []int64) (string, []any) {
	marks := make([]string, len(values))
	args := make([]any, 0, len(values))
	for i, value := range values {
		marks[i] = "?"
		args = append(args, value)
	}
	return strings.Join(marks, ","), args
}

func scanScopeMessages(rows *sql.Rows) ([]ScopeMessage, error) {
	out := make([]ScopeMessage, 0, 64)
	for rows.Next() {
		var item ScopeMessage
		if err := rows.Scan(&item.ID, &item.AccountID, &item.MailboxID); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

// ThreadListOrder selects the date direction of a conversation list page. Mail
// lists default to newest first; the reversed order lets a reader walk a folder
// forward from its oldest locally stored message.
type ThreadListOrder string

const (
	ThreadListNewestFirst ThreadListOrder = "newest"
	ThreadListOldestFirst ThreadListOrder = "oldest"
)

// sortDirection maps a list order onto a SQL keyword. Anything unrecognized
// falls back to newest first, so a caller can hand this a raw request value
// without the query ever taking text from the URL.
func (o ThreadListOrder) sortDirection() string {
	if o == ThreadListOldestFirst {
		return "ASC"
	}
	return "DESC"
}

// ListLatestThreadMessagesForUser returns one latest message per thread for all-mail list rendering.
// A thread is always represented by its newest message; order only decides where
// that thread lands in the page.
func (s *Store) ListLatestThreadMessagesForUser(ctx context.Context, userID int64, limit, offset int, order ThreadListOrder) ([]MessageRecord, error) {
	return s.listLatestThreadMessagesForUser(ctx, userID, limit, offset, order, false)
}

// ListUnarchivedLatestThreadMessagesForUser renders the Inbox list: All
// Mail minus the messages sitting in each account's chosen Archive folder. A
// thread whose newest message is archived still appears through its newest
// unarchived message, matching how single-folder lists represent threads.
func (s *Store) ListUnarchivedLatestThreadMessagesForUser(ctx context.Context, userID int64, limit, offset int, order ThreadListOrder) ([]MessageRecord, error) {
	return s.listLatestThreadMessagesForUser(ctx, userID, limit, offset, order, true)
}

// messageColumns is the projection every message list shares. scanMessages
// reads these in order, so the two only stay in step while there is a single
// copy of the list to change.
const messageColumns = `m.id, m.user_id, m.account_id, m.mailbox_id, m.blob_id, m.message_id_header, m.in_reply_to, m.references_header, m.thread_key, m.subject, m.language_code, m.from_addr, m.to_addr, m.cc_addr,
		m.date_unix, m.internal_date_unix, m.uid, m.size, m.blob_path, m.body_text, m.body_html, m.is_read, m.read_sync_pending, m.is_starred, m.star_sync_pending, m.has_attachments, m.is_encrypted, m.is_signed, m.attachment_indexed_at, m.created_at, m.updated_at`

// latestThreadMessagesQuery renders the one-row-per-conversation query the mail
// lists share. Callers supply only what makes their list different: extra FROM
// text and an extra WHERE fragment. Thread grouping, snooze handling, ordering,
// and paging stay in one place, and their arguments go between the leading
// user_id and the trailing snooze/limit/offset triple.
func latestThreadMessagesQuery(source, predicate, direction string) string {
	return fmt.Sprintf(`WITH keyed AS (
			SELECT COALESCE(NULLIF(m.thread_key, ''), 'id:' || m.id) AS thread_group,
				MAX(printf('%%020d:%%020d',
					CASE WHEN COALESCE(sn.snoozed_until, 0) > m.date_unix THEN sn.snoozed_until ELSE m.date_unix END,
					m.id)) AS latest_key
			FROM messages m%[1]s
			LEFT JOIN message_snoozes sn ON sn.user_id = m.user_id
				AND sn.thread_key = COALESCE(NULLIF(m.thread_key, ''), 'id:' || m.id)
			WHERE m.user_id = ?%[2]s AND m.duplicate_of_message_id = 0
				AND (sn.id IS NULL OR sn.snoozed_until <= ?)
			GROUP BY thread_group
			ORDER BY latest_key %[3]s LIMIT ? OFFSET ?
		)
		SELECT `+messageColumns+`
		FROM keyed k JOIN messages m ON m.id = CAST(substr(k.latest_key, 22) AS INTEGER)
		ORDER BY k.latest_key %[3]s`, source, predicate, direction)
}

// threadListLimit clamps a page size to what one list request may return.
func threadListLimit(limit int) int {
	if limit <= 0 || limit > 200 {
		return 50
	}
	return limit
}

func (s *Store) listLatestThreadMessagesForUser(ctx context.Context, userID int64, limit, offset int, order ThreadListOrder, excludeArchived bool) ([]MessageRecord, error) {
	db, err := s.dataDB(ctx, userID)
	if err != nil {
		return nil, err
	}
	exclusion, exclusionArgs, err := s.archivedMailboxExclusion(ctx, userID, excludeArchived)
	if err != nil {
		return nil, err
	}
	args := make([]any, 0, len(exclusionArgs)+4)
	args = append(args, userID)
	args = append(args, exclusionArgs...)
	args = append(args, nowUnix(), threadListLimit(limit), offset)
	query := latestThreadMessagesQuery(
		"\n\t\t\tJOIN mailboxes mb ON mb.id = m.mailbox_id AND mb.user_id = m.user_id",
		" AND mb.show_in_all_mail = 1"+exclusion,
		order.sortDirection())
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanMessages(rows)
}

// ListLatestThreadMessagesForMailbox returns one latest message per thread within a mailbox.
func (s *Store) ListLatestThreadMessagesForMailbox(ctx context.Context, userID, mailboxID int64, limit, offset int, order ThreadListOrder) ([]MessageRecord, error) {
	db, err := s.dataDB(ctx, userID)
	if err != nil {
		return nil, err
	}
	query := latestThreadMessagesQuery("", " AND m.mailbox_id = ?", order.sortDirection())
	rows, err := db.QueryContext(ctx, query, userID, mailboxID, nowUnix(), threadListLimit(limit), offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanMessages(rows)
}

// ListRoleLatestThreadMessagesForUser renders a global role view such as Sent
// or Drafts: one row per conversation across every account's folder for that
// role. Unlike All Mail it ignores show_in_all_mail, because keeping a folder
// out of the combined list is no reason to empty the list made from it.
func (s *Store) ListRoleLatestThreadMessagesForUser(ctx context.Context, userID int64, role string, limit, offset int, order ThreadListOrder) ([]MessageRecord, error) {
	mailboxIDs, err := s.RoleMailboxIDsForUser(ctx, userID, role)
	if err != nil || len(mailboxIDs) == 0 {
		return nil, err
	}
	db, err := s.dataDB(ctx, userID)
	if err != nil {
		return nil, err
	}
	placeholders, idArgs := int64ListPlaceholders(mailboxIDs)
	args := make([]any, 0, len(idArgs)+4)
	args = append(args, userID)
	args = append(args, idArgs...)
	args = append(args, nowUnix(), threadListLimit(limit), offset)
	query := latestThreadMessagesQuery("", " AND m.mailbox_id IN ("+placeholders+")", order.sortDirection())
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanMessages(rows)
}

// ListMessagesByIDsForUser bulk-loads messages by ID while preserving user ownership checks.
func (s *Store) ListMessagesByIDsForUser(ctx context.Context, userID int64, ids []int64) ([]MessageRecord, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	messages := make([]MessageRecord, 0, len(ids))
	for _, id := range ids {
		m, err := s.GetMessageForUser(ctx, userID, id)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, err
		}
		messages = append(messages, m)
	}
	return messages, nil
}

// ListThreadMessagesForUser loads all messages in the selected message's conversation.
func (s *Store) ListThreadMessagesForUser(ctx context.Context, userID int64, msg MessageRecord) ([]MessageRecord, error) {
	db, err := s.dataDB(ctx, userID)
	if err != nil {
		return nil, err
	}
	key := strings.TrimSpace(msg.ThreadKey)
	if key == "" {
		key = ThreadKey(msg.MessageIDHeader, msg.InReplyTo, msg.ReferencesHeader, msg.Subject)
	}
	if key == "" {
		return []MessageRecord{msg}, nil
	}
	ids, err := s.threadMessageIDProbe(ctx, db, userID, key)
	if err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return []MessageRecord{msg}, nil
	}
	if len(ids) == 1 && ids[0] == msg.ID {
		return []MessageRecord{msg}, nil
	}
	rows, err := db.QueryContext(ctx, `SELECT id, user_id, account_id, mailbox_id, blob_id, message_id_header, in_reply_to, references_header, thread_key, subject, language_code, from_addr, to_addr, cc_addr,
			date_unix, internal_date_unix, uid, size, blob_path, body_text, body_html, is_read, read_sync_pending, is_starred, star_sync_pending, has_attachments, is_encrypted, is_signed, attachment_indexed_at, created_at, updated_at
		FROM messages WHERE user_id = ? AND thread_key = ? AND duplicate_of_message_id = 0
		ORDER BY date_unix ASC, id ASC`, userID, key)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanMessages(rows)
}

func (s *Store) threadMessageIDProbe(ctx context.Context, db *sql.DB, userID int64, key string) ([]int64, error) {
	rows, err := db.QueryContext(ctx, `SELECT id FROM messages WHERE user_id = ? AND thread_key = ? AND duplicate_of_message_id = 0
		ORDER BY date_unix ASC, id ASC LIMIT 2`, userID, key)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := make([]int64, 0, 2)
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// ListThreadMessagesByKeysForUser groups messages by thread keys for conversation hydration.
func (s *Store) ListThreadMessagesByKeysForUser(ctx context.Context, userID int64, keys []string) (map[string][]MessageRecord, error) {
	db, err := s.dataDB(ctx, userID)
	if err != nil {
		return nil, err
	}
	out := make(map[string][]MessageRecord, len(keys))
	seen := map[string]bool{}
	unique := make([]string, 0, len(keys))
	for _, key := range keys {
		key = strings.TrimSpace(key)
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		unique = append(unique, key)
	}
	const chunkSize = 200
	for start := 0; start < len(unique); start += chunkSize {
		end := start + chunkSize
		if end > len(unique) {
			end = len(unique)
		}
		chunk := unique[start:end]
		placeholders := make([]string, len(chunk))
		args := make([]any, 0, len(chunk)+1)
		args = append(args, userID)
		for i, key := range chunk {
			placeholders[i] = "?"
			args = append(args, key)
		}
		rows, err := db.QueryContext(ctx, `SELECT id, user_id, account_id, mailbox_id, blob_id, message_id_header, in_reply_to, references_header, thread_key, subject, language_code, from_addr, to_addr, cc_addr,
			date_unix, internal_date_unix, uid, size, blob_path, body_text, body_html, is_read, read_sync_pending, is_starred, star_sync_pending, has_attachments, is_encrypted, is_signed, attachment_indexed_at, created_at, updated_at
		FROM messages WHERE user_id = ? AND thread_key IN (`+strings.Join(placeholders, ",")+`) AND duplicate_of_message_id = 0
			ORDER BY thread_key ASC, date_unix ASC, id ASC`, args...)
		if err != nil {
			return nil, err
		}
		messages, err := scanMessages(rows)
		closeErr := rows.Close()
		if err != nil {
			return nil, err
		}
		if closeErr != nil {
			return nil, closeErr
		}
		for _, msg := range messages {
			out[msg.ThreadKey] = append(out[msg.ThreadKey], msg)
		}
	}
	return out, nil
}
