// File overview: Direct HTTP calls against the Google Calendar API. It speaks
// only in Calendar resources; turning those into Rolltop rows is event.go's job
// and deciding what to do with them is sync.go's. Access tokens must never
// reach a log line from here.

package googlecalendar

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// DefaultBaseURL is Google's Calendar API host. It is a field on the client
// rather than a constant in the request builders so a test can point the whole
// package at an httptest server.
const DefaultBaseURL = "https://www.googleapis.com"

// pageSize is Google's maximum for events.list. Fewer, larger pages means fewer
// round trips on the initial sync, which is the slow one.
const pageSize = 2500

// calendarListPageSize is the maximum for calendarList.list. An account with
// more calendars than this exists, so the caller still pages.
const calendarListPageSize = 250

// maxResponseBytes caps one response. A full page of 2500 expanded events with
// descriptions and guest lists stays well below this; anything larger is a fault.
const maxResponseBytes = 64 << 20

const defaultMaxAttempts = 4

var (
	// ErrSyncTokenExpired reports that Google has discarded the delta cursor.
	// The only recovery is a full resync, so it is a named error rather than a
	// generic upstream failure.
	ErrSyncTokenExpired = errors.New("google calendar sync token expired")
	// ErrUnauthorized reports that Google rejected the access token. Refreshing
	// it and retrying once is what the caller should do.
	ErrUnauthorized = errors.New("google rejected the access token")
	// ErrForbidden reports a request the grant does not cover, which in practice
	// means the connection was authorized before the calendar scope existed, or
	// a write to a calendar shared read-only.
	ErrForbidden = errors.New("google denied the request")
	// ErrConflict reports that the event changed at Google since the etag
	// Rolltop holds. Google is the leading system, so the caller adopts the
	// remote copy rather than forcing the write through.
	ErrConflict = errors.New("google calendar event changed since it was last read")
	// ErrNotFound reports an event or calendar Google no longer has.
	ErrNotFound = errors.New("google calendar resource not found")
	// ErrUpstream marks any other failure of the call itself, as opposed to a
	// local one.
	ErrUpstream = errors.New("google calendar request failed")
	// ErrServiceDisabled and ErrScopeInsufficient are the two 403s that need
	// opposite answers, and Google tells them apart only in the machine-readable
	// reason beside the status. An API switched off for the Cloud project behind
	// the OAuth client is not something the account holder can grant their way
	// out of; a missing scope is. Both stay part of ErrForbidden so callers that
	// only care that Google said no keep working.
	ErrServiceDisabled   = fmt.Errorf("%w: the Google Calendar API is not enabled for this Google Cloud project", ErrForbidden)
	ErrScopeInsufficient = fmt.Errorf("%w: the access token does not carry the calendar scope", ErrForbidden)
)

// Client performs Calendar API calls. Every method takes the access token to
// use rather than a token source: refreshing and retrying is one policy shared
// with IMAP, SMTP and contacts, and it lives in googletoken.
type Client struct {
	HTTPClient *http.Client
	// BaseURL is the API host without a trailing slash.
	BaseURL string
	// RetryDelay maps a zero-based attempt number to a wait. Tests override it
	// to keep backoff assertions instant.
	RetryDelay func(attempt int) time.Duration
}

// NewClient builds a client with Rolltop's default timeout and backoff.
func NewClient() *Client {
	return &Client{
		HTTPClient: &http.Client{Timeout: 60 * time.Second},
		BaseURL:    DefaultBaseURL,
		RetryDelay: func(attempt int) time.Duration {
			return time.Duration(1<<attempt) * 500 * time.Millisecond
		},
	}
}

func (c *Client) httpClient() *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	return http.DefaultClient
}

func (c *Client) baseURL() string {
	if strings.TrimSpace(c.BaseURL) == "" {
		return DefaultBaseURL
	}
	return strings.TrimRight(c.BaseURL, "/") + "/calendar/v3"
}

func (c *Client) retryDelay(attempt int) time.Duration {
	if c.RetryDelay != nil {
		return c.RetryDelay(attempt)
	}
	return time.Duration(1<<attempt) * 500 * time.Millisecond
}

