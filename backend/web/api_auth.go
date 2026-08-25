// File overview: Login, logout, setup, CSRF, and session API handlers.

package web

import (
	"errors"
	"log"
	"net/http"
	"strings"
	"time"

	"rolltop/backend/auth"
	"rolltop/backend/buildinfo"
	mmcrypto "rolltop/backend/crypto"
	"rolltop/backend/store"
)

// errSessionUnavailable marks a bootstrap that could not resolve the caller's
// session because of a store failure; serving it with user:null would make the
// client drop a valid login.
var errSessionUnavailable = errors.New("session lookup temporarily unavailable")

func (s *Server) apiBootstrap(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	resp, err := s.bootstrapPayload(w, r)
	if errors.Is(err, errSessionUnavailable) {
		sessionUnavailable(w)
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "private, no-store")
	writeJSON(w, resp)
}

func (s *Server) bootstrapPayload(w http.ResponseWriter, r *http.Request) (map[string]any, error) {
	cu, authenticated := current(r)
	if !authenticated && sessionLookupFailed(r) {
		return nil, errSessionUnavailable
	}
	// A resolved session already proves users exist; query the store only for
	// anonymous requests so a CountUsers hiccup cannot fail a signed-in load.
	usersExist := authenticated
	if !authenticated {
		var err error
		usersExist, err = s.usersExist(r.Context())
		if err != nil {
			return nil, err
		}
	}
	info := buildinfo.Current()
	resp := map[string]any{
		"users_exist":           usersExist,
		"csrf":                  s.csrfToken(w, r),
		"server_started_at":     timeString(s.startedAt),
		"server_uptime_seconds": int(time.Since(s.startedAt).Seconds()),
		"build_version":         info.Version,
		"build_date":            info.BuildDate,
		"build_label":           info.Label,
		"build_commit":          info.Commit,
		"public_site_url":       info.PublicSiteURL,
		"available_themes":      s.availableThemes(r.Context()),
		"frontend_plugins":      s.frontendPlugins(r.Context()),
		"auth_providers":        s.authProviders(r.Context()),
		"database_unavailable":  false,
	}
	if authenticated {
		resp["user"] = safeUser(cu.User)
		err := s.addTenantBootstrap(r, cu.User.ID, resp)
		switch {
		case err == nil:
		case store.IsUnavailable(err):
			// A database that is down or failing over must not cost the browser
			// its session. Answering 500 here took /login and /api/bootstrap
			// down together, which locked the operator out of the admin
			// database page that says what is wrong.
			applyUnavailableTenantBootstrap(cu.User.ID, resp)
		default:
			return nil, err
		}
	} else {
		resp["user"] = nil
		resp["mailboxes"] = []apiMailbox{}
		resp["active_sync_runs"] = []apiSyncRun{}
		resp["account_needs_password"] = false
		resp["account_notice"] = ""
		resp["enabled_plugins"] = []string{}
	}
	return resp, nil
}

// addTenantBootstrap fills the parts of the payload that live in the signed-in
// user's own database. Its error is the caller's signal that the tenant cannot
// be read right now.
func (s *Server) addTenantBootstrap(r *http.Request, userID int64, resp map[string]any) error {
	swipePreferences, err := s.store.GetSwipePreferences(r.Context(), userID)
	if err != nil {
		return err
	}
	resp["swipe_preferences"] = apiSwipePreferencesFromStore(swipePreferences)
	// The mail views need the folder the Archive action actually files into,
	// which an identity may override; swipe_preferences stays the raw stored
	// mapping so the settings form keeps editing what it saved.
	archiveMailboxes, err := s.store.ArchiveMailboxesForUser(r.Context(), userID)
	if err != nil {
		return err
	}
	resp["effective_archive_mailboxes"] = apiAccountMailboxChoices(archiveMailboxes)
	categories, categoriesPending, err := s.mailCategoryChrome(r.Context(), userID)
	if err != nil {
		return err
	}
	resp["mail_categories"] = categories
	resp["mail_categories_pending"] = categoriesPending
	var chrome viewData
	s.loadMailboxChrome(r.Context(), userID, &chrome)
	resp["mailboxes"] = apiMailboxes(chrome.Mailboxes)
	resp["latest_sync_run"] = apiSyncRunPtr(chrome.LatestSyncRun)
	resp["active_sync_runs"] = apiSyncRuns(chrome.ActiveSyncRuns)
	resp["unfinished_move_run"] = apiSyncRunPtr(chrome.UnfinishedMoveRun)
	resp["sync_running"] = chrome.SyncRunning
	resp["mail_generation"] = s.mailListGeneration(userID)
	needsPassword, notice := s.accountCredentialNotice(r.Context(), userID)
	resp["account_needs_password"] = needsPassword
	resp["account_notice"] = notice
	if settings, err := s.store.ListPluginSettings(r.Context()); err == nil {
		enabled := make([]string, 0, len(settings))
		for _, setting := range settings {
			if setting.Enabled {
				enabled = append(enabled, setting.ID)
			}
		}
		resp["enabled_plugins"] = enabled
	}
	return nil
}

