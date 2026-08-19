// File overview: The SQL side of full-text search on PostgreSQL. One row per
// indexed message in message_search (migration 0001), carrying a weighted
// tsvector built here from the four bounded text streams the search package
// prepares. Everything volatile — mailbox, read/star flags, dates — stays in
// messages and is joined at query time, so a flag change never needs an index
// write and a moved message never leaves a stale copy behind. Deleting a
// message row deletes its search row through the foreign key; the explicit
// deletes below exist for the paths that prune search rows while the message
// stays (search visibility toggles, purges).

package store

import (
	"context"
	"fmt"
	"strings"
)

// MessageSearchDoc is one message's search payload, already bounded by the
// search package's document limits. The four texts become one tsvector with
// weights A-D: subject, addresses, body, attachments — the same precedence the
// Bleve field boosts encode.
type MessageSearchDoc struct {
	MessageID int64
	UserID    int64
	TextA     string
	TextB     string
	TextC     string
	TextD     string
	// Words is the distinct normalized word list of the four streams, the
	// haystack pg_trgm word similarity probes for fuzzy matching. Empty when
	// the row predates the words column; a re-index fills it.
	Words string
}

// messageSearchVectorSQL renders one row's tsvector. The 'simple'
// configuration is deliberate: no stemming, matching Bleve's standard
// analyzer, with German compound handling done app-side in the text streams
// exactly as before (docs/search-postgres-plan.md §3).
const messageSearchVectorSQL = `setweight(to_tsvector('simple', ?), 'A') || setweight(to_tsvector('simple', ?), 'B') || setweight(to_tsvector('simple', ?), 'C') || setweight(to_tsvector('simple', ?), 'D')`

// UpsertMessageSearch writes one tenant's batch of search rows. Re-indexing an
// existing message replaces its vector, which is what the repair and
// attachment-enrichment paths need.
func (s *Store) UpsertMessageSearch(ctx context.Context, userID int64, docs []MessageSearchDoc) error {
	if userID <= 0 {
		return fmt.Errorf("user id must be positive")
	}
	if len(docs) == 0 {
		return nil
	}
	db, err := s.dataDB(ctx, userID)
	if err != nil {
		return err
	}
	var values strings.Builder
	args := make([]any, 0, len(docs)*6)
	for i, doc := range docs {
		if doc.UserID != userID {
			return fmt.Errorf("message search doc for user %d in a batch for user %d", doc.UserID, userID)
		}
		if doc.MessageID <= 0 {
			return fmt.Errorf("message search doc without a message id")
		}
		if i > 0 {
			values.WriteString(", ")
		}
		values.WriteString("(?, ?, " + messageSearchVectorSQL + ", ?)")
		args = append(args, doc.MessageID, doc.UserID, doc.TextA, doc.TextB, doc.TextC, doc.TextD, doc.Words)
	}
	_, err = db.ExecContext(ctx, `INSERT INTO message_search (message_id, user_id, tsv, words) VALUES `+values.String()+`
		ON CONFLICT (message_id) DO UPDATE SET user_id = EXCLUDED.user_id, tsv = EXCLUDED.tsv, words = EXCLUDED.words`, args...)
	return err
}

// DeleteMessageSearch removes search rows by message id and reports how many
// existed. Chunked like every other id-list query in this package.
func (s *Store) DeleteMessageSearch(ctx context.Context, userID int64, messageIDs []int64) (int64, error) {
	if userID <= 0 {
		return 0, fmt.Errorf("user id must be positive")
	}
	db, err := s.dataDB(ctx, userID)
	if err != nil {
		return 0, err
	}
	var total int64
	for start := 0; start < len(messageIDs); start += 500 {
		end := min(start+500, len(messageIDs))
		chunk := messageIDs[start:end]
		args := make([]any, 0, len(chunk)+1)
		args = append(args, userID)
		for _, id := range chunk {
			args = append(args, id)
		}
		result, err := db.ExecContext(ctx, `DELETE FROM message_search
			WHERE user_id = ? AND message_id IN (`+sqlPlaceholders(len(chunk))+`)`, args...)
		if err != nil {
			return total, err
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return total, err
		}
		total += affected
	}
	return total, nil
}

