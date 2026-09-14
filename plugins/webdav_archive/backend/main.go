// File overview: Lifecycle and HTTP surface for the WebDAV archive plugin.
//
// The plugin does three things and this file wires all three to the host: it
// hooks mail sync so attachments worth keeping are noticed, it runs the worker
// that carries them to the server, and it serves the API the settings page and
// the file browser are built on.

package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	mmcrypto "rolltop/backend/crypto"
	"rolltop/backend/plugins"
	"rolltop/backend/store"
)

const (
	pluginID = "webdav_archive"
	apiPath  = "plugins/webdav_archive"
	// defaultContentTypes is what a new target watches for. Audio is the case
	// this plugin was written for -- voice memos mailed to oneself -- and it is
	// a prefix, so every audio format is covered by the one entry.
	defaultContentTypes = "audio/"
	defaultPathTemplate = "{yyyy}/{mm}/{filename}"
)

type webdavArchiveBackend struct {
	mu     sync.Mutex
	routes []plugins.ProtectedAPIRouteHandle
	worker *worker
}

var (
	_ plugins.BackendPlugin     = (*webdavArchiveBackend)(nil)
	_ plugins.StoredMessageHook = (*webdavArchiveBackend)(nil)
)

// RolltopPlugin is the symbol loaded by plugin.Open.
func RolltopPlugin() plugins.BackendPlugin {
	return &webdavArchiveBackend{}
}

func (*webdavArchiveBackend) ID() string { return pluginID }

func (p *webdavArchiveBackend) Start(host plugins.BackendStartHost) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.stopLocked()
	handle, err := host.RegisterProtectedAPI(p.ID(), plugins.ProtectedAPIRoute{
		Path: apiPath, Prefix: true, Handle: p.handleAPI,
	})
	if err != nil {
		return err
	}
	p.routes = append(p.routes, handle)
	st, ok := host.Store().(*store.Store)
	if !ok || st == nil {
		p.stopLocked()
		return errors.New("the WebDAV archive store is not available")
	}
	p.worker = newWorker(host, st)
	p.worker.Start()
	return nil
}

func (p *webdavArchiveBackend) Stop(plugins.BackendStartHost) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.stopLocked()
	return nil
}

func (p *webdavArchiveBackend) stopLocked() {
	if p.worker != nil {
		p.worker.Stop()
		p.worker = nil
	}
	for _, route := range p.routes {
		route.Unregister()
	}
	p.routes = nil
}

// wake asks the worker for a sweep now. The hook calls it after queuing, so a
// recording that arrives is on its way to the server in the same second rather
// than at the next tick.
func (p *webdavArchiveBackend) wake() {
	p.mu.Lock()
	current := p.worker
	p.mu.Unlock()
	if current != nil {
		current.Wake()
	}
}

type targetInput struct {
	Name            string `json:"name"`
	Enabled         *bool  `json:"enabled"`
	BaseURL         string `json:"base_url"`
	Username        string `json:"username"`
	Password        string `json:"password"`
	ClearPassword   bool   `json:"clear_password"`
	PrivateKey      string `json:"private_key"`
	ClearPrivateKey bool   `json:"clear_private_key"`
	WatchMailboxID  int64  `json:"watch_mailbox_id"`
	ContentTypes    string `json:"content_types"`
	PathTemplate    string `json:"path_template"`
	IncludeInline   *bool  `json:"include_inline"`
}

type enabledInput struct {
	Enabled bool `json:"enabled"`
}

type targetView struct {
	ID            int64  `json:"id"`
	Name          string `json:"name"`
	Enabled       bool   `json:"enabled"`
	BaseURL       string `json:"base_url"`
	Username      string `json:"username"`
	HasPassword   bool   `json:"has_password"`
	HasPrivateKey bool   `json:"has_private_key"`
	// Transport is derived from the address rather than stored beside it, so
	// the two cannot disagree. The settings page reads it to label the target
	// and to show the fields that transport actually needs.
	Transport      string `json:"transport"`
	WatchMailboxID int64  `json:"watch_mailbox_id"`
	ContentTypes   string `json:"content_types"`
	PathTemplate   string `json:"path_template"`
	IncludeInline  bool   `json:"include_inline"`
	LastError      string `json:"last_error"`
	LastSuccessAt  int64  `json:"last_success_at"`
	UploadedTotal  int64  `json:"uploaded_total"`
}

