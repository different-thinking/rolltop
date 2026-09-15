// File overview: A small WebDAV client -- PUT, MKCOL, PROPFIND, GET, DELETE --
// over net/http. The dial guard every transport shares is in dial.go.
//
// There is no WebDAV client in the module and pulling one in for five verbs
// would be a dependency to keep current for less code than this file. What is
// here is deliberately the subset an archive uses: put a file somewhere,
// create the collections above it when they do not exist yet, and list or read
// back what landed. Locking, versioning and property writes are not modelled,
// because nothing in this plugin asks for them.

package main

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"
)

const (
	// requestTimeout bounds one verb. An upload of a voice memo over a slow
	// link is the long case, so it is generous rather than tight; the worker
	// that calls this is asynchronous and nothing waits on it.
	requestTimeout = 2 * time.Minute
	// maxListingBytes bounds a PROPFIND response. A directory listing is read
	// into memory to be parsed, so a server answering with an unbounded body --
	// or an XML bomb -- must not be able to decide this process's memory use.
	maxListingBytes = 8 << 20
	// maxDownloadBytes bounds a proxied GET. The browser streams through this
	// process, so the same argument applies.
	maxDownloadBytes = 512 << 20
)

// errNotFound is what a 404 becomes, so callers can tell "no such collection"
// from "the server refused". MKCOL-on-demand reads it as "create the parent".
var errNotFound = errors.New("webdav: resource not found")

// errConflict is 409, which WebDAV uses for "a parent collection is missing".
var errConflict = errors.New("webdav: parent collection is missing")

type webdavClient struct {
	baseURL  *url.URL
	username string
	password string
	http     *http.Client
}

var _ remoteStore = (*webdavClient)(nil)

// Close satisfies remoteStore. WebDAV is a sequence of independent requests
// over a shared http.Client, so there is no session to end -- the idle
// connections the transport keeps are its own to close when it is collected.
func (c *webdavClient) Close() error { return nil }

// resourceEntry is one row of a PROPFIND listing, reduced to what a file
// browser draws.
type resourceEntry struct {
	Name        string `json:"name"`
	Path        string `json:"path"`
	IsDir       bool   `json:"is_dir"`
	Size        int64  `json:"size"`
	ContentType string `json:"content_type"`
	ModifiedAt  int64  `json:"modified_at"`
}

func newWebDAVClient(address targetAddress, username, password string) (*webdavClient, error) {
	if address.URL == nil {
		return nil, errors.New("the WebDAV address was not parsed")
	}
	return &webdavClient{
		baseURL:  address.URL,
		username: username,
		password: password,
		http: &http.Client{
			Timeout: requestTimeout,
			Transport: &http.Transport{
				DialContext:           safeDialContext,
				TLSHandshakeTimeout:   dialTimeout,
				ResponseHeaderTimeout: requestTimeout,
				MaxIdleConnsPerHost:   2,
			},
			// An archive follows a redirect the way a browser would not: the
			// target of a redirect is a host the dial guard has not been asked
			// about at the URL the user configured. Refusing them keeps the
			// only host this client talks to the one that was validated.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return errors.New("webdav: the server redirected, which this client does not follow")
			},
		},
	}, nil
}

