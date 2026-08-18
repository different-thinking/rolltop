// File overview: Tenant-scoped persistence for subscribed Google calendars.
// A calendar row carries its own delta cursor and the visibility switch the
// week view reads; the events themselves live in calendar_events.go.

package store

import (
	"context"
	"database/sql"
	"strings"
	"time"
)

const (
	// CalendarSyncStatusOK marks a calendar whose last event sync completed.
	CalendarSyncStatusOK = "ok"
	// CalendarSyncStatusError marks one that failed. The cursor is kept so the
	// next attempt is still incremental.
	CalendarSyncStatusError = "error"
)

const (
	// CalendarAccessRoleOwner is the account's own calendar.
	CalendarAccessRoleOwner = "owner"
	// CalendarAccessRoleWriter is a shared calendar the account may edit.
	CalendarAccessRoleWriter = "writer"
)

const calendarSelectColumns = `id, user_id, google_connection_id, google_calendar_id,
	summary, description, time_zone, color, access_role, is_primary, selected,
	sync_token, window_start_at, last_sync_at, last_success_at, status, status_detail`

// Calendar is one calendar of one connected Google account.
type Calendar struct {
	ID                 int64
	UserID             int64
	GoogleConnectionID int64
	// GoogleCalendarID is Google's own identifier, which for a personal
	// calendar is the account's mail address and for a shared one an opaque
	// @group.calendar.google.com address.
	GoogleCalendarID string
	Summary          string
	Description      string
	TimeZone         string
	Color            string
	AccessRole       string
	IsPrimary        bool
	Selected         bool
	// SyncToken is Google's delta cursor for this calendar's events. It is not
	// a credential and is stored as it arrives.
	SyncToken string
	// WindowStartAt is the oldest point the mirror covers. Events before it were
	// never fetched, so an empty week there means "not synced", not "free".
	WindowStartAt time.Time
	LastSyncAt    time.Time
	LastSuccessAt time.Time
	Status        string
	StatusDetail  string
}

// CanWrite reports whether Google would accept a write to this calendar. A
// calendar shared read-only still syncs and is still drawn; offering an edit
// that can only fail at Google is what this prevents.
func (c Calendar) CanWrite() bool {
	switch strings.ToLower(strings.TrimSpace(c.AccessRole)) {
	case CalendarAccessRoleOwner, CalendarAccessRoleWriter:
		return true
	}
	return false
}

// CalendarUpsert is what the calendar-list sync knows about a calendar. It
// deliberately excludes the cursor, the window and the visibility switch: those
// are Rolltop's own state, and a list sync that reset them would restart every
// calendar's event sync and re-enable calendars the user had switched off.
type CalendarUpsert struct {
	GoogleConnectionID int64
	GoogleCalendarID   string
	Summary            string
	Description        string
	TimeZone           string
	Color              string
	AccessRole         string
	IsPrimary          bool
	// Selected seeds the visibility of a calendar Rolltop has not seen before,
	// from the account's own choice at Google. It is ignored on update.
	Selected bool
}

// UpsertCalendar records a calendar the list sync returned and answers with the
// stored row.
func (s *Store) UpsertCalendar(ctx context.Context, userID int64, in CalendarUpsert) (Calendar, error) {
	googleID := strings.TrimSpace(in.GoogleCalendarID)
	if userID <= 0 || in.GoogleConnectionID <= 0 || googleID == "" {
		return Calendar{}, ErrNotFound
	}
	ts := nowUnix()
	_, err := s.mustDataDB(ctx, userID).ExecContext(ctx, `INSERT INTO calendars
			(user_id, google_connection_id, google_calendar_id, summary, description,
			 time_zone, color, access_role, is_primary, selected, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(user_id, google_connection_id, google_calendar_id) DO UPDATE SET
			summary = excluded.summary,
			description = excluded.description,
			time_zone = excluded.time_zone,
			color = excluded.color,
			access_role = excluded.access_role,
			is_primary = excluded.is_primary,
			updated_at = excluded.updated_at`,
		userID, in.GoogleConnectionID, trimLimit(googleID, 300), trimLimit(in.Summary, 300),
		trimLimit(in.Description, 2000), trimLimit(in.TimeZone, 100), trimLimit(in.Color, 20),
		trimLimit(strings.ToLower(strings.TrimSpace(in.AccessRole)), 40),
		boolInt(in.IsPrimary), boolInt(in.Selected), ts, ts)
	if err != nil {
		return Calendar{}, err
	}
	return s.CalendarByGoogleID(ctx, userID, in.GoogleConnectionID, googleID)
}

