// File overview: Direct HTTP calls against Microsoft Graph's calendar API. It
// speaks only in Graph resources; turning them into Rolltop rows is event.go's
// job and deciding what to do with them is sync.go's and writeback.go's.
// Access tokens and Graph's error messages -- which quote the request, and so
// the event's title, notes and guests -- must never reach a log line.

package m365calendar

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

	"rolltop/backend/calendarlink"
)

// DefaultBaseURL is Microsoft Graph v1.0.
const DefaultBaseURL = "https://graph.microsoft.com/v1.0"

// pageSize is how many events one calendarView page asks for.
const pageSize = 250

// maxResponseBytes caps one response; a page of events with notes and guests
// stays far below it.
const maxResponseBytes = 64 << 20

const defaultMaxAttempts = 4

// maxRetryAfter bounds how long a throttled request waits on Graph's word.
// Graph may ask for minutes; a sync that waits that long per request is
// better off failing and resuming on the next poll.
const maxRetryAfter = 30 * time.Second

// preferHeader asks Graph to state every time in UTC -- the stored bounds are
// absolute, and parsing one zone is what keeps them so -- and every body as
// text, which is what the dialog edits and what a copy in another calendar
// carries.
const preferHeader = `outlook.timezone="UTC", outlook.body-content-type="text"`

// eventSelect is every event property Rolltop reads. Naming them keeps a page
// of a busy calendar to what is drawn rather than every property Graph has.
const eventSelect = "id,changeKey,iCalUId,subject,body,location,start,end,isAllDay,isCancelled," +
	"isOrganizer,organizer,attendees,responseStatus,seriesMasterId,type,webLink," +
	"lastModifiedDateTime,originalStartTimeZone,isOnlineMeeting,onlineMeetingProvider,onlineMeeting"

const calendarSelect = "id,name,color,hexColor,isDefaultCalendar,canEdit,owner," +
	"allowedOnlineMeetingProviders,defaultOnlineMeetingProvider"

var (
	// ErrUnauthorized reports that Graph rejected the access token; refreshing
	// it and retrying once is the recovery.
	ErrUnauthorized = calendarlink.Sentinel("microsoft rejected the access token", calendarlink.ErrUnauthorized)
	// ErrForbidden reports a request the grant or the mailbox does not allow:
	// a calendar shared read-only, or a tenant that blocks the permission.
	ErrForbidden = calendarlink.Sentinel("microsoft denied the request", calendarlink.ErrForbidden)
	// ErrConflict reports a write refused because the event changed since the
	// etag Rolltop holds.
	ErrConflict = calendarlink.Sentinel("the microsoft event changed since it was last read", calendarlink.ErrConflict)
	// ErrNotFound reports an event or calendar Graph no longer has.
	ErrNotFound = calendarlink.Sentinel("microsoft calendar resource not found", calendarlink.ErrNotFound)
	// ErrUpstream marks any other failure of the call itself.
	ErrUpstream = calendarlink.Sentinel("microsoft graph request failed", calendarlink.ErrUpstream)
	// errBadRequest is the 400 Graph answers a query it does not support
	// with. It is kept apart so a read that asked for the link properties can
	// fall back to one that does not.
	errBadRequest = fmt.Errorf("%w: bad request", ErrUpstream)
)

// Client performs Graph calls. Every method takes the access token to use
// rather than a token source: refreshing and retrying is the syncer's policy.
type Client struct {
	HTTPClient *http.Client
	// BaseURL is Graph's root including the version, without a trailing
	// slash. A field so a test can point the package at an httptest server.
	BaseURL string
	// RetryDelay maps a zero-based attempt to a wait when Graph gave none.
	RetryDelay func(attempt int) time.Duration
}

// NewClient builds a client with Rolltop's default timeout and backoff.
func NewClient() *Client {
	return &Client{
		HTTPClient: &http.Client{Timeout: 60 * time.Second},
		BaseURL:    DefaultBaseURL,
	}
}

func (c *Client) baseURL() string {
	if strings.TrimSpace(c.BaseURL) == "" {
		return DefaultBaseURL
	}
	return strings.TrimRight(c.BaseURL, "/")
}

func (c *Client) httpClient() *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	return http.DefaultClient
}

func (c *Client) retryDelay(attempt int) time.Duration {
	if c.RetryDelay != nil {
		return c.RetryDelay(attempt)
	}
	return time.Duration(1<<attempt) * 500 * time.Millisecond
}