type uploadView struct {
	ID            int64  `json:"id"`
	TargetID      int64  `json:"target_id"`
	MessageID     int64  `json:"message_id"`
	Filename      string `json:"filename"`
	ContentType   string `json:"content_type"`
	Size          int64  `json:"size"`
	RemotePath    string `json:"remote_path"`
	Status        string `json:"status"`
	Attempts      int    `json:"attempts"`
	NextAttemptAt int64  `json:"next_attempt_at"`
	LastError     string `json:"last_error"`
	Subject       string `json:"subject"`
	FromAddr      string `json:"from_addr"`
	MessageDate   int64  `json:"message_date"`
	CreatedAt     int64  `json:"created_at"`
	CompletedAt   int64  `json:"completed_at"`
}

type mailboxOption struct {
	ID    int64  `json:"id"`
	Name  string `json:"name"`
	Role  string `json:"role"`
	Label string `json:"label"`
}

func presentTarget(item target) targetView {
	return targetView{
		ID:             item.ID,
		Name:           item.Name,
		Enabled:        item.Enabled,
		BaseURL:        item.BaseURL,
		Username:       item.Username,
		HasPassword:    strings.TrimSpace(item.EncryptedPassword) != "",
		HasPrivateKey:  strings.TrimSpace(item.EncryptedPrivateKey) != "",
		Transport:      targetTransport(item),
		WatchMailboxID: item.WatchMailboxID,
		ContentTypes:   item.ContentTypes,
		PathTemplate:   item.PathTemplate,
		IncludeInline:  item.IncludeInline,
		LastError:      item.LastError,
		LastSuccessAt:  unixSeconds(item.LastSuccessAt),
		UploadedTotal:  item.UploadedTotal,
	}
}

// targetTransport names the protocol a stored address speaks. An address that
// no longer parses -- one stored by an older build, or edited by hand in the
// database -- is reported as unknown rather than guessed at, so the settings
// page can say so instead of drawing the wrong fields.
func targetTransport(item target) string {
	address, err := parseTargetAddress(item.BaseURL)
	if err != nil {
		return "unknown"
	}
	return address.Transport
}

func presentUpload(item upload) uploadView {
	return uploadView{
		ID:            item.ID,
		TargetID:      item.TargetID,
		MessageID:     item.MessageID,
		Filename:      item.Filename,
		ContentType:   item.ContentType,
		Size:          item.Size,
		RemotePath:    item.RemotePath,
		Status:        item.Status,
		Attempts:      item.Attempts,
		NextAttemptAt: unixSeconds(item.NextAttemptAt),
		LastError:     item.LastError,
		Subject:       item.Subject,
		FromAddr:      item.FromAddr,
		MessageDate:   unixSeconds(item.MessageDate),
		CreatedAt:     unixSeconds(item.CreatedAt),
		CompletedAt:   unixSeconds(item.CompletedAt),
	}
}

func (p *webdavArchiveBackend) handleAPI(host plugins.APIHost, path string, w http.ResponseWriter, r *http.Request) {
	current, ok := host.RequireAPIAuth(w, r)
	if !ok {
		return
	}
	st, ok := host.Store().(*store.Store)
	if !ok || st == nil {
		host.WriteAPIError(w, http.StatusServiceUnavailable, "the WebDAV archive is not available")
		return
	}
	db, err := st.UserDB(r.Context(), current.UserID)
	if err != nil {
		host.ServerError(w, err)
		return
	}
	rest := strings.Trim(strings.TrimPrefix(path, apiPath), "/")
	switch {
	case rest == "targets" && r.Method == http.MethodGet:
		p.apiListTargets(host, st, db, current.UserID, w, r)
	case rest == "targets" && r.Method == http.MethodPost:
		p.apiSaveTarget(host, db, current.UserID, 0, w, r)
	case strings.HasPrefix(rest, "targets/"):
		p.apiTargetAction(host, db, current.UserID, rest, w, r)
	case rest == "uploads" && r.Method == http.MethodGet:
		p.apiListUploads(host, db, current.UserID, w, r)
	case strings.HasPrefix(rest, "uploads/") && strings.HasSuffix(rest, "/retry") && r.Method == http.MethodPost:
		p.apiRetryUpload(host, db, current.UserID, rest, w, r)
	case rest == "run" && r.Method == http.MethodPost:
		if !host.VerifyCSRF(w, r) {
			return
		}
		p.wake()
		host.WriteJSON(w, map[string]any{"ok": true})
	case rest == "browse" && r.Method == http.MethodGet:
		p.apiBrowse(host, db, current.UserID, w, r)
	case rest == "download" && r.Method == http.MethodGet:
		p.apiDownload(host, db, current.UserID, w, r)
	case rest == "file" && r.Method == http.MethodDelete:
		p.apiDeleteFile(host, db, current.UserID, w, r)
	default:
		host.WriteAPIError(w, http.StatusNotFound, "WebDAV archive route not found")
	}
}

