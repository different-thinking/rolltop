// File overview: Calendar reminders -- the popup reminders the providers hold
// on an event, turned into durable, user-scoped reminder events when they come
// due. The feed and its cursors have the shape snooze reminders have, so Web
// Push and the native clients read it the same way.

package store

import (
	"context"
	"database/sql"
	"errors"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	// CalendarRemindersDefault marks an event that follows its calendar's
	// default reminders (Google's reminders.useDefault).
	CalendarRemindersDefault = "default"
	// CalendarRemindersNone marks an event the reminders were switched off on.
	CalendarRemindersNone = "none"
	// maxCalendarReminderMinutes is the furthest ahead a reminder may be set:
	// four weeks, Google's own limit. Anything beyond it is dropped rather than
	// widening the window a pass has to read.
	maxCalendarReminderMinutes  = 4 * 7 * 24 * 60
	maxCalendarReminderPageSize = 200
	// calendarReminderLateWindow bounds how late a reminder may still fire. A
	// server that was down past it skips the reminder rather than delivering
	// it stale, and -- what matters more -- the first pass after an upgrade
	// does not fire every "one day before" reminder of the coming day at
	// once.
	calendarReminderLateWindow = time.Hour
	// calendarReminderStartGrace is how long after an event's start a
	// reminder for it is still worth delivering.
	calendarReminderStartGrace = 5 * time.Minute
	// calendarReminderEventRetention is how long a recorded reminder is kept
	// for the feed. The push cursors have long moved past it by then.
	calendarReminderEventRetention = 30 * 24 * time.Hour
)

// CalendarReminderEvent is one reminder that came due, recorded once per
// (event, start, minutes) so a pass is idempotent and an event that moves
// reminds again at its new time.
type CalendarReminderEvent struct {
	ID            int64
	UserID        int64
	EventID       int64
	CalendarID    int64
	StartAt       time.Time
	EndAt         time.Time
	AllDay        bool
	MinutesBefore int
	Summary       string
	Location      string
	// TimeZone is the zone the event was entered in, for a client that wants
	// to say the time the way the organizer wrote it.
	TimeZone string
	// SnoozeID names the "remind me later" this reminder fired for, zero for
	// a reminder the provider holds. It is part of the row's identity so a
	// put-off reminder landing on the same minute as the provider's is still
	// delivered. MinutesBefore may be negative on such a row: the reader may
	// ask to be reminded after the event has started.
	SnoozeID  int64
	DueAt     time.Time
	CreatedAt time.Time
}

// EncodeCalendarReminders renders what a provider said about an event's popup
// reminders. useDefault wins: a Google event following its calendar's default
// carries no minutes of its own. Minutes are deduplicated, sorted and bounded.
func EncodeCalendarReminders(useDefault bool, minutes []int) string {
	if useDefault {
		return CalendarRemindersDefault
	}
	cleaned := cleanReminderMinutes(minutes)
	if len(cleaned) == 0 {
		return CalendarRemindersNone
	}
	parts := make([]string, 0, len(cleaned))
	for _, m := range cleaned {
		parts = append(parts, strconv.Itoa(m))
	}
	return strings.Join(parts, ",")
}

// DecodeCalendarReminders reads the encoding back. A row whose reminders were
// never read ("") follows its calendar's default, which is the common case for
// a Google event and the only sensible guess for one.
func DecodeCalendarReminders(raw string) (useDefault bool, minutes []int) {
	raw = strings.TrimSpace(raw)
	switch raw {
	case "", CalendarRemindersDefault:
		return true, nil
	case CalendarRemindersNone:
		return false, nil
	}
	var out []int
	for _, part := range strings.Split(raw, ",") {
		value, err := strconv.Atoi(strings.TrimSpace(part))
		if err != nil {
			continue
		}
		out = append(out, value)
	}
	return false, cleanReminderMinutes(out)
}

// EffectiveCalendarReminders resolves the minutes an event actually reminds at:
// its own, or its calendar's default when it follows that.
func EffectiveCalendarReminders(event CalendarEvent, calendar Calendar) []int {
	useDefault, minutes := DecodeCalendarReminders(event.Reminders)
	if !useDefault {
		return minutes
	}
	// A calendar default that is itself "" or "default" decodes to no minutes,
	// which is right: a calendar whose default was never read reminds of
	// nothing rather than guessing.
	_, defaults := DecodeCalendarReminders(calendar.DefaultReminders)
	return defaults
}