// ListCalendars reads one page of the account's calendar list.
func (c *Client) ListCalendars(ctx context.Context, accessToken, syncToken, pageToken string) (CalendarListPage, error) {
	query := url.Values{}
	query.Set("maxResults", strconv.Itoa(calendarListPageSize))
	// Calendars the account hid at Google are still listed, because hiding one
	// there is a choice about Google's own UI. Rolltop keeps its own visibility
	// switch and seeds it from the account's selection instead.
	query.Set("showHidden", "true")
	query.Set("showDeleted", "true")
	if token := strings.TrimSpace(syncToken); token != "" {
		query.Set("syncToken", token)
	}
	if token := strings.TrimSpace(pageToken); token != "" {
		query.Set("pageToken", token)
	}
	body, err := c.get(ctx, accessToken, "/users/me/calendarList?"+query.Encode())
	if err != nil {
		return CalendarListPage{}, err
	}
	var page CalendarListPage
	if err := json.Unmarshal(body, &page); err != nil {
		return CalendarListPage{}, fmt.Errorf("%w: decode calendar list: %v", ErrUpstream, err)
	}
	return page, nil
}

// EventsRequest is one page of an event read. An empty SyncToken starts a full
// read of the window beginning at TimeMin; anything else asks for the changes
// since that cursor.
type EventsRequest struct {
	CalendarID string
	SyncToken  string
	PageToken  string
	// TimeMin bounds a full read. Google encodes the window into the sync token
	// it hands back, so it must not be sent again on a delta -- doing so is
	// rejected outright.
	TimeMin time.Time
}

// ListEvents reads one page of a calendar's events.
func (c *Client) ListEvents(ctx context.Context, accessToken string, req EventsRequest) (EventsPage, error) {
	calendarID := strings.TrimSpace(req.CalendarID)
	if calendarID == "" {
		return EventsPage{}, fmt.Errorf("%w: events need a calendar id", ErrUpstream)
	}
	query := url.Values{}
	query.Set("maxResults", strconv.Itoa(pageSize))
	// Recurring events arrive as ready-made instances, which is what lets the
	// week view draw them without a recurrence expander anywhere in Rolltop.
	query.Set("singleEvents", "true")
	// Cancelled occurrences are how a delta reports a deletion. Google asks that
	// every parameter stay the same between the initial read and its deltas, so
	// this is set on both; on a full read the tombstones simply match nothing.
	query.Set("showDeleted", "true")
	if token := strings.TrimSpace(req.SyncToken); token != "" {
		query.Set("syncToken", token)
	} else if !req.TimeMin.IsZero() {
		query.Set("timeMin", req.TimeMin.UTC().Format(time.RFC3339))
	}
	if token := strings.TrimSpace(req.PageToken); token != "" {
		query.Set("pageToken", token)
	}
	body, err := c.get(ctx, accessToken, "/calendars/"+url.PathEscape(calendarID)+"/events?"+query.Encode())
	if err != nil {
		return EventsPage{}, err
	}
	var page EventsPage
	if err := json.Unmarshal(body, &page); err != nil {
		return EventsPage{}, fmt.Errorf("%w: decode events: %v", ErrUpstream, err)
	}
	return page, nil
}

// GetEvent reads one event. It is what resolves a conflict: Google is the
// leading system, so a rejected write is answered by adopting its version.
func (c *Client) GetEvent(ctx context.Context, accessToken, calendarID, eventID string) (Event, error) {
	path, err := eventPath(calendarID, eventID)
	if err != nil {
		return Event{}, err
	}
	body, err := c.get(ctx, accessToken, path)
	if err != nil {
		return Event{}, err
	}
	return decodeEvent(body)
}

// CreateEvent adds an event to one calendar. notify decides whether Google
// mails the guests about it.
func (c *Client) CreateEvent(ctx context.Context, accessToken, calendarID string, write EventWrite, notify bool) (Event, error) {
	calendar := strings.TrimSpace(calendarID)
	if calendar == "" {
		return Event{}, fmt.Errorf("%w: an event needs a calendar", ErrUpstream)
	}
	target := "/calendars/" + url.PathEscape(calendar) + "/events?" + sendUpdatesQuery(notify)
	body, err := c.send(ctx, accessToken, http.MethodPost, target, "", write)
	if err != nil {
		return Event{}, err
	}
	return decodeEvent(body)
}

// UpdateEvent overwrites the fields Rolltop's dialog owns. The etag is required
// and is what makes a concurrent change at Google fail loudly rather than being
// silently overwritten.
func (c *Client) UpdateEvent(ctx context.Context, accessToken, calendarID, eventID, etag string, write EventWrite, notify bool) (Event, error) {
	path, err := eventPath(calendarID, eventID)
	if err != nil {
		return Event{}, err
	}
	if strings.TrimSpace(etag) == "" {
		return Event{}, fmt.Errorf("%w: an update needs an etag", ErrConflict)
	}
	body, err := c.send(ctx, accessToken, http.MethodPatch,
		path+"?"+sendUpdatesQuery(notify), etag, write)
	if err != nil {
		return Event{}, err
	}
	return decodeEvent(body)
}

