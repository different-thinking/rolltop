package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"rolltop/backend/search"
	"rolltop/backend/store"
)

type scopeTestTenant struct {
	user      store.User
	accountID int64
	inbox     store.Mailbox
	trash     store.Mailbox
}

func newScopeTestTenant(t *testing.T, ctx context.Context, db *store.Store, email string) scopeTestTenant {
	t.Helper()
	user, err := db.CreateUser(ctx, email, "Scope", "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	account, err := db.UpsertMailAccount(ctx, store.MailAccount{
		UserID: user.ID, Email: email, Host: "imap.example.test", Port: 993,
		Username: email, EncryptedPassword: "encrypted", UseTLS: true, Mailbox: "INBOX",
	})
	if err != nil {
		t.Fatal(err)
	}
	inbox, err := db.GetOrCreateMailbox(ctx, user.ID, account.ID, "INBOX")
	if err != nil {
		t.Fatal(err)
	}
	trash, err := db.GetOrCreateMailboxWithRole(ctx, user.ID, account.ID, "Trash", "trash")
	if err != nil {
		t.Fatal(err)
	}
	if trash.Role != "trash" {
		t.Fatalf("trash role = %q, want trash", trash.Role)
	}
	if trash.ShowInAllMail {
		t.Fatal("trash mailbox should stay out of All Mail")
	}
	return scopeTestTenant{user: user, accountID: account.ID, inbox: inbox, trash: trash}
}

