// File overview: Authenticated routes for connecting, listing, testing, and
// disconnecting Google accounts. Responses describe a connection's state; they
// never carry a token, an authorization code, or a refresh secret.

package web

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"rolltop/backend/googleauth"
	"rolltop/backend/store"
)

// googleSettingsPath is where the OAuth callback returns the browser to.
const googleSettingsPath = "/settings/account/google"

type apiGoogleConnection struct {
	ID            int64    `json:"id"`
	Email         string   `json:"email"`
	Scopes        []string `json:"scopes"`
	Status        string   `json:"status"`
	StatusDetail  string   `json:"status_detail"`
	NeedsReauth   bool     `json:"needs_reauth"`
	HasMailScope  bool     `json:"has_mail_scope"`
	ConnectedAt   string   `json:"connected_at"`
	LastUpdatedAt string   `json:"last_updated_at"`
}

func apiGoogleConnectionFromStore(connection store.GoogleConnection) apiGoogleConnection {
	return apiGoogleConnection{
		ID:            connection.ID,
		Email:         connection.GoogleEmail,
		Scopes:        connection.GrantedScopes,
		Status:        connection.Status,
		StatusDetail:  connection.StatusDetail,
		NeedsReauth:   connection.NeedsReauth(),
		HasMailScope:  connection.HasScope(googleauth.ScopeMail),
		ConnectedAt:   timeString(connection.CreatedAt),
		LastUpdatedAt: timeString(connection.UpdatedAt),
	}
}

// apiGoogleConnections lists the signed-in user's connected Google accounts and
// reports whether the install has Google credentials at all, so settings can
// explain an unconfigured server instead of offering a button that fails.
func (s *Server) apiGoogleConnections(w http.ResponseWriter, r *http.Request) {
	cu, ok := s.requireAPIAuth(w, r)
	if !ok {
		return
	}
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	if s.googleAuth == nil {
		writeJSON(w, map[string]any{"configured": false, "connections": []apiGoogleConnection{}})
		return
	}
	connections, err := s.googleAuth.List(r.Context(), cu.User.ID)
	if err != nil {
		s.serverError(w, err)
		return
	}
	out := make([]apiGoogleConnection, 0, len(connections))
	for _, connection := range connections {
		out = append(out, apiGoogleConnectionFromStore(connection))
	}
	writeJSON(w, map[string]any{
		"configured":  s.googleAuth.Configured(),
		"connections": out,
	})
}

// apiGoogleConnect redirects the browser into Google's consent screen. It is a
// GET because it is a top-level navigation; the flow's integrity rests on the
// single-use state value bound to this user, not on a form token.
func (s *Server) apiGoogleConnect(w http.ResponseWriter, r *http.Request) {
	cu, ok := s.requireAPIAuth(w, r)
	if !ok {
		return
	}
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	if s.googleAuth == nil || !s.googleAuth.Configured() {
		writeAPIError(w, http.StatusServiceUnavailable, "Google is not configured on this server.")
		return
	}
	loginHint := ""
	// Re-authorizing an existing connection should return to the same account.
	if raw := strings.TrimSpace(r.URL.Query().Get("connection_id")); raw != "" {
		connectionID, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			writeAPIError(w, http.StatusBadRequest, "Invalid connection id.")
			return
		}
		connection, err := s.googleAuth.Get(r.Context(), cu.User.ID, connectionID)
		if store.IsNotFound(err) {
			http.NotFound(w, r)
			return
		}
		if err != nil {
			s.serverError(w, err)
			return
		}
		loginHint = connection.GoogleEmail
	}
	authURL, err := s.googleAuth.StartConnect(cu.User.ID, s.googleAuth.Config().RedirectURL(r), loginHint)
	if err != nil {
		s.writeGoogleError(w, err)
		return
	}
	http.Redirect(w, r, authURL, http.StatusFound)
}

// apiGoogleCallback finishes consent and sends the browser back to settings.
// Failures travel as a short reason code in the URL so the settings page can
// render a message without the error text ever passing through a template.
func (s *Server) apiGoogleCallback(w http.ResponseWriter, r *http.Request) {
	cu, ok := s.requireAPIAuth(w, r)
	if !ok {
		return
	}
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	if s.googleAuth == nil || !s.googleAuth.Configured() {
		writeAPIError(w, http.StatusServiceUnavailable, "Google is not configured on this server.")
		return
	}
	query := r.URL.Query()
	if consentErr := strings.TrimSpace(query.Get("error")); consentErr != "" {
		// access_denied is the normal "user pressed cancel" path.
		s.redirectToGoogleSettings(w, r, "error", consentErr)
		return
	}
	code := strings.TrimSpace(query.Get("code"))
	state := strings.TrimSpace(query.Get("state"))
	if code == "" || state == "" {
		s.redirectToGoogleSettings(w, r, "error", "invalid_response")
		return
	}
	connection, err := s.googleAuth.CompleteConnect(r.Context(), cu.User.ID, state, code)
	if errors.Is(err, googleauth.ErrUnknownFlow) {
		s.redirectToGoogleSettings(w, r, "error", "expired")
		return
	}
	if err != nil {
		s.redirectToGoogleSettings(w, r, "error", "exchange_failed")
		return
	}
	s.redirectToGoogleSettings(w, r, "connected", connection.GoogleEmail)
}

