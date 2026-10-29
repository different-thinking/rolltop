// File overview: The scan -- the walk over a reader's mail that finds the
// notes-to-self and records the links they carry. From cursor zero it is the
// backfill; from wherever it stopped it is new mail.

package main

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"rolltop/backend/mailparse"
	"rolltop/plugins/recipe_box/recipe"
)

const (
	// scanBatch is how many message rows one page of the walk reads. Only the
	// header columns are read for all of them; bodies are read for the few that
	// pass the address test.
	scanBatch = 500
	// scanPagesPerRound bounds one reader's share of a round, so a reader with
	// a large mailbox being backfilled does not hold everyone else's new mail.
	scanPagesPerRound = 6
	noteLimit         = 280
)

// excludedRoles are folders whose mail the box never reads. Sent and Drafts
// are the reader's own copies of what they wrote -- the delivered copy is the
// one to act on, and moving the Sent copy would empty their Sent folder of
// every recipe they ever shared. Trash and Junk are mail the reader already
// threw away, and All Mail is a view over mail that lives in another folder.
var excludedRoles = map[string]bool{"sent": true, "drafts": true, "trash": true, "junk": true, "all": true}

// mailboxReadable reports whether the scan reads one folder.
func mailboxReadable(name, role string) bool {
	if excludedRoles[strings.ToLower(strings.TrimSpace(role))] {
		return false
	}
	lower := strings.ToLower(strings.TrimSpace(name))
	// Gmail's system folders are labels and views over the real ones: All
	// Mail, Important and Starred hold copies, and its Sent, Drafts, Spam and
	// Trash are covered by their roles above already.
	return !strings.HasPrefix(lower, "[gmail]/") && !strings.HasPrefix(lower, "[google mail]/")
}

type scanRow struct {
	ID              int64
	AccountID       int64
	MailboxID       int64
	MailboxName     string
	MailboxRole     string
	MessageIDHeader string
	Subject         string
	From, To, CC    string
	DateUnix        int64
	Encrypted       bool
}

// scanUser advances one reader's cursor by up to scanPagesPerRound pages and
// reports whether there is more to read.
func scanUser(ctx context.Context, db *sql.DB, userID int64, settings Settings) (int, bool, error) {
	own, err := ownAddresses(ctx, db, userID)
	if err != nil {
		return 0, false, err
	}
	cursor := settings.ScanCursor
	if settings.ScanStartedAt == 0 {
		if _, err := db.ExecContext(ctx, `UPDATE plugin_recipe_box_settings SET scan_started_at = ? WHERE user_id = ?`, time.Now().UTC().Unix(), userID); err != nil {
			return 0, false, err
		}
	}
	found := 0
	for page := 0; page < scanPagesPerRound; page++ {
		rows, err := scanPage(ctx, db, userID, cursor)
		if err != nil {
			return found, false, err
		}
		if len(rows) == 0 {
			if settings.ScanFinishedAt == 0 || page > 0 {
				_, err = db.ExecContext(ctx, `UPDATE plugin_recipe_box_settings SET scan_finished_at = ? WHERE user_id = ?`, time.Now().UTC().Unix(), userID)
			}
			return found, false, err
		}
		var candidates []scanRow
		for _, row := range rows {
			if row.Encrypted || !mailboxReadable(row.MailboxName, row.MailboxRole) {
				continue
			}
			if isNoteToSelf(own, row.From, row.To, row.CC, settings.OwnRecipientsOnly) {
				candidates = append(candidates, row)
			}
		}
		if len(candidates) > 0 {
			n, err := recordCandidates(ctx, db, userID, candidates)
			found += n
			if err != nil {
				return found, false, err
			}
		}
		// The cursor moves only after the page it covers is recorded, so a
		// failure part-way repeats a page rather than skipping one -- and
		// repeating one is harmless, every write below is an upsert.
		cursor = rows[len(rows)-1].ID
		if _, err := db.ExecContext(ctx, `UPDATE plugin_recipe_box_settings SET scan_cursor = ? WHERE user_id = ?`, cursor, userID); err != nil {
			return found, false, err
		}
		if len(rows) < scanBatch {
			_, err = db.ExecContext(ctx, `UPDATE plugin_recipe_box_settings SET scan_finished_at = ? WHERE user_id = ?`, time.Now().UTC().Unix(), userID)
			return found, false, err
		}
	}
	return found, true, nil
}

func scanPage(ctx context.Context, db *sql.DB, userID, cursor int64) ([]scanRow, error) {
	rows, err := db.QueryContext(ctx, `SELECT m.id, m.account_id, m.mailbox_id, mb.name, mb.role, m.message_id_header, m.subject,
			m.from_addr, m.to_addr, m.cc_addr, m.date_unix, m.is_encrypted
		FROM messages m
		JOIN mailboxes mb ON mb.id = m.mailbox_id AND mb.user_id = m.user_id
		WHERE m.user_id = ? AND m.id > ?
		ORDER BY m.id LIMIT ?`, userID, cursor, scanBatch)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []scanRow
	for rows.Next() {
		var row scanRow
		var encrypted int
		if err := rows.Scan(&row.ID, &row.AccountID, &row.MailboxID, &row.MailboxName, &row.MailboxRole, &row.MessageIDHeader, &row.Subject,
			&row.From, &row.To, &row.CC, &row.DateUnix, &encrypted); err != nil {
			return nil, err
		}
		row.Encrypted = encrypted != 0
		out = append(out, row)
	}
	return out, rows.Err()
}

