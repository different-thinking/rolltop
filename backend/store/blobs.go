// File overview: Database records for user-scoped blob metadata.

package store

import (
	"context"
	"database/sql"
	"strings"
)

// CreateBlob records blob metadata in the user database after the file has been written to the user blob directory.
func (s *Store) CreateBlob(ctx context.Context, b BlobRecord) (BlobRecord, error) {
	ts := nowUnix()
	_, err := s.mustDataDB(ctx, b.UserID).ExecContext(ctx, `INSERT INTO blobs (user_id, kind, path, sha256, size, created_at) VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(user_id, path) DO UPDATE SET
			kind = excluded.kind,
			sha256 = excluded.sha256,
			size = excluded.size,
			created_at = excluded.created_at`,
		b.UserID, b.Kind, b.Path, b.SHA256, b.Size, ts)
	if err != nil {
		return BlobRecord{}, err
	}
	return s.GetBlobByPathForUser(ctx, b.UserID, b.Path)
}

// GetBlobForUser loads blob metadata by ID only when it belongs to the requested user.
func (s *Store) GetBlobForUser(ctx context.Context, userID, id int64) (BlobRecord, error) {
	var b BlobRecord
	var created int64
	err := s.mustDataDB(ctx, userID).QueryRowContext(ctx, `SELECT id, user_id, kind, path, sha256, size, created_at FROM blobs WHERE user_id = ? AND id = ?`, userID, id).
		Scan(&b.ID, &b.UserID, &b.Kind, &b.Path, &b.SHA256, &b.Size, &created)
	b.CreatedAt = unixTime(created)
	return b, err
}

// GetBlobByPathForUser loads blob metadata by path only inside the requested user scope.
func (s *Store) GetBlobByPathForUser(ctx context.Context, userID int64, blobPath string) (BlobRecord, error) {
	var b BlobRecord
	var created int64
	err := s.mustDataDB(ctx, userID).QueryRowContext(ctx, `SELECT id, user_id, kind, path, sha256, size, created_at FROM blobs WHERE user_id = ? AND path = ?`, userID, blobPath).
		Scan(&b.ID, &b.UserID, &b.Kind, &b.Path, &b.SHA256, &b.Size, &created)
	b.CreatedAt = unixTime(created)
	return b, err
}

