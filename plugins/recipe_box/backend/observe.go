// File overview: deleting a note's mail anywhere deletes the note.
//
// A note is the mail, not the link (notes.go), so a reader who opens the mail
// behind a note and deletes it there has deleted the note -- the same answer
// the note's own Delete gives. Without this the note stayed on the shelf
// pointing at a copy in Trash, whose Delete button is greyed out ("Already in
// Trash"), so it read as mail that could not be deleted at all.

package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"rolltop/backend/plugins"
)

// ObserveMessageMove takes a note off the shelf once its mail has been moved
// to Trash and nothing of it is left outside the folders a delete leaves
// alone. It only ever dismisses: a move out of Trash does not bring a note
// back, exactly as restoring the mail after the note's own Delete does not.
func (p *recipeBoxBackend) ObserveMessageMove(ctx context.Context, host plugins.BackendHost, event plugins.MessageMoveContext) error {
	if !isTrashRole(event.DestinationMailboxRole) || isTrashRole(event.SourceMailboxRole) {
		return plugins.ErrUnsupported
	}
	db, err := userDB(ctx, host, event.UserID)
	if err != nil {
		return err
	}
	dismissed, err := dismissNotesOfTrashedMail(ctx, db, event.UserID, event.AccountID, event.MessageID, event.MessageIDHeader)
	if err != nil {
		return err
	}
	if dismissed > 0 {
		if notifier, ok := host.(plugins.UserChangeHost); ok {
			notifier.NotifyUserChanged(event.UserID)
		}
	}
	return nil
}

func isTrashRole(role string) bool {
	return strings.EqualFold(strings.TrimSpace(role), "trash")
}

// dismissNotesOfTrashedMail dismisses the notes the moved message is a copy
// of, when no other copy still holds them. It runs before the host drops the
// moved row, so that row is left out of the count by id; the row the move
// creates in Trash is not there yet, and would not count if it were.
func dismissNotesOfTrashedMail(ctx context.Context, db *sql.DB, userID, accountID, messageID int64, header string) (int, error) {
	var contentSHA string
	err := db.QueryRowContext(ctx, `SELECT canonical_sha256 FROM messages WHERE user_id = ? AND id = ?`, userID, messageID).Scan(&contentSHA)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	// The keys a note can know this message by (messageKey), and the row
	// itself for a note recorded before its key could follow it.
	keys := []any{fmt.Sprintf("row:%d:%d", accountID, messageID)}
	if header = strings.TrimSpace(header); header != "" {
		keys = append(keys, "mid:"+header)
	}
	if contentSHA = strings.TrimSpace(contentSHA); contentSHA != "" {
		keys = append(keys, fmt.Sprintf("sha:%d:%s", accountID, contentSHA))
	}
	args := append([]any{userID, messageID}, keys...)
	rows, err := db.QueryContext(ctx, `SELECT m.id, m.message_key, m.message_id, m.message_id_header, m.content_sha, m.account_id
		FROM plugin_recipe_box_mails m
		WHERE m.user_id = ? AND m.note_dismissed = 0 AND NOT `+carriesRecipe+`
		AND (m.message_id = ? OR m.message_key IN (?`+strings.Repeat(", ?", len(keys)-1)+`))`, args...)
	if err != nil {
		return 0, err
	}
	var notes []pendingMail
	for rows.Next() {
		var mail pendingMail
		if err := rows.Scan(&mail.ID, &mail.Key, &mail.Message, &mail.Header, &mail.ContentSHA, &mail.AccountID); err != nil {
			rows.Close()
			return 0, err
		}
		notes = append(notes, mail)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	dismissed := 0
	for _, mail := range notes {
		copies, err := storedCopies(ctx, db, userID, mail)
		if err != nil {
			return dismissed, err
		}
		held := false
		for _, c := range copies {
			if c.ID != messageID && copyHoldsNote(c, accountID) {
				held = true
				break
			}
		}
		if held {
			continue
		}
		now := time.Now().UTC().Unix()
		if _, err := db.ExecContext(ctx, `UPDATE plugin_recipe_box_mails SET note_dismissed = 1, updated_at = ? WHERE user_id = ? AND id = ? AND note_dismissed = 0`,
			now, userID, mail.ID); err != nil {
			return dismissed, err
		}
		dismissed++
	}
	return dismissed, nil
}

// copyHoldsNote reports whether a copy left behind still keeps the note: the
// mail is not deleted while it is still in a folder the reader reads it from.
// Trash, Junk, Drafts and Sent do not hold it -- they are the folders the
// note's own Delete leaves alone, or ones it would not be read from. Neither
// does a Gmail system folder of the account the trashed copy was in: there it
// is a label of the one message, which Gmail trashes along with it, and the
// row only waits for that folder's next sync to go.
func copyHoldsNote(c storedCopy, trashedAccountID int64) bool {
	switch strings.ToLower(strings.TrimSpace(c.MailboxRole)) {
	case "trash", "junk", "drafts", "sent":
		return false
	}
	return !(c.AccountID == trashedAccountID && gmailSystemFolder(c.MailboxName))
}

var _ plugins.MessageMoveObserver = (*recipeBoxBackend)(nil)