// ListCalendars reads one page of the account's calendars. nextLink continues
// a previous page.
func (c *Client) ListCalendars(ctx context.Context, accessToken, nextLink string) (CalendarsPage, error) {
	target := nextLink
	if strings.TrimSpace(target) == "" {
		query := url.Values{}
		query.Set("$select", calendarSelect)
		query.Set("$top", "100")
		target = c.baseURL() + "/me/calendars?" + query.Encode()
	}
	body, err := c.do(ctx, accessToken, http.MethodGet, target, "", nil)
	if err != nil {
		return CalendarsPage{}, err
	}
	var page CalendarsPage
	if err := json.Unmarshal(body, &page); err != nil {
		return CalendarsPage{}, fmt.Errorf("%w: decode calendars: %v", ErrUpstream, err)
	}
	return page, nil
}

// ViewRequest is one page of a calendarView read: every occurrence overlapping
// [Start, End) in one calendar, recurring series already expanded.
type ViewRequest struct {
	CalendarID string
	Start      time.Time
	End        time.Time
	// NextLink continues a previous page and carries every other parameter.
	NextLink string
	// WithLinks asks for the link properties alongside each event.
	WithLinks bool
}

// CalendarView reads one page of a calendar's events in a window.
func (c *Client) CalendarView(ctx context.Context, accessToken string, req ViewRequest) (EventsPage, error) {
	target := req.NextLink
	if strings.TrimSpace(target) == "" {
		calendarID := strings.TrimSpace(req.CalendarID)
		if calendarID == "" {
			return EventsPage{}, fmt.Errorf("%w: a calendar view needs a calendar id", ErrUpstream)
		}
		query := url.Values{}
		query.Set("startDateTime", req.Start.UTC().Format(time.RFC3339))
		query.Set("endDateTime", req.End.UTC().Format(time.RFC3339))
		query.Set("$select", eventSelect)
		query.Set("$orderby", "start/dateTime")
		query.Set("$top", strconv.Itoa(pageSize))
		if req.WithLinks {
			query.Set("$expand", linkExpand)
		}
		target = c.baseURL() + "/me/calendars/" + url.PathEscape(calendarID) + "/calendarView?" + query.Encode()
	}
	body, err := c.do(ctx, accessToken, http.MethodGet, target, "", nil)
	if err != nil {
		return EventsPage{}, err
	}
	var page EventsPage
	if err := json.Unmarshal(body, &page); err != nil {
		return EventsPage{}, fmt.Errorf("%w: decode events: %v", ErrUpstream, err)
	}
	return page, nil
}

// GetEvent reads one event with its link. It is what resolves a conflict:
// Microsoft is the leading system, so a refused write adopts its version.
func (c *Client) GetEvent(ctx context.Context, accessToken, eventID string) (Event, error) {
	path, err := eventPath(eventID)
	if err != nil {
		return Event{}, err
	}
	query := url.Values{}
	query.Set("$select", eventSelect)
	query.Set("$expand", linkExpand)
	body, err := c.do(ctx, accessToken, http.MethodGet, c.baseURL()+path+"?"+query.Encode(), "", nil)
	if err != nil {
		return Event{}, err
	}
	return decodeEvent(body)
}

// CreateEvent adds an event to one calendar. Graph sends the invitations of an
// event that has attendees by itself; there is no switch for it.
func (c *Client) CreateEvent(ctx context.Context, accessToken, calendarID string, payload map[string]any) (Event, error) {
	calendar := strings.TrimSpace(calendarID)
	if calendar == "" {
		return Event{}, fmt.Errorf("%w: an event needs a calendar", ErrUpstream)
	}
	body, err := c.do(ctx, accessToken, http.MethodPost,
		c.baseURL()+"/me/calendars/"+url.PathEscape(calendar)+"/events", "", payload)
	if err != nil {
		return Event{}, err
	}
	return decodeEvent(body)
}

// UpdateEvent patches an event. Graph merges a PATCH property by property, so
// the payload names only what the edit owns; the etag makes a change made in
// Outlook since the last read fail loudly instead of being overwritten.
func (c *Client) UpdateEvent(ctx context.Context, accessToken, eventID, etag string, payload map[string]any) (Event, error) {
	path, err := eventPath(eventID)
	if err != nil {
		return Event{}, err
	}
	body, err := c.do(ctx, accessToken, http.MethodPatch, c.baseURL()+path, etag, payload)
	if err != nil {
		return Event{}, err
	}
	return decodeEvent(body)
}