// applyUnavailableTenantBootstrap serves the app shell to a signed-in user
// whose mail database is unreadable: an empty mailbox list rather than an
// error, plus the flag the frontend uses to explain the emptiness instead of
// presenting it as an account with no mail.
func applyUnavailableTenantBootstrap(userID int64, resp map[string]any) {
	resp["database_unavailable"] = true
	resp["swipe_preferences"] = apiSwipePreferencesFromStore(store.DefaultSwipePreferences(userID))
	resp["effective_archive_mailboxes"] = []apiAccountMailboxChoice{}
	resp["mail_categories"] = emptyMailCategories()
	resp["mail_categories_pending"] = 0
	resp["mailboxes"] = []apiMailbox{}
	resp["latest_sync_run"] = nil
	resp["active_sync_runs"] = []apiSyncRun{}
	resp["unfinished_move_run"] = nil
	resp["sync_running"] = false
	resp["mail_generation"] = 0
	resp["account_needs_password"] = false
	resp["account_notice"] = ""
	resp["enabled_plugins"] = []string{}
}

func (s *Server) apiSwipePreferences(w http.ResponseWriter, r *http.Request) {
	cu, ok := s.requireAPIAuth(w, r)
	if !ok {
		return
	}
	switch r.Method {
	case http.MethodGet:
		preferences, err := s.store.GetSwipePreferences(r.Context(), cu.User.ID)
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		writeJSON(w, map[string]any{"swipe_preferences": apiSwipePreferencesFromStore(preferences)})
	case http.MethodPost:
		if !s.verifyCSRF(w, r) {
			return
		}
		var in apiSwipePreferences
		if !decodeJSON(w, r, &in) {
			return
		}
		preferences := store.SwipePreferences{
			UserID:            cu.User.ID,
			LeftAction:        in.LeftAction,
			LeftSnoozePreset:  in.LeftSnoozePreset,
			RightAction:       in.RightAction,
			RightSnoozePreset: in.RightSnoozePreset,
			ArchiveMailboxes:  make([]store.SwipeArchiveMailbox, 0, len(in.ArchiveMailboxes)),
		}
		for _, mailbox := range in.ArchiveMailboxes {
			preferences.ArchiveMailboxes = append(preferences.ArchiveMailboxes, store.SwipeArchiveMailbox{AccountID: mailbox.AccountID, MailboxID: mailbox.MailboxID})
		}
		saved, err := s.store.SaveSwipePreferences(r.Context(), preferences)
		if errors.Is(err, store.ErrInvalidSwipePreferences) {
			writeAPIError(w, http.StatusBadRequest, err.Error())
			return
		}
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		// The Archive mapping decides what the Inbox list leaves out, so a
		// changed mapping makes every cached page of that list wrong.
		s.noteMailListChanged(cu.User.ID)
		if s.events != nil {
			s.events.Notify(cu.User.ID)
		}
		writeJSON(w, map[string]any{"swipe_preferences": apiSwipePreferencesFromStore(saved)})
	default:
		methodNotAllowed(w)
	}
}