// RespondToEvent sets the connected account's own answer to an invitation. It
// patches only the attendee list, because that is the one field an invitee is
// allowed to change on somebody else's event.
//
// Google has no merge semantics for attendees: a patch carrying a subset would
// drop everybody it left out, so the caller passes the whole list with one
// answer changed.
func (c *Client) RespondToEvent(ctx context.Context, accessToken, calendarID, eventID, etag string, attendees []EventAttendee) (Event, error) {
	path, err := eventPath(calendarID, eventID)
	if err != nil {
		return Event{}, err
	}
	if len(attendees) == 0 {
		return Event{}, fmt.Errorf("%w: an answer needs the guest list", ErrUpstream)
	}
	payload := map[string]any{"attendees": attendees}
	// sendUpdates=all so the organizer learns the answer; an RSVP nobody is told
	// about is the same as not answering.
	body, err := c.send(ctx, accessToken, http.MethodPatch, path+"?sendUpdates=all", etag, payload)
	if err != nil {
		return Event{}, err
	}
	return decodeEvent(body)
}

// DeleteEvent removes an event. An event Google no longer has is the desired
// end state, so it is reported as success.
func (c *Client) DeleteEvent(ctx context.Context, accessToken, calendarID, eventID, etag string, notify bool) error {
	path, err := eventPath(calendarID, eventID)
	if err != nil {
		return err
	}
	_, err = c.do(ctx, accessToken, func() (*http.Request, error) {
		req, reqErr := http.NewRequestWithContext(ctx, http.MethodDelete,
			c.baseURL()+path+"?"+sendUpdatesQuery(notify), nil)
		if reqErr != nil {
			return nil, reqErr
		}
		setIfMatch(req, etag)
		return req, nil
	})
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

// sendUpdatesQuery decides who Google notifies. An event with guests is a
// meeting and silently changing one leaves everybody holding a stale
// invitation; an event with none is a private note and mailing about it would
// be noise. It is decided from the event, not from the payload: a write that
// changes only the time carries no guest list and still has to reach the
// people expected to show up.
func sendUpdatesQuery(notify bool) string {
	if notify {
		return "sendUpdates=all"
	}
	return "sendUpdates=none"
}

func eventPath(calendarID, eventID string) (string, error) {
	calendar := strings.TrimSpace(calendarID)
	event := strings.TrimSpace(eventID)
	if calendar == "" || event == "" {
		return "", fmt.Errorf("%w: an event is addressed by calendar and id", ErrUpstream)
	}
	return "/calendars/" + url.PathEscape(calendar) + "/events/" + url.PathEscape(event), nil
}

func decodeEvent(body []byte) (Event, error) {
	var event Event
	if err := json.Unmarshal(body, &event); err != nil {
		return Event{}, fmt.Errorf("%w: decode event: %v", ErrUpstream, err)
	}
	return event, nil
}

// setIfMatch attaches the optimistic-concurrency precondition. An empty etag
// means the caller has no expectation about the current state, which is the
// right thing for a delete that only has to end with the event gone.
func setIfMatch(req *http.Request, etag string) {
	if etag = strings.TrimSpace(etag); etag != "" {
		req.Header.Set("If-Match", etag)
	}
}

func (c *Client) get(ctx context.Context, accessToken, path string) ([]byte, error) {
	return c.do(ctx, accessToken, func() (*http.Request, error) {
		return http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL()+path, nil)
	})
}

func (c *Client) send(ctx context.Context, accessToken, method, path, etag string, payload any) ([]byte, error) {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	return c.do(ctx, accessToken, func() (*http.Request, error) {
		req, reqErr := http.NewRequestWithContext(ctx, method, c.baseURL()+path, bytes.NewReader(encoded))
		if reqErr != nil {
			return nil, reqErr
		}
		req.Header.Set("Content-Type", "application/json")
		setIfMatch(req, etag)
		return req, nil
	})
}