// DeleteBlobsForUser removes multiple blob metadata rows for one user in one transaction.
func (s *Store) DeleteBlobsForUser(ctx context.Context, userID int64, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	tx, err := s.mustDataDB(ctx, userID).BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	stmt, err := tx.PrepareContext(ctx, `DELETE FROM blobs WHERE user_id = ? AND id = ?`)
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	for _, id := range ids {
		if id <= 0 {
			continue
		}
		if _, err := stmt.ExecContext(ctx, userID, id); err != nil {
			_ = stmt.Close()
			_ = tx.Rollback()
			return err
		}
	}
	if err := stmt.Close(); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// DeleteBlobForUser removes blob metadata for one user; filesystem deletion is handled by the blob store.
func (s *Store) DeleteBlobForUser(ctx context.Context, userID, id int64) error {
	res, err := s.mustDataDB(ctx, userID).ExecContext(ctx, `DELETE FROM blobs WHERE user_id = ? AND id = ?`, userID, id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteBlobIfUnreferencedForUser removes blob metadata only when no tenant-owned
// row still uses it. Callers may delete the corresponding filesystem path only
// after this returns deleted=true.
func (s *Store) DeleteBlobIfUnreferencedForUser(ctx context.Context, userID, id int64) (deleted bool, err error) {
	if userID <= 0 || id <= 0 {
		return false, nil
	}
	res, err := s.mustDataDB(ctx, userID).ExecContext(ctx, `DELETE FROM blobs
		WHERE user_id = ? AND id = ?
			AND NOT EXISTS (
				SELECT 1 FROM messages
				WHERE messages.user_id = blobs.user_id AND messages.blob_id = blobs.id
			)
			AND NOT EXISTS (
				SELECT 1 FROM attachments
				WHERE attachments.user_id = blobs.user_id AND attachments.blob_id = blobs.id
			)
			AND NOT EXISTS (
				SELECT 1 FROM contact_icons
				WHERE contact_icons.user_id = blobs.user_id AND contact_icons.blob_id = blobs.id
			)
			AND NOT EXISTS (
				SELECT 1 FROM remote_image_cache
				WHERE remote_image_cache.user_id = blobs.user_id AND remote_image_cache.blob_id = blobs.id
			)`, userID, id)
	if err != nil {
		return false, err
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return rows == 1, nil
}

// CreateAttachment records attachment metadata for a stored message.
func (s *Store) CreateAttachment(ctx context.Context, a Attachment) (Attachment, error) {
	ts := nowUnix()
	var id int64
	err := s.mustDataDB(ctx, a.UserID).QueryRowContext(ctx, `INSERT INTO attachments (user_id, message_id, blob_id, filename, content_type, content_id, is_inline, size, blob_path, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		RETURNING id`, a.UserID, a.MessageID, a.BlobID, a.Filename, a.ContentType, a.ContentID, boolInt(a.IsInline), a.Size, a.BlobPath, ts).Scan(&id)
	if err != nil {
		return Attachment{}, err
	}
	return s.GetAttachmentForUser(ctx, a.UserID, id)
}

// GetAttachmentForUser loads one attachment through its message ownership boundary.
func (s *Store) GetAttachmentForUser(ctx context.Context, userID, id int64) (Attachment, error) {
	var a Attachment
	var created int64
	var isInline int
	err := s.mustDataDB(ctx, userID).QueryRowContext(ctx, `SELECT id, user_id, message_id, blob_id, filename, content_type, content_id, is_inline, size, blob_path, created_at
		FROM attachments WHERE user_id = ? AND id = ?`, userID, id).
		Scan(&a.ID, &a.UserID, &a.MessageID, &a.BlobID, &a.Filename, &a.ContentType, &a.ContentID, &isInline, &a.Size, &a.BlobPath, &created)
	a.IsInline = isInline != 0
	a.CreatedAt = unixTime(created)
	return a, err
}

// ListAttachmentsForMessage returns attachment metadata for a user-owned message.
func (s *Store) ListAttachmentsForMessage(ctx context.Context, userID, messageID int64) ([]Attachment, error) {
	rows, err := s.mustDataDB(ctx, userID).QueryContext(ctx, attachmentsForMessageSelectSQL, userID, messageID)
	if err != nil {
		return nil, err
	}
	return scanAttachmentRows(rows)
}

const attachmentsForMessageSelectSQL = `SELECT id, user_id, message_id, blob_id, filename, content_type, content_id, is_inline, size, blob_path, created_at
	FROM attachments WHERE user_id = ? AND message_id = ? ORDER BY id`

// listAttachmentsForMessageForUpdate reads a message's attachment rows inside a
// transaction and locks them with FOR UPDATE, so a reparse can match, update,
// and delete against a consistent set instead of a snapshot a concurrent writer
// may have changed between the read and the writes.
func listAttachmentsForMessageForUpdate(ctx context.Context, tx *sql.Tx, userID, messageID int64) ([]Attachment, error) {
	rows, err := tx.QueryContext(ctx, attachmentsForMessageSelectSQL+` FOR UPDATE`, userID, messageID)
	if err != nil {
		return nil, err
	}
	return scanAttachmentRows(rows)
}

func scanAttachmentRows(rows *sql.Rows) ([]Attachment, error) {
	defer rows.Close()
	var out []Attachment
	for rows.Next() {
		var a Attachment
		var created int64
		var isInline int
		if err := rows.Scan(&a.ID, &a.UserID, &a.MessageID, &a.BlobID, &a.Filename, &a.ContentType, &a.ContentID, &isInline, &a.Size, &a.BlobPath, &created); err != nil {
			return nil, err
		}
		a.IsInline = isInline != 0
		a.CreatedAt = unixTime(created)
		out = append(out, a)
	}
	return out, rows.Err()
}

// ListAttachmentsForMessages returns attachment metadata grouped by message ID.
func (s *Store) ListAttachmentsForMessages(ctx context.Context, userID int64, messageIDs []int64) (map[int64][]Attachment, error) {
	ids := make([]int64, 0, len(messageIDs))
	seen := map[int64]bool{}
	for _, id := range messageIDs {
		if id <= 0 || seen[id] {
			continue
		}
		seen[id] = true
		ids = append(ids, id)
	}
	out := map[int64][]Attachment{}
	if userID <= 0 || len(ids) == 0 {
		return out, nil
	}
	for start := 0; start < len(ids); start += 500 {
		end := start + 500
		if end > len(ids) {
			end = len(ids)
		}
		chunk := ids[start:end]
		args := make([]any, 0, len(chunk)+1)
		args = append(args, userID)
		for _, id := range chunk {
			args = append(args, id)
		}
		rows, err := s.mustDataDB(ctx, userID).QueryContext(ctx, `SELECT id, user_id, message_id, blob_id, filename, content_type, content_id, is_inline, size, blob_path, created_at
			FROM attachments WHERE user_id = ? AND message_id IN (`+sqlPlaceholders(len(chunk))+`) ORDER BY message_id, id`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var a Attachment
			var created int64
			var isInline int
			if err := rows.Scan(&a.ID, &a.UserID, &a.MessageID, &a.BlobID, &a.Filename, &a.ContentType, &a.ContentID, &isInline, &a.Size, &a.BlobPath, &created); err != nil {
				_ = rows.Close()
				return nil, err
			}
			a.IsInline = isInline != 0
			a.CreatedAt = unixTime(created)
			out[a.MessageID] = append(out[a.MessageID], a)
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// ReplaceAttachmentsForMessage rewrites the attachment rows of one message while
// keeping the row IDs that are already in use. Reindexing reparses the very same
// MIME parts, so deleting the rows and inserting them again handed every
// attachment a fresh ID: an open mail view still holding the old ones then got a
// 404 from /attachments/<id>/download and from the preview route, for a message
// nothing about had changed. Rows are matched to files by position — the same
// raw message parses to the same parts in the same order — so only a message
// that really gained or lost a part inserts or deletes anything.
//
// Position alone is not enough for that, because a parse can gain a part that
// belongs in front of the ones already stored: mailparse learning to keep an
// inline image that carries a Content-ID but no filename turns one row for an
// invoice into a picture in first position and an invoice in second, and every
// URL an open view holds for that invoice then serves the picture - worse than
// the 404 above, because it answers. So each part is first matched to the row
// that already holds it (by Content-ID, then by filename and size), and only
// what is left over is paired in order, which is what keeps a row whose
// metadata a better parse genuinely corrected on the ID it already had.
//
// An empty files slice removes every row; callers that reparse a message keep
// what they have instead of calling with one, because a parse that yields no
// files is the failure case, not an attachment-free message.
// matchAttachmentRows pairs each parsed part with the index of the existing
// row that already holds it, or -1 for a part no row holds yet. A part is
// recognised by its Content-ID first - the one identifier a message body refers
// to a part by - then by filename with size and content-type, and whatever is
// still unpaired is matched in order, so a reparse that only corrected metadata
// keeps every row where it was.
func matchAttachmentRows(files, existing []Attachment) []int {
	rowForFile := make([]int, len(files))
	for i := range rowForFile {
		rowForFile[i] = -1
	}
	used := make([]bool, len(existing))
	match := func(fits func(file, row Attachment) bool) {
		for i, file := range files {
			if rowForFile[i] >= 0 {
				continue
			}
			for j, row := range existing {
				if used[j] || !fits(file, row) {
					continue
				}
				rowForFile[i] = j
				used[j] = true
				break
			}
		}
	}
	match(func(file, row Attachment) bool {
		id := strings.TrimSpace(strings.Trim(file.ContentID, "<>"))
		return id != "" && strings.EqualFold(strings.TrimSpace(strings.Trim(row.ContentID, "<>")), id)
	})
	// Filename plus size plus content-type is an exact metadata match; then
	// filename plus size, which still pins the bytes because size is the strong
	// content discriminator. A filename-only pass used to follow, but matching by
	// name alone reaches across positions and could pair two same-named parts of
	// different sizes -- handing one part's stable id the other's blob, so a later
	// download returned the wrong file. Whatever these exact passes leave unpaired
	// falls to the positional pass below, which keeps a part in its place instead
	// of guessing by name.
	match(func(file, row Attachment) bool {
		name := strings.TrimSpace(file.Filename)
		return name != "" && strings.EqualFold(strings.TrimSpace(row.Filename), name) && row.Size == file.Size &&
			strings.EqualFold(strings.TrimSpace(row.ContentType), strings.TrimSpace(file.ContentType))
	})
	match(func(file, row Attachment) bool {
		name := strings.TrimSpace(file.Filename)
		return name != "" && strings.EqualFold(strings.TrimSpace(row.Filename), name) && row.Size == file.Size
	})
	// Whatever is left is paired in order. A part the parse describes
	// differently than the row does - a filename a better parse decoded, a size
	// that changed with it - is the same part in the same place, and pairing it
	// keeps its ID rather than trading the row in for a new one.
	next := 0
	for i := range files {
		if rowForFile[i] >= 0 {
			continue
		}
		for next < len(existing) && used[next] {
			next++
		}
		if next >= len(existing) {
			break
		}
		rowForFile[i] = next
		used[next] = true
	}
	return rowForFile
}

func (s *Store) ReplaceAttachmentsForMessage(ctx context.Context, userID, messageID int64, files []Attachment) ([]Attachment, error) {
	// Most mail carries no attachments at all, and every stored and reindexed
	// message now comes through here, so a cheap unlocked probe answers the case
	// with nothing on either side without opening a transaction. It is only a
	// short-circuit: when there is work to do, the authoritative view is re-read
	// inside the transaction under FOR UPDATE below.
	existing, err := s.ListAttachmentsForMessage(ctx, userID, messageID)
	if err != nil {
		return nil, err
	}
	if len(existing) == 0 && len(files) == 0 {
		return nil, nil
	}
	ts := nowUnix()
	tx, err := s.mustDataDB(ctx, userID).BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	// Re-read the rows under the transaction and lock them, so the match, update,
	// and delete below act on a consistent set rather than the snapshot the
	// unlocked probe saw -- which a concurrent writer could have changed.
	existing, err = listAttachmentsForMessageForUpdate(ctx, tx, userID, messageID)
	if err != nil {
		return nil, err
	}
	if len(existing) == 0 && len(files) == 0 {
		return nil, nil
	}
	rowForFile := matchAttachmentRows(files, existing)
	taken := make(map[int]bool, len(existing))
	for i, file := range files {
		if row := rowForFile[i]; row >= 0 {
			taken[row] = true
			if _, err := tx.ExecContext(ctx, `UPDATE attachments
				SET blob_id = ?, filename = ?, content_type = ?, content_id = ?, is_inline = ?, size = ?, blob_path = ?
				WHERE user_id = ? AND id = ?`,
				file.BlobID, file.Filename, file.ContentType, file.ContentID, boolInt(file.IsInline), file.Size, file.BlobPath,
				userID, existing[row].ID); err != nil {
				return nil, err
			}
			continue
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO attachments (user_id, message_id, blob_id, filename, content_type, content_id, is_inline, size, blob_path, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			userID, messageID, file.BlobID, file.Filename, file.ContentType, file.ContentID, boolInt(file.IsInline), file.Size, file.BlobPath, ts); err != nil {
			return nil, err
		}
	}
	for i, surplus := range existing {
		if taken[i] {
			continue
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM attachments WHERE user_id = ? AND id = ?`, userID, surplus.ID); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return s.ListAttachmentsForMessage(ctx, userID, messageID)
}
