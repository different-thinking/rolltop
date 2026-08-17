// File overview: Direct HTTP calls against Google's People API. It speaks only
// in People resources; turning those into Rolltop contacts is person.go's job,
// and deciding what to do with them is sync.go's. Access tokens must never
// reach a log line from here.

package googlepeople

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

// DefaultBaseURL is Google's People API host. It is a field on the client
// rather than a constant in the request builders so a test can point the whole
// package at an httptest server.
const DefaultBaseURL = "https://people.googleapis.com"

// ReadPersonFields is what Rolltop mirrors. Requesting fields the contact model
// has no room for would only enlarge every response.
const ReadPersonFields = "names,nicknames,emailAddresses,phoneNumbers,addresses,organizations,biographies,birthdays,urls,photos,metadata"

// WritePersonFields is the subset Rolltop is allowed to overwrite at Google.
// It deliberately excludes photos and metadata: a write listing a field clears
// it when the payload omits it, and Rolltop has no editor for those.
const WritePersonFields = "names,nicknames,emailAddresses,phoneNumbers,addresses,organizations,biographies,birthdays,urls"

// pageSize is Google's maximum for connections.list. Fewer, larger pages means
// fewer round trips on the initial sync, which is the slow one.
const pageSize = 1000

// maxResponseBytes caps one response. A full page of 1000 people with photos
// URLs and addresses stays far below this; anything larger is a fault.
const maxResponseBytes = 32 << 20

const defaultMaxAttempts = 4

var (
	// ErrSyncTokenExpired reports that Google has discarded the delta cursor,
	// which it does after roughly a week. The only recovery is a full resync,
	// so it is a named error rather than a generic upstream failure.
	ErrSyncTokenExpired = errors.New("google sync token expired")
	// ErrUnauthorized reports that Google rejected the access token. Refreshing
	// it and retrying once is what the caller should do.
	ErrUnauthorized = errors.New("google rejected the access token")
	// ErrForbidden reports a request the grant does not cover, which in practice
	// means the connection was authorized before the contacts scope existed.
	ErrForbidden = errors.New("google denied the request")
	// ErrConflict reports that the person changed at Google since the etag
	// Rolltop holds. Google is the leading system, so the caller adopts the
	// remote copy rather than forcing the write through.
	ErrConflict = errors.New("google contact changed since it was last read")
	// ErrPrecondition is Google's FAILED_PRECONDITION, which it returns for two
	// unrelated situations -- a stale sync token and a stale etag -- with the
	// same status. Only the caller knows which one it asked for, so each entry
	// point translates this into the error that means something.
	ErrPrecondition = errors.New("google rejected a stale token or etag")
	// ErrNotFound reports a person Google no longer has.
	ErrNotFound = errors.New("google contact not found")
	// ErrUpstream marks any other failure of the call itself, as opposed to a
	// local one.
	ErrUpstream = errors.New("google people request failed")
)

// Client performs People API calls. Every method takes the access token to use
// rather than a token source: refreshing and retrying is one policy shared with
// IMAP and SMTP, and it lives in googletoken.
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
	return strings.TrimRight(c.BaseURL, "/")
}

func (c *Client) retryDelay(attempt int) time.Duration {
	if c.RetryDelay != nil {
		return c.RetryDelay(attempt)
	}
	return time.Duration(1<<attempt) * 500 * time.Millisecond
}

// ConnectionsRequest is one page of a contacts read. Exactly one of SyncToken
// and RequestSyncToken is meaningful per sync: an incremental run sends the
// token it stored, a full run asks for a new one.
type ConnectionsRequest struct {
	SyncToken        string
	PageToken        string
	RequestSyncToken bool
}

// ConnectionsPage is one response from people/me/connections.
type ConnectionsPage struct {
	People []Person `json:"connections"`
	// NextPageToken is empty on the last page.
	NextPageToken string `json:"nextPageToken"`
	// NextSyncToken arrives only on the last page and only when the request
	// asked for it or carried one.
	NextSyncToken string `json:"nextSyncToken"`
	TotalItems    int    `json:"totalItems"`
}

// ListConnections reads one page of the authenticated account's contacts.
func (c *Client) ListConnections(ctx context.Context, accessToken string, req ConnectionsRequest) (ConnectionsPage, error) {
	query := url.Values{}
	query.Set("personFields", ReadPersonFields)
	query.Set("pageSize", strconv.Itoa(pageSize))
	// A request carrying a sync token gets the delta, in which removed people
	// arrive as entries flagged metadata.deleted rather than being omitted.
	if token := strings.TrimSpace(req.SyncToken); token != "" {
		query.Set("syncToken", token)
	} else if req.RequestSyncToken {
		query.Set("requestSyncToken", "true")
	}
	if token := strings.TrimSpace(req.PageToken); token != "" {
		query.Set("pageToken", token)
	}
	target := c.baseURL() + "/v1/people/me/connections?" + query.Encode()
	body, err := c.do(ctx, accessToken, func() (*http.Request, error) {
		return http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	})
	if errors.Is(err, ErrPrecondition) {
		// On a read the only precondition is the sync token, and reporting it
		// as anything else would strand the connection on a cursor it can never
		// use again.
		err = ErrSyncTokenExpired
	}
	if err != nil {
		return ConnectionsPage{}, err
	}
	var page ConnectionsPage
	if err := json.Unmarshal(body, &page); err != nil {
		return ConnectionsPage{}, fmt.Errorf("%w: decode connections: %v", ErrUpstream, err)
	}
	return page, nil
}

