// File overview: Google Calendar storage. One row per subscribed calendar with
// its own delta cursor, one row per event instance, and a per-connection record
// of how the calendar list itself last synced.

package store

func userGoogleCalendarMigrationSet() migrationSet {
	return migrationSet{
		Scope:   "user",
		Version: UserSchemaVersion033,
		Label:   "user schema 033 google calendar",
		Statements: []string{
			// One row per calendar of one connected account. The sync cursor sits
			// here rather than in a companion table because Google issues it per
			// calendar: a token from one calendar is meaningless for another, and
			// a calendar unsubscribed and re-added must start over on its own.
			`CREATE TABLE IF NOT EXISTS calendars (
				id INTEGER PRIMARY KEY AUTOINCREMENT,
				user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
				google_connection_id INTEGER NOT NULL,
				google_calendar_id TEXT NOT NULL,
				summary TEXT NOT NULL DEFAULT '',
				description TEXT NOT NULL DEFAULT '',
				time_zone TEXT NOT NULL DEFAULT '',
				color TEXT NOT NULL DEFAULT '',
				access_role TEXT NOT NULL DEFAULT '',
				is_primary INTEGER NOT NULL DEFAULT 0,
				-- Whether the calendar is drawn in Rolltop. It starts from the
				-- account's own selection so a calendar the user already hid at
				-- Google does not arrive switched on, and is a local choice from
				-- then on.
				selected INTEGER NOT NULL DEFAULT 1,
				sync_token TEXT NOT NULL DEFAULT '',
				-- The lower bound of the mirrored window. Google encodes the
				-- window into the sync token, so a delta only ever describes
				-- events inside it; keeping the bound lets the UI say what is not
				-- mirrored instead of showing an empty week as if it were free.
				window_start_at INTEGER NOT NULL DEFAULT 0,
				last_sync_at INTEGER NOT NULL DEFAULT 0,
				last_success_at INTEGER NOT NULL DEFAULT 0,
				status TEXT NOT NULL DEFAULT '',
				status_detail TEXT NOT NULL DEFAULT '',
				created_at INTEGER NOT NULL,
				updated_at INTEGER NOT NULL
			)`,
			`CREATE UNIQUE INDEX IF NOT EXISTS idx_calendars_google
				ON calendars(user_id, google_connection_id, google_calendar_id)`,
			// Events are stored as the instances Google expands for us
			// (singleEvents=true), so a weekly meeting is many rows and no
			// recurrence rule has to be evaluated here. recurring_event_id keeps
			// the tie back to the series for the UI.
			`CREATE TABLE IF NOT EXISTS calendar_events (
				id INTEGER PRIMARY KEY AUTOINCREMENT,
				user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
				calendar_id INTEGER NOT NULL REFERENCES calendars(id) ON DELETE CASCADE,
				external_id TEXT NOT NULL,
				etag TEXT NOT NULL DEFAULT '',
				-- The cross-calendar identity of the meeting. An invitation that
				-- arrives by mail carries this, which is what a later phase needs
				-- to find the event an RSVP belongs to.
				ical_uid TEXT NOT NULL DEFAULT '',
				summary TEXT NOT NULL DEFAULT '',
				description TEXT NOT NULL DEFAULT '',
				location TEXT NOT NULL DEFAULT '',
				status TEXT NOT NULL DEFAULT '',
				-- Half-open [start_at, end_at) in unix seconds. For an all-day
				-- event these are midnight UTC of Google's plain dates: an
				-- all-day event has no instant, and anchoring it to the viewer's
				-- zone would move it to the previous day for anyone west of it.
				-- Everything reading an all-day row must format it in UTC.
				start_at INTEGER NOT NULL DEFAULT 0,
				end_at INTEGER NOT NULL DEFAULT 0,
				all_day INTEGER NOT NULL DEFAULT 0,
				time_zone TEXT NOT NULL DEFAULT '',
				recurring_event_id TEXT NOT NULL DEFAULT '',
				organizer_email TEXT NOT NULL DEFAULT '',
				organizer_name TEXT NOT NULL DEFAULT '',
				-- The attendee list as Google returned it. It is display data
				-- with no query against it, and normalizing it into its own table
				-- would buy a join for every event in a week.
				attendees_json TEXT NOT NULL DEFAULT '',
				my_response TEXT NOT NULL DEFAULT '',
				html_link TEXT NOT NULL DEFAULT '',
				remote_updated_at INTEGER NOT NULL DEFAULT 0,
				created_at INTEGER NOT NULL,
				updated_at INTEGER NOT NULL
			)`,
			`CREATE UNIQUE INDEX IF NOT EXISTS idx_calendar_events_external
				ON calendar_events(user_id, calendar_id, external_id)`,
			// The week view asks one question: which events of these calendars
			// touch this range. Ordering the index by calendar and then start
			// makes that a range scan per visible calendar.
			`CREATE INDEX IF NOT EXISTS idx_calendar_events_window
				ON calendar_events(user_id, calendar_id, start_at)`,
			// Per-connection state for the calendar list itself, which is a
			// separate sync from any single calendar's events and is what the
			// settings page reports.
			`CREATE TABLE IF NOT EXISTS google_calendar_sync (
				user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
				connection_id INTEGER NOT NULL,
				sync_token TEXT NOT NULL DEFAULT '',
				last_sync_at INTEGER NOT NULL DEFAULT 0,
				last_success_at INTEGER NOT NULL DEFAULT 0,
				status TEXT NOT NULL DEFAULT '',
				status_detail TEXT NOT NULL DEFAULT '',
				created_at INTEGER NOT NULL,
				updated_at INTEGER NOT NULL,
				PRIMARY KEY (user_id, connection_id)
			)`,
		},
	}
}
