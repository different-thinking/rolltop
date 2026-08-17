package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"testing"

	"rolltop/backend/search"
	"rolltop/backend/store"
)

func searchConversationMessageIDs(t *testing.T, server *Server, user store.User, query string) []int64 {
	t.Helper()
	target := "/api/search?q=" + url.QueryEscape(query) + "&page=1"
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req = req.WithContext(context.WithValue(req.Context(), userContextKey, currentUser{User: user}))
	res := httptest.NewRecorder()
	server.apiSearch(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("search %q status = %d body=%s", query, res.Code, res.Body.String())
	}
	var payload struct {
		Conversations []struct {
			Message struct {
				ID int64 `json:"id"`
			} `json:"message"`
		} `json:"conversations"`
	}
	if err := json.NewDecoder(res.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	ids := make([]int64, 0, len(payload.Conversations))
	for _, conversation := range payload.Conversations {
		ids = append(ids, conversation.Message.ID)
	}
	return ids
}

func TestSearchSkipsTrashUnlessTheQueryNamesIt(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, "rolltop.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	searchService, err := search.Open(filepath.Join(dir, "bleve"))
	if err != nil {
		t.Fatal(err)
	}
	defer searchService.Close()
	tenant := newScopeTestTenant(t, ctx, db, "search-trash@example.test")
	kept := createScopeTestMessage(t, ctx, db, tenant, tenant.inbox, 1001, "Newsletter weekly digest")
	deleted := createScopeTestMessage(t, ctx, db, tenant, tenant.trash, 1002, "Newsletter weekly digest")
	for _, id := range []int64{kept.ID, deleted.ID} {
		stored, err := db.GetMessageForUser(ctx, tenant.user.ID, id)
		if err != nil {
			t.Fatal(err)
		}
		if err := searchService.IndexMessage(ctx, stored, nil); err != nil {
			t.Fatal(err)
		}
	}
	server := &Server{store: db, search: searchService, mailListCache: newMailListCache()}

	ids := searchConversationMessageIDs(t, server, tenant.user, "newsletter")
	if len(ids) != 1 || ids[0] != kept.ID {
		t.Fatalf("search results = %v, want only the kept message %d", ids, kept.ID)
	}

	// Naming the folder is how deleted mail is asked for, and then only that.
	trashIDs := searchConversationMessageIDs(t, server, tenant.user, "newsletter in:trash")
	if len(trashIDs) != 1 || trashIDs[0] != deleted.ID {
		t.Fatalf("in:trash results = %v, want only the deleted message %d", trashIDs, deleted.ID)
	}
}

func TestSearchMailboxFilterExcludesOnlyTrashRole(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(filepath.Join(t.TempDir(), "rolltop.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	tenant := newScopeTestTenant(t, ctx, db, "search-filter@example.test")
	junk, err := db.GetOrCreateMailboxWithRole(ctx, tenant.user.ID, tenant.accountID, "Junk", "junk")
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{store: db}

	cleaned, filter, err := server.searchMailboxFilter(ctx, tenant.user.ID, "invoice")
	if err != nil {
		t.Fatal(err)
	}
	if cleaned != "invoice" {
		t.Fatalf("cleaned query = %q, want the query unchanged", cleaned)
	}
	if filter.enabled {
		t.Fatal("a query without in: must not become an allow list")
	}
	if filter.matches(store.MessageRecord{MailboxID: tenant.trash.ID}) {
		t.Fatal("trash message matched a query that did not name trash")
	}
	// Junk and Drafts stay searchable: only deleted mail is filtered here.
	if !filter.matches(store.MessageRecord{MailboxID: junk.ID}) {
		t.Fatal("junk message was filtered out of a plain search")
	}
	if !filter.matches(store.MessageRecord{MailboxID: tenant.inbox.ID}) {
		t.Fatal("inbox message was filtered out of a plain search")
	}

	cleaned, named, err := server.searchMailboxFilter(ctx, tenant.user.ID, "invoice in:Trash")
	if err != nil {
		t.Fatal(err)
	}
	if cleaned != "invoice" {
		t.Fatalf("cleaned query = %q, want the in: operator removed", cleaned)
	}
	if !named.enabled || !named.matches(store.MessageRecord{MailboxID: tenant.trash.ID}) {
		t.Fatal("in:Trash did not select the trash mailbox")
	}
	if named.matches(store.MessageRecord{MailboxID: tenant.inbox.ID}) {
		t.Fatal("in:Trash also matched the inbox")
	}
}
