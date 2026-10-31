// File overview: Notes -- the notes-to-self that carried no recipe -- as an
// overview sorted by what they point at.
//
// A note is the mail, not the link: one note can carry a video and a shop
// page, and the reader's own line beside them belongs to both. Its kind is the
// kind of the first link it carries, which is the one the reader put first, or
// "text" when it carries none. The kinds come from the pages themselves
// (recipe.Classify), so the overview needs no rules of its own about what a
// note is about.

package main

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strings"
	"time"

	"rolltop/backend/plugins"
	"rolltop/backend/store"
	"rolltop/plugins/recipe_box/recipe"
)

// noteListLimit bounds one read of the overview. A reader with more notes
// than this in one state finds the rest by searching.
const noteListLimit = 2000

// Note is one note-to-self in the overview.
type Note struct {
	ID        int64      `json:"id"`
	MessageID int64      `json:"message_id"`
	Subject   string     `json:"subject"`
	DateUnix  int64      `json:"date_unix"`
	Body      string     `json:"body"`
	Kind      string     `json:"kind"`
	DoneAt    int64      `json:"done_at"`
	Filed     bool       `json:"filed"`
	Links     []NoteLink `json:"links"`
}

// NoteLink is one link of a note, with what its page said about itself.
type NoteLink struct {
	ID          int64  `json:"id"`
	URL         string `json:"url"`
	FinalURL    string `json:"final_url"`
	Status      string `json:"status"`
	Kind        string `json:"kind"`
	Detail      string `json:"detail"`
	Title       string `json:"title"`
	Description string `json:"description"`
	SiteName    string `json:"site_name"`
	Error       string `json:"error"`
}

// apiListNotes answers the overview for one state -- open or done -- with the
// count per kind before the kind filter, so the chips can say how many each
// shelf holds while one of them is open.
func (p *recipeBoxBackend) apiListNotes(host plugins.APIHost, db *sql.DB, userID int64, w http.ResponseWriter, r *http.Request) {
	done := r.URL.Query().Get("state") == "done"
	query := strings.Fields(strings.ToLower(r.URL.Query().Get("q")))
	notes, err := loadNotes(r.Context(), db, userID, done, query)
	if err != nil {
		host.ServerError(w, err)
		return
	}
	kind := strings.TrimSpace(r.URL.Query().Get("kind"))
	counts := map[string]int{}
	out := []Note{}
	for _, note := range notes {
		if !noteMatches(note, query) {
			continue
		}
		counts[note.Kind]++
		if kind == "" || note.Kind == kind {
			out = append(out, note)
		}
	}
	totals := map[string]int{"open": 0, "done": 0}
	if err := countInto(r.Context(), db, totals, `SELECT CASE WHEN m.done_at > 0 THEN 'done' ELSE 'open' END AS state, COUNT(*)
		FROM plugin_recipe_box_mails m WHERE m.user_id = ? AND m.note_dismissed = 0 AND NOT `+carriesRecipe+` GROUP BY state`, userID); err != nil {
		host.ServerError(w, err)
		return
	}
	host.WriteJSON(w, map[string]any{"notes": out, "counts": counts, "totals": totals})
}

// noteSearchTerms bounds how many words of a search narrow the read in SQL;
// noteMatches still checks every word on what comes back.
const noteSearchTerms = 6

