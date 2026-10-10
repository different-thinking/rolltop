// File overview: Authenticated routes for connecting, listing, testing, syncing
// and disconnecting Microsoft 365 accounts. Responses describe a connection's
// state; they never carry a token, an authorization code or a refresh secret.

package web

import (
	"context"
	"errors"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"rolltop/backend/microsoftauth"
	"rolltop/backend/store"
)

// microsoftSettingsPath is where the sign-in callback returns the browser to.
const microsoftSettingsPath = "/settings/account/microsoft"

type apiMicrosoftConnection struct {
	ID           int64    `json:"id"`
	Email        string   `json:"email"`
	DisplayName  string   `json:"display_name"`
	Scopes       []string `json:"scopes"`
	Status       string   `json:"status"`
	StatusDetail string   `json:"status_detail"`
	NeedsReauth  bool     `json:"needs_reauth"`
	// HasCalendarScope is false for a grant whose consent left calendars out
	// -- an administrator can approve an app for less than it asks.
	HasCalendarScope bool                      `json:"has_calendar_scope"`
	CalendarSync     *apiMicrosoftCalendarSync `json:"calendar_sync"`
	ConnectedAt      string                    `json:"connected_at"`
	LastUpdatedAt    string                    `json:"last_updated_at"`
}

// apiMicrosoftCalendarSync is the per-connection calendar sync state shown in
// settings.
type apiMicrosoftCalendarSync struct {
	Status        string `json:"status"`
	StatusDetail  string `json:"status_detail"`
	LastSyncAt    string `json:"last_sync_at"`
	LastSuccess   string `json:"last_success_at"`
	CalendarCount int    `json:"calendar_count"`
	EverSynced    bool   `json:"ever_synced"`
}

func apiMicrosoftConnectionFromStore(connection store.MicrosoftConnection) apiMicrosoftConnection {
	scopes := connection.GrantedScopes
	if scopes == nil {
		scopes = []string{}
	}
	return apiMicrosoftConnection{
		ID:               connection.ID,
		Email:            connection.Email,
		DisplayName:      connection.DisplayName,
		Scopes:           scopes,
		Status:           connection.Status,
		StatusDetail:     connection.StatusDetail,
		NeedsReauth:      connection.NeedsReauth(),
		HasCalendarScope: connection.HasScope(microsoftauth.ScopeCalendars),
		ConnectedAt:      timeString(connection.CreatedAt),
		LastUpdatedAt:    timeString(connection.UpdatedAt),
	}
}

// apiMicrosoftPath routes everything under /api/microsoft/.
func (s *Server) apiMicrosoftPath(w http.ResponseWriter, r *http.Request, rest string) {
	switch {
	case rest == "connect":
		s.apiMicrosoftConnect(w, r)
	case rest == "callback":
		s.apiMicrosoftCallback(w, r)
	case rest == "connections":
		s.apiMicrosoftConnections(w, r)
	case strings.HasPrefix(rest, "connections/"):
		s.apiMicrosoftConnectionByID(w, r, strings.TrimPrefix(rest, "connections/"))
	default:
		http.NotFound(w, r)
	}
}