func cleanReminderMinutes(minutes []int) []int {
	seen := map[int]bool{}
	out := make([]int, 0, len(minutes))
	for _, m := range minutes {
		if m < 0 || m > maxCalendarReminderMinutes || seen[m] {
			continue
		}
		seen[m] = true
		out = append(out, m)
	}
	sort.Ints(out)
	return out
}

// CalendarReminderStart is the instant an event's reminders count back from.
// A timed event starts when it starts. An all-day event has no instant: it is
// stored at midnight UTC of its date, and the providers count its reminders
// back from midnight of that date in the zone it was entered in -- Google's
// "the day before at 9:00" is 900 minutes before that midnight -- so the
// event's own zone is read first, then its calendar's, then UTC.
func CalendarReminderStart(event CalendarEvent, calendar Calendar) time.Time {
	if !event.AllDay {
		return event.StartAt
	}
	loc := time.UTC
	for _, name := range []string{event.TimeZone, calendar.TimeZone} {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if parsed, err := time.LoadLocation(name); err == nil {
			loc = parsed
			break
		}
	}
	day := event.StartAt.UTC()
	return time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, loc)
}

// RecordDueCalendarReminderEvents records every reminder that has come due for
// one user and reports the earliest one still ahead, so the scheduler can sleep
// until it. Only the calendars the reader draws are read (selected, which a
// hidden calendar never is), and a travel block, a copy of a linked event, a
// cancelled event or one the reader declined never reminds: the entered copy
// of a pair carries the reminder for all of them. Re-running is idempotent;
// the unique key on (event, start, minutes) is what makes it so.
func (s *Store) RecordDueCalendarReminderEvents(ctx context.Context, userID int64, now time.Time, limit int) ([]CalendarReminderEvent, time.Time, error) {
	if userID <= 0 {
		return nil, time.Time{}, errors.New("invalid user id")
	}
	if limit <= 0 || limit > maxCalendarReminderPageSize {
		limit = 100
	}
	now = now.UTC()
	calendars, err := s.ListCalendars(ctx, userID)
	if err != nil {
		return nil, time.Time{}, err
	}
	byID := map[int64]Calendar{}
	ids := make([]int64, 0, len(calendars))
	for _, calendar := range calendars {
		if !calendar.Selected || !calendar.Listed {
			continue
		}
		byID[calendar.ID] = calendar
		ids = append(ids, calendar.ID)
	}
	if len(ids) == 0 {
		return nil, time.Time{}, s.pruneCalendarReminderEvents(ctx, userID, now)
	}
	// The window is read in UTC against the stored start, which for an all-day
	// event is up to a day away from the midnight its reminders count from;
	// the margins cover that and the exact test happens below.
	args := make([]any, 0, len(ids)+4)
	args = append(args, userID)
	for _, id := range ids {
		args = append(args, id)
	}
	args = append(args,
		timeUnix(now.Add(-calendarReminderStartGrace-36*time.Hour)),
		timeUnix(now.Add(time.Duration(maxCalendarReminderMinutes)*time.Minute+36*time.Hour)),
		CalendarEventStatusCancelled, CalendarResponseDeclined, CalendarRemindersNone)
	rows, err := s.mustDataDB(ctx, userID).QueryContext(ctx, `SELECT `+calendarReminderCandidateColumns+`
		FROM calendar_events
		WHERE user_id = ? AND calendar_id IN (`+sqlPlaceholders(len(ids))+`)
			AND start_at > ? AND start_at <= ?
			AND link_travel = '' AND (link_key = '' OR link_primary <> 0)
			AND status <> ? AND my_response <> ? AND reminders <> ?
		ORDER BY start_at ASC, id ASC`, args...)
	if err != nil {
		return nil, time.Time{}, err
	}
	events, err := collectCalendarReminderCandidates(rows)
	if err != nil {
		return nil, time.Time{}, err
	}
	var next time.Time
	noteNext := func(at time.Time) {
		if next.IsZero() || at.Before(next) {
			next = at
		}
	}
	recorded := make([]CalendarReminderEvent, 0)
candidates:
	for _, event := range events {
		calendar := byID[event.CalendarID]
		start := CalendarReminderStart(event, calendar)
		if !start.After(now.Add(-calendarReminderStartGrace)) {
			continue
		}
		for _, minutes := range EffectiveCalendarReminders(event, calendar) {
			fireAt := start.Add(-time.Duration(minutes) * time.Minute)
			if fireAt.After(now) {
				noteNext(fireAt)
				continue
			}
			if fireAt.Before(now.Add(-calendarReminderLateWindow)) {
				continue
			}
			if len(recorded) >= limit {
				// Whatever is left is due now; the caller comes straight back.
				noteNext(now)
				break candidates
			}
			reminder, inserted, err := s.insertCalendarReminderEvent(ctx, userID, event, minutes, 0, fireAt, now)
			if err != nil {
				return recorded, next, err
			}
			if inserted {
				recorded = append(recorded, reminder)
			}
		}
	}
	snoozed, snoozeNext, err := s.recordDueCalendarReminderSnoozes(ctx, userID, now, limit-len(recorded))
	if err != nil {
		return recorded, next, err
	}
	recorded = append(recorded, snoozed...)
	if !snoozeNext.IsZero() {
		noteNext(snoozeNext)
	}
	return recorded, next, s.pruneCalendarReminderEvents(ctx, userID, now)
}

