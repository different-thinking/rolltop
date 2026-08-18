// File overview: The calendar sync loop. Google is the leading system, so this
// pulls its state down and makes Rolltop match it: the account's calendar list
// first, then the events of each calendar the user has switched on. The reverse
// direction -- local edits travelling to Google -- lives in writeback.go.

package googlecalendar

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"rolltop/backend/googletoken"
	"rolltop/backend/store"
)

// syncTimeout bounds one connection's sync. A first run over a year of a busy
// calendar is paged and can genuinely take minutes; anything past this is stuck.
const syncTimeout = 10 * time.Minute

// DefaultWindow is how far back a first read of a calendar goes. Google encodes
// the window into the cursor it hands back, so this is chosen once per calendar
// and every later delta inherits it. A year covers "what did we decide in that
// meeting" without mirroring a decade of standups.
const DefaultWindow = 365 * 24 * time.Hour

// ErrScopeMissing reports a connection authorized before calendar sync existed.
// Its grant still covers mail and contacts, so this is a prompt to re-authorize
// rather than a failure of the connection as a whole.
var ErrScopeMissing = errors.New("this Google account has not granted access to calendars")

// Result summarizes one connection's sync for the caller and the log.
type Result struct {
	ConnectionID int64
	// Calendars is how many calendars the account currently offers.
	Calendars int
	// Synced is how many of those had their events read, which is the ones the
	// user has switched on.
	Synced  int
	Created int
	Updated int
	Deleted int
	// FullSync reports whether any calendar had to be read in full, which is
	// what happens on a first run and after Google expires a cursor.
	FullSync bool
}

// ConnectionLister is the slice of the Google auth manager the sync needs.
type ConnectionLister interface {
	List(ctx context.Context, userID int64) ([]store.GoogleConnection, error)
	Get(ctx context.Context, userID, connectionID int64) (store.GoogleConnection, error)
}

// Syncer mirrors Google calendars into one Rolltop installation.
type Syncer struct {
	Store  *store.Store
	Client *Client
	// Tokens mints access tokens and is the same manager mail and contacts use,
	// so a refresh triggered here is shared with them rather than racing them.
	Tokens googletoken.TokenSource
	// Connections lists a user's Google accounts. It is an interface so the sync
	// does not depend on the whole auth manager.
	Connections ConnectionLister
	// ScopeGranted reports whether a connection may talk to the Calendar API.
	// Injected rather than imported so this package does not depend on the OAuth
	// configuration.
	ScopeGranted func(store.GoogleConnection) bool
	// Window overrides DefaultWindow. Tests set it to keep fixtures small.
	Window time.Duration
	// Now is the clock. It is a field so a test can place a window boundary
	// without waiting for one.
	Now func() time.Time
}

// NewSyncer wires a syncer for production use. The process builds one in main
// and the web server builds one when it owns its own auth manager; a struct
// literal at each site would let a field added here reach only one of them.
func NewSyncer(db *store.Store, tokens googletoken.TokenSource, connections ConnectionLister, scope string) *Syncer {
	return &Syncer{
		Store:       db,
		Client:      NewClient(),
		Tokens:      tokens,
		Connections: connections,
		ScopeGranted: func(connection store.GoogleConnection) bool {
			return connection.HasScope(scope)
		},
	}
}

func (s *Syncer) ready() error {
	if s == nil || s.Store == nil || s.Tokens == nil || s.Connections == nil {
		return errors.New("google calendar sync is not configured")
	}
	return nil
}

func (s *Syncer) client() *Client {
	if s.Client != nil {
		return s.Client
	}
	s.Client = NewClient()
	return s.Client
}

func (s *Syncer) scopeGranted(connection store.GoogleConnection) bool {
	if s.ScopeGranted == nil {
		return true
	}
	return s.ScopeGranted(connection)
}

func (s *Syncer) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

func (s *Syncer) window() time.Duration {
	if s.Window > 0 {
		return s.Window
	}
	return DefaultWindow
}

