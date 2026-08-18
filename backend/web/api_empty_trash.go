// File overview: Emptying a Trash folder. This is the only route that deletes mail on
// the IMAP server, so it is deliberately narrow: one folder, named by the user, and
// only one that carries the Trash role.

package web

import (
	"errors"
	"net/http"

	"rolltop/backend/store"
	"rolltop/backend/syncer"
)

// apiEmptyTrash permanently deletes everything in one Trash folder as a
// background sync run. Unlike every other message action it does not move mail
// anywhere: the messages are removed from the server, and the local mirror
// follows once the server confirms they are gone.
func (s *Server) apiEmptyTrash(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	cu, ok := s.requireAPIAuth(w, r)
	if !ok {
		return
	}
	if !s.verifyCSRF(w, r) {
		return
	}
	if s.syncer == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "IMAP sync is not configured")
		return
	}
	var in struct {
		MailboxID int64 `json:"mailbox_id"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	if in.MailboxID <= 0 {
		http.NotFound(w, r)
		return
	}
	mailbox, err := s.store.GetMailboxForUser(r.Context(), cu.User.ID, in.MailboxID)
	if err != nil {
		if store.IsNotFound(err) {
			http.NotFound(w, r)
			return
		}
		s.serverError(w, r, err)
		return
	}
	if mailbox.Role != "trash" {
		writeAPIError(w, http.StatusBadRequest, "Only a Trash folder can be emptied.")
		return
	}
	finishForeground := func() {}
	if s.syncRunner != nil {
		finishForeground, err = s.syncRunner.BeginForegroundOperation(r.Context(), cu.User.ID)
		if err != nil {
			s.apiError(w, r, http.StatusServiceUnavailable, "could not schedule emptying the Trash", err)
			return
		}
	}
	run, err := s.syncer.StartEmptyTrash(r.Context(), cu.User.ID, mailbox.ID, func() {
		s.startMoveRefresh(cu.User.ID, mailbox.AccountID, []string{mailbox.Name})
		finishForeground()
	})
	if err != nil {
		finishForeground()
		switch {
		case errors.Is(err, syncer.ErrEmptyTrashUnsupported):
			writeAPIError(w, http.StatusBadRequest,
				"This account's IMAP connection cannot delete messages, so "+mailbox.Name+" cannot be emptied here.")
		case store.IsNotFound(err):
			http.NotFound(w, r)
		default:
			s.apiError(w, r, http.StatusBadGateway, "could not start emptying the Trash", err)
		}
		return
	}
	s.noteMailListChanged(cu.User.ID)
	writeJSON(w, map[string]any{"ok": true, "run_id": run.ID, "mailbox": mailbox.Name})
}