// SnoozeCalendarReminder puts an event's reminder off to a time of the
// reader's choosing: the next pass fires a reminder for it then, whatever the
// provider's reminders say, and only one such wait exists per event -- a new
// choice replaces the old. The event has to be the user's; the time has to be
// ahead, and is not bound to the event's start, because "remind me five
// minutes in" is a reasonable thing to ask of a meeting one is late for.
func (s *Store) SnoozeCalendarReminder(ctx context.Context, userID, eventID int64, remindAt time.Time) error {
	if userID <= 0 || eventID <= 0 || remindAt.IsZero() {
		return ErrNotFound
	}
	if _, err := s.CalendarEvent(ctx, userID, eventID); err != nil {
		return err
	}
	ts := nowUnix()
	_, err := s.mustDataDB(ctx, userID).ExecContext(ctx, `INSERT INTO calendar_reminder_snoozes
			(user_id, event_id, remind_at, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(user_id, event_id) DO UPDATE SET
			remind_at = excluded.remind_at,
			updated_at = excluded.updated_at`,
		userID, eventID, timeUnix(remindAt.UTC()), ts, ts)
	return err
}

// recordDueCalendarReminderSnoozes fires every put-off reminder whose time has
// come and drops its wait, and reports the earliest wait still ahead. A wait
// fires however late it is: the reader asked for it by name. One whose event
// is gone is dropped unfired, and a declined or cancelled event is not asked
// about again either -- the reader put the reminder off, not the decision.
func (s *Store) recordDueCalendarReminderSnoozes(ctx context.Context, userID int64, now time.Time, limit int) ([]CalendarReminderEvent, time.Time, error) {
	db := s.mustDataDB(ctx, userID)
	if _, err := db.ExecContext(ctx, `DELETE FROM calendar_reminder_snoozes
		WHERE user_id = ? AND NOT EXISTS (
			SELECT 1 FROM calendar_events e WHERE e.user_id = calendar_reminder_snoozes.user_id AND e.id = calendar_reminder_snoozes.event_id
		)`, userID); err != nil {
		return nil, time.Time{}, err
	}
	var next time.Time
	var nextUnix int64
	if err := db.QueryRowContext(ctx, `SELECT COALESCE(MIN(remind_at), 0) FROM calendar_reminder_snoozes
		WHERE user_id = ? AND remind_at > ?`, userID, timeUnix(now)).Scan(&nextUnix); err != nil {
		return nil, time.Time{}, err
	}
	next = unixTime(nextUnix)
	if limit <= 0 {
		return nil, now, nil
	}
	rows, err := db.QueryContext(ctx, `SELECT sn.id, sn.remind_at, `+qualifiedColumns("e", calendarEventSelectColumns)+`
		FROM calendar_reminder_snoozes sn
		JOIN calendar_events e ON e.user_id = sn.user_id AND e.id = sn.event_id
		WHERE sn.user_id = ? AND sn.remind_at <= ?
		ORDER BY sn.remind_at ASC, sn.id ASC LIMIT ?`, userID, timeUnix(now), limit)
	if err != nil {
		return nil, time.Time{}, err
	}
	type dueSnooze struct {
		id       int64
		remindAt time.Time
		event    CalendarEvent
	}
	var due []dueSnooze
	for rows.Next() {
		var item dueSnooze
		var remindAt int64
		event, err := scanCalendarEventWithPrefix(rows, &item.id, &remindAt)
		if err != nil {
			_ = rows.Close()
			return nil, time.Time{}, err
		}
		item.remindAt = unixTime(remindAt)
		item.event = event
		due = append(due, item)
	}
	if err := rows.Close(); err != nil {
		return nil, time.Time{}, err
	}
	if err := rows.Err(); err != nil {
		return nil, time.Time{}, err
	}
	recorded := make([]CalendarReminderEvent, 0, len(due))
	dropWait := func(id int64) error {
		_, err := db.ExecContext(ctx, `DELETE FROM calendar_reminder_snoozes WHERE user_id = ? AND id = ?`, userID, id)
		return err
	}
	for _, item := range due {
		if item.event.Status == CalendarEventStatusCancelled || item.event.MyResponse == CalendarResponseDeclined {
			if err := dropWait(item.id); err != nil {
				return recorded, next, err
			}
			continue
		}
		// The reminder is recorded before the wait is dropped, not after: a
		// failure between the two then leaves a wait the next pass fires
		// again, where the other order lost the one reminder the reader asked
		// for by name. The snooze's id in the row's key is what makes the
		// second attempt a no-op rather than a second reminder.
		minutes := int(item.event.StartAt.Sub(item.remindAt).Round(time.Minute) / time.Minute)
		reminder, inserted, err := s.insertCalendarReminderEvent(ctx, userID, item.event, minutes, item.id, item.remindAt, now)
		if err != nil {
			return recorded, next, err
		}
		if err := dropWait(item.id); err != nil {
			return recorded, next, err
		}
		if inserted {
			recorded = append(recorded, reminder)
		}
	}
	return recorded, next, nil
}