func createScopeTestMessage(t *testing.T, ctx context.Context, db *store.Store, tenant scopeTestTenant, mailbox store.Mailbox, uid uint32, subject string) store.MessageRecord {
	t.Helper()
	if subject == "" {
		subject = fmt.Sprintf("Scope %d", uid)
	}
	blob, err := db.CreateBlob(ctx, store.BlobRecord{
		UserID: tenant.user.ID, Kind: "message",
		Path:   fmt.Sprintf("users/%d/scope-%d.eml", tenant.user.ID, uid),
		SHA256: fmt.Sprintf("scope-%d-%d", tenant.user.ID, uid), Size: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	message, err := db.CreateMessage(ctx, store.CreateMessage{
		UserID: tenant.user.ID, AccountID: tenant.accountID, MailboxID: mailbox.ID, BlobID: blob.ID,
		FromAddr: "sender@example.test", Subject: subject,
		Date: time.Now().UTC(), InternalDate: time.Now().UTC(), UID: uid, BlobPath: blob.Path,
	})
	if err != nil {
		t.Fatal(err)
	}
	return message
}

func planMessageIDs(plan scopeTrashPlan) []int64 {
	ids := make([]int64, 0, 8)
	for _, group := range plan.Groups {
		ids = append(ids, group.MessageIDs...)
	}
	return ids
}

func TestScopeTrashPlanCoversAllMailAndSkipsTrash(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(filepath.Join(t.TempDir(), "rolltop.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	tenant := newScopeTestTenant(t, ctx, db, "scope-owner@example.test")
	first := createScopeTestMessage(t, ctx, db, tenant, tenant.inbox, 501, "")
	second := createScopeTestMessage(t, ctx, db, tenant, tenant.inbox, 502, "")
	trashed := createScopeTestMessage(t, ctx, db, tenant, tenant.trash, 503, "")
	snoozed := createScopeTestMessage(t, ctx, db, tenant, tenant.inbox, 504, "")
	if _, err := db.SnoozeMessage(ctx, tenant.user.ID, snoozed.ID, time.Now().UTC().Add(24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	server := &Server{store: db, masterKey: []byte("12345678901234567890123456789012")}

	plan, err := server.scopeTrashPlan(ctx, tenant.user, scopeSelection{})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Matched != 2 {
		t.Fatalf("matched = %d, want the two All Mail messages", plan.Matched)
	}
	if plan.Truncated {
		t.Fatal("plan reported truncation for two messages")
	}
	if len(plan.Groups) != 1 {
		t.Fatalf("groups = %d, want one account group", len(plan.Groups))
	}
	group := plan.Groups[0]
	if group.Target.ID != tenant.trash.ID {
		t.Fatalf("target mailbox = %d, want trash %d", group.Target.ID, tenant.trash.ID)
	}
	ids := planMessageIDs(plan)
	if len(ids) != 2 || !slices.Contains(ids, first.ID) || !slices.Contains(ids, second.ID) {
		t.Fatalf("planned ids = %v, want %d and %d", ids, first.ID, second.ID)
	}
	if slices.Contains(ids, trashed.ID) {
		t.Fatalf("planned ids = %v, must not include the already trashed message %d", ids, trashed.ID)
	}
	if slices.Contains(ids, snoozed.ID) {
		t.Fatalf("planned ids = %v, must not include the snoozed message %d", ids, snoozed.ID)
	}
	if !slices.Contains(group.RefreshMailboxes, tenant.inbox.Name) || !slices.Contains(group.RefreshMailboxes, tenant.trash.Name) {
		t.Fatalf("refresh mailboxes = %v, want source and destination", group.RefreshMailboxes)
	}

	// Viewing Trash itself selects its own rows, and each of them is already home.
	trashPlan, err := server.scopeTrashPlan(ctx, tenant.user, scopeSelection{MailboxID: tenant.trash.ID})
	if err != nil {
		t.Fatal(err)
	}
	if trashPlan.Matched != 1 || trashPlan.Skipped != 1 || len(trashPlan.Groups) != 0 {
		t.Fatalf("trash scope = matched %d skipped %d groups %d, want 1/1/0",
			trashPlan.Matched, trashPlan.Skipped, len(trashPlan.Groups))
	}
}

func TestScopeTrashPlanResolvesSearchFilter(t *testing.T) {
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
	tenant := newScopeTestTenant(t, ctx, db, "scope-search@example.test")
	other := newScopeTestTenant(t, ctx, db, "scope-search-other@example.test")
	matching := createScopeTestMessage(t, ctx, db, tenant, tenant.inbox, 901, "Newsletter weekly digest")
	unrelated := createScopeTestMessage(t, ctx, db, tenant, tenant.inbox, 902, "Invoice for March")
	otherTenantMatch := createScopeTestMessage(t, ctx, db, other, other.inbox, 903, "Newsletter weekly digest")
	for _, item := range []struct {
		user    store.User
		message store.MessageRecord
	}{{tenant.user, matching}, {tenant.user, unrelated}, {other.user, otherTenantMatch}} {
		stored, err := db.GetMessageForUser(ctx, item.user.ID, item.message.ID)
		if err != nil {
			t.Fatal(err)
		}
		if err := searchService.IndexMessage(ctx, stored, nil); err != nil {
			t.Fatal(err)
		}
	}
	server := &Server{store: db, search: searchService, masterKey: []byte("12345678901234567890123456789012")}

	plan, err := server.scopeTrashPlan(ctx, tenant.user, scopeSelection{Query: "newsletter"})
	if err != nil {
		t.Fatal(err)
	}
	ids := planMessageIDs(plan)
	if len(ids) != 1 || ids[0] != matching.ID {
		t.Fatalf("planned ids = %v, want only the matching message %d", ids, matching.ID)
	}
	if slices.Contains(ids, otherTenantMatch.ID) {
		t.Fatalf("planned ids = %v, must not cross tenants", ids)
	}

	// An in: filter narrows the same query, and a folder without matches yields none.
	narrowed, err := server.scopeTrashPlan(ctx, tenant.user, scopeSelection{Query: "newsletter in:Trash"})
	if err != nil {
		t.Fatal(err)
	}
	if narrowed.Matched != 0 || len(narrowed.Groups) != 0 {
		t.Fatalf("in:Trash scope = matched %d groups %d, want nothing", narrowed.Matched, len(narrowed.Groups))
	}
}

func TestScopeTrashPlanIsUserScoped(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(filepath.Join(t.TempDir(), "rolltop.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	owner := newScopeTestTenant(t, ctx, db, "scope-tenant-owner@example.test")
	other := newScopeTestTenant(t, ctx, db, "scope-tenant-other@example.test")
	ownerMessage := createScopeTestMessage(t, ctx, db, owner, owner.inbox, 601, "")
	otherMessage := createScopeTestMessage(t, ctx, db, other, other.inbox, 701, "")
	server := &Server{store: db, masterKey: []byte("12345678901234567890123456789012")}

	plan, err := server.scopeTrashPlan(ctx, owner.user, scopeSelection{})
	if err != nil {
		t.Fatal(err)
	}
	ids := planMessageIDs(plan)
	if len(ids) != 1 || ids[0] != ownerMessage.ID {
		t.Fatalf("planned ids = %v, want only the owner message %d", ids, ownerMessage.ID)
	}
	for _, group := range plan.Groups {
		if group.Target.ID == other.trash.ID {
			t.Fatal("owner plan targets another tenant's trash mailbox")
		}
	}

	// Another tenant's mailbox ID must not resolve into this tenant's plan.
	if _, err := server.scopeTrashPlan(ctx, owner.user, scopeSelection{MailboxID: other.inbox.ID}); !store.IsNotFound(err) {
		t.Fatalf("cross-tenant mailbox scope error = %v, want not found", err)
	}
	unchanged, err := db.GetMessageForUser(ctx, other.user.ID, otherMessage.ID)
	if err != nil {
		t.Fatal(err)
	}
	if unchanged.MailboxID != other.inbox.ID {
		t.Fatalf("other tenant message mailbox = %d, want %d", unchanged.MailboxID, other.inbox.ID)
	}
}

func TestScopeTrashRequiresTrashMailbox(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(filepath.Join(t.TempDir(), "rolltop.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	user, err := db.CreateUser(ctx, "scope-no-trash@example.test", "Scope", "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	account, err := db.UpsertMailAccount(ctx, store.MailAccount{
		UserID: user.ID, Email: user.Email, Host: "imap.example.test", Port: 993,
		Username: user.Email, EncryptedPassword: "encrypted", UseTLS: true, Mailbox: "INBOX",
	})
	if err != nil {
		t.Fatal(err)
	}
	inbox, err := db.GetOrCreateMailbox(ctx, user.ID, account.ID, "INBOX")
	if err != nil {
		t.Fatal(err)
	}
	tenant := scopeTestTenant{user: user, accountID: account.ID, inbox: inbox}
	createScopeTestMessage(t, ctx, db, tenant, inbox, 801, "")
	server := &Server{store: db, masterKey: []byte("12345678901234567890123456789012")}

	_, err = server.scopeTrashPlan(ctx, user, scopeSelection{})
	var missingTrash missingTrashMailboxError
	if !errors.As(err, &missingTrash) {
		t.Fatalf("error = %v, want a missing trash mailbox error", err)
	}

	payload, err := json.Marshal(map[string]any{"scope_mailbox_id": 0, "scope_query": ""})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/messages/scope-trash", bytes.NewReader(payload))
	req = req.WithContext(context.WithValue(req.Context(), userContextKey, currentUser{User: user}))
	csrfBase := "scope-trash-csrf"
	req.AddCookie(&http.Cookie{Name: csrfCookie, Value: csrfBase})
	req.Header.Set("X-CSRF-Token", server.csrfForBase(csrfBase))
	res := httptest.NewRecorder()
	// Without a configured syncer the handler must refuse before resolving, so a
	// missing IMAP setup can never look like a completed delete.
	server.apiScopeTrashMessages(res, req)
	if res.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d body=%s, want 503 without a syncer", res.Code, res.Body.String())
	}
}

func TestScopeTrashRejectsNonPost(t *testing.T) {
	server := &Server{}
	req := httptest.NewRequest(http.MethodGet, "/api/messages/scope-trash", nil)
	res := httptest.NewRecorder()
	server.apiScopeTrashMessages(res, req)
	if res.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", res.Code)
	}
}
