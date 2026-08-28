// File overview: Top-level API dispatcher. It routes authenticated API requests to account, mail, message, contact, plugin, sync, and admin handlers.

package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

func (s *Server) handleAPI(w http.ResponseWriter, r *http.Request) {
	path := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/"), "/")
	switch {
	case path == "bootstrap":
		s.apiBootstrap(w, r)
	case path == "setup":
		s.apiSetup(w, r)
	case path == "login":
		s.apiLogin(w, r)
	case path == "password-reset/request":
		s.apiPasswordResetRequest(w, r)
	case path == "password-reset/complete":
		s.apiPasswordResetComplete(w, r)
	case path == "logout":
		s.apiLogout(w, r)
	case path == "profile":
		s.apiProfile(w, r)
	case path == "profile/swipes":
		s.apiSwipePreferences(w, r)
	case path == "profile/retention":
		s.apiRetention(w, r)
	case path == "mail":
		s.apiMail(w, r)
	case path == "snoozes":
		s.apiSnoozes(w, r)
	case path == "mail/category":
		s.apiMessageCategory(w, r)
	case path == "search":
		s.apiSearch(w, r)
	case path == "compose":
		s.apiCompose(w, r)
	case path == "compose/draft":
		s.apiComposeDraft(w, r)
	case path == "sync/status":
		s.apiSyncStatus(w, r)
	case path == "activity":
		s.apiActivity(w, r)
	case path == "activity/history":
		s.apiActivityHistory(w, r)
	case path == "activity/workers/cancel":
		s.apiActivityWorkerCancel(w, r)
	case path == "events":
		s.apiEvents(w, r)
	case path == "storage":
		s.apiStorage(w, r)
	case path == "storage/search-index/rebuild":
		s.apiStorageSearchRebuild(w, r)
	case path == "push/vapid-public-key":
		s.apiPushVAPIDPublicKey(w, r)
	case path == "push/subscription":
		s.apiPushSubscription(w, r)
	case path == "notifications/new-mail":
		s.apiNewMailNotifications(w, r)
	case path == "notifications/reminders":
		s.apiSnoozeReminderNotifications(w, r)
	case path == "plugins":
		s.apiPlugins(w, r)
	case strings.HasPrefix(path, "plugins/"):
		s.apiBackendPlugin(w, r, strings.TrimPrefix(path, "plugins/"))
	case path == "deliveries":
		s.apiDeliveries(w, r)
	case path == "deliveries/expected":
		s.apiDeliveriesExpected(w, r)
	case path == "contacts":
		s.apiContacts(w, r)
	case strings.HasPrefix(path, "contacts/"):
		s.apiContactPath(w, r, strings.TrimPrefix(path, "contacts/"))
	case strings.HasPrefix(path, "calendar/"):
		s.apiCalendarPath(w, r, strings.TrimPrefix(path, "calendar/"))
	case path == "brand-icons":
		s.apiBrandIcons(w, r)
	case path == "google/connect":
		s.apiGoogleConnect(w, r)
	case path == "google/callback":
		s.apiGoogleCallback(w, r)
	case path == "google/connections":
		s.apiGoogleConnections(w, r)
	case strings.HasPrefix(path, "google/connections/"):
		s.apiGoogleConnectionByID(w, r, strings.TrimPrefix(path, "google/connections/"))
	case path == "account":
		s.apiAccount(w, r)
	case path == "account/imap":
		s.apiIMAPAccount(w, r)
	case strings.HasPrefix(path, "account/imap/"):
		s.apiIMAPAccountPath(w, r, strings.TrimPrefix(path, "account/imap/"))
	case path == "smtp-log":
		s.apiSMTPLog(w, r)
	case path == "account/smtp":
		s.apiSMTPAccount(w, r)
	case strings.HasPrefix(path, "account/smtp/"):
		s.apiSMTPAccountPath(w, r, strings.TrimPrefix(path, "account/smtp/"))
	case path == "account/identities":
		s.apiMailIdentity(w, r)
	case strings.HasPrefix(path, "account/identities/"):
		s.apiMailIdentityPath(w, r, strings.TrimPrefix(path, "account/identities/"))
	case path == "account/sync":
		s.apiAccountSync(w, r)
	case path == "account/duplicates":
		s.apiAccountDuplicates(w, r)
	case path == "account/duplicates/rescan":
		s.apiAccountDuplicatesRescan(w, r)
	case path == "account/duplicates/trash":
		s.apiAccountDuplicatesTrash(w, r)
	case path == "account/folders/progress":
		s.apiAccountFolderProgress(w, r)
	case strings.HasPrefix(path, "account/folders/"):
		s.apiAccountFolder(w, r, strings.TrimPrefix(path, "account/folders/"))
	case path == "admin/users":
		s.apiAdminUsers(w, r)
	case strings.HasPrefix(path, "admin/users/"):
		s.apiAdminUserPath(w, r, strings.TrimPrefix(path, "admin/users/"))
	case path == "admin/password-reset":
		s.apiAdminPasswordResetSettings(w, r)
	case path == "admin/plugins":
		s.apiAdminPlugins(w, r)
	case strings.HasPrefix(path, "admin/plugins/"):
		s.apiAdminPlugin(w, r, strings.TrimPrefix(path, "admin/plugins/"))
	case path == "admin/search-index":
		s.apiAdminSearchIndex(w, r)
	case path == "admin/database":
		s.apiAdminDatabase(w, r)
	case path == "admin/log":
		s.apiAdminLog(w, r)
	case path == "admin/remote-image-blocklist":
		s.apiAdminRemoteImageBlocklist(w, r)
	case path == "messages/bulk-move":
		s.apiBulkMoveMessages(w, r)
	case path == "messages/bulk-copy":
		s.apiBulkCopyMessages(w, r)
	case path == "messages/bulk-read":
		s.apiBulkReadMessages(w, r)
	case path == "messages/scope-trash":
		s.apiScopeTrashMessages(w, r)
	case path == "messages/scope-archive":
		s.apiScopeArchiveMessages(w, r)
	case path == "messages/empty-trash":
		s.apiEmptyTrash(w, r)
	case strings.HasPrefix(path, "messages/"):
		s.apiMessagePath(w, r, strings.TrimPrefix(path, "messages/"))
	case strings.HasPrefix(path, "sync-runs/"):
		s.apiSyncRun(w, r, strings.TrimPrefix(path, "sync-runs/"))
	default:
		http.NotFound(w, r)
	}
}
func (s *Server) requireAPIAuth(w http.ResponseWriter, r *http.Request) (currentUser, bool) {
	cu, ok := current(r)
	if !ok {
		if sessionLookupFailed(r) {
			// The session cookie could not be checked against the store;
			// answering 401 here would read as a forced logout in the client.
			writeAPIError(w, http.StatusServiceUnavailable, "session lookup temporarily unavailable, retry shortly")
			return currentUser{}, false
		}
		writeAPIError(w, http.StatusUnauthorized, "login required")
		return currentUser{}, false
	}
	// Keep an already-warm compose identity cache available while this signed-in
	// user is active. This only touches memory; it never adds a database lookup
	// to ordinary mail, search, or event requests.
	s.touchComposeIdentityCache(cu.User.ID)
	return cu, true
}

