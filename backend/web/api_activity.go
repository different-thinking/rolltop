// File overview: The Activity view's one endpoint. Everything Rolltop is doing
// for the signed-in user in the background -- mailbox syncs, the workers behind
// them, and the Google syncs that run on their own timer -- is collected here so
// there is a single place to look, and a single place to stop something.
//
// It reads live state and reports it; it never starts work. The only writes are
// the cancel and clear actions, which are the two things a user watching a
// stuck job actually needs.

package web

import (
	"log"
	"net/http"
	"strings"

	"rolltop/backend/store"
)

// activityRunHistoryLimit bounds the finished runs the view lists. It is a
// window on what just happened, not an audit log: a longer list answers no
// question the user has while watching work in progress.
const activityRunHistoryLimit = 30

// apiActivityWorker is one background worker as the view shows it.
type apiActivityWorker struct {
	Key string `json:"key"`
	// Kind is the runner's own name for the work, so a report from this view
	// and a line in the server log describe the same thing.
	Kind        string `json:"kind"`
	Label       string `json:"label"`
	Phase       string `json:"phase"`
	AccountID   int64  `json:"account_id"`
	Mailbox     string `json:"mailbox"`
	StartedAt   string `json:"started_at"`
	Cancellable bool   `json:"cancellable"`
	Waiting     bool   `json:"waiting"`
}

// apiActivityService is one thing that syncs on its own schedule rather than
// through the mailbox runner: a Google account's contacts or calendars.
type apiActivityService struct {
	Kind         string `json:"kind"`
	Label        string `json:"label"`
	Account      string `json:"account"`
	ConnectionID int64  `json:"connection_id"`
	Status       string `json:"status"`
	StatusDetail string `json:"status_detail"`
	LastSyncAt   string `json:"last_sync_at"`
	LastSuccess  string `json:"last_success_at"`
	ItemCount    int    `json:"item_count"`
	EverSynced   bool   `json:"ever_synced"`
}

// workerLabels names each kind of work in the words the view uses. A kind with
// no entry falls back to its own name, so a worker added to the runner shows up
// here as soon as it exists rather than being silently left out.
var workerLabels = map[string]string{
	"account_wide_sync":          "Account sync",
	"foreground_operation":       "Waiting for a user action",
	"sender_stats":               "Sender statistics",
	"mailbox_sync":               "Folder sync",
	"mailbox_maintenance":        "Folder maintenance",
	"mailbox_search_maintenance": "Search index rebuild",
	"recovery_replay":            "Folder recovery",
	"attachment_index":           "Attachment index and categories",
}

func workerLabel(kind string) string {
	if label, ok := workerLabels[kind]; ok {
		return label
	}
	return kind
}

