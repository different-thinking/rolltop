// File overview: The Microsoft 365 calendar sync loop. Microsoft is the leading
// system, so this pulls its state down and makes Rolltop match it: the
// account's calendar list first, then the events of each calendar the reader
// has switched on. Local edits travelling to Microsoft live in writeback.go.
//
// Graph v1.0 offers a delta cursor only for the primary calendar's view, and a
// delta cannot carry the link property a second-calendar copy is recognised
// by, so the events are read as windows instead: the near window -- the past
// fortnight and the next four months -- on every poll, and the whole mirrored
// window on fullReadInterval. A window read concludes that an event it did not
// return is gone, which is only sound for rows the read would have returned,
// and the reconciliation is held to exactly those.

package m365calendar

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync/atomic"
	"time"

	"rolltop/backend/calendarlink"
	"rolltop/backend/microsoftauth"
	"rolltop/backend/store"
)

const (
	// syncTimeout bounds one connection's sync. A first full read of a busy
	// calendar is paged and can take minutes; anything past this is stuck.
	syncTimeout = 10 * time.Minute
	// fullWindowBack and fullWindowAhead are the mirrored window. A year back
	// answers "what did we decide in that meeting"; two years ahead covers
	// every appointment anybody plans.
	fullWindowBack  = 365 * 24 * time.Hour
	fullWindowAhead = 730 * 24 * time.Hour
	// nearWindowBack and nearWindowAhead are what every poll reads: the weeks
	// a reader actually looks at, where a change made in Outlook has to show
	// up within one poll.
	nearWindowBack  = 14 * 24 * time.Hour
	nearWindowAhead = 120 * 24 * time.Hour
	// fullReadInterval paces the read of the whole window, which is the only
	// thing that notices a change far from today.
	fullReadInterval = 6 * time.Hour
	// edgeMargin keeps reconciliation away from a window's edges. An all-day
	// event is a date stored at midnight UTC, while Graph decides whether it
	// overlaps the window from its midnight in the zone it was entered in; a
	// row within a day and a half of the edge may be one Graph rightly left
	// out, and deleting it would make it flicker out and back every poll.
	edgeMargin = 36 * time.Hour
)

// ErrScopeMissing reports a connection whose grant does not include calendars.
var ErrScopeMissing = calendarlink.Sentinel("this Microsoft account has not granted access to calendars", calendarlink.ErrScopeMissing)

// Result summarizes one connection's sync for the caller and the log.
type Result struct {
	ConnectionID int64
	Calendars    int
	Synced       int
	Created      int
	Updated      int
	Deleted      int
	FullSync     bool
}

// TokenSource is the slice of the Microsoft auth manager the sync needs.
type TokenSource interface {
	AccessToken(ctx context.Context, userID, connectionID int64) (string, error)
	ForceRefresh(ctx context.Context, userID, connectionID int64) (string, error)
}

// ConnectionLister lists and loads a user's Microsoft connections.
type ConnectionLister interface {
	List(ctx context.Context, userID int64) ([]store.MicrosoftConnection, error)
	Get(ctx context.Context, userID, connectionID int64) (store.MicrosoftConnection, error)
}

// Syncer mirrors Microsoft 365 calendars into one Rolltop installation.
type Syncer struct {
	Store       *store.Store
	Client      *Client
	Tokens      TokenSource
	Connections ConnectionLister
	// Now is the clock; a field so a test can place a window boundary.
	Now func() time.Time

	// linksUnsupported is set once Graph refuses a calendarView that asks for
	// the link property. Reads then go without it and keep the links already
	// mirrored, rather than failing every calendar on every poll.
	linksUnsupported atomic.Bool
}

// NewSyncer wires a syncer for production use.
func NewSyncer(db *store.Store, tokens TokenSource, connections ConnectionLister) *Syncer {
	return &Syncer{Store: db, Client: NewClient(), Tokens: tokens, Connections: connections}
}

func (s *Syncer) ready() error {
	if s == nil || s.Store == nil || s.Tokens == nil || s.Connections == nil {
		return errors.New("microsoft calendar sync is not configured")
	}
	return nil
}

func (s *Syncer) client() *Client {
	if s.Client == nil {
		s.Client = NewClient()
	}
	return s.Client
}

func (s *Syncer) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

