// File overview: Filing the mail behind a recipe into the account's recipe
// folder.
//
// A mail is filed once. Its state ends at "moved" the first time it reaches
// the folder, and nothing moves it again after that: a reader who drags it back
// into the Inbox has answered for that mail, and a box that kept refiling it
// would be fighting them. The move is the host's own -- the same generation-
// proved IMAP MOVE every other move is -- so the mail is out of the Inbox in
// every client, not only in Rolltop.

package main

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"rolltop/backend/plugins"
	"rolltop/backend/store"
)

// moveRetryDelays is the wait after each failed move.
var moveRetryDelays = []time.Duration{5 * time.Minute, 30 * time.Minute, 2 * time.Hour, 12 * time.Hour}

const (
	movesPerRound    = 25
	maxMoveAttempts  = 5
	moveStateMoved   = "moved"
	moveStateFailed  = "failed"
	moveStateSkipped = "skipped"
)

type pendingMail struct {
	ID         int64
	Key        string
	Message    int64
	Header     string
	ContentSHA string
	AccountID  int64
	Attempts   int
}

// mailsToFile lists the mails that carried a recipe the reader has not
// dismissed and that are not filed yet.
func mailsToFile(ctx context.Context, db *sql.DB, userID int64, limit int) ([]pendingMail, error) {
	rows, err := db.QueryContext(ctx, `SELECT m.id, m.message_key, m.message_id, m.message_id_header, m.content_sha, m.account_id, m.move_attempts
		FROM plugin_recipe_box_mails m
		WHERE m.user_id = ? AND m.move_state = '' AND m.move_next_at <= ?
		AND EXISTS (
			SELECT 1 FROM plugin_recipe_box_mail_links ml
			JOIN plugin_recipe_box_links l ON l.id = ml.link_id AND l.user_id = ml.user_id
			JOIN plugin_recipe_box_recipes r ON r.id = l.recipe_id AND r.user_id = l.user_id
			WHERE ml.mail_id = m.id AND ml.user_id = m.user_id AND l.status = 'recipe' AND r.dismissed = 0
		)
		ORDER BY m.id LIMIT ?`, userID, time.Now().UTC().Unix(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []pendingMail
	for rows.Next() {
		var mail pendingMail
		if err := rows.Scan(&mail.ID, &mail.Key, &mail.Message, &mail.Header, &mail.ContentSHA, &mail.AccountID, &mail.Attempts); err != nil {
			return nil, err
		}
		out = append(out, mail)
	}
	return out, rows.Err()
}

type storedCopy struct {
	ID          int64
	AccountID   int64
	MailboxName string
	MailboxRole string
}

// storedCopies is every row the mail is stored as right now. A mail two of the
// reader's accounts received is two rows, and each is filed in its own
// account, because a move cannot cross accounts and a copy left in the other
// Inbox is the mail still sitting there.
func storedCopies(ctx context.Context, db *sql.DB, userID int64, mail pendingMail) ([]storedCopy, error) {
	query := `SELECT m.id, m.account_id, mb.name, mb.role FROM messages m
		JOIN mailboxes mb ON mb.id = m.mailbox_id AND mb.user_id = m.user_id
		WHERE m.user_id = ? AND `
	args := []any{userID}
	switch {
	case strings.HasPrefix(mail.Key, "mid:") && mail.Header != "":
		query += `m.message_id_header = ?`
		args = append(args, mail.Header)
	case strings.HasPrefix(mail.Key, "sha:") && mail.ContentSHA != "":
		// A mail without a Message-ID is followed by its content inside its
		// own account: the same bytes in another account are a delivery there
		// that this note-to-self does not speak for.
		query += `m.account_id = ? AND m.canonical_sha256 = ?`
		args = append(args, mail.AccountID, mail.ContentSHA)
	default:
		query += `m.id = ?`
		args = append(args, mail.Message)
	}
	rows, err := db.QueryContext(ctx, query+` ORDER BY m.id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []storedCopy
	for rows.Next() {
		var c storedCopy
		if err := rows.Scan(&c.ID, &c.AccountID, &c.MailboxName, &c.MailboxRole); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// fileMails moves up to movesPerRound mails and reports whether more wait.
func fileMails(ctx context.Context, host plugins.BackendHost, db *sql.DB, userID int64, settings Settings) (int, bool, error) {
	mover, canMove := host.(plugins.StoredMessageHost)
	folders, canCreate := host.(plugins.MailFolderHost)
	if !settings.MoveEnabled || !canMove || !canCreate {
		return 0, false, nil
	}
	mails, err := mailsToFile(ctx, db, userID, movesPerRound+1)
	if err != nil {
		return 0, false, err
	}
	more := len(mails) > movesPerRound
	if more {
		mails = mails[:movesPerRound]
	}
	folderByAccount := map[int64]int64{}
	moved := 0
	for _, mail := range mails {
		if ctx.Err() != nil {
			return moved, true, ctx.Err()
		}
		state, moveErr := fileMail(ctx, mover, folders, db, userID, settings, mail, folderByAccount)
		now := time.Now().UTC().Unix()
		if moveErr != nil {
			if errors.Is(moveErr, context.Canceled) {
				return moved, true, moveErr
			}
			// A server that refused a move is usually a server that is busy or
			// briefly unreachable, so the mail waits before it is tried again,
			// rather than spending every attempt in the next few seconds.
			attempts := mail.Attempts + 1
			next := ""
			if attempts >= maxMoveAttempts {
				next = moveStateFailed
			}
			retryAt := now + int64(moveRetryDelays[min(attempts-1, len(moveRetryDelays)-1)]/time.Second)
			if _, err := db.ExecContext(ctx, `UPDATE plugin_recipe_box_mails SET move_state = ?, move_attempts = ?, move_next_at = ?, move_error = ?, updated_at = ? WHERE user_id = ? AND id = ?`,
				next, attempts, retryAt, clean(moveErrorText(moveErr)), now, userID, mail.ID); err != nil {
				return moved, more, err
			}
			continue
		}
		if _, err := db.ExecContext(ctx, `UPDATE plugin_recipe_box_mails SET move_state = ?, move_error = '', updated_at = ? WHERE user_id = ? AND id = ?`,
			state, now, userID, mail.ID); err != nil {
			return moved, more, err
		}
		if state == moveStateMoved {
			moved++
		}
	}
	return moved, more, nil
}

func fileMail(ctx context.Context, mover plugins.StoredMessageHost, folders plugins.MailFolderHost, db *sql.DB, userID int64, settings Settings, mail pendingMail, folderByAccount map[int64]int64) (string, error) {
	copies, err := storedCopies(ctx, db, userID, mail)
	if err != nil {
		return "", err
	}
	// No row holds the mail any more: it was deleted. Nothing is left to file,
	// and a mail that stayed waiting would take a place in every round's
	// budget for good.
	if len(copies) == 0 {
		return moveStateSkipped, nil
	}
	filed, eligible := 0, 0
	for _, c := range copies {
		if strings.EqualFold(c.MailboxName, settings.FolderName) {
			filed++
			continue
		}
		// The reader's Sent copy, and copies in Trash or Junk, stay where they
		// are: only the delivered copies the scan reads are filed.
		if !mailboxReadable(c.MailboxName, c.MailboxRole) {
			continue
		}
		eligible++
		folderID, ok := folderByAccount[c.AccountID]
		if !ok {
			folderID, err = recipeFolder(ctx, folders, db, userID, c.AccountID, settings.FolderName)
			if err != nil {
				return "", err
			}
			folderByAccount[c.AccountID] = folderID
		}
		if err := mover.MoveMessage(ctx, userID, c.ID, folderID); err != nil {
			if store.IsNotFound(err) {
				continue
			}
			return "", err
		}
		filed++
	}
	if filed == 0 && eligible == 0 {
		return moveStateSkipped, nil
	}
	return moveStateMoved, nil
}

// recipeFolder answers the account's recipe folder, creating it on the server
// the first time. A folder the box created is taken out of All Mail at once,
// because that is what filing a recipe away is for; a folder that already
// existed keeps the settings the reader gave it.
func recipeFolder(ctx context.Context, folders plugins.MailFolderHost, db *sql.DB, userID, accountID int64, name string) (int64, error) {
	var existing int64
	err := db.QueryRowContext(ctx, `SELECT id FROM mailboxes WHERE user_id = ? AND account_id = ? AND LOWER(name) = LOWER(?) ORDER BY id LIMIT 1`, userID, accountID, name).Scan(&existing)
	if err == nil {
		return existing, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	id, created, err := folders.EnsureMailFolder(ctx, userID, accountID, name)
	if err != nil {
		return 0, err
	}
	if created {
		if _, err := hideFolderFromAllMail(ctx, folders, db, userID, id, name); err != nil {
			return 0, err
		}
	}
	return id, nil
}

// hideFolderFromAllMail takes the recipe folder named name out of All Mail and
// the other whole-account lists, and reports whether there was such a folder.
// Its duplicate pointers are refreshed in the same step, exactly as the folder
// settings page does (Store.UpdateMailboxSettings): a copy hidden behind a row
// of this folder would otherwise stay hidden behind a row no list shows.
func hideFolderFromAllMail(ctx context.Context, host plugins.BackendHost, db *sql.DB, userID, mailboxID int64, name string) (bool, error) {
	res, err := db.ExecContext(ctx, `UPDATE mailboxes SET show_in_all_mail = 0, updated_at = ? WHERE user_id = ? AND id = ? AND LOWER(name) = LOWER(?)`,
		time.Now().UTC().Unix(), userID, mailboxID, name)
	if err != nil {
		return false, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return false, nil
	}
	st, ok := host.Store().(*store.Store)
	if !ok || st == nil {
		return true, errors.New("plugin host store is unavailable")
	}
	return true, st.RefreshDuplicateCopiesForMailbox(ctx, userID, mailboxID)
}

func moveErrorText(err error) string {
	text := strings.TrimSpace(err.Error())
	if len(text) > 300 {
		text = text[:300]
	}
	return text
}