// apiMicrosoftConnections lists the signed-in user's Microsoft accounts and
// reports whether the server has a Microsoft app registration at all.
func (s *Server) apiMicrosoftConnections(w http.ResponseWriter, r *http.Request) {
	cu, ok := s.requireAPIAuth(w, r)
	if !ok {
		return
	}
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	if s.microsoftAuth == nil {
		writeJSON(w, map[string]any{"configured": false, "connections": []apiMicrosoftConnection{}})
		return
	}
	connections, err := s.microsoftAuth.List(r.Context(), cu.User.ID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	out := make([]apiMicrosoftConnection, 0, len(connections))
	for _, connection := range connections {
		item := apiMicrosoftConnectionFromStore(connection)
		if item.HasCalendarScope {
			item.CalendarSync = s.microsoftCalendarSyncState(r.Context(), cu.User.ID, connection.ID)
		}
		out = append(out, item)
	}
	writeJSON(w, map[string]any{
		"configured":  s.microsoftAuth.Configured(),
		"connections": out,
	})
}

// apiMicrosoftConnect starts a sign-in and returns the URL for the browser to
// navigate to. It is a CSRF-checked POST for the reason apiGoogleConnect is:
// starting a flow takes one of the user's pending-flow slots.
func (s *Server) apiMicrosoftConnect(w http.ResponseWriter, r *http.Request) {
	cu, ok := s.requireAPIAuth(w, r)
	if !ok {
		return
	}
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	if !s.verifyCSRF(w, r) {
		return
	}
	if s.microsoftAuth == nil || !s.microsoftAuth.Configured() {
		writeAPIError(w, http.StatusServiceUnavailable, "Microsoft 365 is not configured on this server.")
		return
	}
	var in struct {
		ConnectionID int64 `json:"connection_id"`
	}
	if r.Body != nil && r.ContentLength != 0 {
		if !decodeJSON(w, r, &in) {
			return
		}
	}
	loginHint := ""
	if in.ConnectionID != 0 {
		if in.ConnectionID < 0 {
			writeAPIError(w, http.StatusBadRequest, "Invalid connection id.")
			return
		}
		connection, err := s.microsoftAuth.Get(r.Context(), cu.User.ID, in.ConnectionID)
		if store.IsNotFound(err) {
			http.NotFound(w, r)
			return
		}
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		loginHint = connection.Email
	}
	authURL, err := s.microsoftAuth.StartConnect(cu.User.ID, s.microsoftAuth.Config().RedirectURL(r), loginHint)
	if err != nil {
		s.writeMicrosoftError(w, r, err)
		return
	}
	writeJSON(w, map[string]any{"authorization_url": authURL})
}

// apiMicrosoftCallback finishes a sign-in and sends the browser back to
// settings. Every exit is a page the reader can read; failures travel as a
// short reason code, never as Microsoft's own text.
func (s *Server) apiMicrosoftCallback(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	cu, ok := current(r)
	if !ok {
		if sessionLookupFailed(r) {
			s.redirectToMicrosoftSettings(w, r, "error", "unavailable")
			return
		}
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}
	if s.microsoftAuth == nil || !s.microsoftAuth.Configured() {
		s.redirectToMicrosoftSettings(w, r, "error", "unavailable")
		return
	}
	query := r.URL.Query()
	if consentErr := strings.TrimSpace(query.Get("error")); consentErr != "" {
		s.microsoftAuth.AbandonFlow(cu.User.ID, strings.TrimSpace(query.Get("state")))
		s.redirectToMicrosoftSettings(w, r, "error", consentErr)
		return
	}
	code := strings.TrimSpace(query.Get("code"))
	state := strings.TrimSpace(query.Get("state"))
	if code == "" || state == "" {
		s.redirectToMicrosoftSettings(w, r, "error", "invalid_response")
		return
	}
	connection, err := s.microsoftAuth.CompleteConnect(r.Context(), cu.User.ID, state, code)
	if errors.Is(err, microsoftauth.ErrUnknownFlow) {
		s.redirectToMicrosoftSettings(w, r, "error", "expired")
		return
	}
	if err != nil {
		log.Printf("microsoft connect user_id=%d: %v", cu.User.ID, err)
		s.redirectToMicrosoftSettings(w, r, "error", "exchange_failed")
		return
	}
	// The calendars should be there when the reader opens the week, not one
	// poll later. The sync bounds itself and runs past the redirect.
	if s.microsoftCalendar != nil && connection.HasScope(microsoftauth.ScopeCalendars) {
		userID, connectionID := cu.User.ID, connection.ID
		go func() {
			if _, err := s.microsoftCalendar.SyncConnection(context.WithoutCancel(r.Context()), userID, connectionID); err != nil {
				log.Printf("microsoft calendar first sync user_id=%d connection_id=%d: %v", userID, connectionID, err)
			}
		}()
	}
	s.redirectToMicrosoftSettings(w, r, "connected", connection.Email)
}

func (s *Server) redirectToMicrosoftSettings(w http.ResponseWriter, r *http.Request, key, value string) {
	http.Redirect(w, r, microsoftSettingsPath+"?"+url.Values{key: []string{value}}.Encode(), http.StatusFound)
}

// apiMicrosoftConnectionByID handles the per-connection operations.
func (s *Server) apiMicrosoftConnectionByID(w http.ResponseWriter, r *http.Request, rest string) {
	cu, ok := s.requireAPIAuth(w, r)
	if !ok {
		return
	}
	if s.microsoftAuth == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "Microsoft 365 is not configured on this server.")
		return
	}
	idPart, action, _ := strings.Cut(rest, "/")
	connectionID, err := strconv.ParseInt(strings.TrimSpace(idPart), 10, 64)
	if err != nil || connectionID <= 0 {
		writeAPIError(w, http.StatusBadRequest, "Invalid connection id.")
		return
	}
	switch {
	case action == "test" && r.Method == http.MethodPost:
		s.microsoftConnectionTest(w, r, cu.User.ID, connectionID)
	case action == "calendar/sync" && r.Method == http.MethodPost:
		s.microsoftCalendarSyncNow(w, r, cu.User.ID, connectionID)
	case action == "" && r.Method == http.MethodDelete:
		s.microsoftConnectionDisconnect(w, r, cu.User.ID, connectionID)
	default:
		methodNotAllowed(w)
	}
}

