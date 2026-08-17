// File overview: Message category storage: the classification backfill's work
// queue, the per-sender corrections that override it, and the sender
// normalization both sides agree on.

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/mail"
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

// CategoryBackfillLimit bounds one backfill pass. The pass opens a stored
// message per row, so the ceiling is about how long the worker holds its turn,
// not about how much SQLite can return.
const CategoryBackfillLimit = 500

// NormalizeCategorySender reduces a From header to the key a correction is
// remembered under. Display names vary between messages from the same sender and
// address case is not significant, so neither may reach the stored key.
func NormalizeCategorySender(from string) string {
	value := strings.TrimSpace(from)
	if value == "" {
		return ""
	}
	// A From header naming several addresses is degenerate but legal. The first
	// one is taken so the key stays stable, rather than depending on where the
	// scan happened to stop.
	if parsed, err := mail.ParseAddressList(value); err == nil && len(parsed) > 0 {
		value = parsed[0].Address
	} else if start := strings.Index(value, "<"); start >= 0 {
		if end := strings.Index(value[start:], ">"); end > 0 {
			value = value[start+1 : start+end]
		}
	}
	value = strings.TrimSpace(strings.ToLower(value))
	if !strings.Contains(value, "@") || strings.ContainsAny(value, " \t") {
		return ""
	}
	return value
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

// ListMessagesNeedingCategory returns the next batch of unclassified messages.
// Rows are taken oldest-id first so a pass that is interrupted resumes where it
// stopped rather than revisiting the newest mail every time.
func (s *Store) ListMessagesNeedingCategory(ctx context.Context, userID int64, limit int) ([]CategoryCandidate, error) {
	if limit <= 0 || limit > CategoryBackfillLimit {
		limit = CategoryBackfillLimit
	}
	db, err := s.dataDB(ctx, userID)
	if err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx, `SELECT id, from_addr, blob_path
		FROM messages WHERE user_id = ? AND category = '' ORDER BY id LIMIT ?`, userID, limit)
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

// CountMessagesNeedingCategory reports how much of the backfill is left, which
// is what tells the browser its category lists are still filling up.
func (s *Store) CountMessagesNeedingCategory(ctx context.Context, userID int64) (int, error) {
	db, err := s.dataDB(ctx, userID)
	if err != nil {
		return 0, err
	}
	var n int
	err = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM messages WHERE user_id = ? AND category = ''`, userID).Scan(&n)
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

// SetMessageCategories records classified categories for one batch. Corrections
// the user already made are applied here rather than left for the caller to
// remember, so every path that files a message honours them the same way.
func (s *Store) SetMessageCategories(ctx context.Context, userID int64, updates []MessageCategoryUpdate) error {
	if len(updates) == 0 {
		return nil
	}
	senders := make([]string, 0, len(updates))
	for _, update := range updates {
		senders = append(senders, update.FromAddr)
	}
	overrides, err := s.SenderCategoryOverridesFor(ctx, userID, senders)
	if err != nil {
		return err
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
	ts := nowUnix()
	for _, update := range updates {
		sender := NormalizeCategorySender(update.FromAddr)
		category := update.Category
		if pinned, ok := overrides[sender]; ok {
			category = pinned
		}
		normalized, err := validCategoryOrError(category)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE messages SET category = ?, sender_address = ?, updated_at = ?
			WHERE user_id = ? AND id = ?`, normalized, sender, ts, userID, update.MessageID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// SenderCategoryOverridesFor looks up the corrections covering a batch of
// senders in one query. Classification consults this before writing, so a
// sender the user has already moved keeps landing where they put it.
func (s *Store) SenderCategoryOverridesFor(ctx context.Context, userID int64, senders []string) (map[string]string, error) {
	unique := make([]string, 0, len(senders))
	seen := make(map[string]bool, len(senders))
	for _, sender := range senders {
		normalized := NormalizeCategorySender(sender)
		if normalized == "" || seen[normalized] {
			continue
		}
		seen[normalized] = true
		unique = append(unique, normalized)
	}
	out := map[string]string{}
	if len(unique) == 0 {
		return out, nil
	}
	db, err := s.dataDB(ctx, userID)
	if err != nil {
		return nil, err
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
		for i, sender := range chunk {
			placeholders[i] = "?"
			args = append(args, sender)
		}
		rows, err := db.QueryContext(ctx, `SELECT sender, category FROM category_sender_overrides
			WHERE user_id = ? AND sender IN (`+strings.Join(placeholders, ",")+`)`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var sender, category string
			if err := rows.Scan(&sender, &category); err != nil {
				rows.Close()
				return nil, err
			}
			out[sender] = category
		}
		err = errors.Join(rows.Err(), rows.Close())
		if err != nil {
			return nil, err
		}
	}
	return out, nil
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
	normalizedSender := NormalizeCategorySender(sender)
	if normalizedSender == "" {
		return 0, fmt.Errorf("message category override: %q has no address to key on", sender)
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
	if _, err := tx.ExecContext(ctx, `INSERT INTO category_sender_overrides (user_id, sender, category, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(user_id, sender) DO UPDATE SET category = excluded.category, updated_at = excluded.updated_at`,
		userID, normalizedSender, normalizedCategory, ts, ts); err != nil {
		return 0, err
	}
	// Messages the backfill has not reached yet keep their empty category on
	// purpose: the override is applied when they are classified, and filing them
	// here would take them out of the queue without ever reading them.
	res, err := tx.ExecContext(ctx, `UPDATE messages SET category = ?, updated_at = ?
		WHERE user_id = ? AND category <> '' AND category <> ? AND sender_address = ?`,
		normalizedCategory, ts, userID, normalizedCategory, normalizedSender)
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
