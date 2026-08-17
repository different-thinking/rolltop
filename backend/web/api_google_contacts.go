// File overview: The Google contact sync surface: per-connection sync state on
// the connections list, and the route that runs a sync on demand. The sync
// itself lives in backend/googlepeople; this file only decides who may ask for
// it and how a failure is described to the user.

package web

import (
	"context"
	"errors"
	"log"
	"net/http"

	"rolltop/backend/googlepeople"
	"rolltop/backend/store"
)

// apiGoogleContactsSync is the per-connection contact sync state shown in
// settings. It never carries the sync token: it is a cursor Rolltop keeps for
// itself, and nothing in the UI can act on it.
type apiGoogleContactsSync struct {
	Status       string `json:"status"`
	StatusDetail string `json:"status_detail"`
	LastSyncAt   string `json:"last_sync_at"`
	LastSuccess  string `json:"last_success_at"`
	ContactCount int    `json:"contact_count"`
	// EverSynced separates "no contacts yet" from "never ran", which look the
	// same in the counters but mean very different things to a user.
	EverSynced bool `json:"ever_synced"`
}

// googleContactsSyncState assembles the sync state of one connection. A store
// failure is logged and reported as no state rather than failing the whole
// connections list, which the settings page needs to render either way.
func (s *Server) googleContactsSyncState(ctx context.Context, userID, connectionID int64) *apiGoogleContactsSync {
	if s.store == nil {
		return nil
	}
	state, err := s.store.GetGooglePeopleSync(ctx, userID, connectionID)
	if err != nil {
		log.Printf("google contact sync state user_id=%d connection_id=%d: %v", userID, connectionID, err)
		return nil
	}
	count, err := s.store.CountGoogleContactsForConnection(ctx, userID, connectionID)
	if err != nil {
		log.Printf("google contact count user_id=%d connection_id=%d: %v", userID, connectionID, err)
	}
	return &apiGoogleContactsSync{
		Status:       state.Status,
		StatusDetail: state.StatusDetail,
		LastSyncAt:   timeString(state.LastSyncAt),
		LastSuccess:  timeString(state.LastSuccessAt),
		ContactCount: count,
		EverSynced:   !state.LastSuccessAt.IsZero(),
	}
}

// googleContactsSyncNow runs a sync for one connection and answers with the
// state the settings page should now show.
//
// It runs inline rather than in the background: the user pressed a button and
// is waiting for the answer, and the sync already bounds itself. A background
// run would have to invent a way to report its result back to this page.
func (s *Server) googleContactsSyncNow(w http.ResponseWriter, r *http.Request, userID, connectionID int64) {
	if !s.verifyCSRF(w, r) {
		return
	}
	if s.googleContacts == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "Contact sync is not available on this server.")
		return
	}
	result, err := s.googleContacts.SyncConnection(r.Context(), userID, connectionID)
	if store.IsNotFound(err) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.writeGoogleContactsError(w, r, err)
		return
	}
	s.clearComposeIdentityCache(userID)
	s.clearSenderContactIconCache(userID)
	writeJSON(w, map[string]any{
		"ok":         true,
		"created":    result.Created,
		"updated":    result.Updated,
		"deleted":    result.Deleted,
		"full_sync":  result.FullSync,
		"sync_state": s.googleContactsSyncState(r.Context(), userID, connectionID),
	})
}

// writeGoogleContactsError maps a sync or write-back failure onto a status code
// and a message the user can act on. Upstream error text is never forwarded: on
// a write it echoes the contact's own data back.
func (s *Server) writeGoogleContactsError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, googlepeople.ErrScopeMissing), errors.Is(err, googlepeople.ErrForbidden):
		writeAPIError(w, http.StatusConflict,
			"This Google account has not granted access to contacts. Reconnect it in Google settings.")
	case errors.Is(err, googlepeople.ErrRemoteChanged):
		writeAPIError(w, http.StatusConflict,
			"This contact was changed in Google while you were editing it. The Google version is now shown.")
	case errors.Is(err, googlepeople.ErrUnauthorized):
		writeAPIError(w, http.StatusConflict, "This Google account needs to be authorized again.")
	case errors.Is(err, googlepeople.ErrNotFound):
		writeAPIError(w, http.StatusNotFound, "Google no longer has this contact.")
	case errors.Is(err, googlepeople.ErrUpstream), errors.Is(err, googlepeople.ErrSyncTokenExpired):
		writeAPIError(w, http.StatusBadGateway, "Google could not be reached.")
	case errors.Is(err, context.DeadlineExceeded):
		writeAPIError(w, http.StatusGatewayTimeout, "The Google request took too long.")
	default:
		s.serverError(w, r, err)
	}
}
