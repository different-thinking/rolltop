package web

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
	"time"

	"rolltop/backend/mailparse"
	"rolltop/backend/store"
)

func createCategoryTestMessage(t *testing.T, ctx context.Context, db *store.Store, tenant scopeTestTenant,
	mailbox store.Mailbox, uid uint32, from, category string,
) store.MessageRecord {
	t.Helper()
	return createScopeTestMessageFrom(t, ctx, db, tenant, mailbox, uid, "", from, category, time.Now().UTC())
}

func TestMailViewNamesResolveOnlyToListsThisServerRenders(t *testing.T) {
	known := []string{"", "inbox", "sent", "drafts", "relevant", "newsletters", "forums", "notifications"}
	for _, name := range known {
		view, ok := parseMailView(name)
		if !ok || string(view) != name {
			t.Fatalf("parseMailView(%q) = %q ok=%t, want the same name", name, view, ok)
		}
	}
	for _, name := range []string{"everything", "category:newsletters", "junk", "Relevant "} {
		if view, ok := parseMailView(name); ok && string(view) != "relevant" {
			t.Fatalf("parseMailView(%q) = %q ok=%t, want a rejection", name, view, ok)
		}
	}
	// The trimmed, lowercased form of a real name is still that name, which is
	// what lets a URL carry it without the handler doing its own normalizing.
	if view, ok := parseMailView(" Newsletters "); !ok || view != "newsletters" {
		t.Fatalf("parseMailView(\" Newsletters \") = %q ok=%t", view, ok)
	}
	// A retired name still resolves, to the view that replaced it: bookmarks and
	// cached app shells keep asking for the Inbox list as "unarchived".
	if view, ok := parseMailView("unarchived"); !ok || view != mailViewInbox {
		t.Fatalf("parseMailView(\"unarchived\") = %q ok=%t, want the Inbox view", view, ok)
	}
}