// recordCandidates reads the bodies of the notes-to-self on one page and
// records each one with its links. A note without a link is not recorded at
// all: there is nothing the box could show for it.
func recordCandidates(ctx context.Context, db *sql.DB, userID int64, candidates []scanRow) (int, error) {
	ids := make([]any, 0, len(candidates)+1)
	ids = append(ids, userID)
	placeholders := make([]string, 0, len(candidates))
	for _, row := range candidates {
		ids = append(ids, row.ID)
		placeholders = append(placeholders, "?")
	}
	bodies := map[int64][2]string{}
	rows, err := db.QueryContext(ctx, `SELECT id, body_text, body_html FROM messages WHERE user_id = ? AND id IN (`+strings.Join(placeholders, ",")+`)`, ids...)
	if err != nil {
		return 0, err
	}
	for rows.Next() {
		var id int64
		var text, markup string
		if err := rows.Scan(&id, &text, &markup); err != nil {
			rows.Close()
			return 0, err
		}
		bodies[id] = [2]string{text, markup}
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	recorded := 0
	for _, row := range candidates {
		body := bodies[row.ID]
		links := recipe.ExtractLinks(body[0], body[1])
		if len(links) == 0 {
			continue
		}
		if err := recordMail(ctx, db, userID, row, links, recipe.NoteText(body[0], noteLimit)); err != nil {
			return recorded, err
		}
		recorded++
	}
	return recorded, nil
}

// messageKey identifies one mail across the rows it is stored as. A move
// deletes the row and the destination folder's sync creates a new one, and two
// accounts can hold copies of the same delivery; the Message-ID is what all of
// those share. A message without one is known by its row.
func messageKey(accountID, messageID int64, header string) string {
	if header = strings.TrimSpace(header); header != "" {
		return "mid:" + header
	}
	return fmt.Sprintf("row:%d:%d", accountID, messageID)
}

func recordMail(ctx context.Context, db *sql.DB, userID int64, row scanRow, links []string, note string) error {
	now := time.Now().UTC().Unix()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var mailID int64
	// The row the mail is stored as now is always the newest one the scan
	// reached, so the link back to the mail follows it into its new folder.
	err = tx.QueryRowContext(ctx, `INSERT INTO plugin_recipe_box_mails (user_id, message_key, message_id, message_id_header, account_id, subject, date_unix, note, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (user_id, message_key) DO UPDATE SET message_id = excluded.message_id, account_id = excluded.account_id, updated_at = excluded.updated_at
		RETURNING id`,
		userID, messageKey(row.AccountID, row.ID, row.MessageIDHeader), row.ID, strings.TrimSpace(row.MessageIDHeader), row.AccountID,
		mailparse.SanitizeText(row.Subject), row.DateUnix, mailparse.SanitizeText(note), now, now).Scan(&mailID)
	if err != nil {
		return err
	}
	for position, link := range links {
		var linkID int64
		err := tx.QueryRowContext(ctx, `INSERT INTO plugin_recipe_box_links (user_id, url, created_at, updated_at) VALUES (?, ?, ?, ?)
			ON CONFLICT (user_id, url) DO UPDATE SET url = excluded.url
			RETURNING id`, userID, mailparse.SanitizeText(link), now, now).Scan(&linkID)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO plugin_recipe_box_mail_links (user_id, mail_id, link_id, position) VALUES (?, ?, ?, ?)
			ON CONFLICT (mail_id, link_id) DO NOTHING`, userID, mailID, linkID, position); err != nil {
			return err
		}
	}
	// The earliest mail that named a recipe is when the reader found it; a
	// recipe shared again later keeps its place in the list.
	if row.DateUnix > 0 {
		if err := markFirstSeen(ctx, tx, userID, mailID, row.DateUnix); err != nil {
			return err
		}
	}
	return tx.Commit()
}

type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// markFirstSeen moves the date a recipe was first shared back to dateUnix when
// the mail given names it and is older than what the recipe holds.
func markFirstSeen(ctx context.Context, db execer, userID, mailID, dateUnix int64) error {
	_, err := db.ExecContext(ctx, `UPDATE plugin_recipe_box_recipes r SET first_seen_at = ?
		WHERE r.user_id = ? AND (r.first_seen_at = 0 OR r.first_seen_at > ?)
		AND r.id IN (SELECT l.recipe_id FROM plugin_recipe_box_links l
			JOIN plugin_recipe_box_mail_links ml ON ml.link_id = l.id AND ml.user_id = l.user_id
			WHERE ml.mail_id = ? AND l.user_id = ?)`, dateUnix, userID, dateUnix, mailID, userID)
	return err
}
