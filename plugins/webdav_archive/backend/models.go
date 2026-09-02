// File overview: The plugin's two tables -- configured WebDAV targets and the
// upload queue -- and the SQL that reads and writes them.

package main

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	// The states one queued upload moves through. Everything but queued and
	// uploading is terminal for that attachment.
	statusQueued    = "queued"
	statusUploading = "uploading"
	statusDone      = "done"
	// statusDuplicate is what a second copy of the same bytes becomes: mail
	// with the same recording reaches the folder twice often enough (a resend,
	// a CC filed separately) that uploading it twice would be the surprise.
	statusDuplicate = "duplicate"
	statusFailed    = "failed"
	// statusAbandoned is a failure that has stopped being retried. It is kept
	// visible rather than deleted, because an upload that never happened is
	// exactly what a reader needs to be told about.
	statusAbandoned = "abandoned"
)

// maxAttempts bounds the retry ladder. Ten attempts spread over the backoff
// below is the better part of a day, which covers a server that is down for a
// night without retrying something broken forever.
const maxAttempts = 10

const targetColumns = `id, user_id, name, enabled, base_url, username, encrypted_password,
	watch_mailbox_id, content_types, path_template, include_inline, last_error,
	last_success_at, uploaded_total, created_at, updated_at`

type target struct {
	ID                int64
	UserID            int64
	Name              string
	Enabled           bool
	BaseURL           string
	Username          string
	EncryptedPassword string
	WatchMailboxID    int64
	ContentTypes      string
	PathTemplate      string
	IncludeInline     bool
	LastError         string
	LastSuccessAt     time.Time
	UploadedTotal     int64
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

// contentTypePrefixes reads the target's filter into the list the hook matches
// an attachment's Content-Type against. An empty list means every attachment,
// which is what a target configured with a blank filter asks for.
func (t target) contentTypePrefixes() []string {
	out := make([]string, 0, 4)
	for _, raw := range strings.Split(t.ContentTypes, ",") {
		value := strings.ToLower(strings.TrimSpace(raw))
		if value != "" {
			out = append(out, value)
		}
	}
	return out
}

// matchesContentType reports whether one attachment belongs to this target. A
// filter entry is a prefix, so `audio/` takes every audio format and
// `audio/mpeg` takes only that one.
func (t target) matchesContentType(contentType string) bool {
	prefixes := t.contentTypePrefixes()
	if len(prefixes) == 0 {
		return true
	}
	// The stored Content-Type may carry parameters (`audio/mpeg; name="x.mp3"`),
	// which are not part of what is being matched.
	value := strings.ToLower(strings.TrimSpace(contentType))
	if index := strings.IndexByte(value, ';'); index >= 0 {
		value = strings.TrimSpace(value[:index])
	}
	for _, prefix := range prefixes {
		if strings.HasPrefix(value, prefix) {
			return true
		}
	}
	return false
}

type upload struct {
	ID           int64
	UserID       int64
	TargetID     int64
	MessageID    int64
	AttachmentID int64
	// AttachmentIndex is the part's position in the message's attachment list
	// as it stood when the row was written. It is what pairs a queue row back
	// to a MIME part when the metadata no longer distinguishes the parts.
	AttachmentIndex int
	Filename        string
	ContentType     string
	Size            int64
	RemotePath      string
	ContentHash     string
	Status          string
	Attempts        int
	NextAttemptAt   time.Time
	LastError       string
	Subject         string
	FromAddr        string
	MessageDate     time.Time
	CreatedAt       time.Time
	UpdatedAt       time.Time
	CompletedAt     time.Time
}

const uploadColumns = `id, user_id, target_id, message_id, attachment_id, attachment_index,
	filename, content_type, size, remote_path, content_hash, status, attempts, next_attempt_at,
	last_error, subject, from_addr, message_date, created_at, updated_at, completed_at`

type rowScanner interface {
	Scan(...any) error
}

func unixTime(value int64) time.Time {
	if value <= 0 {
		return time.Time{}
	}
	return time.Unix(value, 0).UTC()
}

func unixSeconds(value time.Time) int64 {
	if value.IsZero() {
		return 0
	}
	return value.UTC().Unix()
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func scanTarget(row rowScanner) (target, error) {
	var out target
	var enabled, inline int
	var success, created, updated int64
	err := row.Scan(&out.ID, &out.UserID, &out.Name, &enabled, &out.BaseURL, &out.Username,
		&out.EncryptedPassword, &out.WatchMailboxID, &out.ContentTypes, &out.PathTemplate,
		&inline, &out.LastError, &success, &out.UploadedTotal, &created, &updated)
	out.Enabled = enabled != 0
	out.IncludeInline = inline != 0
	out.LastSuccessAt = unixTime(success)
	out.CreatedAt = unixTime(created)
	out.UpdatedAt = unixTime(updated)
	return out, err
}

func getTarget(ctx context.Context, db *sql.DB, userID, targetID int64) (target, error) {
	if userID <= 0 || targetID <= 0 {
		return target{}, sql.ErrNoRows
	}
	return scanTarget(db.QueryRowContext(ctx, `SELECT `+targetColumns+`
		FROM plugin_webdav_archive_targets WHERE user_id = ? AND id = ?`, userID, targetID))
}

func listTargets(ctx context.Context, db *sql.DB, userID int64, enabledOnly bool) ([]target, error) {
	query := `SELECT ` + targetColumns + ` FROM plugin_webdav_archive_targets WHERE user_id = ?`
	if enabledOnly {
		query += ` AND enabled = 1`
	}
	query += ` ORDER BY lower(name), id`
	rows, err := db.QueryContext(ctx, query, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]target, 0)
	for rows.Next() {
		item, err := scanTarget(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

// listTargetsWatching returns the enabled targets one newly stored message
// could belong to: those watching its folder, and those watching nothing,
// which means every folder.
func listTargetsWatching(ctx context.Context, db *sql.DB, userID, mailboxID int64) ([]target, error) {
	rows, err := db.QueryContext(ctx, `SELECT `+targetColumns+`
		FROM plugin_webdav_archive_targets
		WHERE user_id = ? AND enabled = 1 AND (watch_mailbox_id = ? OR watch_mailbox_id = 0)
		ORDER BY id`, userID, mailboxID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]target, 0)
	for rows.Next() {
		item, err := scanTarget(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func persistTarget(ctx context.Context, db *sql.DB, item target) (target, error) {
	now := time.Now().UTC().Unix()
	if item.ID > 0 {
		res, err := db.ExecContext(ctx, `UPDATE plugin_webdav_archive_targets
			SET name = ?, enabled = ?, base_url = ?, username = ?, encrypted_password = ?,
				watch_mailbox_id = ?, content_types = ?, path_template = ?, include_inline = ?,
				updated_at = ?
			WHERE user_id = ? AND id = ?`,
			item.Name, boolInt(item.Enabled), item.BaseURL, item.Username, item.EncryptedPassword,
			item.WatchMailboxID, item.ContentTypes, item.PathTemplate, boolInt(item.IncludeInline),
			now, item.UserID, item.ID)
		if err != nil {
			return target{}, err
		}
		if affected, err := res.RowsAffected(); err == nil && affected == 0 {
			return target{}, sql.ErrNoRows
		}
		return getTarget(ctx, db, item.UserID, item.ID)
	}
	var id int64
	err := db.QueryRowContext(ctx, `INSERT INTO plugin_webdav_archive_targets
		(user_id, name, enabled, base_url, username, encrypted_password, watch_mailbox_id,
		 content_types, path_template, include_inline, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) RETURNING id`,
		item.UserID, item.Name, boolInt(item.Enabled), item.BaseURL, item.Username,
		item.EncryptedPassword, item.WatchMailboxID, item.ContentTypes, item.PathTemplate,
		boolInt(item.IncludeInline), now, now).Scan(&id)
	if err != nil {
		return target{}, err
	}
	return getTarget(ctx, db, item.UserID, id)
}

func setTargetEnabled(ctx context.Context, db *sql.DB, userID, targetID int64, enabled bool) error {
	res, err := db.ExecContext(ctx, `UPDATE plugin_webdav_archive_targets
		SET enabled = ?, updated_at = ? WHERE user_id = ? AND id = ?`,
		boolInt(enabled), time.Now().UTC().Unix(), userID, targetID)
	if err != nil {
		return err
	}
	if affected, err := res.RowsAffected(); err == nil && affected == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func deleteTarget(ctx context.Context, db *sql.DB, userID, targetID int64) error {
	res, err := db.ExecContext(ctx, `DELETE FROM plugin_webdav_archive_targets
		WHERE user_id = ? AND id = ?`, userID, targetID)
	if err != nil {
		return err
	}
	if affected, err := res.RowsAffected(); err == nil && affected == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func recordTargetResult(ctx context.Context, db *sql.DB, userID, targetID int64, uploaded bool, failure string) error {
	now := time.Now().UTC().Unix()
	if uploaded {
		_, err := db.ExecContext(ctx, `UPDATE plugin_webdav_archive_targets
			SET uploaded_total = uploaded_total + 1, last_success_at = ?, last_error = '', updated_at = ?
			WHERE user_id = ? AND id = ?`, now, now, userID, targetID)
		return err
	}
	_, err := db.ExecContext(ctx, `UPDATE plugin_webdav_archive_targets
		SET last_error = ?, updated_at = ? WHERE user_id = ? AND id = ?`,
		truncateError(failure), now, userID, targetID)
	return err
}

func scanUpload(row rowScanner) (upload, error) {
	var out upload
	var next, date, created, updated, completed int64
	err := row.Scan(&out.ID, &out.UserID, &out.TargetID, &out.MessageID, &out.AttachmentID,
		&out.AttachmentIndex,
		&out.Filename, &out.ContentType, &out.Size, &out.RemotePath, &out.ContentHash,
		&out.Status, &out.Attempts, &next, &out.LastError, &out.Subject, &out.FromAddr,
		&date, &created, &updated, &completed)
	out.NextAttemptAt = unixTime(next)
	out.MessageDate = unixTime(date)
	out.CreatedAt = unixTime(created)
	out.UpdatedAt = unixTime(updated)
	out.CompletedAt = unixTime(completed)
	return out, err
}

// enqueueUpload records one attachment as owed to a target. The unique key on
// (user, target, message, attachment) is what makes a repeated import -- a
// refetched UID, a second pass over the same folder -- add nothing: the row is
// already there, in whatever state the worker left it.
func enqueueUpload(ctx context.Context, db *sql.DB, item upload) (bool, error) {
	now := time.Now().UTC().Unix()
	var id int64
	err := db.QueryRowContext(ctx, `INSERT INTO plugin_webdav_archive_uploads
		(user_id, target_id, message_id, attachment_id, attachment_index, filename, content_type,
		 size, status, next_attempt_at, subject, from_addr, message_date, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (user_id, target_id, message_id, attachment_id) DO NOTHING
		RETURNING id`,
		item.UserID, item.TargetID, item.MessageID, item.AttachmentID, item.AttachmentIndex,
		item.Filename, item.ContentType, item.Size, statusQueued, now, item.Subject, item.FromAddr,
		unixSeconds(item.MessageDate), now, now).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// claimDueUploads takes the next batch of work for one user and marks it
// uploading in the same statement, so two workers -- or a manual run racing
// the ticker -- cannot both pick up the same row.
func claimDueUploads(ctx context.Context, db *sql.DB, userID int64, now time.Time, limit int) ([]upload, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := db.QueryContext(ctx, `UPDATE plugin_webdav_archive_uploads SET status = ?, updated_at = ?
		WHERE id IN (
			SELECT id FROM plugin_webdav_archive_uploads
			WHERE user_id = ? AND status IN (?, ?) AND next_attempt_at <= ?
			ORDER BY next_attempt_at, id
			LIMIT ?
			FOR UPDATE SKIP LOCKED
		)
		RETURNING `+uploadColumns,
		statusUploading, unixSeconds(now), userID, statusQueued, statusFailed, unixSeconds(now), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]upload, 0, limit)
	for rows.Next() {
		item, err := scanUpload(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

// releaseInterruptedUploads puts rows a stopped process left mid-flight back on
// the queue. Without it a shutdown during an upload would leave the row saying
// `uploading` forever, and nothing would ever pick it up again.
func releaseInterruptedUploads(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `UPDATE plugin_webdav_archive_uploads
		SET status = ?, updated_at = ? WHERE status = ?`,
		statusQueued, time.Now().UTC().Unix(), statusUploading)
	return err
}

// reserveUploadPath records where these bytes are about to be written, before
// the write. It is what makes a second attempt idempotent: the first attempt
// may have uploaded the file and then failed to record that it had, and a retry
// that re-derived the path could pick a different one and leave two copies of
// the same recording on the server. With the path on the row, the retry writes
// over its own earlier attempt.
func reserveUploadPath(ctx context.Context, db *sql.DB, item upload, remotePath, hash string) error {
	_, err := db.ExecContext(ctx, `UPDATE plugin_webdav_archive_uploads
		SET remote_path = ?, content_hash = ?, size = ?, updated_at = ?
		WHERE user_id = ? AND id = ?`,
		remotePath, hash, item.Size, time.Now().UTC().Unix(), item.UserID, item.ID)
	return err
}

func completeUpload(ctx context.Context, db *sql.DB, item upload, status, remotePath, hash string) error {
	now := time.Now().UTC().Unix()
	_, err := db.ExecContext(ctx, `UPDATE plugin_webdav_archive_uploads
		SET status = ?, remote_path = ?, content_hash = ?, size = ?, last_error = '',
			completed_at = ?, updated_at = ?
		WHERE user_id = ? AND id = ?`,
		status, remotePath, hash, item.Size, now, now, item.UserID, item.ID)
	return err
}

// failUpload records one failed attempt and schedules the next. A row that has
// spent its attempts becomes abandoned, which stops the ladder but leaves the
// row and its last error where a reader can see them.
func failUpload(ctx context.Context, db *sql.DB, item upload, cause error, now time.Time) error {
	attempts := item.Attempts + 1
	status := statusFailed
	next := now.Add(retryDelay(attempts))
	if attempts >= maxAttempts {
		status = statusAbandoned
		next = time.Time{}
	}
	_, err := db.ExecContext(ctx, `UPDATE plugin_webdav_archive_uploads
		SET status = ?, attempts = ?, next_attempt_at = ?, last_error = ?, updated_at = ?
		WHERE user_id = ? AND id = ?`,
		status, attempts, unixSeconds(next), truncateError(cause.Error()),
		unixSeconds(now), item.UserID, item.ID)
	return err
}

// retryUpload puts one abandoned or failed row back at the front of the queue,
// which is what the Retry button asks for.
func retryUpload(ctx context.Context, db *sql.DB, userID, uploadID int64) error {
	res, err := db.ExecContext(ctx, `UPDATE plugin_webdav_archive_uploads
		SET status = ?, attempts = 0, next_attempt_at = 0, last_error = '', updated_at = ?
		WHERE user_id = ? AND id = ? AND status IN (?, ?)`,
		statusQueued, time.Now().UTC().Unix(), userID, uploadID, statusFailed, statusAbandoned)
	if err != nil {
		return err
	}
	if affected, err := res.RowsAffected(); err == nil && affected == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// duplicateUploadPath answers with where the same bytes already landed for this
// target, or "" when they have not. It is what turns a resend of the same
// recording into one file on the server rather than two.
func duplicateUploadPath(ctx context.Context, db *sql.DB, userID, targetID int64, hash string, exceptID int64) (string, error) {
	if strings.TrimSpace(hash) == "" {
		return "", nil
	}
	var remotePath string
	err := db.QueryRowContext(ctx, `SELECT remote_path FROM plugin_webdav_archive_uploads
		WHERE user_id = ? AND target_id = ? AND content_hash = ? AND status = ? AND id <> ?
		ORDER BY id LIMIT 1`, userID, targetID, hash, statusDone, exceptID).Scan(&remotePath)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return remotePath, err
}

func listUploads(ctx context.Context, db *sql.DB, userID, targetID int64, status string, limit int) ([]upload, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	query := `SELECT ` + uploadColumns + ` FROM plugin_webdav_archive_uploads WHERE user_id = ?`
	args := []any{userID}
	if targetID > 0 {
		query += ` AND target_id = ?`
		args = append(args, targetID)
	}
	if status != "" {
		query += ` AND status = ?`
		args = append(args, status)
	}
	query += ` ORDER BY id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]upload, 0)
	for rows.Next() {
		item, err := scanUpload(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

// uploadCounts is the per-status tally the settings page shows above the list.
func uploadCounts(ctx context.Context, db *sql.DB, userID int64) (map[string]int64, error) {
	rows, err := db.QueryContext(ctx, `SELECT status, COUNT(*) FROM plugin_webdav_archive_uploads
		WHERE user_id = ? GROUP BY status`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var status string
		var count int64
		if err := rows.Scan(&status, &count); err != nil {
			return nil, err
		}
		out[status] = count
	}
	return out, rows.Err()
}

// usersWithWork lists the tenants the worker has something to do for, so a
// tick costs one query rather than one per account on the install.
func usersWithWork(ctx context.Context, db *sql.DB, now time.Time) ([]int64, error) {
	rows, err := db.QueryContext(ctx, `SELECT DISTINCT user_id FROM plugin_webdav_archive_uploads
		WHERE status IN (?, ?) AND next_attempt_at <= ? ORDER BY user_id`,
		statusQueued, statusFailed, unixSeconds(now))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]int64, 0)
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// retryDelay is the ladder between attempts: a minute, then doubling to an
// hour. A WebDAV server that is down for a night is back on the queue within
// the hour of returning, without a failed upload spinning every minute in
// between.
func retryDelay(attempts int) time.Duration {
	if attempts < 1 {
		attempts = 1
	}
	delay := time.Minute
	for i := 1; i < attempts && delay < time.Hour; i++ {
		delay *= 2
	}
	if delay > time.Hour {
		delay = time.Hour
	}
	return delay
}

// truncateError bounds what is stored from a failure. A server answering with
// a long body has already been reduced to its status line by the client, but a
// dial error carrying a resolver's output can still be long.
//
// Cut by characters, not bytes: a byte slice through a message carrying a
// non-ASCII host or filename splits a character in half, and the invalid UTF-8
// is refused by the `text` column this is on its way into -- turning a failure
// worth reporting into a second failure that loses the first one's reason.
func truncateError(value string) string {
	value = strings.TrimSpace(value)
	const limit = 500
	if utf8.RuneCountInString(value) <= limit {
		return value
	}
	return truncateRunes(value, limit) + "..."
}