// loadNotes reads the notes in one state, newest first, with their links. The
// words of a search narrow the read itself, ahead of noteListLimit, so a note
// older than the newest noteListLimit is still found by searching for it.
func loadNotes(ctx context.Context, db *sql.DB, userID int64, done bool, words []string) ([]Note, error) {
	// One state sorts by one column, named outright rather than through a
	// CASE, so the notes index can serve the order.
	state, order := `m.done_at = 0`, `m.date_unix`
	if done {
		state, order = `m.done_at > 0`, `m.done_at`
	}
	args := []any{userID}
	for i, word := range words {
		if i >= noteSearchTerms {
			break
		}
		pattern := "%" + likeEscaper.Replace(word) + "%"
		state += ` AND (LOWER(m.subject || ' ' || m.body || ' ' || m.note) LIKE ? ESCAPE '\'
			OR EXISTS (SELECT 1 FROM plugin_recipe_box_mail_links sml
				JOIN plugin_recipe_box_links sl ON sl.id = sml.link_id AND sl.user_id = sml.user_id
				WHERE sml.mail_id = m.id AND sml.user_id = m.user_id
				AND LOWER(sl.title || ' ' || sl.description || ' ' || sl.site_name || ' ' || sl.url || ' ' || sl.detail) LIKE ? ESCAPE '\'))`
		args = append(args, pattern, pattern)
	}
	args = append(args, noteListLimit)
	// The Message-ID lookup names x.message_id_header <> '' on the messages
	// side, not on the note's: the index that serves it is partial on exactly
	// that predicate, and a planner that cannot see it there scans every
	// message the reader has once per note -- seconds per hundred notes.
	rows, err := db.QueryContext(ctx, `SELECT m.id, m.subject, m.date_unix, CASE WHEN m.body <> '' THEN m.body ELSE m.note END, m.done_at, m.move_state,
			COALESCE((SELECT MAX(x.id) FROM messages x WHERE x.user_id = m.user_id AND x.message_id_header <> '' AND x.message_id_header = m.message_id_header),
				(SELECT x.id FROM messages x WHERE x.user_id = m.user_id AND x.id = m.message_id), 0)
		FROM plugin_recipe_box_mails m
		WHERE m.user_id = ? AND m.note_dismissed = 0 AND `+state+` AND NOT `+carriesRecipe+`
		ORDER BY `+order+` DESC, m.id DESC LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var notes []Note
	index := map[int64]int{}
	for rows.Next() {
		var note Note
		var moveState string
		if err := rows.Scan(&note.ID, &note.Subject, &note.DateUnix, &note.Body, &note.DoneAt, &moveState, &note.MessageID); err != nil {
			return nil, err
		}
		note.Filed = moveState == moveStateMoved
		note.Links = []NoteLink{}
		index[note.ID] = len(notes)
		notes = append(notes, note)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := attachNoteLinks(ctx, db, userID, notes, index); err != nil {
		return nil, err
	}
	for i := range notes {
		notes[i].Kind = noteKind(notes[i])
	}
	return notes, nil
}

// attachNoteLinks reads the links of the notes given, in the order their mail
// named them, a bounded slice of ids at a time.
func attachNoteLinks(ctx context.Context, db *sql.DB, userID int64, notes []Note, index map[int64]int) error {
	const chunk = 500
	for start := 0; start < len(notes); start += chunk {
		end := min(start+chunk, len(notes))
		args := []any{userID}
		marks := make([]string, 0, end-start)
		for _, note := range notes[start:end] {
			args = append(args, note.ID)
			marks = append(marks, "?")
		}
		rows, err := db.QueryContext(ctx, `SELECT ml.mail_id, l.id, l.url, l.final_url, l.status, l.kind, l.detail, l.title, l.description, l.site_name, l.error
			FROM plugin_recipe_box_mail_links ml
			JOIN plugin_recipe_box_links l ON l.id = ml.link_id AND l.user_id = ml.user_id
			WHERE ml.user_id = ? AND ml.mail_id IN (`+strings.Join(marks, ",")+`)
			ORDER BY ml.mail_id, ml.position, l.id`, args...)
		if err != nil {
			return err
		}
		for rows.Next() {
			var mailID int64
			var link NoteLink
			if err := rows.Scan(&mailID, &link.ID, &link.URL, &link.FinalURL, &link.Status, &link.Kind, &link.Detail, &link.Title, &link.Description, &link.SiteName, &link.Error); err != nil {
				rows.Close()
				return err
			}
			if link.Kind == "" {
				// A link answered before kinds existed, or one whose page has
				// not answered yet, is filed by its platform.
				link.Kind = hostKindOrLink(link.URL)
			}
			if i, ok := index[mailID]; ok {
				notes[i].Links = append(notes[i].Links, link)
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
	}
	return nil
}

// noteKind files a note by its first link. A link whose page has not answered
// yet is filed by its platform until it has, so a note does not jump between
// shelves only once its page is read -- unless the platform says nothing.
func noteKind(note Note) string {
	if len(note.Links) == 0 {
		return recipe.KindText
	}
	// attachNoteLinks has given every link a kind; a recipe the reader took
	// out of the cookbook is filed by its platform like any other page.
	first := note.Links[0]
	if first.Kind != recipe.KindRecipe {
		return first.Kind
	}
	return hostKindOrLink(first.URL)
}

// noteMatches is the overview's search: every word somewhere in what the note
// says or what its links' pages said.
func noteMatches(note Note, words []string) bool {
	if len(words) == 0 {
		return true
	}
	parts := []string{note.Subject, note.Body}
	for _, link := range note.Links {
		parts = append(parts, link.Title, link.Description, link.SiteName, link.URL, link.Detail)
	}
	text := strings.ToLower(strings.Join(parts, " \n"))
	for _, word := range words {
		if !strings.Contains(text, word) {
			return false
		}
	}
	return true
}

// apiUpdateNote marks a note done or open again, or takes it out of the
// overview. Taking it out also keeps the box from filing it.
func (p *recipeBoxBackend) apiUpdateNote(host plugins.APIHost, db *sql.DB, userID, id int64, w http.ResponseWriter, r *http.Request) {
	var in struct {
		Done      *bool `json:"done"`
		Dismissed *bool `json:"dismissed"`
	}
	if !host.DecodeJSON(w, r, &in) {
		return
	}
	now := time.Now().UTC().Unix()
	var res sql.Result
	var err error
	switch {
	case in.Done != nil:
		doneAt := int64(0)
		if *in.Done {
			doneAt = now
		}
		res, err = db.ExecContext(r.Context(), `UPDATE plugin_recipe_box_mails SET done_at = ?, updated_at = ? WHERE user_id = ? AND id = ?`, doneAt, now, userID, id)
	case in.Dismissed != nil:
		res, err = db.ExecContext(r.Context(), `UPDATE plugin_recipe_box_mails SET note_dismissed = ?, updated_at = ? WHERE user_id = ? AND id = ?`, boolInt(*in.Dismissed), now, userID, id)
	default:
		host.WriteAPIError(w, http.StatusBadRequest, "nothing to change")
		return
	}
	if err != nil {
		host.ServerError(w, err)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		host.WriteAPIError(w, http.StatusNotFound, "note not found")
		return
	}
	host.WriteJSON(w, map[string]any{"ok": true})
}

// apiDeleteNote deletes the mail behind a note: every copy goes to its own
// account's Trash, the same move the mail view's Delete makes, and the note
// leaves the overview. Nothing leaves the server until that Trash is emptied.
// Unlike filing, Delete also takes the copies the scan never reads -- a Gmail
// label view such as All Mail, Junk -- because a reader who archived the note
// in Gmail has nothing left but the All Mail copy, and a delete that left it
// there would have deleted nothing. The Sent and Drafts copies stay where they
// are, except a Gmail Sent copy in the account the note was delivered to: there
// it is a label of the one message, so trashing the Inbox copy trashes it with
// it -- and once the note is archived it is the only copy a default sync
// mirrors, since All Mail is not synced unless the reader asks for it.
func (p *recipeBoxBackend) apiDeleteNote(host plugins.APIHost, db *sql.DB, userID, id int64, w http.ResponseWriter, r *http.Request) {
	mover, ok := host.(plugins.StoredMessageHost)
	if !ok {
		host.WriteAPIError(w, http.StatusNotImplemented, "deleting mail is not available")
		return
	}
	ctx := r.Context()
	var mail pendingMail
	err := db.QueryRowContext(ctx, `SELECT m.id, m.message_key, m.message_id, m.message_id_header, m.content_sha, m.account_id, m.move_attempts
		FROM plugin_recipe_box_mails m
		WHERE m.user_id = ? AND m.id = ? AND m.note_dismissed = 0 AND NOT `+carriesRecipe, userID, id).
		Scan(&mail.ID, &mail.Key, &mail.Message, &mail.Header, &mail.ContentSHA, &mail.AccountID, &mail.Attempts)
	if errors.Is(err, sql.ErrNoRows) {
		host.WriteAPIError(w, http.StatusNotFound, "note not found")
		return
	}
	if err != nil {
		host.ServerError(w, err)
		return
	}
	// The copies are read again after every pass. A copy reported gone may be
	// gone because something else moved it -- the box filing this very note
	// into its notes folder is the usual case -- and the row it left behind is
	// then a new one in that folder, not mail in Trash. Only a pass that finds
	// nothing left outside Trash ends the delete. A row asked about once is not
	// asked again: a move the server answered with "no longer in that folder"
	// succeeds and leaves its row for reconciliation to remove.
	trashByAccount := map[int64]int64{}
	attempted := map[int64]bool{}
	moved := 0
	for round := 0; ; round++ {
		copies, err := storedCopies(ctx, db, userID, mail)
		if err != nil {
			host.ServerError(w, err)
			return
		}
		// Every account's Trash is resolved before anything moves, so an
		// account without one refuses the delete instead of leaving it half
		// done.
		var targets []storedCopy
		for _, c := range copies {
			if attempted[c.ID] || deleteKeeps(c, mail.AccountID) {
				continue
			}
			if _, ok := trashByAccount[c.AccountID]; !ok {
				trashID, err := trashMailboxID(ctx, db, userID, c.AccountID)
				if err != nil {
					host.ServerError(w, err)
					return
				}
				if trashID == 0 {
					host.WriteAPIError(w, http.StatusConflict, "this account has no Trash folder")
					return
				}
				trashByAccount[c.AccountID] = trashID
			}
			targets = append(targets, c)
		}
		if len(targets) == 0 {
			break
		}
		if round >= deleteNoteRounds {
			host.WriteAPIError(w, http.StatusConflict, "the mail moved while it was being deleted; try again")
			return
		}
		for _, c := range targets {
			attempted[c.ID] = true
			if err := mover.MoveMessage(ctx, userID, c.ID, trashByAccount[c.AccountID]); err != nil {
				// A copy that is already gone is settled by the next pass.
				// Anything else stops here with the note still listed, and
				// pressing Delete again moves only what is still out of Trash.
				if store.IsNotFound(err) {
					continue
				}
				host.WriteAPIError(w, http.StatusBadGateway, moveErrorText(err))
				return
			}
			moved++
		}
	}
	now := time.Now().UTC().Unix()
	if _, err := db.ExecContext(ctx, `UPDATE plugin_recipe_box_mails SET note_dismissed = 1, updated_at = ? WHERE user_id = ? AND id = ?`, now, userID, id); err != nil {
		host.ServerError(w, err)
		return
	}
	if notifier, ok := host.(plugins.UserChangeHost); ok && moved > 0 {
		notifier.NotifyUserChanged(userID)
	}
	host.WriteJSON(w, map[string]any{"ok": true, "moved": moved})
}

// deleteKeeps reports whether Delete leaves a copy where it is: what is in
// Trash already, and the reader's own Sent and Drafts copies. Gmail's Sent
// folder is a label, so in the account the note was delivered to its copy is
// the note itself and goes with it; in any other account it is the reader's
// record of what they sent somewhere else and stays.
func deleteKeeps(c storedCopy, noteAccountID int64) bool {
	switch strings.ToLower(strings.TrimSpace(c.MailboxRole)) {
	case "trash", "drafts":
		return true
	case "sent":
		return !(c.AccountID == noteAccountID && gmailSystemFolder(c.MailboxName))
	}
	return false
}

// deleteNoteRounds bounds how many times a delete reads the note's copies
// again after moving them: a copy that keeps turning up somewhere new is mail
// something else is moving at the same time, and the reader is told so.
const deleteNoteRounds = 3

// trashMailboxID answers one account's Trash folder, zero when it has none.
func trashMailboxID(ctx context.Context, db *sql.DB, userID, accountID int64) (int64, error) {
	var id int64
	err := db.QueryRowContext(ctx, `SELECT id FROM mailboxes WHERE user_id = ? AND account_id = ? AND role = 'trash' ORDER BY id LIMIT 1`, userID, accountID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return id, err
}