// do runs a request with backoff on rate limits and transient server errors.
// The request is rebuilt per attempt because its body is consumed each time.
func (c *Client) do(ctx context.Context, accessToken string, build func() (*http.Request, error)) ([]byte, error) {
	if strings.TrimSpace(accessToken) == "" {
		return nil, fmt.Errorf("%w: no access token", ErrUnauthorized)
	}
	var lastErr error
	for attempt := 0; attempt < defaultMaxAttempts; attempt++ {
		if attempt > 0 {
			if err := sleepContext(ctx, c.retryDelay(attempt-1)); err != nil {
				return nil, err
			}
		}
		body, retryable, err := c.attempt(accessToken, build)
		if err == nil {
			return body, nil
		}
		if !retryable {
			return nil, err
		}
		lastErr = err
	}
	return nil, fmt.Errorf("%w after %d attempts: %w", ErrUpstream, defaultMaxAttempts, lastErr)
}

func (c *Client) attempt(accessToken string, build func() (*http.Request, error)) (body []byte, retryable bool, err error) {
	req, err := build()
	if err != nil {
		return nil, false, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, true, fmt.Errorf("%w: %v", ErrUpstream, err)
	}
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	_ = resp.Body.Close()
	if readErr != nil {
		return nil, true, fmt.Errorf("%w: %v", ErrUpstream, readErr)
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return body, false, nil
	}
	reason := errorReason(body)
	return nil, retryableStatus(resp.StatusCode, reason), statusError(resp.StatusCode, reason)
}

// retryableStatus decides whether waiting could help. Calendar answers a quota
// problem with 403 and the same status it uses for "you may not do this at
// all", so the reason has to separate them: retrying an insufficient permission
// would burn three more requests to reach the same answer.
func retryableStatus(status int, reason string) bool {
	switch status {
	case http.StatusTooManyRequests:
		return true
	case http.StatusForbidden:
		switch reason {
		case "rateLimitExceeded", "userRateLimitExceeded", "quotaExceeded", "backendError":
			return true
		}
		return false
	}
	return status >= http.StatusInternalServerError
}

// statusError classifies a failed response. Only the reason is kept: an error
// body from Google echoes the request, which for a write is the event's own
// title, guest list and notes, and that has no business in an operator log.
func statusError(status int, reason string) error {
	switch status {
	case http.StatusGone:
		// fullSyncRequired. Calendar reports an expired cursor with 410, which
		// it uses for nothing else here.
		return fmt.Errorf("%w", ErrSyncTokenExpired)
	case http.StatusUnauthorized:
		return fmt.Errorf("%w", ErrUnauthorized)
	case http.StatusForbidden:
		switch reason {
		case "accessNotConfigured", "SERVICE_DISABLED":
			return fmt.Errorf("%w", ErrServiceDisabled)
		case "insufficientPermissions", "ACCESS_TOKEN_SCOPE_INSUFFICIENT":
			return fmt.Errorf("%w", ErrScopeInsufficient)
		}
		return fmt.Errorf("%w: %s", ErrForbidden, orUnknown(reason))
	case http.StatusNotFound:
		return fmt.Errorf("%w", ErrNotFound)
	case http.StatusConflict, http.StatusPreconditionFailed:
		return fmt.Errorf("%w", ErrConflict)
	case http.StatusBadRequest:
		if reason == "fullSyncRequired" {
			return fmt.Errorf("%w", ErrSyncTokenExpired)
		}
		return fmt.Errorf("%w: HTTP 400 %s", ErrUpstream, orUnknown(reason))
	}
	return fmt.Errorf("%w: HTTP %d %s", ErrUpstream, status, orUnknown(reason))
}

// errorReason pulls Google's machine-readable reason out of an error body. The
// human message beside it is skipped on purpose: it quotes the request.
func errorReason(body []byte) string {
	var payload struct {
		Error struct {
			Status string `json:"status"`
			Errors []struct {
				Reason string `json:"reason"`
			} `json:"errors"`
			// Details is the newer shape of the same field. Calendar answers in
			// the legacy one, but a disabled API is reported by the API gateway
			// ahead of Calendar, which does not.
			Details []struct {
				Reason string `json:"reason"`
			} `json:"details"`
		} `json:"error"`
	}
	_ = json.Unmarshal(body, &payload)
	if len(payload.Error.Errors) > 0 {
		if reason := strings.TrimSpace(payload.Error.Errors[0].Reason); reason != "" {
			return reason
		}
	}
	for _, detail := range payload.Error.Details {
		if reason := strings.TrimSpace(detail.Reason); reason != "" {
			return reason
		}
	}
	return strings.TrimSpace(payload.Error.Status)
}

func orUnknown(reason string) string {
	if reason == "" {
		return "unspecified"
	}
	return reason
}

func sleepContext(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
