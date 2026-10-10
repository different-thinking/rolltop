// File overview: Tenant-scoped persistence for subscribed calendars, Google's
// and Microsoft 365's alike. A calendar row says which provider it mirrors and
// carries its own sync state and the visibility switch the week view reads;
// the events themselves live in calendar_events.go.

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

const (
	// CalendarProviderGoogle marks a calendar mirrored from Google Calendar.
	CalendarProviderGoogle = "google"
	// CalendarProviderMicrosoft marks one mirrored from Microsoft 365 through
	// Microsoft Graph.
	CalendarProviderMicrosoft = "microsoft"
)

const calendarSelectColumns = `id, user_id, google_connection_id, remote_calendar_id,
	summary, description, time_zone, color, access_role, is_primary, selected,
	sync_token, window_start_at, last_sync_at, last_success_at, status, status_detail,
	copy_target, provider, microsoft_connection_id, full_read_at, online_meeting_providers`

// Calendar is one calendar of one connected account.
type Calendar struct {
	ID     int64
	UserID int64
	// Provider is CalendarProviderGoogle or CalendarProviderMicrosoft and
	// decides which of the two connection ids below is meaningful; the other
	// is zero.
	Provider              string
	GoogleConnectionID    int64
	MicrosoftConnectionID int64
	// RemoteCalendarID is the provider's own identifier. For Google that is
	// the account's mail address for a personal calendar and an opaque
	// @group.calendar.google.com address for a shared one; for Microsoft it is
	// Graph's opaque calendar id.
	RemoteCalendarID string
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
	// CopyTarget marks the reader's second calendar: the one an event entered
	// in any other calendar can be copied into as well. At most one calendar of
	// a user carries it. The second calendar may belong to the other provider.
	CopyTarget bool
	// FullReadAt is when the Microsoft sync last read the calendar's whole
	// window rather than the near one. Google rows leave it zero.
	FullReadAt time.Time
	// OnlineMeetingProviders lists the online meeting kinds the provider
	// offers for events in this calendar (Graph's teamsForBusiness and its
	// relatives). Empty means none can be created here.
	OnlineMeetingProviders []string
}

// IsMicrosoft reports whether the calendar is mirrored from Microsoft 365.
func (c Calendar) IsMicrosoft() bool {
	return c.Provider == CalendarProviderMicrosoft
}

// ConnectionID is the id of the connection the calendar came from, whichever
// provider that is.
func (c Calendar) ConnectionID() int64 {
	if c.IsMicrosoft() {
		return c.MicrosoftConnectionID
	}
	return c.GoogleConnectionID
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
	RemoteCalendarID   string
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
	googleID := strings.TrimSpace(in.RemoteCalendarID)
	if userID <= 0 || in.GoogleConnectionID <= 0 || googleID == "" {
		return Calendar{}, ErrNotFound
	}
	ts := nowUnix()
	_, err := s.mustDataDB(ctx, userID).ExecContext(ctx, `INSERT INTO calendars
			(user_id, provider, google_connection_id, microsoft_connection_id, remote_calendar_id,
			 summary, description, time_zone, color, access_role, is_primary, selected,
			 created_at, updated_at)
		VALUES (?, 'google', ?, 0, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(user_id, provider, google_connection_id, microsoft_connection_id, remote_calendar_id) DO UPDATE SET
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

// ListCalendarsForConnection returns the calendars of one connected Google
// account.
func (s *Store) ListCalendarsForConnection(ctx context.Context, userID, connectionID int64) ([]Calendar, error) {
	if userID <= 0 || connectionID <= 0 {
		return nil, nil
	}
	return s.queryCalendars(ctx, userID, `SELECT `+calendarSelectColumns+`
		FROM calendars WHERE user_id = ? AND provider = 'google' AND google_connection_id = ?
		ORDER BY is_primary DESC, summary ASC, id ASC`, userID, connectionID)
}

// ListCalendarsForMicrosoftConnection returns the calendars of one connected
// Microsoft account.
func (s *Store) ListCalendarsForMicrosoftConnection(ctx context.Context, userID, connectionID int64) ([]Calendar, error) {
	if userID <= 0 || connectionID <= 0 {
		return nil, nil
	}
	return s.queryCalendars(ctx, userID, `SELECT `+calendarSelectColumns+`
		FROM calendars WHERE user_id = ? AND provider = 'microsoft' AND microsoft_connection_id = ?
		ORDER BY is_primary DESC, summary ASC, id ASC`, userID, connectionID)
}

func (s *Store) queryCalendars(ctx context.Context, userID int64, query string, args ...any) ([]Calendar, error) {
	rows, err := s.mustDataDB(ctx, userID).QueryContext(ctx, query, args...)
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
		FROM calendars WHERE user_id = ? AND provider = 'google' AND google_connection_id = ? AND remote_calendar_id = ?`,
		userID, connectionID, googleID))
}