// DeleteEvent removes an event. For a meeting the reader organizes, Graph
// sends the guests a cancellation. An event Graph no longer has is the desired
// end state and is reported as success.
func (c *Client) DeleteEvent(ctx context.Context, accessToken, eventID string) error {
	path, err := eventPath(eventID)
	if err != nil {
		return err
	}
	_, err = c.do(ctx, accessToken, http.MethodDelete, c.baseURL()+path, "", nil)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

// RespondToEvent answers an invitation. action is accept, tentativelyAccept or
// decline; the organizer is told, because an answer nobody hears is the same
// as not answering.
func (c *Client) RespondToEvent(ctx context.Context, accessToken, eventID, action string) error {
	path, err := eventPath(eventID)
	if err != nil {
		return err
	}
	switch action {
	case "accept", "tentativelyAccept", "decline":
	default:
		return fmt.Errorf("%w: unknown answer %q", ErrUpstream, action)
	}
	_, err = c.do(ctx, accessToken, http.MethodPost, c.baseURL()+path+"/"+action, "",
		map[string]any{"sendResponse": true})
	return err
}

func eventPath(eventID string) (string, error) {
	event := strings.TrimSpace(eventID)
	if event == "" {
		return "", fmt.Errorf("%w: an event is addressed by its id", ErrUpstream)
	}
	return "/me/events/" + url.PathEscape(event), nil
}

func decodeEvent(body []byte) (Event, error) {
	var event Event
	if err := json.Unmarshal(body, &event); err != nil {
		return Event{}, fmt.Errorf("%w: decode event: %v", ErrUpstream, err)
	}
	return event, nil
}

// do runs one request with backoff on throttling and transient server errors.
func (c *Client) do(ctx context.Context, accessToken, method, target, etag string, payload any) ([]byte, error) {
	if strings.TrimSpace(accessToken) == "" {
		return nil, fmt.Errorf("%w: no access token", ErrUnauthorized)
	}
	var encoded []byte
	if payload != nil {
		var err error
		if encoded, err = json.Marshal(payload); err != nil {
			return nil, err
		}
	}
	var lastErr error
	wait := time.Duration(0)
	for attempt := 0; attempt < defaultMaxAttempts; attempt++ {
		if attempt > 0 {
			if wait <= 0 {
				wait = c.retryDelay(attempt - 1)
			}
			if err := sleepContext(ctx, wait); err != nil {
				return nil, err
			}
		}
		body, retryAfter, retryable, err := c.attempt(ctx, accessToken, method, target, etag, encoded)
		if err == nil {
			return body, nil
		}
		if !retryable {
			return nil, err
		}
		lastErr = err
		wait = retryAfter
	}
	return nil, fmt.Errorf("%w after %d attempts: %w", ErrUpstream, defaultMaxAttempts, lastErr)
}

func (c *Client) attempt(ctx context.Context, accessToken, method, target, etag string, encoded []byte) ([]byte, time.Duration, bool, error) {
	var reader io.Reader
	if encoded != nil {
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, reader)
	if err != nil {
		return nil, 0, false, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Prefer", preferHeader)
	if encoded != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if etag = strings.TrimSpace(etag); etag != "" {
		req.Header.Set("If-Match", etag)
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, 0, false, ctx.Err()
		}
		return nil, 0, true, fmt.Errorf("%w: %v", ErrUpstream, err)
	}
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	_ = resp.Body.Close()
	if readErr != nil {
		return nil, 0, true, fmt.Errorf("%w: %v", ErrUpstream, readErr)
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return body, 0, false, nil
	}
	code := errorCode(body)
	retryable := resp.StatusCode == http.StatusTooManyRequests ||
		resp.StatusCode == http.StatusServiceUnavailable ||
		resp.StatusCode == http.StatusGatewayTimeout ||
		resp.StatusCode == http.StatusBadGateway ||
		resp.StatusCode == http.StatusInternalServerError
	return nil, retryAfter(resp.Header.Get("Retry-After")), retryable, statusError(resp.StatusCode, code)
}

// statusError classifies a failed response. Only Graph's error code is kept:
// the message beside it quotes the request.
func statusError(status int, code string) error {
	switch status {
	case http.StatusUnauthorized:
		return fmt.Errorf("%w", ErrUnauthorized)
	case http.StatusForbidden:
		return fmt.Errorf("%w: %s", ErrForbidden, orUnknown(code))
	case http.StatusNotFound:
		return fmt.Errorf("%w", ErrNotFound)
	case http.StatusConflict, http.StatusPreconditionFailed:
		return fmt.Errorf("%w", ErrConflict)
	case http.StatusBadRequest:
		if code == "ErrorItemNotFound" {
			return fmt.Errorf("%w", ErrNotFound)
		}
		return fmt.Errorf("%w %s", errBadRequest, orUnknown(code))
	}
	return fmt.Errorf("%w: HTTP %d %s", ErrUpstream, status, orUnknown(code))
}

func errorCode(body []byte) string {
	var payload struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(body, &payload)
	return strings.TrimSpace(payload.Error.Code)
}

// retryAfter reads Graph's throttling hint, in seconds, bounded.
func retryAfter(value string) time.Duration {
	seconds, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || seconds <= 0 {
		return 0
	}
	wait := time.Duration(seconds) * time.Second
	if wait > maxRetryAfter {
		return maxRetryAfter
	}
	return wait
}

func orUnknown(code string) string {
	if code == "" {
		return "unspecified"
	}
	return code
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
