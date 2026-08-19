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
	"rolltop/backend/store/storetest"
	"rolltop/backend/syncer"
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
	return createScopeTestMessageDated(t, ctx, db, tenant, mailbox, uid, subject, time.Now().UTC())
}

func createScopeTestMessageDated(t *testing.T, ctx context.Context, db *store.Store, tenant scopeTestTenant, mailbox store.Mailbox, uid uint32, subject string, date time.Time) store.MessageRecord {
	t.Helper()
	return createScopeTestMessageFrom(t, ctx, db, tenant, mailbox, uid, subject, "sender@example.test", "", date)
}

// createScopeTestMessageFrom is the same fixture with the sender and category a
// test cares about. Both are properties of the stored row rather than of the
// scope machinery, so they stay optional for the tests that do not set them.
func createScopeTestMessageFrom(t *testing.T, ctx context.Context, db *store.Store, tenant scopeTestTenant,
	mailbox store.Mailbox, uid uint32, subject, from, category string, date time.Time,
) store.MessageRecord {
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
		FromAddr: from, Subject: subject, Category: category,
		Date: date, InternalDate: date, UID: uid, BlobPath: blob.Path,
	})
	if err != nil {
		t.Fatal(err)
	}
	return message
}

func planMessageIDs(plan scopeMovePlan) []int64 {
	ids := make([]int64, 0, 8)
	for _, group := range plan.Groups {
		ids = append(ids, group.MessageIDs...)
	}
	return ids
}