func (p *webdavArchiveBackend) apiListTargets(host plugins.APIHost, st *store.Store, db *sql.DB, userID int64, w http.ResponseWriter, r *http.Request) {
	items, err := listTargets(r.Context(), db, userID, false)
	if err != nil {
		host.ServerError(w, err)
		return
	}
	views := make([]targetView, 0, len(items))
	for _, item := range items {
		views = append(views, presentTarget(item))
	}
	mailboxes, err := listMailboxOptions(r.Context(), st, userID)
	if err != nil {
		host.ServerError(w, err)
		return
	}
	counts, err := uploadCounts(r.Context(), db, userID)
	if err != nil {
		host.ServerError(w, err)
		return
	}
	host.WriteJSON(w, map[string]any{"targets": views, "mailboxes": mailboxes, "counts": counts})
}

func (p *webdavArchiveBackend) apiTargetAction(host plugins.APIHost, db *sql.DB, userID int64, rest string, w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.Trim(rest, "/"), "/")
	if len(parts) < 2 {
		host.WriteAPIError(w, http.StatusNotFound, "WebDAV archive route not found")
		return
	}
	id, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || id <= 0 {
		host.WriteAPIError(w, http.StatusBadRequest, "invalid target id")
		return
	}
	switch {
	case len(parts) == 2 && r.Method == http.MethodPut:
		p.apiSaveTarget(host, db, userID, id, w, r)
	case len(parts) == 2 && r.Method == http.MethodDelete:
		if !host.VerifyCSRF(w, r) {
			return
		}
		if err := deleteTarget(r.Context(), db, userID, id); err != nil {
			writeScopedError(host, w, err, "target not found")
			return
		}
		host.WriteJSON(w, map[string]any{"ok": true})
	case len(parts) == 3 && parts[2] == "enabled" && r.Method == http.MethodPost:
		if !host.VerifyCSRF(w, r) {
			return
		}
		var in enabledInput
		if !host.DecodeJSON(w, r, &in) {
			return
		}
		if err := setTargetEnabled(r.Context(), db, userID, id, in.Enabled); err != nil {
			writeScopedError(host, w, err, "target not found")
			return
		}
		p.wake()
		host.WriteJSON(w, map[string]any{"ok": true})
	case len(parts) == 3 && parts[2] == "test" && r.Method == http.MethodPost:
		p.apiTestTarget(host, db, userID, id, w, r)
	default:
		host.WriteAPIError(w, http.StatusNotFound, "WebDAV archive route not found")
	}
}

func (p *webdavArchiveBackend) apiSaveTarget(host plugins.APIHost, db *sql.DB, userID, targetID int64, w http.ResponseWriter, r *http.Request) {
	if !host.VerifyCSRF(w, r) {
		return
	}
	var in targetInput
	if !host.DecodeJSON(w, r, &in) {
		return
	}
	item, err := prepareTarget(r.Context(), host, db, userID, targetID, in)
	if err != nil {
		host.WriteAPIError(w, http.StatusBadRequest, err.Error())
		return
	}
	saved, err := persistTarget(r.Context(), db, item)
	if err != nil {
		writeScopedError(host, w, err, "target not found")
		return
	}
	p.wake()
	host.WriteJSON(w, map[string]any{"ok": true, "target": presentTarget(saved)})
}

