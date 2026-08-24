// File overview: Tenant-scoped persistence and validation for Android message swipe behavior.

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
)

const (
	SwipeActionTrash      = "trash"
	SwipeActionArchive    = "archive"
	SwipeActionSnooze     = "snooze"
	SwipeActionMarkRead   = "mark_read"
	SwipeActionMarkUnread = "mark_unread"

	SwipeSnoozeLaterToday = "later_today"
	SwipeSnoozeTomorrow   = "tomorrow"
	SwipeSnoozeNextWeek   = "next_week"
)

// SwipeArchiveMailbox selects one archive destination for an IMAP account.
// Move operations cannot cross accounts, so archive targets are account-specific.
type SwipeArchiveMailbox struct {
	AccountID int64
	MailboxID int64
}

// SwipePreferences controls the actions revealed by left and right message swipes.
type SwipePreferences struct {
	UserID            int64
	LeftAction        string
	LeftSnoozePreset  string
	RightAction       string
	RightSnoozePreset string
	ArchiveMailboxes  []SwipeArchiveMailbox
}

// DefaultSwipePreferences preserves the original Android swipe behavior where
// possible while making read-state actions explicit rather than toggles.
func DefaultSwipePreferences(userID int64) SwipePreferences {
	return SwipePreferences{
		UserID:            userID,
		LeftAction:        SwipeActionSnooze,
		LeftSnoozePreset:  SwipeSnoozeTomorrow,
		RightAction:       SwipeActionMarkRead,
		RightSnoozePreset: SwipeSnoozeTomorrow,
		ArchiveMailboxes:  []SwipeArchiveMailbox{},
	}
}

// ArchiveMailboxesForUser resolves each account's effective Archive folder. An
// identity's own choice wins because it is the more specific setting, and an
// account no identity speaks for keeps the per-account swipe mapping. Where
// several identities on one account disagree, the primary identity decides and
// the rest are ignored, so the answer stays single-valued per account.
//
// Both sources are re-validated against the folder's current role, so a folder
// that has since become Trash or Sent stops standing in as an Archive target.
func (s *Store) ArchiveMailboxesForUser(ctx context.Context, userID int64) ([]SwipeArchiveMailbox, error) {
	db, err := s.dataDB(ctx, userID)
	if err != nil {
		return nil, err
	}
	byAccount := map[int64]int64{}
	// The account comes from the chosen folder, not from the identity's IMAP
	// server field: that field may be left on Automatic, and a choice that named
	// a real folder would otherwise be stored and then silently ignored.
	//
	// Eligibility is re-checked here in Go via archiveMailboxRoleBlocked, the
	// same helper the save-time validators use, rather than a second copy of
	// the blocked-role list in SQL: a folder that has since become Trash or
	// Sent stops standing in as an Archive target either way, but there is
	// only one place that says which roles disqualify it.
	identityRows, err := db.QueryContext(ctx, `SELECT mb.account_id, i.archive_mailbox_id, mb.role
		FROM mail_identities i
		JOIN mailboxes mb ON mb.user_id = i.user_id AND mb.id = i.archive_mailbox_id
			AND (i.imap_account_id = 0 OR mb.account_id = i.imap_account_id)
		WHERE i.user_id = ? AND i.archive_mailbox_id > 0
		ORDER BY i.is_primary DESC, i.id ASC`, userID)
	if err != nil {
		return nil, err
	}
	if err := scanArchiveMailboxCandidates(identityRows, byAccount); err != nil {
		return nil, err
	}
	swipeRows, err := db.QueryContext(ctx, `SELECT sam.account_id, sam.mailbox_id, mb.role
		FROM swipe_archive_mailboxes sam
		JOIN mailboxes mb ON mb.user_id = sam.user_id AND mb.id = sam.mailbox_id
		WHERE sam.user_id = ? ORDER BY sam.account_id`, userID)
	if err != nil {
		return nil, err
	}
	if err := scanArchiveMailboxCandidates(swipeRows, byAccount); err != nil {
		return nil, err
	}
	accountIDs := make([]int64, 0, len(byAccount))
	for accountID := range byAccount {
		accountIDs = append(accountIDs, accountID)
	}
	sort.Slice(accountIDs, func(i, j int) bool { return accountIDs[i] < accountIDs[j] })
	out := make([]SwipeArchiveMailbox, 0, len(accountIDs))
	for _, accountID := range accountIDs {
		out = append(out, SwipeArchiveMailbox{AccountID: accountID, MailboxID: byAccount[accountID]})
	}
	return out, nil
}