// MicrosoftCalendarUpsert is what the Microsoft calendar-list sync knows about
// a calendar. Like CalendarUpsert it leaves Rolltop's own state alone.
type MicrosoftCalendarUpsert struct {
	ConnectionID     int64
	RemoteCalendarID string
	Summary          string
	Color            string
	AccessRole       string
	IsPrimary        bool
	// OnlineMeetingProviders is what Graph allows in this calendar, in the
	// order it listed them.
	OnlineMeetingProviders []string
	// Selected seeds the visibility of a calendar seen for the first time and
	// is ignored afterwards.
	Selected bool
}

// UpsertMicrosoftCalendar records a calendar the Microsoft list sync returned.
func (s *Store) UpsertMicrosoftCalendar(ctx context.Context, userID int64, in MicrosoftCalendarUpsert) (Calendar, error) {
	remoteID := strings.TrimSpace(in.RemoteCalendarID)
	if userID <= 0 || in.ConnectionID <= 0 || remoteID == "" {
		return Calendar{}, ErrNotFound
	}
	ts := nowUnix()
	_, err := s.mustDataDB(ctx, userID).ExecContext(ctx, `INSERT INTO calendars
			(user_id, provider, google_connection_id, microsoft_connection_id, remote_calendar_id,
			 summary, description, time_zone, color, access_role, is_primary, selected,
			 online_meeting_providers, created_at, updated_at)
		VALUES (?, 'microsoft', 0, ?, ?, ?, '', '', ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(user_id, provider, google_connection_id, microsoft_connection_id, remote_calendar_id) DO UPDATE SET
			summary = excluded.summary,
			color = excluded.color,
			access_role = excluded.access_role,
			is_primary = excluded.is_primary,
			online_meeting_providers = excluded.online_meeting_providers,
			updated_at = excluded.updated_at`,
		userID, in.ConnectionID, trimLimit(remoteID, 600), trimLimit(in.Summary, 300),
		trimLimit(in.Color, 20), trimLimit(strings.ToLower(strings.TrimSpace(in.AccessRole)), 40),
		boolInt(in.IsPrimary), boolInt(in.Selected),
		trimLimit(strings.Join(cleanWords(in.OnlineMeetingProviders), " "), 300), ts, ts)
	if err != nil {
		return Calendar{}, err
	}
	return s.CalendarByMicrosoftID(ctx, userID, in.ConnectionID, remoteID)
}

// CalendarByMicrosoftID loads one calendar by the identifier Graph knows it
// under.
func (s *Store) CalendarByMicrosoftID(ctx context.Context, userID, connectionID int64, remoteCalendarID string) (Calendar, error) {
	remoteID := strings.TrimSpace(remoteCalendarID)
	if userID <= 0 || connectionID <= 0 || remoteID == "" {
		return Calendar{}, ErrNotFound
	}
	return scanCalendar(s.mustDataDB(ctx, userID).QueryRowContext(ctx, `SELECT `+calendarSelectColumns+`
		FROM calendars WHERE user_id = ? AND provider = 'microsoft' AND microsoft_connection_id = ? AND remote_calendar_id = ?`,
		userID, connectionID, remoteID))
}

// MarkCalendarFullRead records that the whole window of a Microsoft calendar
// was just read, which is what paces the next full read.
func (s *Store) MarkCalendarFullRead(ctx context.Context, userID, calendarID int64, at time.Time) error {
	if userID <= 0 || calendarID <= 0 {
		return ErrNotFound
	}
	res, err := s.mustDataDB(ctx, userID).ExecContext(ctx,
		`UPDATE calendars SET full_read_at = ?, updated_at = ? WHERE user_id = ? AND id = ?`,
		timeUnix(at), nowUnix(), userID, calendarID)
	if err != nil {
		return err
	}
	return requireCalendarRow(res)
}