func (s *Server) redirectToGoogleSettings(w http.ResponseWriter, r *http.Request, key, value string) {
	target := googleSettingsPath + "?" + url.Values{key: []string{value}}.Encode()
	http.Redirect(w, r, target, http.StatusFound)
}

// apiGoogleConnectionByID handles the per-connection operations: DELETE to
// disconnect, POST .../test to prove the stored refresh token still works.
func (s *Server) apiGoogleConnectionByID(w http.ResponseWriter, r *http.Request) {
	cu, ok := s.requireAPIAuth(w, r)
	if !ok {
		return
	}
	if s.googleAuth == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "Google is not configured on this server.")
		return
	}
	rest := strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, "/api/"), "google/connections/")
	idPart, action, _ := strings.Cut(rest, "/")
	connectionID, err := strconv.ParseInt(strings.TrimSpace(idPart), 10, 64)
	if err != nil || connectionID <= 0 {
		writeAPIError(w, http.StatusBadRequest, "Invalid connection id.")
		return
	}
	switch {
	case action == "test" && r.Method == http.MethodPost:
		s.googleConnectionTest(w, r, cu.User.ID, connectionID)
	case action == "" && r.Method == http.MethodDelete:
		s.googleConnectionDisconnect(w, r, cu.User.ID, connectionID)
	default:
		methodNotAllowed(w)
	}
}

// googleConnectionTest exercises the full token path: it refreshes when needed
// and asks Google who the token belongs to. Phase 1 has no other consumer, so
// this is how a user verifies a connection actually works.
func (s *Server) googleConnectionTest(w http.ResponseWriter, r *http.Request, userID, connectionID int64) {
	if !s.verifyCSRF(w, r) {
		return
	}
	token, err := s.googleAuth.AccessToken(r.Context(), userID, connectionID)
	if store.IsNotFound(err) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.writeGoogleError(w, err)
		return
	}
	info, err := s.googleAuth.Client().Userinfo(r.Context(), token)
	if err != nil {
		s.writeGoogleError(w, err)
		return
	}
	connection, err := s.googleAuth.Get(r.Context(), userID, connectionID)
	if err != nil {
		s.serverError(w, err)
		return
	}
	writeJSON(w, map[string]any{
		"ok":         true,
		"email":      info.Email,
		"connection": apiGoogleConnectionFromStore(connection),
	})
}

// googleConnectionDisconnect revokes the grant at Google and removes the local
// connection. A failed revoke still leaves the account detached and is reported
// as a warning; a failed delete means nothing was disconnected at all and must
// not be dressed up as success.
func (s *Server) googleConnectionDisconnect(w http.ResponseWriter, r *http.Request, userID, connectionID int64) {
	if !s.verifyCSRF(w, r) {
		return
	}
	revokeErr, err := s.googleAuth.Disconnect(r.Context(), userID, connectionID)
	if store.IsNotFound(err) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.serverError(w, err)
		return
	}
	if revokeErr != nil {
		writeJSON(w, map[string]any{
			"disconnected": true,
			"warning":      "The account was removed from Rolltop, but Google could not be reached to revoke access. Revoke it manually in your Google account settings.",
		})
		return
	}
	writeJSON(w, map[string]any{"disconnected": true})
}

// writeGoogleError maps the manager's failures onto status codes without
// forwarding upstream error text, which can quote request parameters.
func (s *Server) writeGoogleError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, googleauth.ErrNotConfigured):
		writeAPIError(w, http.StatusServiceUnavailable, "Google is not configured on this server.")
	case errors.Is(err, googleauth.ErrNoRedirectURI):
		writeAPIError(w, http.StatusServiceUnavailable,
			"No configured Google redirect URI matches this server's address. Add it to ROLLTOP_GOOGLE_REDIRECT_URLS and to the OAuth client in the Google Cloud console.")
	case errors.Is(err, googleauth.ErrReauthRequired):
		writeAPIError(w, http.StatusConflict, "This Google account needs to be authorized again.")
	case errors.Is(err, googleauth.ErrUnknownFlow):
		writeAPIError(w, http.StatusBadRequest, "This Google authorization request has expired. Start again.")
	default:
		writeAPIError(w, http.StatusBadGateway, "Google could not be reached.")
	}
}