// prepareTarget validates and normalizes what the form sent. A password is
// only replaced when one was typed: the form never receives the stored one, so
// an empty field means "leave it", and clearing it is its own explicit flag.
func prepareTarget(ctx context.Context, host plugins.APIHost, db *sql.DB, userID, targetID int64, in targetInput) (target, error) {
	var existing target
	if targetID > 0 {
		var err error
		existing, err = getTarget(ctx, db, userID, targetID)
		if err != nil {
			return target{}, errors.New("target not found")
		}
	}
	address, err := parseTargetAddress(in.BaseURL)
	if err != nil {
		return target{}, err
	}
	enabled := true
	if in.Enabled != nil {
		enabled = *in.Enabled
	} else if targetID > 0 {
		enabled = existing.Enabled
	}
	includeInline := false
	if in.IncludeInline != nil {
		includeInline = *in.IncludeInline
	} else if targetID > 0 {
		includeInline = existing.IncludeInline
	}
	contentTypes := strings.TrimSpace(in.ContentTypes)
	if contentTypes == "" {
		contentTypes = defaultContentTypes
	}
	pathTemplate := strings.TrimSpace(in.PathTemplate)
	if pathTemplate == "" {
		pathTemplate = defaultPathTemplate
	}
	if strings.Contains(pathTemplate, "..") {
		return target{}, errors.New("the path template may not contain ..")
	}
	name := strings.TrimSpace(in.Name)
	if name == "" {
		name = address.Host
	}
	item := target{
		ID:             targetID,
		UserID:         userID,
		Name:           name,
		Enabled:        enabled,
		BaseURL:        address.URL.String(),
		Username:       strings.TrimSpace(in.Username),
		WatchMailboxID: in.WatchMailboxID,
		ContentTypes:   contentTypes,
		PathTemplate:   pathTemplate,
		IncludeInline:  includeInline,
	}
	switch {
	case in.ClearPassword:
		item.EncryptedPassword = ""
	case strings.TrimSpace(in.Password) != "":
		encrypted, err := mmcrypto.EncryptString(host.MasterKey(), in.Password)
		if err != nil {
			return target{}, errors.New("the password could not be stored")
		}
		item.EncryptedPassword = encrypted
	default:
		item.EncryptedPassword = existing.EncryptedPassword
	}
	switch {
	case in.ClearPrivateKey:
		item.EncryptedPrivateKey = ""
	case strings.TrimSpace(in.PrivateKey) != "":
		encrypted, err := mmcrypto.EncryptString(host.MasterKey(), in.PrivateKey)
		if err != nil {
			return target{}, errors.New("the private key could not be stored")
		}
		item.EncryptedPrivateKey = encrypted
	default:
		item.EncryptedPrivateKey = existing.EncryptedPrivateKey
	}
	// Everything a transport needs beyond the address is checked here rather
	// than at the first upload, so a target that cannot work says so while the
	// reader is still looking at the form.
	switch address.Transport {
	case transportSFTP:
		if item.Username == "" {
			return target{}, errors.New("an sftp:// target needs a user name")
		}
		if item.EncryptedPassword == "" && item.EncryptedPrivateKey == "" {
			return target{}, errors.New("an sftp:// target needs a password or a private key")
		}
	case transportSMB:
		if item.Username == "" {
			return target{}, errors.New("an smb:// target needs a user name")
		}
	}
	return item, nil
}