func TestScopeTrashPlanCoversAllMailAndSkipsTrash(t *testing.T) {
	ctx := context.Background()
	db, err := storetest.Open(t)
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

func TestScopeTrashPlanUnarchivedSkipsArchiveFolder(t *testing.T) {
	ctx := context.Background()
	db, err := storetest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	tenant := newScopeTestTenant(t, ctx, db, "scope-unarchived@example.test")
	archive, err := db.GetOrCreateMailbox(ctx, tenant.user.ID, tenant.accountID, "Archive")
	if err != nil {
		t.Fatal(err)
	}
	kept := createScopeTestMessage(t, ctx, db, tenant, tenant.inbox, 601, "")
	archived := createScopeTestMessage(t, ctx, db, tenant, archive, 602, "")
	if _, err := db.SaveSwipePreferences(ctx, store.SwipePreferences{
		UserID:     tenant.user.ID,
		LeftAction: store.SwipeActionSnooze, LeftSnoozePreset: store.SwipeSnoozeTomorrow,
		RightAction: store.SwipeActionMarkRead, RightSnoozePreset: store.SwipeSnoozeTomorrow,
		ArchiveMailboxes: []store.SwipeArchiveMailbox{{AccountID: tenant.accountID, MailboxID: archive.ID}},
	}); err != nil {
		t.Fatal(err)
	}
	server := &Server{store: db, masterKey: []byte("12345678901234567890123456789012")}

	// The Unarchived list's whole-view delete must never reach archived mail.
	plan, err := server.scopeTrashPlan(ctx, tenant.user, scopeSelection{View: mailViewInbox})
	if err != nil {
		t.Fatal(err)
	}
	ids := planMessageIDs(plan)
	if plan.Matched != 1 || len(ids) != 1 || ids[0] != kept.ID {
		t.Fatalf("unarchived plan = matched %d ids %v, want only the inbox message %d", plan.Matched, ids, kept.ID)
	}

	// The plain All Mail scope still covers the Archive folder's messages.
	allPlan, err := server.scopeTrashPlan(ctx, tenant.user, scopeSelection{})
	if err != nil {
		t.Fatal(err)
	}
	allIDs := planMessageIDs(allPlan)
	if allPlan.Matched != 2 || !slices.Contains(allIDs, archived.ID) {
		t.Fatalf("all mail plan = matched %d ids %v, want both messages including %d", allPlan.Matched, allIDs, archived.ID)
	}
}

func TestScopeTrashPlanRoleViewsCoverOnlyTheirRoleFolders(t *testing.T) {
	ctx := context.Background()
	db, err := storetest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	tenant := newScopeTestTenant(t, ctx, db, "scope-roles@example.test")
	sent, err := db.GetOrCreateMailbox(ctx, tenant.user.ID, tenant.accountID, "Sent")
	if err != nil {
		t.Fatal(err)
	}
	drafts, err := db.GetOrCreateMailbox(ctx, tenant.user.ID, tenant.accountID, "Drafts")
	if err != nil {
		t.Fatal(err)
	}
	if sent.Role != "sent" || drafts.Role != "drafts" {
		t.Fatalf("roles = %q/%q, want sent/drafts", sent.Role, drafts.Role)
	}
	received := createScopeTestMessage(t, ctx, db, tenant, tenant.inbox, 701, "")
	sentMessage := createScopeTestMessage(t, ctx, db, tenant, sent, 702, "")
	draftMessage := createScopeTestMessage(t, ctx, db, tenant, drafts, 703, "")

	for _, tt := range []struct {
		name string
		view mailView
		want int64
	}{
		{name: "sent", view: mailViewSent, want: sentMessage.ID},
		{name: "drafts", view: mailViewDrafts, want: draftMessage.ID},
	} {
		t.Run(tt.name, func(t *testing.T) {
			server := &Server{store: db, masterKey: []byte("12345678901234567890123456789012")}
			plan, err := server.scopeTrashPlan(ctx, tenant.user, scopeSelection{View: tt.view})
			if err != nil {
				t.Fatal(err)
			}
			ids := planMessageIDs(plan)
			if plan.Matched != 1 || len(ids) != 1 || ids[0] != tt.want {
				t.Fatalf("%s plan = matched %d ids %v, want only %d; the inbox message was %d",
					tt.name, plan.Matched, ids, tt.want, received.ID)
			}
		})
	}
}

func TestScopeTrashPlanResolvesSearchFilter(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db, err := storetest.Open(t)
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

// A search scope has to page the whole hit list. One batch is all a plain
// "ask for everything" would ever get, which would quietly cap a whole-filter
// delete at one page worth of messages.
func TestScopeTrashPlanPagesBeyondOneSearchBatch(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db, err := storetest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	searchService, err := search.Open(filepath.Join(dir, "bleve"))
	if err != nil {
		t.Fatal(err)
	}
	defer searchService.Close()
	tenant := newScopeTestTenant(t, ctx, db, "scope-search-paging@example.test")
	const total = scopeSearchHitBatch + 30
	documents := make([]search.MessageIndexDocument, 0, total)
	for i := 0; i < total; i++ {
		message := createScopeTestMessage(t, ctx, db, tenant, tenant.inbox, uint32(2000+i), "Newsletter weekly digest")
		stored, err := db.GetMessageForUser(ctx, tenant.user.ID, message.ID)
		if err != nil {
			t.Fatal(err)
		}
		documents = append(documents, search.MessageIndexDocument{Message: stored})
	}
	if err := searchService.IndexMessages(ctx, documents); err != nil {
		t.Fatal(err)
	}
	server := &Server{store: db, search: searchService, masterKey: []byte("12345678901234567890123456789012")}

	plan, err := server.scopeTrashPlan(ctx, tenant.user, scopeSelection{Query: "newsletter"})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Matched != total {
		t.Fatalf("matched = %d, want all %d matches across batches", plan.Matched, total)
	}
	if plan.Truncated {
		t.Fatalf("plan reported truncation for %d matches", total)
	}
	if ids := planMessageIDs(plan); len(ids) != total {
		t.Fatalf("planned ids = %d, want %d", len(ids), total)
	}
}

// A filter can span accounts, and Trash belongs to an account: that is the path
// that produces more than one group, and one background run per account.
func TestScopeTrashPlanGroupsPerAccount(t *testing.T) {
	ctx := context.Background()
	db, err := storetest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	tenant := newScopeTestTenant(t, ctx, db, "scope-multi@example.test")
	second, err := db.UpsertMailAccount(ctx, store.MailAccount{
		UserID: tenant.user.ID, Email: "scope-multi-second@example.test", Host: "imap.example.test", Port: 993,
		Username: "scope-multi-second@example.test", EncryptedPassword: "encrypted", UseTLS: true, Mailbox: "INBOX",
	})
	if err != nil {
		t.Fatal(err)
	}
	secondInbox, err := db.GetOrCreateMailbox(ctx, tenant.user.ID, second.ID, "Second/INBOX")
	if err != nil {
		t.Fatal(err)
	}
	secondTrash, err := db.GetOrCreateMailboxWithRole(ctx, tenant.user.ID, second.ID, "Second/Trash", "trash")
	if err != nil {
		t.Fatal(err)
	}
	secondTenant := scopeTestTenant{user: tenant.user, accountID: second.ID, inbox: secondInbox, trash: secondTrash}
	first := createScopeTestMessage(t, ctx, db, tenant, tenant.inbox, 1101, "")
	other := createScopeTestMessage(t, ctx, db, secondTenant, secondInbox, 1102, "")
	server := &Server{store: db, masterKey: []byte("12345678901234567890123456789012")}

	plan, err := server.scopeTrashPlan(ctx, tenant.user, scopeSelection{})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Groups) != 2 {
		t.Fatalf("groups = %d, want one per account", len(plan.Groups))
	}
	if plan.Groups[0].AccountID > plan.Groups[1].AccountID {
		t.Fatalf("groups ordered %d then %d, want ascending account IDs",
			plan.Groups[0].AccountID, plan.Groups[1].AccountID)
	}
	byAccount := map[int64]scopeMoveGroup{}
	for _, group := range plan.Groups {
		byAccount[group.AccountID] = group
	}
	if got := byAccount[tenant.accountID]; got.Target.ID != tenant.trash.ID || !slices.Contains(got.MessageIDs, first.ID) {
		t.Fatalf("first account group = target %d ids %v", got.Target.ID, got.MessageIDs)
	}
	if got := byAccount[second.ID]; got.Target.ID != secondTrash.ID || !slices.Contains(got.MessageIDs, other.ID) {
		t.Fatalf("second account group = target %d ids %v", got.Target.ID, got.MessageIDs)
	}
	// Each account's messages go to its own Trash and nowhere else.
	if slices.Contains(byAccount[tenant.accountID].MessageIDs, other.ID) ||
		slices.Contains(byAccount[second.ID].MessageIDs, first.ID) {
		t.Fatal("messages were grouped under the wrong account")
	}
}

// A filter larger than one pass has to say so, because the UI offers another
// pass rather than pretending the whole filter was handled.
func TestScopeTrashPlanReportsTruncation(t *testing.T) {
	ctx := context.Background()
	db, err := storetest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	tenant := newScopeTestTenant(t, ctx, db, "scope-truncate@example.test")
	for i := 0; i < 5; i++ {
		createScopeTestMessage(t, ctx, db, tenant, tenant.inbox, uint32(1200+i), "")
	}
	server := &Server{store: db, masterKey: []byte("12345678901234567890123456789012")}

	overFetched, err := db.ListAllMailScopeMessagesForUser(ctx, tenant.user.ID, store.ScopeFilter{}, 4)
	if err != nil {
		t.Fatal(err)
	}
	messages, truncated, err := trimScopeMessages(overFetched, 3, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !truncated || len(messages) != 3 {
		t.Fatalf("trim = %d messages truncated %t, want 3/true", len(messages), truncated)
	}

	// The full pass over the same folder fits, so nothing is reported as cut off.
	plan, err := server.scopeTrashPlan(ctx, tenant.user, scopeSelection{MailboxID: tenant.inbox.ID})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Truncated || plan.Matched != 5 {
		t.Fatalf("plan = matched %d truncated %t, want 5/false", plan.Matched, plan.Truncated)
	}
}

func TestScopeTrashPlanIsUserScoped(t *testing.T) {
	ctx := context.Background()
	db, err := storetest.Open(t)
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
	db, err := storetest.Open(t)
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
	var missingTrash missingScopeTargetError
	if !errors.As(err, &missingTrash) || missingTrash.folder != "Trash" {
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

func TestScopeArchivePlanMovesOnlyMailOlderThanTheCutoff(t *testing.T) {
	ctx := context.Background()
	db, err := storetest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	tenant := newScopeTestTenant(t, ctx, db, "scope-archive@example.test")
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
	cutoff := time.Date(2024, time.March, 1, 0, 0, 0, 0, time.UTC)
	old := createScopeTestMessageDated(t, ctx, db, tenant, tenant.inbox, 901, "Old", cutoff.Add(-72*time.Hour))
	// A message stamped on the cutoff day is not older than it, so it stays.
	sameDay := createScopeTestMessageDated(t, ctx, db, tenant, tenant.inbox, 902, "Same day", cutoff.Add(6*time.Hour))
	recent := createScopeTestMessageDated(t, ctx, db, tenant, tenant.inbox, 903, "Recent", cutoff.Add(90*24*time.Hour))
	alreadyArchived := createScopeTestMessageDated(t, ctx, db, tenant, archive, 904, "Archived", cutoff.Add(-96*time.Hour))
	server := &Server{store: db, masterKey: []byte("12345678901234567890123456789012")}

	plan, err := server.scopeArchivePlan(ctx, tenant.user, scopeSelection{Filter: store.ScopeFilter{Before: cutoff}})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Groups) != 1 {
		t.Fatalf("groups = %d, want one account group", len(plan.Groups))
	}
	if plan.Groups[0].Target.ID != archive.ID {
		t.Fatalf("target mailbox = %d, want the archive folder %d", plan.Groups[0].Target.ID, archive.ID)
	}
	ids := planMessageIDs(plan)
	if len(ids) != 1 || ids[0] != old.ID {
		t.Fatalf("planned ids = %v, want only the message older than the cutoff %d", ids, old.ID)
	}
	if slices.Contains(ids, sameDay.ID) || slices.Contains(ids, recent.ID) {
		t.Fatalf("planned ids = %v, must leave mail from the cutoff day onwards alone", ids)
	}
	if slices.Contains(ids, alreadyArchived.ID) {
		t.Fatalf("planned ids = %v, must not move already archived mail %d", ids, alreadyArchived.ID)
	}
	if plan.Skipped != 1 {
		t.Fatalf("skipped = %d, want the message already in Archive", plan.Skipped)
	}
}

// A whole-account list shows Sent, Drafts, Trash, and Junk, but archiving a
// received backlog must not empty them.
func TestScopeArchiveLeavesTheUsersOwnFoldersAlone(t *testing.T) {
	ctx := context.Background()
	db, err := storetest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	tenant := newScopeTestTenant(t, ctx, db, "scope-archive-sent@example.test")
	archive, err := db.GetOrCreateMailbox(ctx, tenant.user.ID, tenant.accountID, "Archive")
	if err != nil {
		t.Fatal(err)
	}
	sent, err := db.GetOrCreateMailboxWithRole(ctx, tenant.user.ID, tenant.accountID, "Sent", "sent")
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
	cutoff := time.Date(2024, time.June, 1, 0, 0, 0, 0, time.UTC)
	received := createScopeTestMessageDated(t, ctx, db, tenant, tenant.inbox, 921, "Received", cutoff.Add(-48*time.Hour))
	ownMail := createScopeTestMessageDated(t, ctx, db, tenant, sent, 922, "Sent", cutoff.Add(-48*time.Hour))
	server := &Server{store: db, masterKey: []byte("12345678901234567890123456789012")}
	protected, err := server.archiveProtectedMailboxIDs(ctx, tenant.user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(protected, sent.ID) || !slices.Contains(protected, tenant.trash.ID) {
		t.Fatalf("protected mailboxes = %v, want the Sent and Trash folders %d and %d", protected, sent.ID, tenant.trash.ID)
	}

	plan, err := server.scopeArchivePlan(ctx, tenant.user, scopeSelection{
		Filter: store.ScopeFilter{Before: cutoff, ExcludeMailboxIDs: protected},
	})
	if err != nil {
		t.Fatal(err)
	}
	ids := planMessageIDs(plan)
	if len(ids) != 1 || ids[0] != received.ID {
		t.Fatalf("planned ids = %v, want only the received message %d", ids, received.ID)
	}
	if slices.Contains(ids, ownMail.ID) {
		t.Fatalf("planned ids = %v, must not archive sent mail %d", ids, ownMail.ID)
	}
	if plan.Matched != 1 {
		t.Fatalf("matched = %d, want only the archivable message", plan.Matched)
	}
}

// An archive pass is resolved from the signed-in user's own rows, so another
// tenant's old mail can never enter the plan even when both users have the
// same folder layout.
func TestScopeArchivePlanStaysInsideOneTenant(t *testing.T) {
	ctx := context.Background()
	db, err := storetest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	owner := newScopeTestTenant(t, ctx, db, "scope-archive-owner@example.test")
	other := newScopeTestTenant(t, ctx, db, "scope-archive-other@example.test")
	cutoff := time.Date(2024, time.May, 1, 0, 0, 0, 0, time.UTC)
	for _, tenant := range []scopeTestTenant{owner, other} {
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
	}
	ownerMessage := createScopeTestMessageDated(t, ctx, db, owner, owner.inbox, 931, "Owner", cutoff.Add(-48*time.Hour))
	otherMessage := createScopeTestMessageDated(t, ctx, db, other, other.inbox, 932, "Other", cutoff.Add(-48*time.Hour))
	server := &Server{store: db, masterKey: []byte("12345678901234567890123456789012")}

	plan, err := server.scopeArchivePlan(ctx, owner.user, scopeSelection{Filter: store.ScopeFilter{Before: cutoff}})
	if err != nil {
		t.Fatal(err)
	}
	ids := planMessageIDs(plan)
	if len(ids) != 1 || ids[0] != ownerMessage.ID {
		t.Fatalf("planned ids = %v, want only the owner's message %d", ids, ownerMessage.ID)
	}
	if slices.Contains(ids, otherMessage.ID) {
		t.Fatalf("planned ids = %v, must not reach the other tenant's message %d", ids, otherMessage.ID)
	}
	// A folder the other tenant owns is not a folder this scope can name.
	if _, err := server.scopeArchivePlan(ctx, owner.user, scopeSelection{
		MailboxID: other.inbox.ID, Filter: store.ScopeFilter{Before: cutoff},
	}); !store.IsNotFound(err) {
		t.Fatalf("cross-tenant mailbox scope error = %v, want not found", err)
	}
}

// The folders an archive pass protects cannot be archived by naming them
// directly either, and saying so beats a successful pass that moved nothing.
func TestScopeArchiveRefusesAProtectedFolder(t *testing.T) {
	ctx := context.Background()
	db, err := storetest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	tenant := newScopeTestTenant(t, ctx, db, "scope-archive-protected@example.test")
	server := &Server{store: db, masterKey: []byte("12345678901234567890123456789012"), syncer: &syncer.Service{}}

	payload, err := json.Marshal(map[string]any{"scope_mailbox_id": tenant.trash.ID, "before": "2024-03-01"})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/messages/scope-archive", bytes.NewReader(payload))
	req = req.WithContext(context.WithValue(req.Context(), userContextKey, currentUser{User: tenant.user}))
	csrfBase := "scope-archive-protected-csrf"
	req.AddCookie(&http.Cookie{Name: csrfCookie, Value: csrfBase})
	req.Header.Set("X-CSRF-Token", server.csrfForBase(csrfBase))
	res := httptest.NewRecorder()
	server.apiScopeArchiveMessages(res, req)
	if res.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for archiving the Trash folder", res.Code)
	}
}

func TestScopeArchivePlanReportsAccountsWithoutAnArchiveFolder(t *testing.T) {
	ctx := context.Background()
	db, err := storetest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	tenant := newScopeTestTenant(t, ctx, db, "scope-archive-missing@example.test")
	cutoff := time.Now().UTC()
	createScopeTestMessageDated(t, ctx, db, tenant, tenant.inbox, 911, "Old", cutoff.Add(-72*time.Hour))
	server := &Server{store: db, masterKey: []byte("12345678901234567890123456789012")}

	_, err = server.scopeArchivePlan(ctx, tenant.user, scopeSelection{Filter: store.ScopeFilter{Before: cutoff}})
	var missingTarget missingScopeTargetError
	if !errors.As(err, &missingTarget) {
		t.Fatalf("error = %v, want a missing archive folder error", err)
	}
	if missingTarget.folder != "Archive" {
		t.Fatalf("missing folder = %q, want Archive", missingTarget.folder)
	}
}

func TestScopeArchiveRequestRequiresACutoff(t *testing.T) {
	ctx := context.Background()
	db, err := storetest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	tenant := newScopeTestTenant(t, ctx, db, "scope-archive-cutoff@example.test")
	server := &Server{store: db, masterKey: []byte("12345678901234567890123456789012"), syncer: &syncer.Service{}}

	payload, err := json.Marshal(map[string]any{"scope_mailbox_id": 0, "scope_query": "", "before": ""})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/messages/scope-archive", bytes.NewReader(payload))
	req = req.WithContext(context.WithValue(req.Context(), userContextKey, currentUser{User: tenant.user}))
	csrfBase := "scope-archive-csrf"
	req.AddCookie(&http.Cookie{Name: csrfCookie, Value: csrfBase})
	req.Header.Set("X-CSRF-Token", server.csrfForBase(csrfBase))
	res := httptest.NewRecorder()
	server.apiScopeArchiveMessages(res, req)
	if res.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for an archive without a cutoff", res.Code)
	}
}

func TestParseScopeCutoffKeepsTheNamedDay(t *testing.T) {
	day, err := parseScopeCutoff("2024-03-01")
	if err != nil {
		t.Fatal(err)
	}
	if !day.Equal(time.Date(2024, time.March, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("cutoff = %s, want the start of 1 March 2024 UTC", day)
	}
	moment, err := parseScopeCutoff("2024-03-01T12:30:00Z")
	if err != nil {
		t.Fatal(err)
	}
	if !moment.Equal(time.Date(2024, time.March, 1, 12, 30, 0, 0, time.UTC)) {
		t.Fatalf("cutoff = %s, want the exact timestamp", moment)
	}
	if empty, err := parseScopeCutoff("  "); err != nil || !empty.IsZero() {
		t.Fatalf("empty cutoff = %s, %v, want the zero time and no error", empty, err)
	}
	if _, err := parseScopeCutoff("last tuesday"); err == nil {
		t.Fatal("unparsable cutoff was accepted")
	}
	// The browser sends the instant its own calendar day begins at, which is what
	// keeps that day whole for a reader who is not on UTC.
	local, err := parseScopeCutoff("2024-02-29T13:00:00+13:00")
	if err != nil {
		t.Fatal(err)
	}
	if !local.Equal(time.Date(2024, time.February, 29, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("cutoff = %s, want the offset resolved to 29 February 00:00 UTC", local)
	}
}