// resolve turns a store-relative path into an absolute URL under the base. The
// path is cleaned and confined: a target configured with a subdirectory cannot
// be walked out of with `..`.
//
// Both halves of url.URL's path are set, and that is the whole subtlety here.
// `Path` is the decoded path and `RawPath` its encoding; `String()` emits
// `RawPath` when it is a valid encoding of `Path`, and otherwise escapes
// `Path` itself. Writing pre-escaped text into `Path` alone therefore escapes
// it a second time on the way out -- `voice memo.m4a` leaves as
// `voice%2520memo.m4a`, and the file lands on the server under a name with a
// literal percent in it that nothing can then fetch back.
func (c *webdavClient) resolve(relative string) (*url.URL, error) {
	clean := cleanRemotePath(relative)
	out := *c.baseURL
	// Each segment is escaped on its own so a slash in the input stays a
	// separator and a space or a `#` in a filename does not become one.
	segments := strings.Split(clean, "/")
	decoded := make([]string, 0, len(segments))
	escaped := make([]string, 0, len(segments))
	for _, segment := range segments {
		if segment == "" {
			continue
		}
		decoded = append(decoded, segment)
		escaped = append(escaped, url.PathEscape(segment))
	}
	trailing := ""
	if strings.HasSuffix(clean, "/") && len(decoded) > 0 {
		trailing = "/"
	}
	out.Path = c.baseURL.Path + strings.Join(decoded, "/") + trailing
	out.RawPath = c.baseURL.EscapedPath() + strings.Join(escaped, "/") + trailing
	// A RawPath that does not decode back to Path is ignored by EscapedPath,
	// which then escapes Path itself -- correct, just less precise than the
	// per-segment escaping above. Clearing it says so rather than leaving a
	// field behind that no longer describes Path.
	if out.EscapedPath() != out.RawPath {
		out.RawPath = ""
	}
	return &out, nil
}

// cleanRemotePath reduces a caller's path to a relative, traversal-free one.
// It keeps a trailing slash, because that is how a collection is addressed.
func cleanRemotePath(value string) string {
	value = strings.TrimSpace(strings.ReplaceAll(value, "\\", "/"))
	trailing := strings.HasSuffix(value, "/")
	cleaned := path.Clean("/" + value)
	cleaned = strings.TrimPrefix(cleaned, "/")
	if cleaned == "." {
		cleaned = ""
	}
	if trailing && cleaned != "" {
		cleaned += "/"
	}
	return cleaned
}

func (c *webdavClient) request(ctx context.Context, method, relative string, body io.Reader, contentType string, headers map[string]string) (*http.Response, error) {
	target, err := c.resolve(relative)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, target.String(), body)
	if err != nil {
		return nil, err
	}
	if c.username != "" || c.password != "" {
		req.SetBasicAuth(c.username, c.password)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	return c.http.Do(req)
}

// statusError maps a response the caller did not accept onto an error whose
// text is safe to show a reader: the server's status line, never its body,
// which can carry a full HTML error page.
func statusError(method string, res *http.Response) error {
	switch res.StatusCode {
	case http.StatusNotFound:
		return errNotFound
	case http.StatusConflict:
		return errConflict
	case http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Errorf("the WebDAV server rejected the credentials (%s)", res.Status)
	}
	return fmt.Errorf("%s failed: %s", method, res.Status)
}

// CheckAccess verifies the base URL is reachable and the credentials are
// accepted, which is what the settings form's Test button asks.
func (c *webdavClient) CheckAccess(ctx context.Context) error {
	_, err := c.List(ctx, "")
	return err
}

// List reads one collection, one level deep.
func (c *webdavClient) List(ctx context.Context, relative string) ([]resourceEntry, error) {
	if relative != "" && !strings.HasSuffix(relative, "/") {
		relative += "/"
	}
	const body = `<?xml version="1.0" encoding="utf-8"?>
<d:propfind xmlns:d="DAV:"><d:prop>
<d:resourcetype/><d:getcontentlength/><d:getcontenttype/><d:getlastmodified/>
</d:prop></d:propfind>`
	res, err := c.request(ctx, "PROPFIND", relative, strings.NewReader(body), `application/xml; charset="utf-8"`,
		map[string]string{"Depth": "1"})
	if err != nil {
		return nil, err
	}
	defer drainAndClose(res)
	if res.StatusCode != http.StatusMultiStatus && res.StatusCode != http.StatusOK {
		return nil, statusError("PROPFIND", res)
	}
	return c.parseListing(res.Body, relative)
}

// multistatus is the subset of RFC 4918's response this client reads.
type multistatus struct {
	XMLName   xml.Name         `xml:"DAV: multistatus"`
	Responses []davResponseXML `xml:"DAV: response"`
}

type davResponseXML struct {
	Href     string           `xml:"DAV: href"`
	Propstat []davPropstatXML `xml:"DAV: propstat"`
}