// usableConnection loads a connection and checks it may talk to Graph.
func (s *Syncer) usableConnection(ctx context.Context, userID, connectionID int64) (store.MicrosoftConnection, error) {
	connection, err := s.Connections.Get(ctx, userID, connectionID)
	if err != nil {
		return store.MicrosoftConnection{}, err
	}
	if !connection.HasScope(microsoftauth.ScopeCalendars) {
		return connection, ErrScopeMissing
	}
	if connection.NeedsReauth() {
		return connection, fmt.Errorf("%w: microsoft connection %d needs re-authorization", ErrUnauthorized, connectionID)
	}
	return connection, nil
}

// SyncUser syncs every eligible connection of one user. A connection that
// cannot sync is skipped rather than failing the others.
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
		if connection.NeedsReauth() || !connection.HasScope(microsoftauth.ScopeCalendars) {
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

// SyncConnection brings one Microsoft account's calendars into Rolltop. The
// outcome is stored on every path, so a failure shows in settings.
func (s *Syncer) SyncConnection(ctx context.Context, userID, connectionID int64) (Result, error) {
	if err := s.ready(); err != nil {
		return Result{}, err
	}
	connection, err := s.usableConnection(ctx, userID, connectionID)
	if err != nil {
		return Result{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, syncTimeout)
	defer cancel()
	result, err := s.run(ctx, userID, connection)
	s.recordConnectionOutcome(ctx, userID, connectionID, result, err)
	return result, err
}

// SyncCalendar reads one calendar on its own, which is what switching one on
// for the first time needs: an unsynced week reads as an empty one.
func (s *Syncer) SyncCalendar(ctx context.Context, userID, calendarID int64) error {
	if err := s.ready(); err != nil {
		return err
	}
	calendar, err := s.Store.Calendar(ctx, userID, calendarID)
	if err != nil {
		return err
	}
	if !calendar.IsMicrosoft() {
		return fmt.Errorf("%w: calendar %d is not a Microsoft calendar", ErrNotFound, calendarID)
	}
	connection, err := s.usableConnection(ctx, userID, calendar.MicrosoftConnectionID)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, syncTimeout)
	defer cancel()
	_, err = s.syncCalendarEvents(ctx, userID, connection, calendar)
	return err
}

func (s *Syncer) run(ctx context.Context, userID int64, connection store.MicrosoftConnection) (Result, error) {
	result := Result{ConnectionID: connection.ID}
	calendars, err := s.syncCalendarList(ctx, userID, connection)
	if err != nil {
		return result, err
	}
	result.Calendars = len(calendars)
	var firstErr error
	for _, calendar := range calendars {
		if !calendar.Selected {
			continue
		}
		counts, err := s.syncCalendarEvents(ctx, userID, connection, calendar)
		if err != nil {
			// One unreadable calendar -- a colleague's whose sharing was
			// withdrawn -- must not stop the reader's own from updating.
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		result.Synced++
		result.Created += counts.Created
		result.Updated += counts.Updated
		result.Deleted += counts.Deleted
		result.FullSync = result.FullSync || counts.FullSync
	}
	return result, firstErr
}

// syncCalendarList mirrors the account's calendars. Graph returns the whole
// list every time, so a calendar it no longer lists is gone.
func (s *Syncer) syncCalendarList(ctx context.Context, userID int64, connection store.MicrosoftConnection) ([]store.Calendar, error) {
	existing, err := s.Store.ListCalendarsForMicrosoftConnection(ctx, userID, connection.ID)
	if err != nil {
		return nil, err
	}
	known := make(map[string]int64, len(existing))
	for _, calendar := range existing {
		known[calendar.RemoteCalendarID] = calendar.ID
	}
	nextLink := ""
	for {
		var page CalendarsPage
		err := s.withToken(ctx, userID, connection.ID, func(token string) error {
			var callErr error
			page, callErr = s.client().ListCalendars(ctx, token, nextLink)
			return callErr
		})
		if err != nil {
			return nil, err
		}
		for _, entry := range page.Value {
			remoteID := strings.TrimSpace(entry.ID)
			if remoteID == "" {
				continue
			}
			delete(known, remoteID)
			if _, err := s.Store.UpsertMicrosoftCalendar(ctx, userID, ToCalendarUpsert(entry, connection.ID, connection.Email)); err != nil {
				return nil, err
			}
		}
		if page.NextLink == "" {
			break
		}
		nextLink = page.NextLink
	}
	for _, calendarID := range known {
		if err := s.Store.DeleteCalendar(ctx, userID, calendarID); err != nil && !store.IsNotFound(err) {
			return nil, err
		}
	}
	return s.Store.ListCalendarsForMicrosoftConnection(ctx, userID, connection.ID)
}

type eventCounts struct {
	Created  int
	Updated  int
	Deleted  int
	FullSync bool
}

// syncCalendarEvents brings one calendar up to date and records the outcome on
// the calendar row, so one broken calendar shows in the sidebar.
func (s *Syncer) syncCalendarEvents(ctx context.Context, userID int64, connection store.MicrosoftConnection, calendar store.Calendar) (eventCounts, error) {
	now := s.now()
	full := calendar.FullReadAt.IsZero() || calendar.WindowStartAt.IsZero() ||
		now.Sub(calendar.FullReadAt) >= fullReadInterval
	from, to := now.Add(-nearWindowBack), now.Add(nearWindowAhead)
	windowStart := calendar.WindowStartAt
	if full {
		from, to = now.Add(-fullWindowBack), now.Add(fullWindowAhead)
		windowStart = from
	}
	counts, err := s.readWindow(ctx, userID, connection, calendar, from, to, full)
	if err != nil {
		s.recordCalendarFailure(ctx, userID, calendar, err)
		return counts, err
	}
	if full {
		// Whatever ended before the window starts has fallen out of the
		// mirror and is no longer kept current.
		removed, err := s.Store.DeleteCalendarEventsEndingBefore(ctx, userID, calendar.ID, from)
		if err != nil {
			return counts, err
		}
		counts.Deleted += int(removed)
		if err := s.Store.MarkCalendarFullRead(ctx, userID, calendar.ID, now); err != nil {
			return counts, err
		}
	}
	if err := s.Store.SaveCalendarSyncState(ctx, userID, calendar.ID, store.CalendarSyncState{
		WindowStartAt: windowStart,
		LastSyncAt:    now,
		LastSuccessAt: now,
		Status:        store.CalendarSyncStatusOK,
	}); err != nil {
		return counts, err
	}
	return counts, nil
}

// readWindow reads every event overlapping [from, to) and applies it, then
// removes the rows that start well inside the window and were not returned.
func (s *Syncer) readWindow(ctx context.Context, userID int64, connection store.MicrosoftConnection, calendar store.Calendar, from, to time.Time, full bool) (eventCounts, error) {
	counts := eventCounts{FullSync: full}
	known, err := s.Store.ListCalendarEventRefsStartingIn(ctx, userID, calendar.ID, from.Add(edgeMargin), to.Add(-edgeMargin))
	if err != nil {
		return counts, err
	}
	withLinks := !s.linksUnsupported.Load()
	nextLink := ""
	for {
		var page EventsPage
		err := s.withToken(ctx, userID, connection.ID, func(token string) error {
			var callErr error
			page, callErr = s.client().CalendarView(ctx, token, ViewRequest{
				CalendarID: calendar.RemoteCalendarID,
				Start:      from,
				End:        to,
				NextLink:   nextLink,
				WithLinks:  withLinks,
			})
			return callErr
		})
		if errors.Is(err, errBadRequest) && withLinks && nextLink == "" {
			// The link property could not be asked for. Reading without it
			// keeps the calendar in step; the links already mirrored are kept
			// by applyEvent rather than cleared.
			log.Printf("microsoft calendar sync user_id=%d calendar_id=%d: the link property was refused, reading without it", userID, calendar.ID)
			s.linksUnsupported.Store(true)
			withLinks = false
			continue
		}
		if err != nil {
			return counts, err
		}
		for _, event := range page.Value {
			externalID := strings.TrimSpace(event.ID)
			if externalID == "" || strings.EqualFold(event.Type, "seriesMaster") {
				continue
			}
			delete(known, externalID)
			outcome, err := s.applyEvent(ctx, userID, calendar.ID, connection.Email, event, withLinks)
			if err != nil {
				return counts, err
			}
			switch outcome {
			case outcomeCreated:
				counts.Created++
			case outcomeUpdated:
				counts.Updated++
			}
		}
		if page.NextLink == "" {
			break
		}
		nextLink = page.NextLink
	}
	for _, ref := range known {
		if err := s.Store.DeleteCalendarEvent(ctx, userID, ref.EventID); err != nil && !store.IsNotFound(err) {
			return counts, err
		}
		counts.Deleted++
	}
	return counts, nil
}

type applyOutcome int

const (
	outcomeUnchanged applyOutcome = iota
	outcomeCreated
	outcomeUpdated
)

func (s *Syncer) applyEvent(ctx context.Context, userID, calendarID int64, selfEmail string, event Event, withLinks bool) (applyOutcome, error) {
	incoming := ToEvent(event, calendarID, selfEmail)
	existing, err := s.Store.CalendarEventByExternalID(ctx, userID, calendarID, incoming.ExternalID)
	switch {
	case err == nil:
		if existing.ETag != "" && existing.ETag == incoming.ETag {
			return outcomeUnchanged, nil
		}
		if !withLinks {
			incoming.LinkKey, incoming.LinkPrimary = existing.LinkKey, existing.LinkPrimary
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

// withToken runs one call with a valid access token, retrying once against a
// refreshed one when Graph rejects it.
func (s *Syncer) withToken(ctx context.Context, userID, connectionID int64, attempt func(token string) error) error {
	token, err := s.Tokens.AccessToken(ctx, userID, connectionID)
	if err != nil {
		return tokenError(err)
	}
	err = attempt(token)
	if !errors.Is(err, ErrUnauthorized) {
		return err
	}
	refreshCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	refreshed, refreshErr := s.Tokens.ForceRefresh(refreshCtx, userID, connectionID)
	if refreshErr != nil || refreshed == token {
		return err
	}
	return attempt(refreshed)
}

// tokenError makes a failure to mint a token read as what it means for a
// calendar: a connection that must be signed in again is unauthorized, and
// an unreachable identity platform is an upstream failure.
func tokenError(err error) error {
	switch {
	case errors.Is(err, microsoftauth.ErrReauthRequired):
		return fmt.Errorf("%w: %w", ErrUnauthorized, err)
	case errors.Is(err, microsoftauth.ErrUpstream):
		return fmt.Errorf("%w: %w", ErrUpstream, err)
	}
	return err
}

func (s *Syncer) recordCalendarFailure(ctx context.Context, userID int64, calendar store.Calendar, syncErr error) {
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := s.Store.SaveCalendarSyncState(writeCtx, userID, calendar.ID, store.CalendarSyncState{
		WindowStartAt: calendar.WindowStartAt,
		LastSyncAt:    s.now(),
		LastSuccessAt: calendar.LastSuccessAt,
		Status:        store.CalendarSyncStatusError,
		StatusDetail:  SummarizeError(syncErr),
	}); err != nil && !store.IsNotFound(err) {
		log.Printf("microsoft calendar sync user_id=%d calendar_id=%d save state: %v", userID, calendar.ID, err)
	}
	log.Printf("microsoft calendar sync user_id=%d calendar_id=%d failed: %v", userID, calendar.ID, syncErr)
}

func (s *Syncer) recordConnectionOutcome(ctx context.Context, userID, connectionID int64, result Result, syncErr error) {
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	state, err := s.Store.GetMicrosoftCalendarSync(writeCtx, userID, connectionID)
	if err != nil {
		log.Printf("microsoft calendar sync user_id=%d connection_id=%d read state: %v", userID, connectionID, err)
		return
	}
	state.UserID = userID
	state.ConnectionID = connectionID
	state.LastSyncAt = s.now()
	if syncErr == nil {
		state.Status = store.CalendarSyncStatusOK
		state.StatusDetail = ""
		state.LastSuccessAt = state.LastSyncAt
		log.Printf("microsoft calendar sync user_id=%d connection_id=%d calendars=%d synced=%d created=%d updated=%d deleted=%d full=%t",
			userID, connectionID, result.Calendars, result.Synced, result.Created, result.Updated, result.Deleted, result.FullSync)
	} else {
		state.Status = store.CalendarSyncStatusError
		state.StatusDetail = SummarizeError(syncErr)
		log.Printf("microsoft calendar sync user_id=%d connection_id=%d failed: %v", userID, connectionID, syncErr)
	}
	if err := s.Store.SaveMicrosoftCalendarSync(writeCtx, state); err != nil {
		log.Printf("microsoft calendar sync user_id=%d connection_id=%d save state: %v", userID, connectionID, err)
	}
}

// SummarizeError turns a failure into something the reader can act on. Only
// the classification travels into storage and the UI.
func SummarizeError(err error) string {
	switch {
	case errors.Is(err, ErrScopeMissing):
		return "This Microsoft account has not granted access to calendars. Sign in again to allow calendar sync."
	case errors.Is(err, ErrUnauthorized):
		return "Microsoft rejected the sign-in for this account. Sign in again."
	case errors.Is(err, ErrForbidden):
		return "Microsoft refused the request. Your organisation may not allow this app access to calendars."
	case errors.Is(err, ErrNotFound):
		return "Microsoft no longer has this calendar."
	case errors.Is(err, context.DeadlineExceeded):
		return "The sync took too long and was stopped. It resumes on the next run."
	case errors.Is(err, ErrUpstream):
		return "Microsoft could not be reached."
	}
	return "The sync failed. See the server log for details."
}
