// File overview: Message category storage: the classification backfill's work
// queue, the per-sender corrections that override it, and the sender
// normalization both sides agree on.

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"

	"rolltop/backend/mailparse"
)

// CategoryCandidate is one message the classifier still has to decide. Only what
// classification reads is carried: the stored raw message when there is one, and
// the sender the header-less fallback works from.
type CategoryCandidate struct {
	ID       int64
	FromAddr string
	BlobPath string
}

// CategoryVersion is the classifier generation a message's category was decided
// by, stored beside the category itself. The backfill re-reads mail an older
// generation filed, which is what lets a new category take mail that is already
// sitting in another one.
//
// The alternative -- emptying the column so the existing pass picks it up --
// was rejected: an empty category is not in any category list, so every list
// would go blank for as long as the re-pass takes. Rows keep the answer they
// have until the new one replaces it.
//
// Bump this whenever a change to mailparse's rules should reach mail that is
// already stored. It costs one blob read per message, spread over the
// background worker's turns.
const CategoryVersion = 1

// CategoryBackfillLimit bounds one backfill pass, and is the batch size the
// worker uses. The pass opens a stored message per row, so the ceiling is about
// how long the worker holds its turn, not about how much SQLite can return.
const CategoryBackfillLimit = 200

// NormalizeCategorySender reduces a From header to the key a correction is
// remembered under. It is the classifier's own address reading, so a sender the
// classifier can file is always a sender the user can correct.
func NormalizeCategorySender(from string) string {
	return mailparse.BareAddress(from)
}

// validCategoryOrError keeps unknown names out of every query. Category names
// arrive from requests and from stored rows alike, and both are checked here.
func validCategoryOrError(category string) (string, error) {
	normalized := strings.ToLower(strings.TrimSpace(category))
	if !mailparse.ValidCategory(normalized) {
		return "", fmt.Errorf("message category: unsupported category %q", category)
	}
	return normalized, nil
}

