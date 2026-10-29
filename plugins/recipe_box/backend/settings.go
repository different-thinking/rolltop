// File overview: A reader's recipe box settings and the addresses that count as
// their own.

package main

import (
	"context"
	"database/sql"
	"errors"
	"net/mail"
	"regexp"
	"strings"
	"time"
	"unicode"
)

// defaultFolderName is where the box files mail when the reader names no
// folder. It is only a default: the settings page writes whatever they chose.
const defaultFolderName = "Recipes"

// defaultNotesFolderName is where notes are filed when the reader names no
// folder for them.
const defaultNotesFolderName = "Notes"

// Settings is one reader's choice. A reader without a row has not switched
// the box on, and nothing reads their mail.
type Settings struct {
	Enabled bool `json:"enabled"`
	// OwnRecipientsOnly accepts a mail only when every recipient is one of the
	// reader's own addresses, as well as the sender. Without it the sender
	// alone decides: mail the reader sent somebody else that landed in a folder
	// the scan reads.
	OwnRecipientsOnly bool   `json:"own_recipients_only"`
	MoveEnabled       bool   `json:"move_enabled"`
	FolderName        string `json:"folder_name"`
	// NotesMoveEnabled files notes -- notes-to-self that carried no recipe --
	// into their own folder. It is off until the reader switches it on.
	NotesMoveEnabled bool   `json:"notes_move_enabled"`
	NotesFolderName  string `json:"notes_folder_name"`
	ScanCursor       int64  `json:"-"`
	ScanStartedAt    int64  `json:"scan_started_at"`
	ScanFinishedAt   int64  `json:"scan_finished_at"`
	exists           bool
}

func defaultSettings() Settings {
	return Settings{OwnRecipientsOnly: true, MoveEnabled: true, FolderName: defaultFolderName, NotesFolderName: defaultNotesFolderName}
}

func loadSettings(ctx context.Context, db *sql.DB, userID int64) (Settings, error) {
	s := defaultSettings()
	var enabled, own, move, notesMove int
	err := db.QueryRowContext(ctx, `SELECT enabled, own_recipients_only, move_enabled, folder_name, notes_move_enabled, notes_folder_name, scan_cursor, scan_started_at, scan_finished_at
		FROM plugin_recipe_box_settings WHERE user_id = ?`, userID).Scan(&enabled, &own, &move, &s.FolderName, &notesMove, &s.NotesFolderName, &s.ScanCursor, &s.ScanStartedAt, &s.ScanFinishedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	s.Enabled, s.OwnRecipientsOnly, s.MoveEnabled, s.NotesMoveEnabled, s.exists = enabled != 0, own != 0, move != 0, notesMove != 0, true
	if strings.TrimSpace(s.FolderName) == "" {
		s.FolderName = defaultFolderName
	}
	if strings.TrimSpace(s.NotesFolderName) == "" {
		s.NotesFolderName = defaultNotesFolderName
	}
	return s, nil
}

// saveSettings writes the reader's choice. Switching the box on for the first
// time starts the scan from the oldest mail, which is the backfill; switching
// it off and on again resumes where it stopped rather than walking the mailbox
// a second time.
func saveSettings(ctx context.Context, db *sql.DB, userID int64, in Settings) (Settings, error) {
	name, err := normalizeFolderName(in.FolderName, defaultFolderName)
	if err != nil {
		return Settings{}, err
	}
	notesName, err := normalizeFolderName(in.NotesFolderName, defaultNotesFolderName)
	if err != nil {
		return Settings{}, err
	}
	// The two shelves are two folders. One folder for both would file every
	// note into the recipe folder, where the cookbook claims to be the reason
	// the mail is there. It is asked only while notes are filed: the notes
	// folder is not shown otherwise, and a reader whose recipe folder happens
	// to carry the default notes name could not save anything else.
	if in.NotesMoveEnabled && strings.EqualFold(name, notesName) {
		return Settings{}, errors.New("recipes and notes need two different folders")
	}
	now := time.Now().UTC().Unix()
	_, err = db.ExecContext(ctx, `INSERT INTO plugin_recipe_box_settings (user_id, enabled, own_recipients_only, move_enabled, folder_name, notes_move_enabled, notes_folder_name, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (user_id) DO UPDATE SET enabled = excluded.enabled, own_recipients_only = excluded.own_recipients_only,
			move_enabled = excluded.move_enabled, folder_name = excluded.folder_name, notes_move_enabled = excluded.notes_move_enabled,
			notes_folder_name = excluded.notes_folder_name, updated_at = excluded.updated_at`,
		userID, boolInt(in.Enabled), boolInt(in.OwnRecipientsOnly), boolInt(in.MoveEnabled), name, boolInt(in.NotesMoveEnabled), notesName, now, now)
	if err != nil {
		return Settings{}, err
	}
	return loadSettings(ctx, db, userID)
}

// restartScan puts the cursor back to the first mail, for a reader who wants
// the whole mailbox looked at again. Links already answered keep their answer
// -- a page is not fetched twice for the same reader -- except the ones that
// failed, which are worth another try now that somebody asked; the same goes
// for mail that could not be filed. Mail already filed stays filed.
func restartScan(ctx context.Context, db *sql.DB, userID int64) error {
	now := time.Now().UTC().Unix()
	if _, err := db.ExecContext(ctx, `UPDATE plugin_recipe_box_settings SET scan_cursor = 0, scan_started_at = 0, scan_finished_at = 0, updated_at = ? WHERE user_id = ?`, now, userID); err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, `UPDATE plugin_recipe_box_links SET status = 'pending', attempts = 0, next_attempt_at = 0, error = '', updated_at = ?
		WHERE user_id = ? AND status = 'failed'`, now, userID); err != nil {
		return err
	}
	_, err := db.ExecContext(ctx, `UPDATE plugin_recipe_box_mails SET move_state = '', move_attempts = 0, move_next_at = 0, move_error = '', updated_at = ?
		WHERE user_id = ? AND move_state = 'failed'`, now, userID)
	return err
}