// microsoftConnectionTest exercises the whole token path and asks Graph who
// the token belongs to.
func (s *Server) microsoftConnectionTest(w http.ResponseWriter, r *http.Request, userID, connectionID int64) {
	if !s.verifyCSRF(w, r) {
		return
	}
	token, _, err := s.microsoftAuth.AccessTokenAndConnection(r.Context(), userID, connectionID)
	if store.IsNotFound(err) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.writeMicrosoftError(w, r, err)
		return
	}
	me, err := s.microsoftAuth.Client().Me(r.Context(), token)
	if errors.Is(err, microsoftauth.ErrUnauthorized) {
		// Graph no longer honours a token this side still believes in, which
		// is what revoked consent looks like. A refresh turns that into the
		// real answer: a working token, or a connection flagged for sign-in.
		token, err = s.microsoftAuth.ForceRefresh(r.Context(), userID, connectionID)
		if err == nil {
			me, err = s.microsoftAuth.Client().Me(r.Context(), token)
		}
	}
	if err != nil {
		s.writeMicrosoftError(w, r, err)
		return
	}
	connection, err := s.microsoftAuth.Get(r.Context(), userID, connectionID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	writeJSON(w, map[string]any{
		"ok":         true,
		"email":      me.Email(),
		"connection": apiMicrosoftConnectionFromStore(connection),
	})
}

// microsoftConnectionDisconnect removes the connection and its calendars. The
// identity platform has no per-app revocation for one user, so the answer says
// where the reader removes the grant itself.
func (s *Server) microsoftConnectionDisconnect(w http.ResponseWriter, r *http.Request, userID, connectionID int64) {
	if !s.verifyCSRF(w, r) {
		return
	}
	if err := s.microsoftAuth.Disconnect(r.Context(), userID, connectionID); err != nil {
		if store.IsNotFound(err) {
			http.NotFound(w, r)
			return
		}
		s.serverError(w, r, err)
		return
	}
	writeJSON(w, map[string]any{
		"disconnected": true,
		"notice":       "The account was removed from Rolltop. Microsoft keeps listing Rolltop among the apps with access until you remove it under My Apps (work or school) or account.live.com/consent/Manage (personal).",
	})
}

// microsoftCalendarSyncState assembles the calendar sync state of one
// connection. A store failure costs the state, not the connection list.
func (s *Server) microsoftCalendarSyncState(ctx context.Context, userID, connectionID int64) *apiMicrosoftCalendarSync {
	if s.store == nil {
		return nil
	}
	state, err := s.store.GetMicrosoftCalendarSync(ctx, userID, connectionID)
	if err != nil {
		log.Printf("microsoft calendar sync state user_id=%d connection_id=%d: %v", userID, connectionID, err)
		return nil
	}
	count, err := s.store.CountCalendarsForMicrosoftConnection(ctx, userID, connectionID)
	if err != nil {
		log.Printf("microsoft calendar count user_id=%d connection_id=%d: %v", userID, connectionID, err)
	}
	return &apiMicrosoftCalendarSync{
		Status:        state.Status,
		StatusDetail:  state.StatusDetail,
		LastSyncAt:    timeString(state.LastSyncAt),
		LastSuccess:   timeString(state.LastSuccessAt),
		CalendarCount: count,
		EverSynced:    !state.LastSuccessAt.IsZero(),
	}
}

// microsoftCalendarSyncNow runs a sync for one connection inline: the reader
// pressed a button and is waiting for the answer.
func (s *Server) microsoftCalendarSyncNow(w http.ResponseWriter, r *http.Request, userID, connectionID int64) {
	if !s.verifyCSRF(w, r) {
		return
	}
	if s.microsoftCalendar == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "Calendar sync is not available on this server.")
		return
	}
	result, err := s.microsoftCalendar.SyncConnection(r.Context(), userID, connectionID)
	if store.IsNotFound(err) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.writeCalendarError(w, r, err)
		return
	}
	writeJSON(w, map[string]any{
		"ok":         true,
		"calendars":  result.Calendars,
		"created":    result.Created,
		"updated":    result.Updated,
		"deleted":    result.Deleted,
		"full_sync":  result.FullSync,
		"sync_state": s.microsoftCalendarSyncState(r.Context(), userID, connectionID),
	})
}

// writeMicrosoftError maps the auth manager's failures onto status codes
// without forwarding Microsoft's text.
func (s *Server) writeMicrosoftError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, microsoftauth.ErrNotConfigured):
		writeAPIError(w, http.StatusServiceUnavailable, "Microsoft 365 is not configured on this server.")
	case errors.Is(err, microsoftauth.ErrNoRedirectURI):
		writeAPIError(w, http.StatusServiceUnavailable,
			"No configured Microsoft redirect URI matches this server's address. Add it to ROLLTOP_MICROSOFT_REDIRECT_URLS and to the app registration in Microsoft Entra.")
	case errors.Is(err, microsoftauth.ErrReauthRequired):
		writeAPIError(w, http.StatusConflict, "This Microsoft account needs to be signed in again.")
	case errors.Is(err, microsoftauth.ErrUnknownFlow):
		writeAPIError(w, http.StatusBadRequest, "This Microsoft sign-in has expired. Start again.")
	case errors.Is(err, microsoftauth.ErrUnauthorized), errors.Is(err, microsoftauth.ErrUpstream):
		writeAPIError(w, http.StatusBadGateway, "Microsoft could not be reached.")
	default:
		s.serverError(w, r, err)
	}
}