// scanArchiveMailboxCandidates fills in accounts the map does not cover yet
// with folders that still qualify as an Archive destination, so the first
// source scanned wins and later ones only fill gaps.
func scanArchiveMailboxCandidates(rows *sql.Rows, byAccount map[int64]int64) error {
	defer rows.Close()
	for rows.Next() {
		var accountID, mailboxID int64
		var role string
		if err := rows.Scan(&accountID, &mailboxID, &role); err != nil {
			return err
		}
		if archiveMailboxRoleBlocked(role) {
			continue
		}
		if _, taken := byAccount[accountID]; !taken {
			byAccount[accountID] = mailboxID
		}
	}
	return rows.Err()
}

// ArchiveMailboxIDsForUser lists the effective Archive folders as bare IDs, for
// queries that only need to know which folders count as archived.
func (s *Store) ArchiveMailboxIDsForUser(ctx context.Context, userID int64) ([]int64, error) {
	targets, err := s.ArchiveMailboxesForUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	ids := make([]int64, 0, len(targets))
	for _, target := range targets {
		ids = append(ids, target.MailboxID)
	}
	return ids, nil
}

// GetSwipePreferences loads one user's swipe settings, returning stable defaults
// before that user has saved an explicit preference row.
func (s *Store) GetSwipePreferences(ctx context.Context, userID int64) (SwipePreferences, error) {
	prefs := DefaultSwipePreferences(userID)
	db := s.mustDataDB(ctx, userID)
	err := db.QueryRowContext(ctx, `SELECT left_action, left_snooze_preset, right_action, right_snooze_preset
		FROM swipe_preferences WHERE user_id = ?`, userID).
		Scan(&prefs.LeftAction, &prefs.LeftSnoozePreset, &prefs.RightAction, &prefs.RightSnoozePreset)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return SwipePreferences{}, err
	}
	rows, err := db.QueryContext(ctx, `SELECT mappings.account_id, mappings.mailbox_id, mailboxes.role
		FROM swipe_archive_mailboxes AS mappings
		JOIN mailboxes ON mailboxes.user_id = mappings.user_id
			AND mailboxes.account_id = mappings.account_id AND mailboxes.id = mappings.mailbox_id
		WHERE mappings.user_id = ?
		ORDER BY mappings.account_id`, userID)
	if err != nil {
		return SwipePreferences{}, err
	}
	defer rows.Close()
	prefs.ArchiveMailboxes = []SwipeArchiveMailbox{}
	for rows.Next() {
		var target SwipeArchiveMailbox
		var role string
		if err := rows.Scan(&target.AccountID, &target.MailboxID, &role); err != nil {
			return SwipePreferences{}, err
		}
		// Re-checked against archiveMailboxRoleBlocked rather than filtered in
		// SQL, so this stays the one place that decides Archive eligibility.
		if archiveMailboxRoleBlocked(role) {
			continue
		}
		prefs.ArchiveMailboxes = append(prefs.ArchiveMailboxes, target)
	}
	if err := rows.Err(); err != nil {
		return SwipePreferences{}, err
	}
	return prefs, nil
}