func (p *webdavArchiveBackend) apiTestTarget(host plugins.APIHost, db *sql.DB, userID, targetID int64, w http.ResponseWriter, r *http.Request) {
	if !host.VerifyCSRF(w, r) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	// A destination that cannot be opened at all is the same answer to the
	// reader as one that opens and then refuses: both are "this is not working
	// yet", and both belong in the panel rather than in a 500.
	store, _, err := openTargetStore(ctx, host, db, userID, targetID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			host.WriteAPIError(w, http.StatusNotFound, "target not found")
			return
		}
		host.WriteJSON(w, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	defer store.Close()
	if err := store.CheckAccess(ctx); err != nil {
		host.WriteJSON(w, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	host.WriteJSON(w, map[string]any{"ok": true})
}

func (p *webdavArchiveBackend) apiListUploads(host plugins.APIHost, db *sql.DB, userID int64, w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	targetID, _ := strconv.ParseInt(query.Get("target"), 10, 64)
	limit, _ := strconv.Atoi(query.Get("limit"))
	items, err := listUploads(r.Context(), db, userID, targetID, strings.TrimSpace(query.Get("status")), limit)
	if err != nil {
		host.ServerError(w, err)
		return
	}
	views := make([]uploadView, 0, len(items))
	for _, item := range items {
		views = append(views, presentUpload(item))
	}
	counts, err := uploadCounts(r.Context(), db, userID)
	if err != nil {
		host.ServerError(w, err)
		return
	}
	host.WriteJSON(w, map[string]any{"uploads": views, "counts": counts})
}

func (p *webdavArchiveBackend) apiRetryUpload(host plugins.APIHost, db *sql.DB, userID int64, rest string, w http.ResponseWriter, r *http.Request) {
	if !host.VerifyCSRF(w, r) {
		return
	}
	parts := strings.Split(strings.Trim(rest, "/"), "/")
	if len(parts) != 3 {
		host.WriteAPIError(w, http.StatusNotFound, "WebDAV archive route not found")
		return
	}
	id, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || id <= 0 {
		host.WriteAPIError(w, http.StatusBadRequest, "invalid upload id")
		return
	}
	if err := retryUpload(r.Context(), db, userID, id); err != nil {
		writeScopedError(host, w, err, "no upload to retry")
		return
	}
	p.wake()
	host.WriteJSON(w, map[string]any{"ok": true})
}

// openTargetStore opens one of the caller's own targets, whichever protocol it
// speaks. Both the target lookup and the decrypt happen here rather than at
// each call site, so no route can reach a target belonging to another user.
//
// The caller closes the store: SFTP and SMB hold a live connection.
func openTargetStore(ctx context.Context, host plugins.BackendHost, db *sql.DB, userID, targetID int64) (remoteStore, target, error) {
	configured, err := getTarget(ctx, db, userID, targetID)
	if err != nil {
		return nil, target{}, err
	}
	password, privateKey, err := targetCredentials(host, configured)
	if err != nil {
		return nil, target{}, err
	}
	store, err := openRemoteStore(ctx, configured, password, privateKey)
	if err != nil {
		return nil, target{}, err
	}
	return store, configured, nil
}

// targetCredentials decrypts what a target stored. It is one function because
// both secrets are read in the same two places -- the API and the worker -- and
// a second copy of "which key, which field" is a copy that can drift.
func targetCredentials(host plugins.BackendHost, configured target) (string, string, error) {
	password := ""
	if strings.TrimSpace(configured.EncryptedPassword) != "" {
		decrypted, err := mmcrypto.DecryptString(host.MasterKey(), configured.EncryptedPassword)
		if err != nil {
			return "", "", errors.New("the stored password could not be read")
		}
		password = decrypted
	}
	privateKey := ""
	if strings.TrimSpace(configured.EncryptedPrivateKey) != "" {
		decrypted, err := mmcrypto.DecryptString(host.MasterKey(), configured.EncryptedPrivateKey)
		if err != nil {
			return "", "", errors.New("the stored private key could not be read")
		}
		privateKey = decrypted
	}
	return password, privateKey, nil
}

func listMailboxOptions(ctx context.Context, st *store.Store, userID int64) ([]mailboxOption, error) {
	accounts, err := st.ListMailAccountsForUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	labels := make(map[int64]string, len(accounts))
	for _, account := range accounts {
		label := strings.TrimSpace(account.Label)
		if label == "" {
			label = account.Email
		}
		labels[account.ID] = label
	}
	mailboxes, err := st.ListMailboxesForUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	out := make([]mailboxOption, 0, len(mailboxes))
	for _, mailbox := range mailboxes {
		label := mailbox.Name
		if account := labels[mailbox.AccountID]; account != "" {
			label = fmt.Sprintf("%s — %s", account, mailbox.Name)
		}
		out = append(out, mailboxOption{ID: mailbox.ID, Name: mailbox.Name, Role: mailbox.Role, Label: label})
	}
	return out, nil
}

// writeScopedError keeps a missing row and a row belonging to someone else the
// same 404, so an id cannot be probed for existence.
func writeScopedError(host plugins.APIHost, w http.ResponseWriter, err error, notFound string) {
	if errors.Is(err, sql.ErrNoRows) {
		host.WriteAPIError(w, http.StatusNotFound, notFound)
		return
	}
	host.ServerError(w, err)
}
