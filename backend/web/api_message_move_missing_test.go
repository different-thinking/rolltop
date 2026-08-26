package web

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"rolltop/backend/store"
	"rolltop/backend/store/storetest"
	"rolltop/backend/syncer"
)

// A lone move of a message this user no longer has is the outcome the caller
// asked for, not a failure. Answering 404 turned a stale row - a page the
// browser cached before the message left, a message another tab already filed -
// into a "Delete failed" the reader could never get past: the row came back on
// screen and every further attempt asked the same question about the same
// missing message. bulk-move has answered this way for a while and the lone
// path, which is what deleting one message uses, has to agree with it.
func TestMoveMessageAnswersMissingMessageAsNothingToMove(t *testing.T) {
	ctx := context.Background()
	db, err := storetest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	owner, err := db.CreateUser(ctx, "move-missing-owner@example.test", "Owner", "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	other, err := db.CreateUser(ctx, "move-missing-other@example.test", "Other", "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	ownerMessage := createNotificationTestMessage(t, ctx, db, owner, 501, "Sender", "Owner message")
	otherMessage := createNotificationTestMessage(t, ctx, db, other, 502, "Sender", "Other message")
	trash, err := db.GetOrCreateMailbox(ctx, owner.ID, ownerMessage.AccountID, "Trash")
	if err != nil {
		t.Fatal(err)
	}
	// The fetcher is never reached: both cases answer before anything is
	// dispatched, which is also what keeps them off the tenant's foreground turn.
	server := &Server{store: db, syncer: &syncer.Service{Store: db}}

	for _, tc := range []struct {
		name      string
		messageID int64
	}{
		{name: "no such message", messageID: ownerMessage.ID + 100_000},
		{name: "another tenant's message", messageID: otherMessage.ID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := moveMessageRequest(t, server, owner, tc.messageID, trash)
			if res.Code != http.StatusOK {
				t.Fatalf("status = %d body=%s", res.Code, res.Body.String())
			}
			var response struct {
				OK      bool   `json:"ok"`
				Moved   int    `json:"moved"`
				Mailbox string `json:"mailbox"`
			}
			if err := json.NewDecoder(res.Body).Decode(&response); err != nil {
				t.Fatal(err)
			}
			if !response.OK || response.Moved != 0 || response.Mailbox != trash.Name {
				t.Fatalf("response = %+v, want ok with nothing moved into %s", response, trash.Name)
			}
		})
	}

	// The other tenant's message is still where it was: answering success must
	// not be reachable as a way to touch mail this user does not own.
	unchanged, err := db.GetMessageForUser(ctx, other.ID, otherMessage.ID)
	if err != nil {
		t.Fatal(err)
	}
	if unchanged.MailboxID != otherMessage.MailboxID {
		t.Fatalf("other tenant's message moved to mailbox %d", unchanged.MailboxID)
	}
}

// The reader's view files a message away for good when it opens to nothing, so
// the answer that says so has to be one only Rolltop can give. A bare 404 is
// not: Go's own NotFound answers in plain text and so does every proxy in front
// of the app, and a client reading the status alone would hide mail that is
// still there because a gateway answered for it.
func TestMissingMessageIsNamedRatherThanAnsweredWithABare404(t *testing.T) {
	ctx := context.Background()
	db, err := storetest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	owner, err := db.CreateUser(ctx, "message-gone-owner@example.test", "Owner", "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	other, err := db.CreateUser(ctx, "message-gone-other@example.test", "Other", "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	strangers := createNotificationTestMessage(t, ctx, db, other, 601, "Sender", "Other message")
	server := &Server{store: db}

	for _, tc := range []struct {
		name      string
		messageID int64
	}{
		{name: "no such message", messageID: strangers.ID + 100_000},
		{name: "another tenant's message", messageID: strangers.ID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/messages/"+strconvInt64(tc.messageID), nil)
			req = req.WithContext(context.WithValue(req.Context(), userContextKey, currentUser{User: owner}))
			res := httptest.NewRecorder()
			server.apiMessage(res, req, tc.messageID)
			if res.Code != http.StatusNotFound {
				t.Fatalf("status = %d body=%s", res.Code, res.Body.String())
			}
			var payload struct {
				Error string `json:"error"`
				Code  string `json:"code"`
			}
			if err := json.NewDecoder(res.Body).Decode(&payload); err != nil {
				t.Fatalf("body did not parse as a Rolltop error: %v", err)
			}
			if payload.Code != messageGoneCode || payload.Error == "" {
				t.Fatalf("payload = %+v, want code %q with a reason", payload, messageGoneCode)
			}
		})
	}
}

func moveMessageRequest(t *testing.T, server *Server, user store.User, messageID int64, dest store.Mailbox) *httptest.ResponseRecorder {
	t.Helper()
	payload, err := json.Marshal(map[string]any{"mailbox_id": dest.ID})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/messages/move", bytes.NewReader(payload))
	req = req.WithContext(context.WithValue(req.Context(), userContextKey, currentUser{User: user}))
	csrfBase := "move-missing-csrf"
	req.AddCookie(&http.Cookie{Name: csrfCookie, Value: csrfBase})
	req.Header.Set("X-CSRF-Token", server.csrfForBase(csrfBase))
	res := httptest.NewRecorder()
	server.apiMoveMessage(res, req, messageID)
	return res
}