func (s *Server) requireAPIAdmin(w http.ResponseWriter, r *http.Request) (currentUser, bool) {
	cu, ok := s.requireAPIAuth(w, r)
	if !ok {
		return currentUser{}, false
	}
	if !cu.User.IsAdmin {
		writeAPIError(w, http.StatusForbidden, "forbidden")
		return currentUser{}, false
	}
	return cu, true
}

func decodeJSON(w http.ResponseWriter, r *http.Request, dest any) bool {
	defer r.Body.Close()
	if err := json.NewDecoder(r.Body).Decode(dest); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid JSON")
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(value)
}

// writeJSONStatus answers with a body and a non-200 status. It exists for the
// few failures that carry data the client needs -- a rejected edit that has to
// show what the record looks like now -- which writeAPIError cannot express.
func writeJSONStatus(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeJSONCached(w http.ResponseWriter, r *http.Request, value any) {
	_, _ = writeJSONCachedWithETag(w, r, value)
}

func writeJSONCachedWithETag(w http.ResponseWriter, r *http.Request, value any) (string, bool) {
	body, etag, err := cachedJSONBody(value)
	if err != nil {
		logHandlerError(r, fmt.Errorf("encode cached JSON response: %w", err))
		writeAPIError(w, http.StatusInternalServerError, "failed to encode response")
		return "", false
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "private, max-age=0, must-revalidate")
	w.Header().Set("ETag", etag)
	if r.Method == http.MethodGet && etagMatches(r.Header.Get("If-None-Match"), etag) {
		w.WriteHeader(http.StatusNotModified)
		return etag, true
	}
	_, _ = w.Write(body)
	return etag, true
}

func etagMatches(header string, etag string) bool {
	for _, part := range strings.Split(header, ",") {
		candidate := strings.TrimSpace(part)
		if candidate == "*" || candidate == etag {
			return true
		}
	}
	return false
}

func writeAPIError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": message})
}

// messageGoneCode marks the one 404 a client may act on as "this message is not
// here any more". A bare 404 cannot carry that: Go's own NotFound answers in
// plain text, and so does every proxy, gateway and hosting platform in front of
// the app, so a client reading the status alone would file mail away as gone
// because something between it and Rolltop answered. The code is the promise
// that Rolltop itself looked and found nothing.
const messageGoneCode = "message_gone"

// writeMessageGone answers for a message this user does not have.
func writeMessageGone(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusNotFound)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": "message not found", "code": messageGoneCode})
}

// writeRetryAfter answers 429 with a Retry-After header rounded up to whole
// seconds (the header has no sub-second form), so a throttled client is told how
// long to wait instead of guessing.
func writeRetryAfter(w http.ResponseWriter, wait time.Duration, message string) {
	seconds := int(wait / time.Second)
	if wait%time.Second != 0 {
		seconds++
	}
	if seconds < 1 {
		seconds = 1
	}
	w.Header().Set("Retry-After", strconv.Itoa(seconds))
	writeAPIError(w, http.StatusTooManyRequests, message)
}