func normalizeFolderName(name, fallback string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return fallback, nil
	}
	if len(name) > 120 {
		return "", errors.New("folder name is too long")
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return "", errors.New("folder name contains an invalid character")
		}
	}
	if strings.EqualFold(name, "INBOX") {
		return "", errors.New("the folder cannot be the Inbox")
	}
	return name, nil
}

// ownAddresses is every address the reader sends or receives as: the accounts
// they connected and the identities they created. Lowercased, because the
// comparison is about the mailbox, not its spelling.
func ownAddresses(ctx context.Context, db *sql.DB, userID int64) (map[string]bool, error) {
	out := map[string]bool{}
	rows, err := db.QueryContext(ctx, `SELECT email, username FROM mail_accounts WHERE user_id = ?
		UNION ALL SELECT email, '' FROM mail_identities WHERE user_id = ?`, userID, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var email, username string
		if err := rows.Scan(&email, &username); err != nil {
			return nil, err
		}
		for _, value := range []string{email, username} {
			if address := normalizeAddress(value); address != "" {
				out[address] = true
			}
		}
	}
	return out, rows.Err()
}

var looseAddressRE = regexp.MustCompile(`[A-Za-z0-9._%+'=\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`)

func normalizeAddress(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if !strings.Contains(value, "@") || strings.ContainsAny(value, " <>,") {
		return ""
	}
	return value
}

// headerAddresses reads the addresses of a stored From/To/Cc value. The store
// writes them as RFC 5322 lists, which net/mail reads; a value it cannot parse
// falls back to whatever looks like an address in it, so one malformed display
// name cannot hide a recipient.
func headerAddresses(value string) []string {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	var out []string
	if list, err := mail.ParseAddressList(value); err == nil {
		for _, addr := range list {
			if address := normalizeAddress(addr.Address); address != "" {
				out = append(out, address)
			}
		}
		return out
	}
	for _, match := range looseAddressRE.FindAllString(value, -1) {
		if address := normalizeAddress(match); address != "" {
			out = append(out, address)
		}
	}
	return out
}

// isNoteToSelf is the whole admission test. The sender must be one of the
// reader's addresses, exactly one, and -- unless the reader relaxed it --
// every recipient must be one too. A mail with no visible recipient at all
// reached the reader by Bcc, which is still the reader writing to themselves.
// Mail from any other address never qualifies, whatever it links to: a
// newsletter full of recipes is somebody else's selection, not the reader's.
// The From header is taken at its word; a forged one the provider did not
// send to Junk passes (see the README for why that limit was accepted).
func isNoteToSelf(own map[string]bool, from, to, cc string, ownRecipientsOnly bool) bool {
	senders := headerAddresses(from)
	if len(senders) != 1 || !own[senders[0]] {
		return false
	}
	if !ownRecipientsOnly {
		return true
	}
	for _, recipient := range append(headerAddresses(to), headerAddresses(cc)...) {
		if !own[recipient] {
			return false
		}
	}
	return true
}

func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}
