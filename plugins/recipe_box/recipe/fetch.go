// File overview: Fetching a shared page and its picture.
//
// This is the one place the recipe box reaches out of the installation, and it
// is shaped by that. The dialer resolves names itself and refuses private and
// special-purpose addresses on every hop of a redirect, so a link in a mail
// cannot become a request against the network Rolltop runs in. Nothing is sent
// that identifies the reader -- no cookie jar, no referrer. And every read is
// bounded in time and in bytes, because a page is hostile input and the worker
// fetching it holds no lock anybody else waits on but its own queue.

package recipe

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/text/encoding/htmlindex"
)

const (
	// MaxPageBytes bounds one page read. JSON-LD sits in the head on nearly
	// every site, and the ones that put it at the end of the body still do so
	// well inside this.
	MaxPageBytes = 3 * 1024 * 1024
	// MaxImageBytes bounds a stored picture. A hero image larger than this is
	// a print-resolution original, and the card is a few hundred pixels wide.
	MaxImageBytes = 2 * 1024 * 1024
	// FetchTimeout bounds one request from dial to the last byte.
	FetchTimeout = 20 * time.Second
	maxRedirects = 5
	// userAgent says what is asking. It reads as a browser because a good
	// share of recipe sites answer anything else with a challenge page.
	userAgent = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0 Safari/537.36 RolltopRecipeBox/1.0"
)

// ErrNotHTML is a link that answered with something other than a page.
var ErrNotHTML = errors.New("link is not a web page")

// HTTPStatusError is a page that answered with an error status. Callers tell
// a page that is gone from one that is busy by its code.
type HTTPStatusError struct {
	Code int
}

func (e HTTPStatusError) Error() string { return fmt.Sprintf("page answered HTTP %d", e.Code) }

// Permanent reports whether asking again later cannot help.
func (e HTTPStatusError) Permanent() bool {
	return e.Code == http.StatusNotFound || e.Code == http.StatusGone || e.Code == http.StatusBadRequest ||
		e.Code == http.StatusUnauthorized || e.Code == http.StatusPaymentRequired
}

// Fetcher fetches pages and pictures through one guarded client.
type Fetcher struct {
	Client *http.Client
}

// NewFetcher builds a fetcher whose connections are made by dial, which must
// refuse addresses that are not on the public internet. Tests pass a dialer
// that reaches their own server; everything else passes
// remoteimages.SafeDialContext.
func NewFetcher(dial func(ctx context.Context, network, address string) (net.Conn, error)) *Fetcher {
	transport := &http.Transport{
		Proxy:                 nil,
		DialContext:           dial,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          4,
		IdleConnTimeout:       30 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 15 * time.Second,
	}
	return &Fetcher{Client: &http.Client{
		Timeout:   FetchTimeout,
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= maxRedirects {
				return errors.New("too many redirects")
			}
			scheme := strings.ToLower(req.URL.Scheme)
			if scheme != "http" && scheme != "https" {
				return errors.New("redirect leaves the web")
			}
			req.Header.Del("Referer")
			return nil
		},
	}}
}

// FetchPage reads one page and answers what it says about itself, with the
// URL it ended up at after redirects.
func (f *Fetcher) FetchPage(ctx context.Context, link string) (Page, string, error) {
	body, finalURL, contentType, err := f.get(ctx, link, "text/html,application/xhtml+xml;q=0.9,*/*;q=0.5", MaxPageBytes)
	if err != nil {
		return Page{}, "", err
	}
	mediaType, params, _ := mime.ParseMediaType(contentType)
	if mediaType != "" && mediaType != "text/html" && mediaType != "application/xhtml+xml" {
		return Page{}, finalURL, ErrNotHTML
	}
	markup := decodeMarkup(body, params["charset"])
	return ParsePage(markup, finalURL), finalURL, nil
}

