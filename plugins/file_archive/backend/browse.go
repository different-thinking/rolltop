// File overview: The three routes the file browser is built on -- list a
// collection, read one file, remove one file.
//
// Every one of them goes through this server rather than letting the browser
// talk to the remote store: the credentials are stored encrypted here and must
// not reach a page, the host may be one only this server can route to, and the
// dial guard in the client is worth nothing if the browser can be pointed
// anywhere instead.

package main

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"rolltop/backend/httpfile"
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

func (p *fileArchiveBackend) apiBrowse(host plugins.APIHost, db *sql.DB, userID int64, w http.ResponseWriter, r *http.Request) {
	store, _, requested, ok := p.resolveBrowseRequest(host, db, userID, w, r)
	if !ok {
		return
	}
	defer store.Close()
	targetID, _ := strconv.ParseInt(r.URL.Query().Get("target"), 10, 64)
	ctx, cancel := context.WithTimeout(r.Context(), browseTimeout)
	defer cancel()
	entries, err := store.List(ctx, requested)
	if err != nil {
		if errors.Is(err, errNotFound) {
			host.WriteAPIError(w, http.StatusNotFound, "that folder is not on the server")
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

func (p *fileArchiveBackend) apiDownload(host plugins.APIHost, db *sql.DB, userID int64, w http.ResponseWriter, r *http.Request) {
	store, _, requested, ok := p.resolveBrowseRequest(host, db, userID, w, r)
	if !ok {
		return
	}
	defer store.Close()
	if requested == "" || strings.HasSuffix(requested, "/") {
		host.WriteAPIError(w, http.StatusBadRequest, "a file path is required")
		return
	}
	body, contentType, size, err := store.Get(r.Context(), requested)
	if err != nil {
		if errors.Is(err, errNotFound) {
			host.WriteAPIError(w, http.StatusNotFound, "that file is not on the server")
			return
		}
		host.WriteAPIError(w, http.StatusBadGateway, err.Error())
		return
	}
	defer body.Close()
	inline := r.URL.Query().Get("inline") == "1"
	if strings.TrimSpace(contentType) == "" {
		// SFTP and SMB store bytes and a name, not a type. Naming the file by
		// its extension is what lets a recording play in place instead of
		// downloading as an opaque blob.
		contentType = guessContentType(requested)
	}
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

func (p *fileArchiveBackend) apiDeleteFile(host plugins.APIHost, db *sql.DB, userID int64, w http.ResponseWriter, r *http.Request) {
	if !host.VerifyCSRF(w, r) {
		return
	}
	store, _, requested, ok := p.resolveBrowseRequest(host, db, userID, w, r)
	if !ok {
		return
	}
	defer store.Close()
	if requested == "" {
		host.WriteAPIError(w, http.StatusBadRequest, "a path is required")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), browseTimeout)
	defer cancel()
	if err := store.Delete(ctx, requested); err != nil {
		if errors.Is(err, errNotFound) {
			host.WriteAPIError(w, http.StatusNotFound, "that file is not on the server")
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
func (p *fileArchiveBackend) resolveBrowseRequest(host plugins.APIHost, db *sql.DB, userID int64, w http.ResponseWriter, r *http.Request) (remoteStore, target, string, bool) {
	query := r.URL.Query()
	targetID, err := strconv.ParseInt(strings.TrimSpace(query.Get("target")), 10, 64)
	if err != nil || targetID <= 0 {
		host.WriteAPIError(w, http.StatusBadRequest, "a target is required")
		return nil, target{}, "", false
	}
	store, configured, err := openTargetStore(r.Context(), host, db, userID, targetID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			host.WriteAPIError(w, http.StatusNotFound, "target not found")
			return nil, target{}, "", false
		}
		// Opening is where SFTP and SMB do their connecting, so a destination
		// that is unreachable or refusing the credentials fails here rather
		// than on the first read. It is a configuration answer either way.
		host.WriteAPIError(w, http.StatusBadGateway, err.Error())
		return nil, target{}, "", false
	}
	// cleanRemotePath is what confines the request to the configured base: it
	// resolves `..` away rather than rejecting it, so no combination of
	// segments addresses anything above the target's own root.
	return store, configured, cleanRemotePath(query.Get("path")), true
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
	return httpfile.Disposition(inline && renderableInline(contentType), filename, "download")
}

func renderableInline(contentType string) bool {
	value := normalizeContentType(contentType)
	// SVG is an image by MIME type and a document by behaviour: it can carry
	// script, and these bytes come from a server this Rolltop does not control.
	// The response's own CSP would stop the script running, but a file that has
	// no reason to render in place is not the thing to lean on it for.
	if value == "image/svg+xml" || value == "image/svg" {
		return false
	}
	return strings.HasPrefix(value, "audio/") || strings.HasPrefix(value, "video/") ||
		strings.HasPrefix(value, "image/")
}