// ListCalendars returns every subscribed calendar of one user. The account's
// own calendar sorts first because it is the one an event defaults to.
func (s *Store) ListCalendars(ctx context.Context, userID int64) ([]Calendar, error) {
	rows, err := s.mustDataDB(ctx, userID).QueryContext(ctx, `SELECT `+calendarSelectColumns+`
		FROM calendars WHERE user_id = ?
		ORDER BY is_primary DESC, summary ASC, id ASC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	calendars := []Calendar{}
	for rows.Next() {
		calendar, err := scanCalendar(rows)
		if err != nil {
			return nil, err
		}
		calendars = append(calendars, calendar)
	}
	return calendars, rows.Err()
}

// ListCalendarsForConnection returns the calendars of one connected account.
func (s *Store) ListCalendarsForConnection(ctx context.Context, userID, connectionID int64) ([]Calendar, error) {
	if userID <= 0 || connectionID <= 0 {
		return nil, nil
	}
	rows, err := s.mustDataDB(ctx, userID).QueryContext(ctx, `SELECT `+calendarSelectColumns+`
		FROM calendars WHERE user_id = ? AND google_connection_id = ?
		ORDER BY is_primary DESC, summary ASC, id ASC`, userID, connectionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	calendars := []Calendar{}
	for rows.Next() {
		calendar, err := scanCalendar(rows)
		if err != nil {
			return nil, err
		}
		calendars = append(calendars, calendar)
	}
	return calendars, rows.Err()
}

// Calendar loads one calendar, scoped to its owner so an id guessed from
// another tenant reads as not found.
func (s *Store) Calendar(ctx context.Context, userID, calendarID int64) (Calendar, error) {
	if userID <= 0 || calendarID <= 0 {
		return Calendar{}, ErrNotFound
	}
	return scanCalendar(s.mustDataDB(ctx, userID).QueryRowContext(ctx, `SELECT `+calendarSelectColumns+`
		FROM calendars WHERE user_id = ? AND id = ?`, userID, calendarID))
}

// CalendarByGoogleID loads one calendar by the identifier Google knows it under.
func (s *Store) CalendarByGoogleID(ctx context.Context, userID, connectionID int64, googleCalendarID string) (Calendar, error) {
	googleID := strings.TrimSpace(googleCalendarID)
	if userID <= 0 || connectionID <= 0 || googleID == "" {
		return Calendar{}, ErrNotFound
	}
	return scanCalendar(s.mustDataDB(ctx, userID).QueryRowContext(ctx, `SELECT `+calendarSelectColumns+`
		FROM calendars WHERE user_id = ? AND google_connection_id = ? AND google_calendar_id = ?`,
		userID, connectionID, googleID))
}

// SetCalendarSelected switches one calendar's visibility in the week view.
func (s *Store) SetCalendarSelected(ctx context.Context, userID, calendarID int64, selected bool) error {
	if userID <= 0 || calendarID <= 0 {
		return ErrNotFound
	}
	res, err := s.mustDataDB(ctx, userID).ExecContext(ctx,
		`UPDATE calendars SET selected = ?, updated_at = ? WHERE user_id = ? AND id = ?`,
		boolInt(selected), nowUnix(), userID, calendarID)
	if err != nil {
		return err
	}
	return requireCalendarRow(res)
}

// CalendarSyncState is the outcome of one calendar's event sync.
type CalendarSyncState struct {
	SyncToken     string
	WindowStartAt time.Time
	LastSyncAt    time.Time
	LastSuccessAt time.Time
	Status        string
	StatusDetail  string
}

// SaveCalendarSyncState records where a calendar's event sync got to.
func (s *Store) SaveCalendarSyncState(ctx context.Context, userID, calendarID int64, state CalendarSyncState) error {
	if userID <= 0 || calendarID <= 0 {
		return ErrNotFound
	}
	res, err := s.mustDataDB(ctx, userID).ExecContext(ctx, `UPDATE calendars SET
			sync_token = ?, window_start_at = ?, last_sync_at = ?, last_success_at = ?,
			status = ?, status_detail = ?, updated_at = ?
		WHERE user_id = ? AND id = ?`,
		state.SyncToken, timeUnix(state.WindowStartAt), timeUnix(state.LastSyncAt),
		timeUnix(state.LastSuccessAt), state.Status, trimLimit(state.StatusDetail, 500),
		nowUnix(), userID, calendarID)
	if err != nil {
		return err
	}
	return requireCalendarRow(res)
}

// DeleteCalendar removes a calendar and, through the schema's cascade, every
// event mirrored from it.
func (s *Store) DeleteCalendar(ctx context.Context, userID, calendarID int64) error {
	if userID <= 0 || calendarID <= 0 {
		return ErrNotFound
	}
	// The cascade is declared in the schema but SQLite only honours it when the
	// connection has foreign keys enabled, and one missed pragma would leave
	// events behind that no calendar can switch off. Deleting them here makes
	// the outcome the same either way.
	db, err := s.dataDB(ctx, userID)
	if err != nil {
		return err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM calendar_events WHERE user_id = ? AND calendar_id = ?`, userID, calendarID); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM calendars WHERE user_id = ? AND id = ?`, userID, calendarID)
	if err != nil {
		return err
	}
	if err := requireCalendarRow(res); err != nil {
		return err
	}
	return tx.Commit()
}

// deleteCalendarsForConnection removes every calendar of one connection. It
// takes an execer so the disconnect path can run it inside the same transaction
// that removes the connection itself.
func deleteCalendarsForConnection(ctx context.Context, db execer, userID, connectionID int64) error {
	if _, err := db.ExecContext(ctx,
		`DELETE FROM calendar_events WHERE user_id = ? AND calendar_id IN
			(SELECT id FROM calendars WHERE user_id = ? AND google_connection_id = ?)`,
		userID, userID, connectionID); err != nil {
		return err
	}
	_, err := db.ExecContext(ctx,
		`DELETE FROM calendars WHERE user_id = ? AND google_connection_id = ?`, userID, connectionID)
	return err
}

// CountCalendarsForConnection reports how many calendars one connection
// currently mirrors, for the settings page.
func (s *Store) CountCalendarsForConnection(ctx context.Context, userID, connectionID int64) (int, error) {
	if userID <= 0 || connectionID <= 0 {
		return 0, nil
	}
	var count int
	err := s.mustDataDB(ctx, userID).QueryRowContext(ctx,
		`SELECT COUNT(*) FROM calendars WHERE user_id = ? AND google_connection_id = ?`,
		userID, connectionID).Scan(&count)
	return count, err
}

// GoogleCalendarSync is the per-connection state of the calendar-list sync,
// which is separate from any one calendar's event sync.
type GoogleCalendarSync struct {
	UserID        int64
	ConnectionID  int64
	SyncToken     string
	LastSyncAt    time.Time
	LastSuccessAt time.Time
	Status        string
	StatusDetail  string
}

// GetGoogleCalendarSync loads the calendar-list cursor for one connection. A
// connection that has never synced has no row, which is the state that makes
// the next run a full read rather than an error.
func (s *Store) GetGoogleCalendarSync(ctx context.Context, userID, connectionID int64) (GoogleCalendarSync, error) {
	state := GoogleCalendarSync{UserID: userID, ConnectionID: connectionID}
	if userID <= 0 || connectionID <= 0 {
		return state, ErrNotFound
	}
	var lastSync, lastSuccess int64
	err := s.mustDataDB(ctx, userID).QueryRowContext(ctx,
		`SELECT sync_token, last_sync_at, last_success_at, status, status_detail
			FROM google_calendar_sync WHERE user_id = ? AND connection_id = ?`,
		userID, connectionID).
		Scan(&state.SyncToken, &lastSync, &lastSuccess, &state.Status, &state.StatusDetail)
	if err == sql.ErrNoRows {
		return state, nil
	}
	if err != nil {
		return GoogleCalendarSync{}, err
	}
	state.LastSyncAt = unixTime(lastSync)
	state.LastSuccessAt = unixTime(lastSuccess)
	return state, nil
}

// SaveGoogleCalendarSync writes the cursor and outcome of one list sync.
func (s *Store) SaveGoogleCalendarSync(ctx context.Context, state GoogleCalendarSync) error {
	if state.UserID <= 0 || state.ConnectionID <= 0 {
		return ErrNotFound
	}
	ts := nowUnix()
	_, err := s.mustDataDB(ctx, state.UserID).ExecContext(ctx,
		`INSERT INTO google_calendar_sync
				(user_id, connection_id, sync_token, last_sync_at, last_success_at, status, status_detail, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(user_id, connection_id) DO UPDATE SET
				sync_token = excluded.sync_token,
				last_sync_at = excluded.last_sync_at,
				last_success_at = excluded.last_success_at,
				status = excluded.status,
				status_detail = excluded.status_detail,
				updated_at = excluded.updated_at`,
		state.UserID, state.ConnectionID, state.SyncToken, timeUnix(state.LastSyncAt),
		timeUnix(state.LastSuccessAt), state.Status, trimLimit(state.StatusDetail, 500), ts, ts)
	return err
}

func requireCalendarRow(res sql.Result) error {
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func scanCalendar(dest scanDest) (Calendar, error) {
	var calendar Calendar
	var isPrimary, selected int64
	var windowStart, lastSync, lastSuccess int64
	if err := dest.Scan(&calendar.ID, &calendar.UserID, &calendar.GoogleConnectionID,
		&calendar.GoogleCalendarID, &calendar.Summary, &calendar.Description,
		&calendar.TimeZone, &calendar.Color, &calendar.AccessRole, &isPrimary, &selected,
		&calendar.SyncToken, &windowStart, &lastSync, &lastSuccess,
		&calendar.Status, &calendar.StatusDetail); err != nil {
		return Calendar{}, err
	}
	calendar.IsPrimary = isPrimary != 0
	calendar.Selected = selected != 0
	calendar.WindowStartAt = unixTime(windowStart)
	calendar.LastSyncAt = unixTime(lastSync)
	calendar.LastSuccessAt = unixTime(lastSuccess)
	return calendar, nil
}