// FetchImage reads one picture. It accepts only the raster formats a browser
// draws from an <img> without running anything: an SVG is a document, and
// serving one back from Rolltop's own origin would be serving script.
func (f *Fetcher) FetchImage(ctx context.Context, link string) ([]byte, string, error) {
	body, _, contentType, err := f.get(ctx, link, "image/avif,image/webp,image/png,image/jpeg,image/*;q=0.8", MaxImageBytes)
	if err != nil {
		return nil, "", err
	}
	sniffed := SniffImageType(body)
	if sniffed == "" {
		return nil, "", fmt.Errorf("picture is not a supported image (%s)", strings.TrimSpace(contentType))
	}
	return body, sniffed, nil
}

// SniffImageType names the raster format a picture really is, from its bytes
// rather than from what the server called it, or empty when it is none of
// them. It is what decides what Rolltop serves the bytes as.
func SniffImageType(data []byte) string {
	switch {
	case bytes.HasPrefix(data, []byte{0xFF, 0xD8, 0xFF}):
		return "image/jpeg"
	case bytes.HasPrefix(data, []byte("\x89PNG\r\n\x1a\n")):
		return "image/png"
	case bytes.HasPrefix(data, []byte("GIF87a")) || bytes.HasPrefix(data, []byte("GIF89a")):
		return "image/gif"
	case len(data) >= 12 && bytes.Equal(data[0:4], []byte("RIFF")) && bytes.Equal(data[8:12], []byte("WEBP")):
		return "image/webp"
	case len(data) >= 12 && bytes.Equal(data[4:8], []byte("ftyp")) && (bytes.Equal(data[8:12], []byte("avif")) || bytes.Equal(data[8:12], []byte("avis"))):
		return "image/avif"
	}
	return ""
}

func (f *Fetcher) get(ctx context.Context, link, accept string, limit int64) ([]byte, string, string, error) {
	u, err := url.Parse(link)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, "", "", errors.New("link is not an http(s) address")
	}
	ctx, cancel := context.WithTimeout(ctx, FetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, "", "", err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", accept)
	req.Header.Set("Accept-Language", "de,en;q=0.8")
	resp, err := f.Client.Do(req)
	if err != nil {
		return nil, "", "", err
	}
	defer resp.Body.Close()
	finalURL := link
	if resp.Request != nil && resp.Request.URL != nil {
		finalURL = resp.Request.URL.String()
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return nil, finalURL, "", HTTPStatusError{Code: resp.StatusCode}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, finalURL, "", err
	}
	if int64(len(body)) > limit {
		// A page longer than the bound still has its head inside it; a picture
		// longer than the bound is not one to keep.
		if strings.HasPrefix(accept, "image/") {
			return nil, finalURL, "", errors.New("picture is too large")
		}
		body = body[:limit]
	}
	return body, finalURL, resp.Header.Get("Content-Type"), nil
}

// decodeMarkup turns a page into UTF-8 by the charset its server or its own
// meta tag names. Most pages are UTF-8 already; the ones that are not are old
// German recipe sites written in ISO-8859-1, which are exactly the ones whose
// umlauts would otherwise be lost.
func decodeMarkup(body []byte, charset string) string {
	if charset == "" {
		head := body
		if len(head) > 4096 {
			head = head[:4096]
		}
		lower := strings.ToLower(string(head))
		if index := strings.Index(lower, "charset="); index >= 0 {
			rest := strings.TrimLeft(lower[index+len("charset="):], `"' `)
			end := strings.IndexAny(rest, `"'; />`)
			if end > 0 {
				charset = rest[:end]
			}
		}
	}
	charset = strings.ToLower(strings.TrimSpace(charset))
	if charset == "" || charset == "utf-8" || charset == "utf8" {
		return string(body)
	}
	encoding, err := htmlindex.Get(charset)
	if err != nil {
		return string(body)
	}
	decoded, err := encoding.NewDecoder().Bytes(body)
	if err != nil {
		return string(body)
	}
	return string(decoded)
}