func (s *Server) apiActivity(w http.ResponseWriter, r *http.Request) {
	cu, ok := s.requireAPIAuth(w, r)
	if !ok {
		return
	}
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	runs, err := s.store.ListSyncRunsForUser(r.Context(), cu.User.ID, activityRunHistoryLimit)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	_, categoriesPending, err := s.mailCategoryChrome(r.Context(), cu.User.ID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	writeJSON(w, map[string]any{
		"sync_runs":          apiSyncRuns(runs),
		"workers":            s.activityWorkers(cu.User.ID),
		"services":           s.activityServices(r, cu.User.ID),
		"categories_pending": categoriesPending,
	})
}

// activityWorkers turns the runner's own reservation record into the view's
// list. Without a runner there is no background work to report, which is a real
// state on an install that only serves a mirror.
func (s *Server) activityWorkers(userID int64) []apiActivityWorker {
	if s.syncRunner == nil {
		return []apiActivityWorker{}
	}
	activities := s.syncRunner.WorkerActivities(userID)
	out := make([]apiActivityWorker, 0, len(activities))
	for _, activity := range activities {
		out = append(out, apiActivityWorker{
			Key:         activity.Key,
			Kind:        activity.Kind,
			Label:       workerLabel(activity.Kind),
			Phase:       activity.Phase,
			AccountID:   activity.AccountID,
			Mailbox:     activity.Mailbox,
			StartedAt:   timeString(activity.StartedAt),
			Cancellable: activity.Cancellable,
			Waiting:     activity.Waiting,
		})
	}
	return out
}

// activityServices reports the Google syncs. They keep their own schedule and
// their own error state, so a mailbox view can never explain why contacts are
// stale; this is where that answer belongs.
func (s *Server) activityServices(r *http.Request, userID int64) []apiActivityService {
	out := []apiActivityService{}
	if s.googleAuth == nil {
		return out
	}
	connections, err := s.googleAuth.List(r.Context(), userID)
	if err != nil {
		log.Printf("activity google connections user_id=%d: %v", userID, err)
		return out
	}
	for _, connection := range connections {
		item := apiGoogleConnectionFromStore(connection)
		if item.HasContactsScope {
			if state := s.googleContactsSyncState(r.Context(), userID, connection.ID); state != nil {
				out = append(out, apiActivityService{
					Kind: "google_contacts", Label: "Google contacts", Account: item.Email, ConnectionID: connection.ID,
					Status: state.Status, StatusDetail: state.StatusDetail, LastSyncAt: state.LastSyncAt,
					LastSuccess: state.LastSuccess, ItemCount: state.ContactCount, EverSynced: state.EverSynced,
				})
			}
		}
		if item.HasCalendarScope {
			if state := s.googleCalendarSyncState(r.Context(), userID, connection.ID); state != nil {
				out = append(out, apiActivityService{
					Kind: "google_calendars", Label: "Google calendars", Account: item.Email, ConnectionID: connection.ID,
					Status: state.Status, StatusDetail: state.StatusDetail, LastSyncAt: state.LastSyncAt,
					LastSuccess: state.LastSuccess, ItemCount: state.CalendarCount, EverSynced: state.EverSynced,
				})
			}
		}
	}
	return out
}

// apiActivityWorkerAction stops one background worker. Cancelling is not the
// same as forbidding: the work is dropped for now and its own scheduling
// decides when it comes back, which is what a user stopping a job that is in
// the way of something else wants.
func (s *Server) apiActivityWorkerAction(w http.ResponseWriter, r *http.Request, rest string) {
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
	key, action, found := strings.Cut(strings.Trim(rest, "/"), "|")
	if !found {
		// The key carries slashes of its own, so the action is separated from
		// it by a character a reservation key cannot contain.
		http.NotFound(w, r)
		return
	}
	if action != "cancel" || s.syncRunner == nil {
		http.NotFound(w, r)
		return
	}
	cancelled := s.syncRunner.CancelWorkerActivity(cu.User.ID, key)
	if !cancelled {
		writeAPIError(w, http.StatusConflict, "This background task is no longer running, or cannot be stopped.")
		return
	}
	s.events.Notify(cu.User.ID)
	writeJSON(w, map[string]any{"ok": true})
}

// apiActivityHistory clears finished runs. Running ones are left alone: the
// list is history, and a run still in flight is cancelled rather than erased.
func (s *Server) apiActivityHistory(w http.ResponseWriter, r *http.Request) {
	cu, ok := s.requireAPIAuth(w, r)
	if !ok {
		return
	}
	if r.Method != http.MethodDelete {
		methodNotAllowed(w)
		return
	}
	if !s.verifyCSRF(w, r) {
		return
	}
	removed, err := s.store.DeleteFinishedSyncRunsForUser(r.Context(), cu.User.ID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.events.Notify(cu.User.ID)
	writeJSON(w, map[string]any{"ok": true, "removed": removed})
}

// activityRunDelete removes one finished run, and is reached through the
// sync-run route so a run is cancelled and deleted at the same address.
func (s *Server) activityRunDelete(w http.ResponseWriter, r *http.Request, userID, runID int64) {
	if !s.verifyCSRF(w, r) {
		return
	}
	err := s.store.DeleteSyncRunForUser(r.Context(), userID, runID)
	if store.IsNotFound(err) {
		writeAPIError(w, http.StatusConflict, "This sync run is still running. Cancel it first.")
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.events.Notify(userID)
	writeJSON(w, map[string]any{"ok": true})
}
