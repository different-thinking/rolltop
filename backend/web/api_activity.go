// File overview: The Activity view's endpoints. Everything Rolltop is doing
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

	"rolltop/backend/syncer"
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
	// Only the pending count is shown, so it is read directly rather than
	// through the category chrome: the chrome's cache keys on the mail-list
	// generation, which churns exactly while a sync runs -- when this view is
	// polled -- and each miss would recount every category for an answer this
	// view throws away.
	categoriesPending, err := s.store.CountMessagesNeedingCategory(r.Context(), cu.User.ID)
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
			Label:       syncer.WorkerKindLabel(activity.Kind),
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
	connections, err := s.googleConnectionsWithSyncState(r.Context(), userID)
	if err != nil {
		log.Printf("activity google connections user_id=%d: %v", userID, err)
		return out
	}
	for _, connection := range connections {
		if state := connection.ContactsSync; state != nil {
			out = append(out, apiActivityService{
				Kind: "google_contacts", Label: "Google contacts", Account: connection.Email, ConnectionID: connection.ID,
				Status: state.Status, StatusDetail: state.StatusDetail, LastSyncAt: state.LastSyncAt,
				LastSuccess: state.LastSuccess, ItemCount: state.ContactCount, EverSynced: state.EverSynced,
			})
		}
		if state := connection.CalendarSync; state != nil {
			out = append(out, apiActivityService{
				Kind: "google_calendars", Label: "Google calendars", Account: connection.Email, ConnectionID: connection.ID,
				Status: state.Status, StatusDetail: state.StatusDetail, LastSyncAt: state.LastSyncAt,
				LastSuccess: state.LastSuccess, ItemCount: state.CalendarCount, EverSynced: state.EverSynced,
			})
		}
	}
	return out
}

// apiActivityWorkerCancel stops one background worker. The key travels in the
// request body rather than the path: reservation keys embed raw IMAP mailbox
// names, which may contain any separator a path scheme could pick, and a POST
// body has no such invariant to defend.
//
// Cancelling stops the sync turn the worker belongs to -- a reservation batch
// shares one context -- and is not a prohibition: the work's own scheduling
// decides when it comes back.
func (s *Server) apiActivityWorkerCancel(w http.ResponseWriter, r *http.Request) {
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
	var in struct {
		Key string `json:"key"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	if s.syncRunner == nil || in.Key == "" || !s.syncRunner.CancelWorkerActivity(cu.User.ID, in.Key) {
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