// cleanWords trims and drops empty entries and duplicates, keeping order.
func cleanWords(values []string) []string {
	out := make([]string, 0, len(values))
	seen := map[string]bool{}
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	return out
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

// SetCalendarCopyTarget makes one calendar the reader's second calendar, or
// stops it being one. Choosing a calendar takes the mark off whichever calendar
// carried it before, in the same transaction, so there is never a moment with
// two -- the unique index would refuse it anyway.
func (s *Store) SetCalendarCopyTarget(ctx context.Context, userID, calendarID int64, on bool) error {
	if userID <= 0 || calendarID <= 0 {
		return ErrNotFound
	}
	db, err := s.dataDB(ctx, userID)
	if err != nil {
		return err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	ts := nowUnix()
	if on {
		if _, err := tx.ExecContext(ctx,
			`UPDATE calendars SET copy_target = 0, updated_at = ? WHERE user_id = ? AND copy_target = 1 AND id <> ?`,
			ts, userID, calendarID); err != nil {
			return err
		}
	}
	res, err := tx.ExecContext(ctx,
		`UPDATE calendars SET copy_target = ?, updated_at = ? WHERE user_id = ? AND id = ?`,
		boolInt(on), ts, userID, calendarID)
	if err != nil {
		return err
	}
	if err := requireCalendarRow(res); err != nil {
		return err
	}
	return tx.Commit()
}

// CopyTargetCalendar returns the reader's second calendar. A user who never
// chose one reads as ErrNotFound, which is the answer "no copy is offered".
func (s *Store) CopyTargetCalendar(ctx context.Context, userID int64) (Calendar, error) {
	if userID <= 0 {
		return Calendar{}, ErrNotFound
	}
	return scanCalendar(s.mustDataDB(ctx, userID).QueryRowContext(ctx, `SELECT `+calendarSelectColumns+`
		FROM calendars WHERE user_id = ? AND copy_target = 1`, userID))
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

// deleteCalendarsForConnection removes every calendar of one Google
// connection. It takes an execer so the disconnect path can run it inside the
// same transaction that removes the connection itself.
func deleteCalendarsForConnection(ctx context.Context, db execer, userID, connectionID int64) error {
	return deleteProviderCalendars(ctx, db, userID, `provider = 'google' AND google_connection_id = ?`, connectionID)
}

// deleteCalendarsForMicrosoftConnection is the Microsoft counterpart.
func deleteCalendarsForMicrosoftConnection(ctx context.Context, db execer, userID, connectionID int64) error {
	return deleteProviderCalendars(ctx, db, userID, `provider = 'microsoft' AND microsoft_connection_id = ?`, connectionID)
}

func deleteProviderCalendars(ctx context.Context, db execer, userID int64, predicate string, connectionID int64) error {
	if _, err := db.ExecContext(ctx,
		`DELETE FROM calendar_events WHERE user_id = ? AND calendar_id IN
			(SELECT id FROM calendars WHERE user_id = ? AND `+predicate+`)`,
		userID, userID, connectionID); err != nil {
		return err
	}
	_, err := db.ExecContext(ctx,
		`DELETE FROM calendars WHERE user_id = ? AND `+predicate, userID, connectionID)
	return err
}

// CountCalendarsForConnection reports how many calendars one Google connection
// currently mirrors, for the settings page.
func (s *Store) CountCalendarsForConnection(ctx context.Context, userID, connectionID int64) (int, error) {
	return s.countProviderCalendars(ctx, userID, `provider = 'google' AND google_connection_id = ?`, connectionID)
}

// CountCalendarsForMicrosoftConnection is the Microsoft counterpart.
func (s *Store) CountCalendarsForMicrosoftConnection(ctx context.Context, userID, connectionID int64) (int, error) {
	return s.countProviderCalendars(ctx, userID, `provider = 'microsoft' AND microsoft_connection_id = ?`, connectionID)
}

func (s *Store) countProviderCalendars(ctx context.Context, userID int64, predicate string, connectionID int64) (int, error) {
	if userID <= 0 || connectionID <= 0 {
		return 0, nil
	}
	var count int
	err := s.mustDataDB(ctx, userID).QueryRowContext(ctx,
		`SELECT COUNT(*) FROM calendars WHERE user_id = ? AND `+predicate,
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
	var isPrimary, selected, copyTarget int64
	var windowStart, lastSync, lastSuccess, fullRead int64
	var meetingProviders string
	if err := dest.Scan(&calendar.ID, &calendar.UserID, &calendar.GoogleConnectionID,
		&calendar.RemoteCalendarID, &calendar.Summary, &calendar.Description,
		&calendar.TimeZone, &calendar.Color, &calendar.AccessRole, &isPrimary, &selected,
		&calendar.SyncToken, &windowStart, &lastSync, &lastSuccess,
		&calendar.Status, &calendar.StatusDetail, &copyTarget,
		&calendar.Provider, &calendar.MicrosoftConnectionID, &fullRead, &meetingProviders); err != nil {
		return Calendar{}, err
	}
	calendar.FullReadAt = unixTime(fullRead)
	calendar.OnlineMeetingProviders = strings.Fields(meetingProviders)
	calendar.IsPrimary = isPrimary != 0
	calendar.Selected = selected != 0
	calendar.CopyTarget = copyTarget != 0
	calendar.WindowStartAt = unixTime(windowStart)
	calendar.LastSyncAt = unixTime(lastSync)
	calendar.LastSuccessAt = unixTime(lastSuccess)
	return calendar, nil
}