func TestCategoryScopeCoversOnlyItsOwnUnarchivedMail(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(filepath.Join(t.TempDir(), "rolltop.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	tenant := newScopeTestTenant(t, ctx, db, "scope-categories@example.test")
	archive, err := db.GetOrCreateMailbox(ctx, tenant.user.ID, tenant.accountID, "Archive")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.SaveSwipePreferences(ctx, store.SwipePreferences{
		UserID:     tenant.user.ID,
		LeftAction: store.SwipeActionSnooze, LeftSnoozePreset: store.SwipeSnoozeTomorrow,
		RightAction: store.SwipeActionMarkRead, RightSnoozePreset: store.SwipeSnoozeTomorrow,
		ArchiveMailboxes: []store.SwipeArchiveMailbox{{AccountID: tenant.accountID, MailboxID: archive.ID}},
	}); err != nil {
		t.Fatal(err)
	}
	newsletter := createCategoryTestMessage(t, ctx, db, tenant, tenant.inbox, 701, "news@example.test", mailparse.CategoryNewsletters)
	personal := createCategoryTestMessage(t, ctx, db, tenant, tenant.inbox, 702, "ada@example.test", mailparse.CategoryRelevant)
	archived := createCategoryTestMessage(t, ctx, db, tenant, archive, 703, "news@example.test", mailparse.CategoryNewsletters)
	server := &Server{store: db, masterKey: []byte("12345678901234567890123456789012")}

	plan, err := server.scopeTrashPlan(ctx, tenant.user, scopeSelection{View: mailView(mailparse.CategoryNewsletters)})
	if err != nil {
		t.Fatal(err)
	}
	ids := planMessageIDs(plan)
	if plan.Matched != 1 || len(ids) != 1 || ids[0] != newsletter.ID {
		t.Fatalf("newsletters plan = matched %d ids %v, want only %d (not %d or %d)",
			plan.Matched, ids, newsletter.ID, personal.ID, archived.ID)
	}
}

func TestCategoryChromeCountsWhatTheListShows(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(filepath.Join(t.TempDir(), "rolltop.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	tenant := newScopeTestTenant(t, ctx, db, "chrome-categories@example.test")
	read := createCategoryTestMessage(t, ctx, db, tenant, tenant.inbox, 801, "news@example.test", mailparse.CategoryNewsletters)
	createCategoryTestMessage(t, ctx, db, tenant, tenant.inbox, 802, "news@example.test", mailparse.CategoryNewsletters)
	// A message still waiting for classification counts as pending, not as a
	// member of any category.
	createCategoryTestMessage(t, ctx, db, tenant, tenant.inbox, 803, "later@example.test", "")
	if err := db.MarkMessageReadForUser(ctx, tenant.user.ID, read.ID, true, false); err != nil {
		t.Fatal(err)
	}
	server := &Server{store: db, masterKey: []byte("12345678901234567890123456789012")}

	categories, pending, err := server.mailCategoryChrome(ctx, tenant.user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if pending != 1 {
		t.Fatalf("pending = %d, want 1", pending)
	}
	names := make([]string, 0, len(categories))
	for _, category := range categories {
		names = append(names, category.Name)
		if category.Label == "" || category.Icon == "" {
			t.Fatalf("category %q has no display text: %+v", category.Name, category)
		}
		if category.Name != mailparse.CategoryNewsletters {
			continue
		}
		if category.Total != 2 || category.Unread != 1 {
			t.Fatalf("newsletters counts = total %d unread %d, want 2/1", category.Total, category.Unread)
		}
	}
	// Every category is listed even when empty, so the sidebar does not grow a
	// new entry the first time a message happens to land in one.
	if !slices.Equal(names, mailparse.Categories()) {
		t.Fatalf("category names = %v, want %v", names, mailparse.Categories())
	}
}

func categoryRequest(t *testing.T, server *Server, user store.User, body string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/api/mail/category", bytes.NewReader([]byte(body)))
	request = request.WithContext(context.WithValue(request.Context(), userContextKey, currentUser{User: user}))
	const csrfBase = "message-category-csrf"
	request.AddCookie(&http.Cookie{Name: csrfCookie, Value: csrfBase})
	request.Header.Set("X-CSRF-Token", server.csrfForBase(csrfBase))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	server.apiMessageCategory(response, request)
	return response
}

// A correction has to be aimed at a message the caller can see. Accepting an
// address from the request body instead would let overrides be minted for
// senders the tenant has never received mail from.
func TestCategoryCorrectionOnlyFilesSendersTheCallerCanSee(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(filepath.Join(t.TempDir(), "rolltop.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	tenant := newScopeTestTenant(t, ctx, db, "correction@example.test")
	other := newScopeTestTenant(t, ctx, db, "correction-other@example.test")
	message := createCategoryTestMessage(t, ctx, db, tenant, tenant.inbox, 901, "news@example.test", mailparse.CategoryNewsletters)
	strangers := createCategoryTestMessage(t, ctx, db, other, other.inbox, 902, "stranger@example.test", mailparse.CategoryRelevant)
	server := &Server{store: db, masterKey: bytes.Repeat([]byte{9}, 32), events: newEventHub(), mailListCache: newMailListCache(),
		mailCategoryCache: newMailCategoryChromeCache()}

	if response := categoryRequest(t, server, tenant.user, `{"category":"forums"}`); response.Code != http.StatusBadRequest {
		t.Fatalf("naming no message = %d, want 400; body %s", response.Code, response.Body.String())
	}
	if response := categoryRequest(t, server, tenant.user, `{"message_id":`+itoa(strangers.ID)+`,"category":"forums"}`); response.Code != http.StatusNotFound {
		t.Fatalf("another tenant's message = %d, want 404", response.Code)
	}
	if response := categoryRequest(t, server, tenant.user, `{"message_id":`+itoa(message.ID)+`,"category":"everything"}`); response.Code != http.StatusBadRequest {
		t.Fatalf("unknown category = %d, want 400", response.Code)
	}

	response := categoryRequest(t, server, tenant.user, `{"message_id":`+itoa(message.ID)+`,"category":"forums"}`)
	if response.Code != http.StatusOK {
		t.Fatalf("filing the sender = %d, body %s", response.Code, response.Body.String())
	}
	var result struct {
		Category string `json:"category"`
		Sender   string `json:"sender"`
		Moved    int64  `json:"moved"`
	}
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if result.Category != mailparse.CategoryForums || result.Sender != "news@example.test" || result.Moved != 1 {
		t.Fatalf("result = %+v", result)
	}
	if pinned, err := db.SenderCategoryOverride(ctx, tenant.user.ID, "news@example.test"); err != nil || pinned != mailparse.CategoryForums {
		t.Fatalf("stored override = %q err=%v", pinned, err)
	}
	if pinned, err := db.SenderCategoryOverride(ctx, other.user.ID, "news@example.test"); err != nil || pinned != "" {
		t.Fatalf("other tenant override = %q err=%v, want empty", pinned, err)
	}
}

func itoa(v int64) string {
	return strconv.FormatInt(v, 10)
}