// PurgeMessageSearchForMailbox removes every search row whose message currently
// lives in the given mailbox — the search-visibility toggle path.
func (s *Store) PurgeMessageSearchForMailbox(ctx context.Context, userID, mailboxID int64) (int64, error) {
	if userID <= 0 {
		return 0, fmt.Errorf("user id must be positive")
	}
	db, err := s.dataDB(ctx, userID)
	if err != nil {
		return 0, err
	}
	result, err := db.ExecContext(ctx, `DELETE FROM message_search
		WHERE user_id = ? AND message_id IN (SELECT id FROM messages WHERE user_id = ? AND mailbox_id = ?)`,
		userID, userID, mailboxID)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

// CountMessageSearchForUser reports one tenant's indexed row count.
func (s *Store) CountMessageSearchForUser(ctx context.Context, userID int64) (int, error) {
	if userID <= 0 {
		return 0, fmt.Errorf("user id must be positive")
	}
	db, err := s.dataDB(ctx, userID)
	if err != nil {
		return 0, err
	}
	var count int
	err = db.QueryRowContext(ctx, `SELECT count(*) FROM message_search WHERE user_id = ?`, userID).Scan(&count)
	return count, err
}

// CountMessageSearchForMailbox counts the indexed messages currently in one
// mailbox. The join means the answer follows moves, which is what the repair
// coverage comparison against the local row count needs.
func (s *Store) CountMessageSearchForMailbox(ctx context.Context, userID, mailboxID int64) (int, error) {
	if userID <= 0 {
		return 0, fmt.Errorf("user id must be positive")
	}
	db, err := s.dataDB(ctx, userID)
	if err != nil {
		return 0, err
	}
	var count int
	err = db.QueryRowContext(ctx, `SELECT count(*) FROM message_search ms
		JOIN messages m ON m.id = ms.message_id
		WHERE ms.user_id = ? AND m.mailbox_id = ?`, userID, mailboxID).Scan(&count)
	return count, err
}

// MessageSearchPresence reports which of the given messages have a search row.
func (s *Store) MessageSearchPresence(ctx context.Context, userID int64, messageIDs []int64) (map[int64]bool, error) {
	if userID <= 0 {
		return nil, fmt.Errorf("user id must be positive")
	}
	db, err := s.dataDB(ctx, userID)
	if err != nil {
		return nil, err
	}
	present := make(map[int64]bool, len(messageIDs))
	for start := 0; start < len(messageIDs); start += 500 {
		end := min(start+500, len(messageIDs))
		chunk := messageIDs[start:end]
		args := make([]any, 0, len(chunk)+1)
		args = append(args, userID)
		for _, id := range chunk {
			args = append(args, id)
		}
		rows, err := db.QueryContext(ctx, `SELECT message_id FROM message_search
			WHERE user_id = ? AND message_id IN (`+sqlPlaceholders(len(chunk))+`)`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return nil, err
			}
			present[id] = true
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}
	return present, nil
}

// MessageSearchMailboxIDs lists the indexed message ids currently in one mailbox.
func (s *Store) MessageSearchMailboxIDs(ctx context.Context, userID, mailboxID int64) (map[int64]bool, error) {
	if userID <= 0 {
		return nil, fmt.Errorf("user id must be positive")
	}
	db, err := s.dataDB(ctx, userID)
	if err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx, `SELECT ms.message_id FROM message_search ms
		JOIN messages m ON m.id = ms.message_id
		WHERE ms.user_id = ? AND m.mailbox_id = ?`, userID, mailboxID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := make(map[int64]bool)
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids[id] = true
	}
	return ids, rows.Err()
}

// DropMessageSearchForUser removes one tenant's search rows entirely.
func (s *Store) DropMessageSearchForUser(ctx context.Context, userID int64) error {
	if userID <= 0 {
		return fmt.Errorf("user id must be positive")
	}
	db, err := s.dataDB(ctx, userID)
	if err != nil {
		return err
	}
	_, err = db.ExecContext(ctx, `DELETE FROM message_search WHERE user_id = ?`, userID)
	return err
}

// MessageSearchBytes reports one tenant's stored vector bytes, the admin
// page's per-tenant footprint number in Postgres mode.
func (s *Store) MessageSearchBytes(ctx context.Context, userID int64) (int64, error) {
	if userID <= 0 {
		return 0, fmt.Errorf("user id must be positive")
	}
	db, err := s.dataDB(ctx, userID)
	if err != nil {
		return 0, err
	}
	var bytes int64
	err = db.QueryRowContext(ctx, `SELECT coalesce(sum(pg_column_size(tsv)), 0) FROM message_search WHERE user_id = ?`, userID).Scan(&bytes)
	return bytes, err
}

// EnsureTrigramSearch makes fuzzy search available when the server allows it:
// the pg_trgm extension and a trigram index over the word lists. Both are
// deliberately not migrations — CREATE EXTENSION needs privileges a managed
// database may withhold, and search has to degrade to exact matching then
// rather than refuse to start. Call once at startup on the postgres backend;
// TrigramSearchEnabled answers for the query path afterwards.
//
// The returned error describes why fuzzy is unavailable and is for the log:
// the caller keeps serving either way.
func (s *Store) EnsureTrigramSearch(ctx context.Context) error {
	// IF NOT EXISTS still needs privileges when the extension is absent, so
	// probe first and only create when it is genuinely missing.
	var installed bool
	if err := s.db.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_extension WHERE extname = 'pg_trgm')`).Scan(&installed); err != nil {
		return postgresError("probe pg_trgm", err)
	}
	if !installed {
		if _, err := s.db.ExecContext(ctx, `CREATE EXTENSION pg_trgm`); err != nil {
			return postgresError("create the pg_trgm extension (fuzzy search stays off)", err)
		}
	}
	// Idempotent, and cheap while the table is small; on an already filled
	// table this is a one-time index build at startup.
	if _, err := s.db.ExecContext(ctx,
		`CREATE INDEX IF NOT EXISTS idx_message_search_words_trgm ON message_search USING GIN (words gin_trgm_ops)`); err != nil {
		return postgresError("create the trigram index (fuzzy search stays off)", err)
	}
	s.trigramSearch.Store(true)
	return nil
}

// TrigramSearchEnabled reports whether EnsureTrigramSearch succeeded, which is
// what lets the query side offer fuzzy matching.
func (s *Store) TrigramSearchEnabled() bool {
	return s.trigramSearch.Load()
}