// calendarReminderCandidateColumns is what the reminder pass needs of an
// event: enough to place its reminders and to describe it in the feed. The
// description and the guest list stay in the table -- the pass reads a
// four-week window of every drawn calendar on every run, and an agenda is the
// bulk of a row. Status and the reader's answer are filtered in the query.
const calendarReminderCandidateColumns = `id, calendar_id, start_at, end_at, all_day, time_zone, summary, location, reminders`

func collectCalendarReminderCandidates(rows *sql.Rows) ([]CalendarEvent, error) {
	defer rows.Close()
	var out []CalendarEvent
	for rows.Next() {
		var event CalendarEvent
		var startAt, endAt, allDay int64
		if err := rows.Scan(&event.ID, &event.CalendarID, &startAt, &endAt, &allDay,
			&event.TimeZone, &event.Summary, &event.Location, &event.Reminders); err != nil {
			return nil, err
		}
		event.StartAt = unixTime(startAt)
		event.EndAt = unixTime(endAt)
		event.AllDay = allDay != 0
		out = append(out, event)
	}
	return out, rows.Err()
}

// qualifiedColumns prefixes every name of a comma-separated column list with a
// table alias, for a select that joins the table onto another.
func qualifiedColumns(alias, columns string) string {
	parts := strings.Split(columns, ",")
	for i, part := range parts {
		parts[i] = alias + "." + strings.TrimSpace(part)
	}
	return strings.Join(parts, ", ")
}

// insertCalendarReminderEvent records one reminder and answers with the row as
// a read would return it -- built from what was written, at the second
// precision the table keeps, rather than read back: a pass may record a
// hundred of these and a query per row was the bulk of what it cost.
func (s *Store) insertCalendarReminderEvent(ctx context.Context, userID int64, event CalendarEvent, minutes int, snoozeID int64, dueAt, now time.Time) (CalendarReminderEvent, bool, error) {
	reminder := CalendarReminderEvent{
		UserID:        userID,
		EventID:       event.ID,
		CalendarID:    event.CalendarID,
		StartAt:       unixTime(timeUnix(event.StartAt)),
		EndAt:         unixTime(timeUnix(event.EndAt)),
		AllDay:        event.AllDay,
		MinutesBefore: minutes,
		Summary:       trimLimit(event.Summary, 500),
		Location:      trimLimit(event.Location, 1000),
		TimeZone:      trimLimit(event.TimeZone, 100),
		SnoozeID:      snoozeID,
		DueAt:         unixTime(timeUnix(dueAt)),
		CreatedAt:     unixTime(timeUnix(now)),
	}
	err := s.mustDataDB(ctx, userID).QueryRowContext(ctx, `INSERT INTO calendar_reminder_events
			(user_id, event_id, calendar_id, start_at, end_at, all_day, minutes_before,
			 summary, location, time_zone, snooze_id, due_at, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT DO NOTHING
		RETURNING id`,
		userID, reminder.EventID, reminder.CalendarID, timeUnix(reminder.StartAt), timeUnix(reminder.EndAt), boolInt(reminder.AllDay),
		reminder.MinutesBefore, reminder.Summary, reminder.Location, reminder.TimeZone,
		reminder.SnoozeID, timeUnix(reminder.DueAt), timeUnix(reminder.CreatedAt)).Scan(&reminder.ID)
	if errors.Is(err, sql.ErrNoRows) {
		return CalendarReminderEvent{}, false, nil
	}
	if err != nil {
		return CalendarReminderEvent{}, false, err
	}
	return reminder, true, nil
}

