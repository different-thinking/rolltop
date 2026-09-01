// File overview: The three routes the file browser is built on -- list a
// collection, read one file, remove one file.
//
// Every one of them goes through this server rather than letting the browser
// talk to the WebDAV host: the credentials are stored encrypted here and must
// not reach a page, the host may be one only this server can route to, and the
// dial guard in the client is worth nothing if the browser can be pointed
// anywhere instead.

package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"rolltop/backend/plugins"
)

// browseTimeout bounds a listing. A reader is waiting on this one, unlike an
// upload, so it gives up sooner.
const browseTimeout = 30 * time.Second

type browseView struct {
	TargetID int64           `json:"target_id"`
	Path     string          `json:"path"`
	Parent   string          `json:"parent"`
	Entries  []resourceEntry `json:"entries"`
}

func (p *webdavArchiveBackend) apiBrowse(host plugins.APIHost, db *sql.DB, userID int64, w http.ResponseWriter, r *http.Request) {
	client, _, requested, ok := p.resolveBrowseRequest(host, db, userID, w, r)
	if !ok {
		return
	}
	targetID, _ := strconv.ParseInt(r.URL.Query().Get("target"), 10, 64)
	ctx, cancel := context.WithTimeout(r.Context(), browseTimeout)
	defer cancel()
	entries, err := client.List(ctx, requested)
	if err != nil {
		if errors.Is(err, errNotFound) {
			host.WriteAPIError(w, http.StatusNotFound, "that folder is not on the WebDAV server")
			return
		}
		// A server that is unreachable or rejecting the credentials is a
		// configuration answer, not a server fault of this Rolltop's: it is
		// reported as a readable message rather than a 500 with a stack behind
		// it.
		host.WriteAPIError(w, http.StatusBadGateway, err.Error())
		return
	}
	host.WriteJSON(w, browseView{
		TargetID: targetID,
		Path:     cleanRemotePath(requested),
		Parent:   parentPath(requested),
		Entries:  entries,
	})
}

func (p *webdavArchiveBackend) apiDownload(host plugins.APIHost, db *sql.DB, userID int64, w http.ResponseWriter, r *http.Request) {
	client, _, requested, ok := p.resolveBrowseRequest(host, db, userID, w, r)
	if !ok {
		return
	}
	if requested == "" || strings.HasSuffix(requested, "/") {
		host.WriteAPIError(w, http.StatusBadRequest, "a file path is required")
		return
	}
	body, contentType, size, err := client.Get(r.Context(), requested)
	if err != nil {
		if errors.Is(err, errNotFound) {
			host.WriteAPIError(w, http.StatusNotFound, "that file is not on the WebDAV server")
			return
		}
		host.WriteAPIError(w, http.StatusBadGateway, err.Error())
		return
	}
	defer body.Close()
	inline := r.URL.Query().Get("inline") == "1"
	if strings.TrimSpace(contentType) == "" {
		contentType = "application/octet-stream"
	}
	header := w.Header()
	header.Set("Content-Type", contentType)
	// The bytes come from a server this Rolltop does not own, so they are
	// served with the guards a same-origin download of foreign content needs:
	// the declared type is the only one honoured, nothing is cached, and the
	// response is not allowed to pull in or run anything of its own.
	header.Set("X-Content-Type-Options", "nosniff")
	header.Set("Content-Security-Policy", "default-src 'none'; sandbox")
	header.Set("Cache-Control", "private, no-store")
	header.Set("Vary", "Cookie")
	header.Set("Content-Disposition", contentDisposition(inline, path.Base(requested), contentType))
	if size > 0 {
		header.Set("Content-Length", strconv.FormatInt(size, 10))
	}
	if _, err := io.Copy(w, body); err != nil {
		// The status line is already written, so this can only be logged. The
		// browser sees a truncated response, which is what actually happened.
		return
	}
}

func (p *webdavArchiveBackend) apiDeleteFile(host plugins.APIHost, db *sql.DB, userID int64, w http.ResponseWriter, r *http.Request) {
	if !host.VerifyCSRF(w, r) {
		return
	}
	client, _, requested, ok := p.resolveBrowseRequest(host, db, userID, w, r)
	if !ok {
		return
	}
	if requested == "" {
		host.WriteAPIError(w, http.StatusBadRequest, "a path is required")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), browseTimeout)
	defer cancel()
	if err := client.Delete(ctx, requested); err != nil {
		if errors.Is(err, errNotFound) {
			host.WriteAPIError(w, http.StatusNotFound, "that file is not on the WebDAV server")
			return
		}
		host.WriteAPIError(w, http.StatusBadGateway, err.Error())
		return
	}
	host.WriteJSON(w, map[string]any{"ok": true})
}

// resolveBrowseRequest is the shared front half of all three: it reads the
// target, checks it belongs to this user, and reduces the requested path to a
// relative one. It answers the error itself and reports whether the caller
// should continue.
func (p *webdavArchiveBackend) resolveBrowseRequest(host plugins.APIHost, db *sql.DB, userID int64, w http.ResponseWriter, r *http.Request) (*webdavClient, target, string, bool) {
	query := r.URL.Query()
	targetID, err := strconv.ParseInt(strings.TrimSpace(query.Get("target")), 10, 64)
	if err != nil || targetID <= 0 {
		host.WriteAPIError(w, http.StatusBadRequest, "a WebDAV target is required")
		return nil, target{}, "", false
	}
	client, configured, err := targetClient(r.Context(), host, db, userID, targetID)
	if err != nil {
		writeScopedError(host, w, err, "target not found")
		return nil, target{}, "", false
	}
	// cleanRemotePath is what confines the request to the configured base: it
	// resolves `..` away rather than rejecting it, so no combination of
	// segments addresses anything above the target's own root.
	return client, configured, cleanRemotePath(query.Get("path")), true
}

// parentPath is the folder one level up, or "" at the root of the target.
func parentPath(value string) string {
	cleaned := strings.TrimSuffix(cleanRemotePath(value), "/")
	if cleaned == "" {
		return ""
	}
	parent := path.Dir(cleaned)
	if parent == "." || parent == "/" {
		return ""
	}
	return parent + "/"
}

// contentDisposition decides whether the browser may render the file in place.
// Only media types get that: an audio recording is the thing this archive is
// full of and playing it without downloading it first is the point, while an
// HTML or SVG file from a server this Rolltop does not control would be a
// document running on this origin.
func contentDisposition(inline bool, filename, contentType string) string {
	disposition := "attachment"
	if inline && renderableInline(contentType) {
		disposition = "inline"
	}
	name := path.Base(strings.TrimSpace(filename))
	if name == "." || name == "/" || name == "" {
		name = "download"
	}
	return fmt.Sprintf("%s; filename=%q", disposition, name)
}

func renderableInline(contentType string) bool {
	value := normalizeContentType(contentType)
	return strings.HasPrefix(value, "audio/") || strings.HasPrefix(value, "video/") ||
		strings.HasPrefix(value, "image/")
}