// SaveSwipePreferences validates and atomically replaces one user's swipe
// settings and per-account archive destinations.
func (s *Store) SaveSwipePreferences(ctx context.Context, prefs SwipePreferences) (SwipePreferences, error) {
	prefs.LeftAction = strings.ToLower(strings.TrimSpace(prefs.LeftAction))
	prefs.LeftSnoozePreset = strings.ToLower(strings.TrimSpace(prefs.LeftSnoozePreset))
	prefs.RightAction = strings.ToLower(strings.TrimSpace(prefs.RightAction))
	prefs.RightSnoozePreset = strings.ToLower(strings.TrimSpace(prefs.RightSnoozePreset))
	if prefs.UserID <= 0 {
		return SwipePreferences{}, fmt.Errorf("%w: user is required", ErrInvalidSwipePreferences)
	}
	if !validSwipeAction(prefs.LeftAction) || !validSwipeAction(prefs.RightAction) {
		return SwipePreferences{}, fmt.Errorf("%w: unsupported swipe action", ErrInvalidSwipePreferences)
	}
	if !validSwipeSnoozePreset(prefs.LeftSnoozePreset) || !validSwipeSnoozePreset(prefs.RightSnoozePreset) {
		return SwipePreferences{}, fmt.Errorf("%w: unsupported snooze preset", ErrInvalidSwipePreferences)
	}

	targets, err := s.validateSwipeArchiveMailboxes(ctx, prefs.UserID, prefs.ArchiveMailboxes)
	if err != nil {
		return SwipePreferences{}, err
	}
	prefs.ArchiveMailboxes = targets
	if prefs.LeftAction == SwipeActionArchive || prefs.RightAction == SwipeActionArchive {
		if err := s.validateSwipeArchiveCoverage(ctx, prefs.UserID, targets); err != nil {
			return SwipePreferences{}, err
		}
	}
	if prefs.LeftAction == SwipeActionTrash || prefs.RightAction == SwipeActionTrash {
		if err := s.validateSwipeTrashCoverage(ctx, prefs.UserID); err != nil {
			return SwipePreferences{}, err
		}
	}

	db := s.mustDataDB(ctx, prefs.UserID)
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return SwipePreferences{}, err
	}
	ts := nowUnix()
	if _, err := tx.ExecContext(ctx, `INSERT INTO swipe_preferences
			(user_id, left_action, left_snooze_preset, right_action, right_snooze_preset, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(user_id) DO UPDATE SET
			left_action = excluded.left_action,
			left_snooze_preset = excluded.left_snooze_preset,
			right_action = excluded.right_action,
			right_snooze_preset = excluded.right_snooze_preset,
			updated_at = excluded.updated_at
		WHERE swipe_preferences.user_id = ?`,
		prefs.UserID, prefs.LeftAction, prefs.LeftSnoozePreset, prefs.RightAction, prefs.RightSnoozePreset, ts, ts, prefs.UserID); err != nil {
		_ = tx.Rollback()
		return SwipePreferences{}, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM swipe_archive_mailboxes WHERE user_id = ?`, prefs.UserID); err != nil {
		_ = tx.Rollback()
		return SwipePreferences{}, err
	}
	for _, target := range targets {
		if _, err := tx.ExecContext(ctx, `INSERT INTO swipe_archive_mailboxes
				(user_id, account_id, mailbox_id, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?)`, prefs.UserID, target.AccountID, target.MailboxID, ts, ts); err != nil {
			_ = tx.Rollback()
			return SwipePreferences{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return SwipePreferences{}, err
	}
	return s.GetSwipePreferences(ctx, prefs.UserID)
}

func validSwipeAction(action string) bool {
	switch action {
	case SwipeActionTrash, SwipeActionArchive, SwipeActionSnooze, SwipeActionMarkRead, SwipeActionMarkUnread:
		return true
	default:
		return false
	}
}

func validSwipeSnoozePreset(preset string) bool {
	switch preset {
	case SwipeSnoozeLaterToday, SwipeSnoozeTomorrow, SwipeSnoozeNextWeek:
		return true
	default:
		return false
	}
}

func (s *Store) validateSwipeArchiveMailboxes(ctx context.Context, userID int64, targets []SwipeArchiveMailbox) ([]SwipeArchiveMailbox, error) {
	byAccount := make(map[int64]int64, len(targets))
	mailboxes := make(map[int64]int64, len(targets))
	for _, target := range targets {
		if target.AccountID <= 0 || target.MailboxID <= 0 {
			return nil, fmt.Errorf("%w: archive account and mailbox are required", ErrInvalidSwipePreferences)
		}
		if existing, ok := byAccount[target.AccountID]; ok && existing != target.MailboxID {
			return nil, fmt.Errorf("%w: multiple archive folders selected for one account", ErrInvalidSwipePreferences)
		}
		if existingAccount, ok := mailboxes[target.MailboxID]; ok && existingAccount != target.AccountID {
			return nil, fmt.Errorf("%w: archive folder cannot belong to multiple accounts", ErrInvalidSwipePreferences)
		}
		account, err := s.GetMailAccountForUser(ctx, userID, target.AccountID)
		if err != nil {
			if IsNotFound(err) {
				return nil, fmt.Errorf("%w: archive account is not owned by user", ErrInvalidSwipePreferences)
			}
			return nil, err
		}
		mailbox, err := s.GetMailboxForUser(ctx, userID, target.MailboxID)
		if err != nil {
			if IsNotFound(err) {
				return nil, fmt.Errorf("%w: archive mailbox is not owned by user", ErrInvalidSwipePreferences)
			}
			return nil, err
		}
		if mailbox.AccountID != account.ID {
			return nil, fmt.Errorf("%w: archive mailbox belongs to another account", ErrInvalidSwipePreferences)
		}
		if archiveMailboxRoleBlocked(mailbox.Role) {
			return nil, fmt.Errorf("%w: archive destination must be a regular folder", ErrInvalidSwipePreferences)
		}
		byAccount[target.AccountID] = target.MailboxID
		mailboxes[target.MailboxID] = target.AccountID
	}
	accountIDs := make([]int64, 0, len(byAccount))
	for accountID := range byAccount {
		accountIDs = append(accountIDs, accountID)
	}
	sort.Slice(accountIDs, func(i, j int) bool { return accountIDs[i] < accountIDs[j] })
	out := make([]SwipeArchiveMailbox, 0, len(accountIDs))
	for _, accountID := range accountIDs {
		out = append(out, SwipeArchiveMailbox{AccountID: accountID, MailboxID: byAccount[accountID]})
	}
	return out, nil
}

func (s *Store) validateSwipeArchiveCoverage(ctx context.Context, userID int64, targets []SwipeArchiveMailbox) error {
	accounts, err := s.ListMailAccountsForUser(ctx, userID)
	if err != nil {
		return err
	}
	if len(accounts) == 0 || len(targets) != len(accounts) {
		return fmt.Errorf("%w: archive action requires a destination folder for every account", ErrInvalidSwipePreferences)
	}
	byAccount := make(map[int64]bool, len(targets))
	for _, target := range targets {
		byAccount[target.AccountID] = true
	}
	for _, account := range accounts {
		if !byAccount[account.ID] {
			return fmt.Errorf("%w: archive action requires a destination folder for every account", ErrInvalidSwipePreferences)
		}
	}
	return nil
}

func (s *Store) validateSwipeTrashCoverage(ctx context.Context, userID int64) error {
	accounts, err := s.ListMailAccountsForUser(ctx, userID)
	if err != nil {
		return err
	}
	if len(accounts) == 0 {
		return fmt.Errorf("%w: trash action requires a Trash folder for every account", ErrInvalidSwipePreferences)
	}
	for _, account := range accounts {
		if _, err := s.GetMailboxByRoleForAccount(ctx, userID, account.ID, "trash"); err != nil {
			if IsNotFound(err) {
				return fmt.Errorf("%w: trash action requires a Trash folder for every account", ErrInvalidSwipePreferences)
			}
			return err
		}
	}
	return nil
}