// SyncUser syncs every eligible connection of one user and returns the results
// of those that ran. A connection that cannot sync -- no scope, awaiting
// re-consent -- is skipped rather than failing the others.
func (s *Syncer) SyncUser(ctx context.Context, userID int64) ([]Result, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	connections, err := s.Connections.List(ctx, userID)
	if err != nil {
		return nil, err
	}
	var results []Result
	var firstErr error
	for _, connection := range connections {
		if connection.NeedsReauth() || !s.scopeGranted(connection) {
			continue
		}
		result, err := s.SyncConnection(ctx, userID, connection.ID)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		results = append(results, result)
	}
	return results, firstErr
}

// SyncConnection brings one Google account's calendars into Rolltop. The stored
// sync state is written on every outcome, so a failure is visible in settings
// rather than only in the log.
func (s *Syncer) SyncConnection(ctx context.Context, userID, connectionID int64) (Result, error) {
	if err := s.ready(); err != nil {
		return Result{}, err
	}
	connection, err := s.Connections.Get(ctx, userID, connectionID)
	if err != nil {
		return Result{}, err
	}
	if !s.scopeGranted(connection) {
		return Result{}, ErrScopeMissing
	}
	if connection.NeedsReauth() {
		return Result{}, fmt.Errorf("google connection %d needs re-authorization", connectionID)
	}
	ctx, cancel := context.WithTimeout(ctx, syncTimeout)
	defer cancel()

	result, err := s.run(ctx, userID, connectionID)
	s.recordConnectionOutcome(ctx, userID, connectionID, result, err)
	return result, err
}

// SyncCalendar reads the events of one calendar on its own. Switching a
// calendar on is the case this exists for: its events were never fetched, and
// waiting for the next poll would show the user an empty week they would read
// as "nothing scheduled".
func (s *Syncer) SyncCalendar(ctx context.Context, userID, calendarID int64) error {
	if err := s.ready(); err != nil {
		return err
	}
	calendar, err := s.Store.Calendar(ctx, userID, calendarID)
	if err != nil {
		return err
	}
	connection, err := s.Connections.Get(ctx, userID, calendar.GoogleConnectionID)
	if err != nil {
		return err
	}
	if !s.scopeGranted(connection) {
		return ErrScopeMissing
	}
	if connection.NeedsReauth() {
		return fmt.Errorf("google connection %d needs re-authorization", calendar.GoogleConnectionID)
	}
	ctx, cancel := context.WithTimeout(ctx, syncTimeout)
	defer cancel()
	_, err = s.syncCalendarEvents(ctx, userID, calendar)
	return err
}