func (s *Store) pruneCalendarReminderEvents(ctx context.Context, userID int64, now time.Time) error {
	_, err := s.mustDataDB(ctx, userID).ExecContext(ctx, `DELETE FROM calendar_reminder_events
		WHERE user_id = ? AND created_at < ?`, userID, timeUnix(now.Add(-calendarReminderEventRetention)))
	return err
}

const calendarReminderEventColumns = `id, user_id, event_id, calendar_id, start_at, end_at, all_day,
	minutes_before, summary, location, time_zone, snooze_id, due_at, created_at`

// CalendarReminderEventsAfter returns the retained reminders after a cursor,
// oldest first, their count, and the user's current high-water mark. Unlike
// the snooze feed, whose page is the newest few behind one summary
// notification, every reminder here is delivered on its own, so the page is
// the **oldest** past the cursor: a caller that advances to the last id it was
// handed and comes back for the rest (count > len) skips nothing, where the
// newest few would have left the oldest behind the cursor for good.
func (s *Store) CalendarReminderEventsAfter(ctx context.Context, userID, afterID int64, limit int) ([]CalendarReminderEvent, int, int64, error) {
	if userID <= 0 || afterID < 0 {
		return nil, 0, 0, errors.New("invalid calendar reminder cursor")
	}
	if limit <= 0 || limit > 20 {
		limit = 5
	}
	db, err := s.dataDB(ctx, userID)
	if err != nil {
		return nil, 0, 0, err
	}
	var count int
	var cursor int64
	if err := db.QueryRowContext(ctx, `SELECT
		(SELECT COUNT(*) FROM calendar_reminder_events WHERE user_id = ? AND id > ?),
		(SELECT COALESCE(MAX(id), 0) FROM calendar_reminder_events WHERE user_id = ?)`,
		userID, afterID, userID).Scan(&count, &cursor); err != nil {
		return nil, 0, 0, err
	}
	rows, err := db.QueryContext(ctx, `SELECT `+calendarReminderEventColumns+`
		FROM calendar_reminder_events
		WHERE user_id = ? AND id > ? AND id <= ? ORDER BY id ASC LIMIT ?`, userID, afterID, cursor, limit)
	if err != nil {
		return nil, 0, 0, err
	}
	defer rows.Close()
	events := make([]CalendarReminderEvent, 0, limit)
	for rows.Next() {
		event, err := scanCalendarReminderEvent(rows)
		if err != nil {
			return nil, 0, 0, err
		}
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, 0, err
	}
	return events, count, cursor, nil
}

// LatestCalendarReminderEventID establishes a silent baseline for a new client.
func (s *Store) LatestCalendarReminderEventID(ctx context.Context, userID int64) (int64, error) {
	if userID <= 0 {
		return 0, errors.New("invalid user id")
	}
	var cursor int64
	err := s.mustDataDB(ctx, userID).QueryRowContext(ctx, `SELECT COALESCE(MAX(id), 0)
		FROM calendar_reminder_events WHERE user_id = ?`, userID).Scan(&cursor)
	return cursor, err
}

func scanCalendarReminderEvent(row scanDest) (CalendarReminderEvent, error) {
	var event CalendarReminderEvent
	var start, end, allDay, due, created int64
	err := row.Scan(&event.ID, &event.UserID, &event.EventID, &event.CalendarID, &start, &end, &allDay,
		&event.MinutesBefore, &event.Summary, &event.Location, &event.TimeZone, &event.SnoozeID, &due, &created)
	event.StartAt = unixTime(start)
	event.EndAt = unixTime(end)
	event.AllDay = allDay != 0
	event.DueAt = unixTime(due)
	event.CreatedAt = unixTime(created)
	return event, err
}