// ListMessagesNeedingCategory returns the next batch of messages the classifier
// still owes an answer: the ones that have no category at all, and then the
// ones an older classifier generation filed. Rows are taken oldest-id first so
// a pass that is interrupted resumes where it stopped rather than revisiting
// the newest mail every time.
//
// Unclassified mail is always taken first and in full. It is the only kind that
// is missing from every list while it waits, so a re-pass over a large mailbox
// must never push it behind an hour of work.
func (s *Store) ListMessagesNeedingCategory(ctx context.Context, userID int64, limit int) ([]CategoryCandidate, error) {
	if limit <= 0 || limit > CategoryBackfillLimit {
		limit = CategoryBackfillLimit
	}
	db, err := s.dataDB(ctx, userID)
	if err != nil {
		return nil, err
	}
	// Mail the category lists can show is classified before the rest. The
	// pending counter beside those lists is scoped the same way, so a plain
	// oldest-first order lets it sit at a small number for hours while the
	// worker is busy filing a Trash folder nobody is looking at. The rest is
	// not skipped, only queued behind: a message moved back out of Trash still
	// arrives with a category.
	//
	// The join is a LEFT one so a row whose mailbox is gone is filed last
	// rather than never. Such a row still counts as pending in the worker's
	// eyes, and one of them would otherwise be selected forever.
	priority, priorityArgs, err := s.inPlayMailScope(ctx, userID, true, "")
	if err != nil {
		return nil, err
	}
	args := make([]any, 0, len(priorityArgs)+2)
	args = append(args, priorityArgs...)
	args = append(args, userID, limit)
	rows, err := db.QueryContext(ctx, `SELECT m.id, m.from_addr, m.blob_path,
			CASE WHEN 1 = 1`+priority+` THEN 0 ELSE 1 END AS backlog
		FROM messages m
		LEFT JOIN mailboxes mb ON mb.id = m.mailbox_id AND mb.user_id = m.user_id
		WHERE m.user_id = ? AND m.category = ''
		ORDER BY backlog, m.id
		LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]CategoryCandidate, 0, limit)
	for rows.Next() {
		var item CategoryCandidate
		var backlog int
		if err := rows.Scan(&item.ID, &item.FromAddr, &item.BlobPath, &backlog); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) >= limit {
		return out, nil
	}
	stale, err := s.listMessagesWithStaleCategory(ctx, userID, limit-len(out))
	if err != nil {
		return nil, err
	}
	return append(out, stale...), nil
}

// listMessagesWithStaleCategory returns messages an older classifier generation
// filed, oldest-id first.
//
// The in-play priority the pending query applies is deliberately absent here.
// These rows are already in a category and already visible; ordering them by
// what a list can show would mean sorting the tenant's entire mailbox on every
// pass to reorder work nobody is waiting on.
func (s *Store) listMessagesWithStaleCategory(ctx context.Context, userID int64, limit int) ([]CategoryCandidate, error) {
	if limit <= 0 {
		return nil, nil
	}
	db, err := s.dataDB(ctx, userID)
	if err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx, `SELECT m.id, m.from_addr, m.blob_path
		FROM messages m
		WHERE m.user_id = ? AND m.category <> '' AND m.category_version < ?
		ORDER BY m.id
		LIMIT ?`, userID, CategoryVersion, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]CategoryCandidate, 0, limit)
	for rows.Next() {
		var item CategoryCandidate
		if err := rows.Scan(&item.ID, &item.FromAddr, &item.BlobPath); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

// CountMessagesNeedingCategory reports how much of the backfill is still to
// come, counted over the folders the category lists actually draw from. The
// worker classifies every message it finds, including Trash and Junk, but
// counting those here would leave the browser reporting tens of thousands of
// messages still to sort long after every list it can render is complete.
//
// Only mail that has no category at all is counted. A message an older
// generation filed is in a list and readable while it waits to be re-read, and
// reporting a re-pass as mail "waiting to be sorted" would tell the reader
// their whole mailbox had come undone.
//
// Snoozed threads are counted: they are hidden for now, not out of scope, and
// they will want a category when they come back.
func (s *Store) CountMessagesNeedingCategory(ctx context.Context, userID int64) (int, error) {
	db, err := s.dataDB(ctx, userID)
	if err != nil {
		return 0, err
	}
	predicate, predicateArgs, err := s.inPlayMailScope(ctx, userID, true, " AND m.category = ''")
	if err != nil {
		return 0, err
	}
	args := make([]any, 0, len(predicateArgs)+1)
	args = append(args, userID)
	args = append(args, predicateArgs...)
	var n int
	err = db.QueryRowContext(ctx, `SELECT COUNT(*)
		FROM messages m`+allMailSource+`
		WHERE m.user_id = ?`+predicate, args...).Scan(&n)
	return n, err
}

// MessageCategoryUpdate is one classified message on its way back to SQLite.
// The sender travels with it because the row's normalized address is written at
// the same time, and because a correction for that sender outranks the decision
// the classifier just made.
type MessageCategoryUpdate struct {
	MessageID int64
	FromAddr  string
	Category  string
}

// SetMessageCategories records classified categories for one batch, stamping
// each row with the generation that decided it so the same pass cannot select
// it again. The stored correction for each sender is read inside the same
// transaction that does the write, and wins: reading it beforehand would let a
// correction made while the batch was in flight be overwritten by the
// classifier and then never revisited, because the row is no longer pending
// once it has a category.
//
// Rows are grouped by the answer they get rather than updated one by one. A
// backfill batch is dominated by repeat senders, so a batch of 200 messages is
// usually a handful of statements.
func (s *Store) SetMessageCategories(ctx context.Context, userID int64, updates []MessageCategoryUpdate) error {
	if len(updates) == 0 {
		return nil
	}
	type categoryGroup struct {
		sender   string
		category string
	}
	groups := map[categoryGroup][]int64{}
	for _, update := range updates {
		normalized, err := validCategoryOrError(update.Category)
		if err != nil {
			return err
		}
		key := categoryGroup{sender: NormalizeCategorySender(update.FromAddr), category: normalized}
		groups[key] = append(groups[key], update.MessageID)
	}
	keys := make([]categoryGroup, 0, len(groups))
	for key := range groups {
		keys = append(keys, key)
	}
	// Map order is deliberately not the write order: a stable order keeps the
	// statements a failing batch issued reproducible.
	sort.Slice(keys, func(a, b int) bool {
		if keys[a].sender == keys[b].sender {
			return keys[a].category < keys[b].category
		}
		return keys[a].sender < keys[b].sender
	})
	db, err := s.dataDB(ctx, userID)
	if err != nil {
		return err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	ts := nowUnix()
	for _, key := range keys {
		ids := groups[key]
		args := make([]any, 0, len(ids)+7)
		args = append(args, userID, key.sender, key.category, key.sender, CategoryVersion, ts, userID)
		for _, id := range ids {
			args = append(args, id)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE messages SET
			category = COALESCE((SELECT o.category FROM category_sender_overrides o
				WHERE o.user_id = ? AND o.sender = ?), ?),
			sender_address = ?,
			category_version = ?,
			updated_at = ?
			WHERE user_id = ? AND id IN (`+sqlPlaceholders(len(ids))+`)`, args...); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// SenderCategoryOverride reads the category the user pinned one sender to, if
// any. An empty result means the headers still decide.
func (s *Store) SenderCategoryOverride(ctx context.Context, userID int64, sender string) (string, error) {
	normalized := NormalizeCategorySender(sender)
	if normalized == "" {
		return "", nil
	}
	db, err := s.dataDB(ctx, userID)
	if err != nil {
		return "", err
	}
	var category string
	err = db.QueryRowContext(ctx, `SELECT category FROM category_sender_overrides
		WHERE user_id = ? AND sender = ?`, userID, normalized).Scan(&category)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return category, nil
}

// SetSenderCategoryOverride pins every message from one sender to a category and
// remembers the choice for mail that has not arrived yet. Messages already
// stored are rewritten in the same transaction, because a correction that only
// applied to future mail would leave the list the user is looking at unchanged.
func (s *Store) SetSenderCategoryOverride(ctx context.Context, userID int64, sender, category string) (int64, error) {
	return s.SetSenderCategoryOverrides(ctx, userID, []string{sender}, category)
}

// SetSenderCategoryOverrides pins several senders to one category at once. One
// transaction covers the whole set on purpose: a drop that named five senders
// either files all five or none. Committing them one by one would let a failure
// halfway through leave some senders corrected and some not, with the caller
// reporting a failure it cannot undo and no way for the user to tell which half
// took.
func (s *Store) SetSenderCategoryOverrides(ctx context.Context, userID int64, senders []string, category string) (int64, error) {
	normalizedSenders := make([]string, 0, len(senders))
	seen := make(map[string]struct{}, len(senders))
	for _, sender := range senders {
		normalized := NormalizeCategorySender(sender)
		if normalized == "" {
			return 0, fmt.Errorf("message category override: %q has no address to key on", sender)
		}
		if _, ok := seen[normalized]; ok {
			continue
		}
		seen[normalized] = struct{}{}
		normalizedSenders = append(normalizedSenders, normalized)
	}
	if len(normalizedSenders) == 0 {
		return 0, nil
	}
	normalizedCategory, err := validCategoryOrError(category)
	if err != nil {
		return 0, err
	}
	db, err := s.dataDB(ctx, userID)
	if err != nil {
		return 0, err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	ts := nowUnix()
	for _, sender := range normalizedSenders {
		if _, err := tx.ExecContext(ctx, `INSERT INTO category_sender_overrides (user_id, sender, category, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(user_id, sender) DO UPDATE SET category = excluded.category, updated_at = excluded.updated_at`,
			userID, sender, normalizedCategory, ts, ts); err != nil {
			return 0, err
		}
	}
	// Messages the backfill has not reached yet keep their empty category on
	// purpose: the override is applied when they are classified, and filing them
	// here would take them out of the queue without ever reading them.
	args := make([]any, 0, len(normalizedSenders)+4)
	args = append(args, normalizedCategory, ts, userID, normalizedCategory)
	for _, sender := range normalizedSenders {
		args = append(args, sender)
	}
	res, err := tx.ExecContext(ctx, `UPDATE messages SET category = ?, updated_at = ?
		WHERE user_id = ? AND category <> '' AND category <> ? AND sender_address IN (`+sqlPlaceholders(len(normalizedSenders))+`)`, args...)
	if err != nil {
		return 0, err
	}
	moved, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return moved, nil
}

// ClearSenderCategoryOverride drops a correction and hands the sender back to
// header classification. Messages already filed keep the category they have
// until they are classified again, which only new mail from that sender is.
func (s *Store) ClearSenderCategoryOverride(ctx context.Context, userID int64, sender string) error {
	normalized := NormalizeCategorySender(sender)
	if normalized == "" {
		return nil
	}
	db, err := s.dataDB(ctx, userID)
	if err != nil {
		return err
	}
	_, err = db.ExecContext(ctx, `DELETE FROM category_sender_overrides WHERE user_id = ? AND sender = ?`, userID, normalized)
	return err
}