// run syncs the calendar list and then the events of every calendar the user
// has switched on.
func (s *Syncer) run(ctx context.Context, userID, connectionID int64) (Result, error) {
	result := Result{ConnectionID: connectionID}
	calendars, err := s.syncCalendarList(ctx, userID, connectionID)
	if err != nil {
		return result, err
	}
	result.Calendars = len(calendars)

	var firstErr error
	for _, calendar := range calendars {
		if !calendar.Selected {
			// A calendar nobody looks at costs a page of requests per poll and
			// a slice of the database. It syncs the moment it is switched on.
			continue
		}
		events, err := s.syncCalendarEvents(ctx, userID, calendar)
		if err != nil {
			// One unreadable calendar must not cost the others their sync: a
			// single shared calendar whose owner revoked access would otherwise
			// stop the account's own appointments from updating.
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		result.Synced++
		result.Created += events.Created
		result.Updated += events.Updated
		result.Deleted += events.Deleted
		result.FullSync = result.FullSync || events.FullSync
	}
	return result, firstErr
}

// syncCalendarList mirrors the account's subscribed calendars and returns them
// as stored.
func (s *Syncer) syncCalendarList(ctx context.Context, userID, connectionID int64) ([]store.Calendar, error) {
	state, err := s.Store.GetGoogleCalendarSync(ctx, userID, connectionID)
	if err != nil {
		return nil, err
	}
	nextToken, err := s.pullCalendarList(ctx, userID, connectionID, state.SyncToken)
	recovered := false
	if errors.Is(err, ErrSyncTokenExpired) && state.SyncToken != "" {
		log.Printf("google calendar sync user_id=%d connection_id=%d calendar list cursor expired, reading the whole list", userID, connectionID)
		nextToken, err = s.pullCalendarList(ctx, userID, connectionID, "")
		recovered = true
	}
	if err != nil {
		return nil, err
	}
	// An answer without a cursor leaves the usable one in place. Not after a
	// recovery, though: there the stored cursor is the very token Google just
	// rejected, and putting it back would fail the same way on every poll.
	if nextToken == "" && !recovered {
		nextToken = state.SyncToken
	}
	now := s.now()
	if err := s.Store.SaveGoogleCalendarSync(ctx, store.GoogleCalendarSync{
		UserID:        userID,
		ConnectionID:  connectionID,
		SyncToken:     nextToken,
		LastSyncAt:    now,
		LastSuccessAt: now,
		Status:        store.CalendarSyncStatusOK,
	}); err != nil {
		return nil, err
	}
	return s.Store.ListCalendarsForConnection(ctx, userID, connectionID)
}

// pullCalendarList walks every page of the calendar list and applies it.
func (s *Syncer) pullCalendarList(ctx context.Context, userID, connectionID int64, syncToken string) (string, error) {
	full := strings.TrimSpace(syncToken) == ""

	// Only a full read can conclude that a calendar Google did not mention is
	// gone; a delta says nothing about the entries it omits.
	known := map[string]int64{}
	if full {
		existing, err := s.Store.ListCalendarsForConnection(ctx, userID, connectionID)
		if err != nil {
			return "", err
		}
		for _, calendar := range existing {
			known[calendar.GoogleCalendarID] = calendar.ID
		}
	}

	pageToken := ""
	nextSyncToken := ""
	for {
		var page CalendarListPage
		err := s.withToken(ctx, userID, connectionID, func(token string) error {
			var callErr error
			page, callErr = s.client().ListCalendars(ctx, token, syncToken, pageToken)
			return callErr
		})
		if err != nil {
			return "", err
		}
		for _, entry := range page.Items {
			googleID := strings.TrimSpace(entry.ID)
			if googleID == "" {
				continue
			}
			delete(known, googleID)
			if entry.Deleted {
				if err := s.removeCalendar(ctx, userID, connectionID, googleID); err != nil {
					return "", err
				}
				continue
			}
			if _, err := s.Store.UpsertCalendar(ctx, userID, ToCalendarUpsert(entry, connectionID)); err != nil {
				return "", err
			}
		}
		if page.NextSyncToken != "" {
			nextSyncToken = page.NextSyncToken
		}
		if page.NextPageToken == "" {
			break
		}
		pageToken = page.NextPageToken
	}

	// Whatever the full read never mentioned is no longer subscribed.
	for _, calendarID := range known {
		if err := s.Store.DeleteCalendar(ctx, userID, calendarID); err != nil && !store.IsNotFound(err) {
			return "", err
		}
	}
	return nextSyncToken, nil
}

func (s *Syncer) removeCalendar(ctx context.Context, userID, connectionID int64, googleID string) error {
	calendar, err := s.Store.CalendarByGoogleID(ctx, userID, connectionID, googleID)
	if store.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := s.Store.DeleteCalendar(ctx, userID, calendar.ID); err != nil && !store.IsNotFound(err) {
		return err
	}
	return nil
}

// eventCounts is one calendar's contribution to the connection's result.
type eventCounts struct {
	Created  int
	Updated  int
	Deleted  int
	FullSync bool
}

// syncCalendarEvents brings one calendar's events up to date and records the
// outcome on the calendar row, so a single broken calendar is visible in the
// sidebar rather than only in the log.
func (s *Syncer) syncCalendarEvents(ctx context.Context, userID int64, calendar store.Calendar) (eventCounts, error) {
	counts, nextToken, windowStart, err := s.pullEvents(ctx, userID, calendar, calendar.SyncToken)
	recovered := false
	if errors.Is(err, ErrSyncTokenExpired) && calendar.SyncToken != "" {
		log.Printf("google calendar sync user_id=%d calendar_id=%d cursor expired, reading the window again", userID, calendar.ID)
		counts, nextToken, windowStart, err = s.pullEvents(ctx, userID, calendar, "")
		recovered = true
	}
	now := s.now()
	if err != nil {
		s.recordCalendarFailure(ctx, userID, calendar, err)
		return counts, err
	}
	if nextToken == "" && !recovered {
		nextToken = calendar.SyncToken
	}
	if err := s.Store.SaveCalendarSyncState(ctx, userID, calendar.ID, store.CalendarSyncState{
		SyncToken:     nextToken,
		WindowStartAt: windowStart,
		LastSyncAt:    now,
		LastSuccessAt: now,
		Status:        store.CalendarSyncStatusOK,
	}); err != nil {
		return counts, err
	}
	return counts, nil
}

// pullEvents walks every page Google offers for one calendar and applies it.
// It returns the counts, the cursor for the next run, and the lower bound of
// the window the mirror now covers.
func (s *Syncer) pullEvents(ctx context.Context, userID int64, calendar store.Calendar, syncToken string) (eventCounts, string, time.Time, error) {
	full := strings.TrimSpace(syncToken) == ""
	counts := eventCounts{FullSync: full}
	windowStart := calendar.WindowStartAt
	if full {
		windowStart = s.now().Add(-s.window())
	}

	var known map[string]store.CalendarEventRef
	if full {
		var err error
		known, err = s.Store.ListCalendarEventRefs(ctx, userID, calendar.ID)
		if err != nil {
			return counts, "", windowStart, err
		}
	}

	pageToken := ""
	nextSyncToken := ""
	for {
		var page EventsPage
		err := s.withToken(ctx, userID, calendar.GoogleConnectionID, func(token string) error {
			var callErr error
			page, callErr = s.client().ListEvents(ctx, token, EventsRequest{
				CalendarID: calendar.GoogleCalendarID,
				SyncToken:  syncToken,
				PageToken:  pageToken,
				TimeMin:    windowStart,
			})
			return callErr
		})
		if err != nil {
			return counts, "", windowStart, err
		}
		for _, event := range page.Items {
			externalID := strings.TrimSpace(event.ID)
			if externalID == "" {
				continue
			}
			delete(known, externalID)
			if event.IsCancelled() {
				removed, err := s.Store.DeleteCalendarEventByExternalID(ctx, userID, calendar.ID, externalID)
				if err != nil {
					return counts, "", windowStart, err
				}
				if removed {
					counts.Deleted++
				}
				continue
			}
			outcome, err := s.applyEvent(ctx, userID, calendar.ID, event)
			if err != nil {
				return counts, "", windowStart, err
			}
			switch outcome {
			case outcomeCreated:
				counts.Created++
			case outcomeUpdated:
				counts.Updated++
			}
		}
		if page.NextSyncToken != "" {
			nextSyncToken = page.NextSyncToken
		}
		if page.NextPageToken == "" {
			break
		}
		pageToken = page.NextPageToken
	}

	// Whatever the full read never mentioned no longer exists at Google, or has
	// fallen out of the window this read covered.
	for _, ref := range known {
		if err := s.Store.DeleteCalendarEvent(ctx, userID, ref.EventID); err != nil && !store.IsNotFound(err) {
			return counts, "", windowStart, err
		}
		counts.Deleted++
	}
	return counts, nextSyncToken, windowStart, nil
}

// applyOutcome is what one event did to the local calendar. "Unchanged" is a
// distinct answer rather than a quiet update: a full read re-reports every
// event, so counting those as updates would report an untouched calendar as
// fully rewritten.
type applyOutcome int

const (
	outcomeUnchanged applyOutcome = iota
	outcomeCreated
	outcomeUpdated
)

func (s *Syncer) applyEvent(ctx context.Context, userID, calendarID int64, event Event) (applyOutcome, error) {
	incoming := ToEvent(event, calendarID)
	existing, err := s.Store.CalendarEventByExternalID(ctx, userID, calendarID, incoming.ExternalID)
	switch {
	case err == nil:
		if existing.ETag != "" && existing.ETag == incoming.ETag {
			return outcomeUnchanged, nil
		}
		if _, err := s.Store.UpsertCalendarEvent(ctx, userID, incoming); err != nil {
			return outcomeUnchanged, err
		}
		return outcomeUpdated, nil
	case store.IsNotFound(err):
		if _, err := s.Store.UpsertCalendarEvent(ctx, userID, incoming); err != nil {
			return outcomeUnchanged, err
		}
		return outcomeCreated, nil
	default:
		return outcomeUnchanged, err
	}
}

// withToken runs one API call with a valid access token, retrying once against
// a refreshed one when Google rejects it. It shares the policy IMAP, SMTP and
// contacts use, so a token this process still believes in cannot fail a sync
// outright.
func (s *Syncer) withToken(ctx context.Context, userID, connectionID int64, attempt func(token string) error) error {
	return googletoken.WithFreshToken(ctx, s.Tokens, userID, connectionID, func(token string) error {
		err := attempt(token)
		if errors.Is(err, ErrUnauthorized) {
			return googletoken.AuthError{Err: err}
		}
		return err
	})
}

// recordCalendarFailure marks one calendar as broken without touching its
// cursor: the next attempt should resume the delta rather than re-read the
// window because of one network hiccup.
func (s *Syncer) recordCalendarFailure(ctx context.Context, userID int64, calendar store.Calendar, syncErr error) {
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := s.Store.SaveCalendarSyncState(writeCtx, userID, calendar.ID, store.CalendarSyncState{
		SyncToken:     calendar.SyncToken,
		WindowStartAt: calendar.WindowStartAt,
		LastSyncAt:    s.now(),
		LastSuccessAt: calendar.LastSuccessAt,
		Status:        store.CalendarSyncStatusError,
		StatusDetail:  SummarizeError(syncErr),
	}); err != nil && !store.IsNotFound(err) {
		log.Printf("google calendar sync user_id=%d calendar_id=%d save state: %v", userID, calendar.ID, err)
	}
	log.Printf("google calendar sync user_id=%d calendar_id=%d failed: %v", userID, calendar.ID, syncErr)
}

// recordConnectionOutcome persists what happened so the settings page can show
// it.
func (s *Syncer) recordConnectionOutcome(ctx context.Context, userID, connectionID int64, result Result, syncErr error) {
	// The sync's own context may already be cancelled by the failure being
	// recorded, and losing the record is what would leave settings claiming a
	// sync that never finished is still fine.
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	state, err := s.Store.GetGoogleCalendarSync(writeCtx, userID, connectionID)
	if err != nil {
		log.Printf("google calendar sync user_id=%d connection_id=%d read state: %v", userID, connectionID, err)
		return
	}
	state.UserID = userID
	state.ConnectionID = connectionID
	state.LastSyncAt = s.now()
	if syncErr == nil {
		state.Status = store.CalendarSyncStatusOK
		state.StatusDetail = ""
		state.LastSuccessAt = state.LastSyncAt
		log.Printf("google calendar sync user_id=%d connection_id=%d calendars=%d synced=%d created=%d updated=%d deleted=%d full=%t",
			userID, connectionID, result.Calendars, result.Synced, result.Created, result.Updated, result.Deleted, result.FullSync)
	} else {
		state.Status = store.CalendarSyncStatusError
		state.StatusDetail = SummarizeError(syncErr)
		log.Printf("google calendar sync user_id=%d connection_id=%d failed: %v", userID, connectionID, syncErr)
	}
	if err := s.Store.SaveGoogleCalendarSync(writeCtx, state); err != nil {
		log.Printf("google calendar sync user_id=%d connection_id=%d save state: %v", userID, connectionID, err)
	}
}

// SummarizeError turns a failure into something a user can act on. The
// underlying text can quote the request, which for a write is the event's own
// title and guest list, so only the classification travels into storage and
// the UI.
func SummarizeError(err error) string {
	switch {
	case errors.Is(err, ErrServiceDisabled):
		return "The Google Calendar API is switched off for the Google Cloud project this connection's OAuth client belongs to. Enable it there; reconnecting the account does not help."
	case errors.Is(err, ErrScopeMissing), errors.Is(err, ErrScopeInsufficient):
		return "This Google account has not granted access to calendars. Reconnect it to allow calendar sync."
	case errors.Is(err, ErrForbidden):
		return "Google refused the request. Reconnect the account to grant access to calendars."
	case errors.Is(err, ErrUnauthorized):
		return "Google rejected the sign-in for this account. Reconnect it."
	case errors.Is(err, ErrSyncTokenExpired):
		return "Google discarded the sync cursor. The next sync reads the whole window again."
	case errors.Is(err, ErrNotFound):
		return "Google no longer has this calendar."
	case errors.Is(err, context.DeadlineExceeded):
		return "The sync took too long and was stopped. It resumes on the next run."
	case errors.Is(err, ErrUpstream):
		return "Google could not be reached."
	}
	return "The sync failed. See the server log for details."
}