type davPropstatXML struct {
	Status string     `xml:"DAV: status"`
	Prop   davPropXML `xml:"DAV: prop"`
}

type davPropXML struct {
	ResourceType  *davResourceTypeXML `xml:"DAV: resourcetype"`
	ContentLength string              `xml:"DAV: getcontentlength"`
	ContentType   string              `xml:"DAV: getcontenttype"`
	LastModified  string              `xml:"DAV: getlastmodified"`
}

type davResourceTypeXML struct {
	Collection *struct{} `xml:"DAV: collection"`
}

func (c *webdavClient) parseListing(body io.Reader, relative string) ([]resourceEntry, error) {
	var parsed multistatus
	decoder := xml.NewDecoder(io.LimitReader(body, maxListingBytes))
	// A WebDAV response is data, not a document that may pull in more of its
	// own: entity expansion and external references are both refused, so a
	// hostile or broken server cannot turn a listing into a local file read.
	decoder.Strict = false
	decoder.Entity = map[string]string{}
	decoder.CharsetReader = func(_ string, input io.Reader) (io.Reader, error) { return input, nil }
	if err := decoder.Decode(&parsed); err != nil {
		return nil, fmt.Errorf("the WebDAV listing could not be read: %w", err)
	}
	basePath := c.baseURL.EscapedPath()
	prefix := basePath + strings.TrimPrefix(cleanRemotePath(relative), "/")
	out := make([]resourceEntry, 0, len(parsed.Responses))
	for _, response := range parsed.Responses {
		entry, ok := resourceFromResponse(response, basePath)
		if !ok {
			continue
		}
		// The collection itself is always the first row of its own listing.
		if strings.TrimSuffix(entry.Path, "/") == strings.TrimSuffix(strings.TrimPrefix(prefix, basePath), "/") {
			continue
		}
		out = append(out, entry)
	}
	return out, nil
}

func resourceFromResponse(response davResponseXML, basePath string) (resourceEntry, bool) {
	href := strings.TrimSpace(response.Href)
	if href == "" {
		return resourceEntry{}, false
	}
	// A href may be absolute or path-only; both reduce to a path under the base.
	if parsed, err := url.Parse(href); err == nil {
		href = parsed.EscapedPath()
	}
	if !strings.HasPrefix(href, basePath) {
		return resourceEntry{}, false
	}
	relative, err := url.PathUnescape(strings.TrimPrefix(href, basePath))
	if err != nil {
		return resourceEntry{}, false
	}
	if relative == "" || relative == "/" {
		return resourceEntry{}, false
	}
	entry := resourceEntry{Path: strings.TrimPrefix(relative, "/")}
	for _, propstat := range response.Propstat {
		// Only the 200 propstat carries values; the others say which properties
		// the server does not have, and reading those as data would report a
		// zero size for every file on a server that answers that way.
		if !strings.Contains(propstat.Status, " 200 ") {
			continue
		}
		prop := propstat.Prop
		if prop.ResourceType != nil && prop.ResourceType.Collection != nil {
			entry.IsDir = true
		}
		if size, err := strconv.ParseInt(strings.TrimSpace(prop.ContentLength), 10, 64); err == nil {
			entry.Size = size
		}
		if value := strings.TrimSpace(prop.ContentType); value != "" {
			entry.ContentType = value
		}
		if value := strings.TrimSpace(prop.LastModified); value != "" {
			if when, err := http.ParseTime(value); err == nil {
				entry.ModifiedAt = when.UTC().Unix()
			}
		}
	}
	trimmed := strings.TrimSuffix(entry.Path, "/")
	entry.Name = path.Base(trimmed)
	if entry.IsDir && !strings.HasSuffix(entry.Path, "/") {
		entry.Path += "/"
	}
	return entry, entry.Name != "" && entry.Name != "."
}