// CreateContact adds a person to the account's own contacts.
func (c *Client) CreateContact(ctx context.Context, accessToken string, person Person) (Person, error) {
	query := url.Values{"personFields": []string{ReadPersonFields}}
	target := c.baseURL() + "/v1/people:createContact?" + query.Encode()
	return c.writePerson(ctx, accessToken, http.MethodPost, target, person)
}

// UpdateContact overwrites the writable fields of one person. The person's
// ETag is required by Google and is what makes a concurrent remote edit fail
// loudly instead of being silently overwritten.
func (c *Client) UpdateContact(ctx context.Context, accessToken string, person Person) (Person, error) {
	resource := strings.TrimSpace(person.ResourceName)
	if resource == "" {
		return Person{}, fmt.Errorf("%w: update needs a resource name", ErrUpstream)
	}
	if strings.TrimSpace(person.ETag) == "" {
		return Person{}, fmt.Errorf("%w: update needs an etag", ErrConflict)
	}
	query := url.Values{
		"updatePersonFields": []string{WritePersonFields},
		"personFields":       []string{ReadPersonFields},
	}
	target := c.baseURL() + "/v1/" + resource + ":updateContact?" + query.Encode()
	updated, err := c.writePerson(ctx, accessToken, http.MethodPatch, target, person)
	if errors.Is(err, ErrPrecondition) {
		// On a write the only precondition is the etag, so this is a person
		// somebody else changed in the meantime.
		return Person{}, ErrConflict
	}
	return updated, err
}

// GetPerson reads one contact. It is what resolves a conflict: Google is the
// leading system, so a rejected write is answered by adopting its version.
func (c *Client) GetPerson(ctx context.Context, accessToken, resourceName string) (Person, error) {
	resource := strings.TrimSpace(resourceName)
	if resource == "" {
		return Person{}, fmt.Errorf("%w: read needs a resource name", ErrUpstream)
	}
	query := url.Values{"personFields": []string{ReadPersonFields}}
	target := c.baseURL() + "/v1/" + resource + "?" + query.Encode()
	body, err := c.do(ctx, accessToken, func() (*http.Request, error) {
		return http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	})
	if err != nil {
		return Person{}, err
	}
	var person Person
	if err := json.Unmarshal(body, &person); err != nil {
		return Person{}, fmt.Errorf("%w: decode person: %v", ErrUpstream, err)
	}
	return person, nil
}

// DeleteContact removes a person from the account's contacts. A person Google
// no longer has is the desired end state, so it is reported as success.
func (c *Client) DeleteContact(ctx context.Context, accessToken, resourceName string) error {
	resource := strings.TrimSpace(resourceName)
	if resource == "" {
		return fmt.Errorf("%w: delete needs a resource name", ErrUpstream)
	}
	target := c.baseURL() + "/v1/" + resource + ":deleteContact"
	_, err := c.do(ctx, accessToken, func() (*http.Request, error) {
		return http.NewRequestWithContext(ctx, http.MethodDelete, target, nil)
	})
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

// FetchPhoto downloads a contact photo. The URL comes from the person payload
// and is served from Google's static hosts, which take no OAuth token; sending
// one there would leak it to a host that never needed it.
func (c *Client) FetchPhoto(ctx context.Context, photoURL string) ([]byte, error) {
	parsed, err := url.Parse(strings.TrimSpace(photoURL))
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" {
		return nil, fmt.Errorf("%w: unusable photo URL", ErrUpstream)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUpstream, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: photo HTTP %d", ErrUpstream, resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, maxPhotoBytes))
}

func (c *Client) writePerson(ctx context.Context, accessToken, method, target string, person Person) (Person, error) {
	payload, err := json.Marshal(person)
	if err != nil {
		return Person{}, err
	}
	body, err := c.do(ctx, accessToken, func() (*http.Request, error) {
		req, reqErr := http.NewRequestWithContext(ctx, method, target, bytes.NewReader(payload))
		if reqErr != nil {
			return nil, reqErr
		}
		req.Header.Set("Content-Type", "application/json")
		return req, nil
	})
	if err != nil {
		return Person{}, err
	}
	var out Person
	if err := json.Unmarshal(body, &out); err != nil {
		return Person{}, fmt.Errorf("%w: decode person: %v", ErrUpstream, err)
	}
	return out, nil
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
	retry := resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= http.StatusInternalServerError
	return nil, retry, statusError(resp.StatusCode, body)
}

// statusError classifies a failed response. Only the reason is kept: an error
// body from Google echoes the request, which for a write is the contact's own
// data, and that has no business in an operator log.
func statusError(status int, body []byte) error {
	var payload struct {
		Error struct {
			Status  string `json:"status"`
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(body, &payload)
	reason := strings.TrimSpace(payload.Error.Status)
	switch status {
	case http.StatusGone:
		return fmt.Errorf("%w", ErrSyncTokenExpired)
	case http.StatusUnauthorized:
		return fmt.Errorf("%w", ErrUnauthorized)
	case http.StatusForbidden:
		return fmt.Errorf("%w: %s", ErrForbidden, orUnknown(reason))
	case http.StatusNotFound:
		return fmt.Errorf("%w", ErrNotFound)
	case http.StatusConflict, http.StatusPreconditionFailed:
		return fmt.Errorf("%w", ErrConflict)
	case http.StatusBadRequest:
		if reason == "FAILED_PRECONDITION" {
			return fmt.Errorf("%w", ErrPrecondition)
		}
		return fmt.Errorf("%w: HTTP 400 %s", ErrUpstream, orUnknown(reason))
	}
	return fmt.Errorf("%w: HTTP %d %s", ErrUpstream, status, orUnknown(reason))
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