func (s *Server) apiSetup(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	usersExist, err := s.usersExist(r.Context())
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if usersExist {
		writeAPIError(w, http.StatusConflict, "setup is already complete")
		return
	}
	if !s.verifyCSRF(w, r) {
		return
	}
	var in struct {
		Email    string `json:"email"`
		Name     string `json:"name"`
		Password string `json:"password"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	if len(in.Password) < 12 {
		writeAPIError(w, http.StatusBadRequest, "Password must be at least 12 characters.")
		return
	}
	hash, err := auth.HashPassword(in.Password)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	user, err := s.store.CreateUser(r.Context(), in.Email, in.Name, hash, true)
	if err != nil {
		log.Printf("setup create admin user: %v", err)
		writeAPIError(w, http.StatusBadRequest, "Could not create admin user.")
		return
	}
	if _, err := s.store.EnsureMeContactForEmail(r.Context(), user.ID, user.Email, firstNonEmpty(user.Name, user.Email)); err != nil && !store.IsNotFound(err) {
		s.serverError(w, r, err)
		return
	}
	// The sign-up address is the one identity a fresh install starts with, so
	// compose has a From address before any mailbox is configured.
	if err := s.store.EnsureMailIdentityForEmail(r.Context(), user.ID, user.Email); err != nil && !store.IsNotFound(err) {
		s.serverError(w, r, err)
		return
	}
	if err := s.loginUser(w, r, user.ID); err != nil {
		s.serverError(w, r, err)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) apiLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	usersExist, err := s.usersExist(r.Context())
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if !usersExist {
		writeAPIError(w, http.StatusPreconditionRequired, "setup is required")
		return
	}
	if !s.verifyCSRF(w, r) {
		return
	}
	var in struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	gateKey := loginGateKey(r, in.Email)
	if allowed, retryAfter := s.loginGate.allow(gateKey, time.Now()); !allowed {
		writeRetryAfter(w, retryAfter, "Too many sign-in attempts. Try again later.")
		return
	}
	user, err := s.store.GetUserByEmail(r.Context(), in.Email)
	if err != nil && !store.IsNotFound(err) {
		// A store failure is not a credential verdict; reporting it as
		// "invalid password" sends users into password resets during outages.
		s.serverError(w, r, err)
		return
	}
	if err != nil {
		// Spend a password verification's worth of work on a missing account so
		// its response time matches an existing one -- otherwise the gap between
		// the two enumerates which addresses are registered.
		auth.VerifyDummyPassword(in.Password)
		s.loginGate.recordFailure(gateKey, time.Now())
		writeAPIError(w, http.StatusUnauthorized, "Invalid email or password.")
		return
	}
	ok, err := auth.VerifyPassword(user.PasswordHash, in.Password)
	if err != nil || !ok {
		s.loginGate.recordFailure(gateKey, time.Now())
		writeAPIError(w, http.StatusUnauthorized, "Invalid email or password.")
		return
	}
	s.loginGate.recordSuccess(gateKey)
	if err := s.loginUser(w, r, user.ID); err != nil {
		s.serverError(w, r, err)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// loginGateKey scopes the login rate limit to the client and the address being
// tried. Keying on the email too keeps per-account brute force throttled even
// when a reverse proxy makes every request share one IP, and keeps one victim's
// failures from throttling logins to unrelated accounts from the same network.
func loginGateKey(r *http.Request, email string) string {
	return clientIP(r) + "\x00" + strings.ToLower(strings.TrimSpace(email))
}

func (s *Server) apiLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	if !s.verifyCSRF(w, r) {
		return
	}
	if cookie, err := r.Cookie(sessionCookie); err == nil {
		_ = s.store.DeleteSession(r.Context(), mmcrypto.TokenHash(cookie.Value))
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: s.cookieSecureFor(r)})
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) apiProfile(w http.ResponseWriter, r *http.Request) {
	cu, ok := s.requireAPIAuth(w, r)
	if !ok {
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, map[string]any{"user": safeUser(cu.User)})
	case http.MethodPost:
		if !s.verifyCSRF(w, r) {
			return
		}
		in := struct {
			BackupEmail            string `json:"backup_email"`
			DateLocale             string `json:"date_locale"`
			DateFormat             string `json:"date_format"`
			Theme                  string `json:"theme"`
			SearchPreset           string `json:"search_preset"`
			SearchRecencyBias      string `json:"search_recency_bias"`
			SearchFuzzy            string `json:"search_fuzzy"`
			SearchSenderBoost      bool   `json:"search_sender_boost"`
			SearchSenderHistory    string `json:"search_sender_history"`
			SearchContactBoost     string `json:"search_contact_boost"`
			SearchAttachmentWeight string `json:"search_attachment_weight"`
			SearchCompactSplitting bool   `json:"search_compact_splitting"`
		}{
			BackupEmail:            cu.User.BackupEmail,
			DateLocale:             cu.User.DateLocale,
			DateFormat:             cu.User.DateFormat,
			Theme:                  cu.User.Theme,
			SearchPreset:           cu.User.SearchPreset,
			SearchRecencyBias:      cu.User.SearchRecencyBias,
			SearchFuzzy:            cu.User.SearchFuzzy,
			SearchSenderBoost:      cu.User.SearchSenderBoost,
			SearchSenderHistory:    cu.User.SearchSenderHistory,
			SearchContactBoost:     cu.User.SearchContactBoost,
			SearchAttachmentWeight: cu.User.SearchAttachmentWeight,
			SearchCompactSplitting: cu.User.SearchCompactSplitting,
		}
		if !decodeJSON(w, r, &in) {
			return
		}
		user, err := s.store.UpdateUserPreferences(r.Context(), cu.User.ID, in.DateLocale, in.DateFormat, in.Theme, in.SearchPreset, in.SearchRecencyBias, in.SearchFuzzy, in.SearchSenderHistory, in.SearchContactBoost, in.SearchAttachmentWeight, in.SearchSenderBoost, in.SearchCompactSplitting)
		if err == nil && in.BackupEmail != cu.User.BackupEmail {
			user, err = s.store.UpdateUserBackupEmail(r.Context(), cu.User.ID, in.BackupEmail)
		}
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		s.notifyUserChanged(cu.User.ID)
		writeJSON(w, map[string]any{"user": safeUser(user)})
	default:
		methodNotAllowed(w)
	}
}