// Exists answers whether one resource is there, which is how the worker skips
// an upload a previous attempt already completed before it failed to record it.
func (c *webdavClient) Exists(ctx context.Context, relative string) (bool, error) {
	res, err := c.request(ctx, http.MethodHead, relative, nil, "", nil)
	if err != nil {
		return false, err
	}
	defer drainAndClose(res)
	switch {
	case res.StatusCode == http.StatusNotFound:
		return false, nil
	case res.StatusCode >= 200 && res.StatusCode < 300:
		return true, nil
	}
	return false, statusError("HEAD", res)
}

// Put writes one file, creating the collections above it if the server says
// they are missing. The body is a byte slice rather than a reader because the
// retry needs to send it a second time.
func (c *webdavClient) Put(ctx context.Context, relative string, data []byte, contentType string) error {
	put := func() (*http.Response, error) {
		return c.request(ctx, http.MethodPut, relative, bytes.NewReader(data), contentType, nil)
	}
	res, err := put()
	if err != nil {
		return err
	}
	if res.StatusCode == http.StatusConflict || res.StatusCode == http.StatusNotFound {
		drainAndClose(res)
		if err := c.MakeCollections(ctx, path.Dir(cleanRemotePath(relative))); err != nil {
			return err
		}
		res, err = put()
		if err != nil {
			return err
		}
	}
	defer drainAndClose(res)
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return statusError("PUT", res)
	}
	return nil
}

// MakeCollections creates every missing collection along a path. Each MKCOL
// that answers 405 (already there) is a success, which is what makes this safe
// to call before every upload rather than only on a miss.
func (c *webdavClient) MakeCollections(ctx context.Context, dir string) error {
	dir = cleanRemotePath(dir)
	if dir == "" || dir == "." || dir == "/" {
		return nil
	}
	segments := strings.Split(strings.Trim(dir, "/"), "/")
	current := ""
	for _, segment := range segments {
		if segment == "" {
			continue
		}
		current += segment + "/"
		res, err := c.request(ctx, "MKCOL", current, nil, "", nil)
		if err != nil {
			return err
		}
		status := res.StatusCode
		drainAndClose(res)
		switch {
		case status >= 200 && status < 300:
		case status == http.StatusMethodNotAllowed:
			// The collection is already there.
		default:
			return fmt.Errorf("creating %q on the WebDAV server failed: %s", current, http.StatusText(status))
		}
	}
	return nil
}

// Get streams one file back. The caller closes the returned reader.
func (c *webdavClient) Get(ctx context.Context, relative string) (io.ReadCloser, string, int64, error) {
	res, err := c.request(ctx, http.MethodGet, relative, nil, "", nil)
	if err != nil {
		return nil, "", 0, err
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		defer drainAndClose(res)
		return nil, "", 0, statusError("GET", res)
	}
	contentType := strings.TrimSpace(res.Header.Get("Content-Type"))
	size := res.ContentLength
	if size > maxDownloadBytes {
		drainAndClose(res)
		return nil, "", 0, errors.New("the file is larger than this proxy will serve")
	}
	return newLimitedReadCloser(res.Body, maxDownloadBytes), contentType, size, nil
}

// Delete removes one resource.
func (c *webdavClient) Delete(ctx context.Context, relative string) error {
	res, err := c.request(ctx, http.MethodDelete, relative, nil, "", nil)
	if err != nil {
		return err
	}
	defer drainAndClose(res)
	if res.StatusCode == http.StatusNotFound {
		return errNotFound
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return statusError("DELETE", res)
	}
	return nil
}

func drainAndClose(res *http.Response) {
	if res == nil || res.Body == nil {
		return
	}
	// Bounded rather than io.Discard on its own: a body being drained only so
	// the connection can be reused should not read an unbounded error page.
	_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 64<<10))
	_ = res.Body.Close()
}

type limitedReadCloser struct {
	reader io.Reader
	closer io.Closer
}

func newLimitedReadCloser(body io.ReadCloser, limit int64) io.ReadCloser {
	return &limitedReadCloser{reader: io.LimitReader(body, limit), closer: body}
}

func (l *limitedReadCloser) Read(p []byte) (int, error) { return l.reader.Read(p) }
func (l *limitedReadCloser) Close() error               { return l.closer.Close() }
