package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"rolltop/backend/plugins"
	"rolltop/backend/store"
	"rolltop/backend/store/storetest"
	"rolltop/plugins/recipe_box/recipe"
)

// openRecipeStore opens a store carrying this plugin's own migrations; the
// shared baseline template predates them.
func openRecipeStore(t *testing.T) *store.Store {
	t.Helper()
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate the recipe box test source")
	}
	manifests, err := plugins.LoadManifests(filepath.Clean(filepath.Join(filepath.Dir(currentFile), "..", "..")))
	if err != nil {
		t.Fatal(err)
	}
	var selected []plugins.Manifest
	for _, manifest := range manifests {
		if manifest.ID == pluginID {
			selected = append(selected, manifest)
		}
	}
	if len(selected) != 1 {
		t.Fatalf("recipe box manifests = %d, want 1", len(selected))
	}
	st, err := storetest.OpenWithManifests(t, selected)
	if err != nil {
		t.Fatalf("open store through the plugin's own migrations: %v", err)
	}
	return st
}

// fakeHost stands in for the web host. Its move does what the real one does
// to the local rows: the source row is gone, and the destination's sync stores
// the message again under a new id.
type fakeHost struct {
	t       *testing.T
	store   *store.Store
	userID  int64
	moved   []int64
	created []string
	moveErr error
}

func (h *fakeHost) Store() any                                 { return h.store }
func (h *fakeHost) MasterKey() []byte                          { return nil }
func (h *fakeHost) PluginEnabled(context.Context, string) bool { return true }
func (h *fakeHost) MatchMessageSearch(context.Context, int64, int64, string) (plugins.SearchMatchResult, error) {
	return plugins.SearchMatchResult{}, nil
}
func (h *fakeHost) StarMessage(context.Context, int64, int64, bool) error { return nil }
func (h *fakeHost) MarkMessageRead(context.Context, int64, int64, bool) (bool, error) {
	return true, nil
}
func (h *fakeHost) ForwardMessage(context.Context, int64, int64, string, []plugins.MailHeader) error {
	return nil
}
func (h *fakeHost) ArchiveMailboxID(context.Context, int64, int64) (int64, error) { return 0, nil }
func (h *fakeHost) NotifyUserChanged(int64)                                       {}

func (h *fakeHost) MoveMessage(ctx context.Context, userID, messageID, destID int64) error {
	if h.moveErr != nil {
		return h.moveErr
	}
	db := h.store.DB()
	var accountID int64
	var header, subject, from, to, body, sha string
	var date int64
	if err := db.QueryRowContext(ctx, `SELECT account_id, message_id_header, subject, from_addr, to_addr, body_text, date_unix, canonical_sha256 FROM messages WHERE user_id = ? AND id = ?`,
		userID, messageID).Scan(&accountID, &header, &subject, &from, &to, &body, &date, &sha); err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM messages WHERE user_id = ? AND id = ?`, userID, messageID); err != nil {
		return err
	}
	h.moved = append(h.moved, messageID)
	createMessageWithSHA(h.t, h.store, userID, accountID, destID, header, sha, from, to, subject, body, time.Unix(date, 0))
	return nil
}

func (h *fakeHost) EnsureMailFolder(ctx context.Context, userID, accountID int64, name string) (int64, bool, error) {
	mb, err := h.store.GetOrCreateMailbox(ctx, userID, accountID, name)
	if err != nil {
		return 0, false, err
	}
	h.created = append(h.created, name)
	return mb.ID, true, nil
}

// The API half: the signed-in user is whoever the test says it is.
func (h *fakeHost) RequireAPIAuth(http.ResponseWriter, *http.Request) (plugins.CurrentUser, bool) {
	return plugins.CurrentUser{UserID: h.userID}, true
}
func (h *fakeHost) LoginUserID(http.ResponseWriter, *http.Request, int64) error { return nil }
func (h *fakeHost) VerifyCSRF(http.ResponseWriter, *http.Request) bool          { return true }
func (h *fakeHost) DecodeJSON(w http.ResponseWriter, r *http.Request, dest any) bool {
	if err := json.NewDecoder(r.Body).Decode(dest); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return false
	}
	return true
}
func (h *fakeHost) WriteJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(value)
}
func (h *fakeHost) WriteAPIError(w http.ResponseWriter, status int, message string) {
	http.Error(w, message, status)
}
func (h *fakeHost) ServerError(w http.ResponseWriter, err error) {
	h.t.Errorf("server error: %v", err)
	http.Error(w, err.Error(), http.StatusInternalServerError)
}

var (
	_ plugins.StoredMessageHost = (*fakeHost)(nil)
	_ plugins.MailFolderHost    = (*fakeHost)(nil)
	_ plugins.APIHost           = (*fakeHost)(nil)
)

var messageSeq int

func createMessage(t *testing.T, st *store.Store, userID, accountID, mailboxID int64, header, from, to, subject, body string, date time.Time) store.MessageRecord {
	t.Helper()
	return createMessageWithSHA(t, st, userID, accountID, mailboxID, header, "", from, to, subject, body, date)
}

func createMessageWithSHA(t *testing.T, st *store.Store, userID, accountID, mailboxID int64, header, sha, from, to, subject, body string, date time.Time) store.MessageRecord {
	t.Helper()
	ctx := context.Background()
	messageSeq++
	path := fmt.Sprintf("users/%d/recipe-test/%d.eml", userID, messageSeq)
	blob, err := st.CreateBlob(ctx, store.BlobRecord{UserID: userID, Kind: "message", Path: path, SHA256: fmt.Sprintf("sha-%d", messageSeq), Size: 1})
	if err != nil {
		t.Fatal(err)
	}
	msg, err := st.CreateMessage(ctx, store.CreateMessage{
		UserID: userID, AccountID: accountID, MailboxID: mailboxID, BlobID: blob.ID, MessageIDHeader: header, CanonicalSHA256: sha,
		UID: uint32(messageSeq), BlobPath: path, Subject: subject, FromAddr: from, ToAddr: to, BodyText: body, Date: date,
	})
	if err != nil {
		t.Fatal(err)
	}
	return msg
}

type fixture struct {
	user    store.User
	account store.MailAccount
	inbox   store.Mailbox
	sent    store.Mailbox
}

func newFixture(t *testing.T, st *store.Store, email string) fixture {
	t.Helper()
	ctx := context.Background()
	user, err := st.CreateUser(ctx, email, "Cook", "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	account, err := st.CreateMailAccount(ctx, store.MailAccount{
		UserID: user.ID, Email: email, Host: "imap.example.test", Port: 993,
		Username: email, EncryptedPassword: "secret", UseTLS: true, Mailbox: "*",
	})
	if err != nil {
		t.Fatal(err)
	}
	inbox, err := st.GetOrCreateMailboxWithRole(ctx, user.ID, account.ID, "INBOX", "inbox")
	if err != nil {
		t.Fatal(err)
	}
	sent, err := st.GetOrCreateMailboxWithRole(ctx, user.ID, account.ID, "Sent", "sent")
	if err != nil {
		t.Fatal(err)
	}
	return fixture{user: user, account: account, inbox: inbox, sent: sent}
}

const carbonaraMarkup = `<html><head><meta property="og:site_name" content="Kochseite">
<script type="application/ld+json">{"@type":"Recipe","name":"Spaghetti Carbonara","image":"/carbonara.png",
"recipeIngredient":["400 g Spaghetti","4 Eigelb"],"recipeInstructions":"Kochen.","totalTime":"PT30M","recipeYield":"4 Portionen"}</script>
</head></html>`

func recipeServer(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/carbonara", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(carbonaraMarkup))
	})
	mux.HandleFunc("/article", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<html><head><title>Ein Artikel</title><meta property="og:image" content="/carbonara.png"></head></html>`))
	})
	mux.HandleFunc("/carbonara.png", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("\x89PNG\r\n\x1a\nimage"))
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

func testBackend() *recipeBoxBackend {
	backend := newBackend(recipe.NewFetcher((&net.Dialer{}).DialContext))
	// The tests serve every page from one host and should not wait out the
	// politeness spacing between them.
	backend.hostSpacing = 0
	backend.scanSettle = 0
	return backend
}

// drain runs the reader's turn until it reports nothing more to do, the way
// the loop's busy rounds would.
func drain(t *testing.T, p *recipeBoxBackend, host *fakeHost, userID int64) {
	t.Helper()
	for i := 0; i < 20; i++ {
		more, err := p.runUser(context.Background(), host, userID)
		if err != nil {
			t.Fatal(err)
		}
		if !more {
			return
		}
	}
	t.Fatal("the recipe box never ran out of work")
}

func TestIsNoteToSelf(t *testing.T) {
	own := map[string]bool{"me@example.test": true, "me@work.test": true}
	cases := []struct {
		name, from, to, cc string
		ownOnly            bool
		want               bool
	}{
		{"own to own", `"Me" <me@example.test>`, "me@work.test", "", true, true},
		{"own by bcc", "me@example.test", "", "", true, true},
		{"case does not matter", "ME@Example.Test", "me@example.test", "", true, true},
		{"newsletter", "news@shop.test", "me@example.test", "", true, false},
		{"own to a friend", "me@example.test", "me@example.test, friend@else.test", "", true, false},
		{"friend in cc", "me@example.test", "me@example.test", "friend@else.test", true, false},
		{"relaxed recipients", "me@example.test", "friend@else.test", "", false, true},
		{"relaxed still needs own sender", "news@shop.test", "me@example.test", "", false, false},
		{"two senders", "me@example.test, other@else.test", "me@example.test", "", true, false},
	}
	for _, c := range cases {
		if got := isNoteToSelf(own, c.from, c.to, c.cc, c.ownOnly); got != c.want {
			t.Errorf("%s: isNoteToSelf = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestBackfillCollectsRecipesFromNotesToSelfAndFilesThem(t *testing.T) {
	st := openRecipeStore(t)
	ctx := context.Background()
	server := recipeServer(t)
	f := newFixture(t, st, "me@example.test")
	date := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	note := createMessage(t, st, f.user.ID, f.account.ID, f.inbox.ID, "<note@example.test>", "me@example.test", "me@example.test",
		"Rezept", "für Sonntag\n"+server.URL+"/carbonara?utm_source=share", date)
	// The Sent copy of the same note: the reader's own record of writing it.
	sentCopy := createMessage(t, st, f.user.ID, f.account.ID, f.sent.ID, "<note@example.test>", "me@example.test", "me@example.test",
		"Rezept", server.URL+"/carbonara", date)
	newsletter := createMessage(t, st, f.user.ID, f.account.ID, f.inbox.ID, "<news@shop.test>", "news@shop.test", "me@example.test",
		"Neue Rezepte", server.URL+"/carbonara", date)
	article := createMessage(t, st, f.user.ID, f.account.ID, f.inbox.ID, "<article@example.test>", "me@example.test", "me@example.test",
		"Lesen", server.URL+"/article", date.Add(time.Hour))

	db := st.DB()
	if _, err := saveSettings(ctx, db, f.user.ID, Settings{Enabled: true, OwnRecipientsOnly: true, MoveEnabled: true}); err != nil {
		t.Fatal(err)
	}
	p := testBackend()
	host := &fakeHost{t: t, store: st, userID: f.user.ID}
	drain(t, p, host, f.user.ID)

	var recipes int
	var title, imageStatus, canonical string
	var firstSeen int64
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*), MIN(title), MIN(image_status), MIN(canonical_url), MIN(first_seen_at) FROM plugin_recipe_box_recipes WHERE user_id = ?`, f.user.ID).
		Scan(&recipes, &title, &imageStatus, &canonical, &firstSeen); err != nil {
		t.Fatal(err)
	}
	if recipes != 1 || title != "Spaghetti Carbonara" || imageStatus != "ok" {
		t.Fatalf("recipes = %d %q image=%q", recipes, title, imageStatus)
	}
	if strings.Contains(canonical, "utm_") || firstSeen != date.Unix() {
		t.Fatalf("canonical = %q first seen = %d", canonical, firstSeen)
	}

	// The newsletter was never read for links: it is somebody else's mail.
	var newsletterRows int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM plugin_recipe_box_mails WHERE user_id = ? AND message_key = ?`, f.user.ID, "mid:<news@shop.test>").Scan(&newsletterRows); err != nil {
		t.Fatal(err)
	}
	if newsletterRows != 0 {
		t.Fatal("a newsletter was taken for a note-to-self")
	}

	// Exactly the Inbox note was filed: not the Sent copy, not the newsletter,
	// and not the note whose link was an article.
	if len(host.moved) != 1 || host.moved[0] != note.ID {
		t.Fatalf("moved = %v, want only the note %d (sent %d, newsletter %d, article %d)", host.moved, note.ID, sentCopy.ID, newsletter.ID, article.ID)
	}
	var folderShown int
	if err := db.QueryRowContext(ctx, `SELECT show_in_all_mail FROM mailboxes WHERE user_id = ? AND account_id = ? AND name = 'Recipes'`, f.user.ID, f.account.ID).Scan(&folderShown); err != nil {
		t.Fatal(err)
	}
	if folderShown != 0 {
		t.Fatal("the recipe folder the box created is still listed in All Mail")
	}
	var articleStatus string
	if err := db.QueryRowContext(ctx, `SELECT status FROM plugin_recipe_box_links WHERE user_id = ? AND url LIKE '%/article'`, f.user.ID).Scan(&articleStatus); err != nil {
		t.Fatal(err)
	}
	if articleStatus != linkPage {
		t.Fatalf("article link status = %q", articleStatus)
	}

	// The filed copy arrives in the folder as a new row. The scan follows the
	// mail to it and the box does not move it a second time.
	drain(t, p, host, f.user.ID)
	if len(host.moved) != 1 {
		t.Fatalf("the filed mail was moved again: %v", host.moved)
	}
	detail, err := loadRecipe(ctx, db, f.user.ID, recipeIDByTitle(t, db, f.user.ID, "Spaghetti Carbonara"))
	if err != nil {
		t.Fatal(err)
	}
	if len(detail.Mails) != 1 || detail.Mails[0].MessageID == note.ID || detail.Mails[0].MessageID == 0 || detail.Mails[0].Note != "für Sonntag" {
		t.Fatalf("recipe mails = %#v (old row %d)", detail.Mails, note.ID)
	}
	if detail.Mails[0].MoveState != moveStateMoved {
		t.Fatalf("move state = %q", detail.Mails[0].MoveState)
	}
}

func TestDisabledReaderIsNeverScanned(t *testing.T) {
	st := openRecipeStore(t)
	server := recipeServer(t)
	f := newFixture(t, st, "off@example.test")
	createMessage(t, st, f.user.ID, f.account.ID, f.inbox.ID, "<off@example.test>", "off@example.test", "off@example.test", "x", server.URL+"/carbonara", time.Now())
	p := testBackend()
	host := &fakeHost{t: t, store: st, userID: f.user.ID}
	drain(t, p, host, f.user.ID)
	var n int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM plugin_recipe_box_mails WHERE user_id = ?`, f.user.ID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 || len(host.moved) != 0 {
		t.Fatalf("a reader without settings had mail read (%d) or moved (%v)", n, host.moved)
	}
}

func TestMoveCanBeSwitchedOff(t *testing.T) {
	st := openRecipeStore(t)
	server := recipeServer(t)
	f := newFixture(t, st, "keep@example.test")
	createMessage(t, st, f.user.ID, f.account.ID, f.inbox.ID, "<keep@example.test>", "keep@example.test", "keep@example.test", "x", server.URL+"/carbonara", time.Now())
	if _, err := saveSettings(context.Background(), st.DB(), f.user.ID, Settings{Enabled: true, OwnRecipientsOnly: true, MoveEnabled: false}); err != nil {
		t.Fatal(err)
	}
	p := testBackend()
	host := &fakeHost{t: t, store: st, userID: f.user.ID}
	drain(t, p, host, f.user.ID)
	if len(host.moved) != 0 || len(host.created) != 0 {
		t.Fatalf("moved %v / created %v with moving switched off", host.moved, host.created)
	}
	if recipeIDByTitle(t, st.DB(), f.user.ID, "Spaghetti Carbonara") == 0 {
		t.Fatal("recipe not collected")
	}
}

func TestRecipeRoutesStayInsideTheTenant(t *testing.T) {
	st := openRecipeStore(t)
	server := recipeServer(t)
	alice := newFixture(t, st, "alice@example.test")
	bob := newFixture(t, st, "bob@example.test")
	createMessage(t, st, alice.user.ID, alice.account.ID, alice.inbox.ID, "<a@example.test>", "alice@example.test", "alice@example.test", "x", server.URL+"/carbonara", time.Now())
	if _, err := saveSettings(context.Background(), st.DB(), alice.user.ID, Settings{Enabled: true, OwnRecipientsOnly: true}); err != nil {
		t.Fatal(err)
	}
	p := testBackend()
	drain(t, p, &fakeHost{t: t, store: st, userID: alice.user.ID}, alice.user.ID)
	aliceRecipe := recipeIDByTitle(t, st.DB(), alice.user.ID, "Spaghetti Carbonara")

	call := func(userID int64, method, target, body string) *httptest.ResponseRecorder {
		host := &fakeHost{t: t, store: st, userID: userID}
		req := httptest.NewRequest(method, "/api/"+target, strings.NewReader(body))
		rec := httptest.NewRecorder()
		// The host hands a route its path without the query string.
		p.handleAPI(host, req.URL.Path[len("/api/"):], rec, req)
		return rec
	}
	list := call(alice.user.ID, http.MethodGet, apiPath+"/recipes?q=eigelb", "")
	var listed struct {
		Recipes []RecipeSummary `json:"recipes"`
	}
	if err := json.Unmarshal(list.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed.Recipes) != 1 || listed.Recipes[0].ImageURL == "" || listed.Recipes[0].TotalMinutes != 30 {
		t.Fatalf("alice's search = %s", list.Body.String())
	}
	if rec := call(alice.user.ID, http.MethodGet, apiPath+fmt.Sprintf("/recipes/%d/image", aliceRecipe), ""); rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "image/png" {
		t.Fatalf("alice's picture = %d %q", rec.Code, rec.Header().Get("Content-Type"))
	}

	// Bob asks for Alice's recipe, its picture, and to dismiss it.
	if rec := call(bob.user.ID, http.MethodGet, apiPath+"/recipes", ""); strings.Contains(rec.Body.String(), "Carbonara") {
		t.Fatalf("bob's list shows alice's recipe: %s", rec.Body.String())
	}
	for _, path := range []string{fmt.Sprintf("/recipes/%d", aliceRecipe), fmt.Sprintf("/recipes/%d/image", aliceRecipe)} {
		if rec := call(bob.user.ID, http.MethodGet, apiPath+path, ""); rec.Code != http.StatusNotFound {
			t.Fatalf("bob reading %s = %d", path, rec.Code)
		}
	}
	if rec := call(bob.user.ID, http.MethodPost, apiPath+fmt.Sprintf("/recipes/%d", aliceRecipe), `{"dismissed":true}`); rec.Code != http.StatusNotFound {
		t.Fatalf("bob dismissing alice's recipe = %d", rec.Code)
	}
	var dismissed int
	if err := st.DB().QueryRow(`SELECT dismissed FROM plugin_recipe_box_recipes WHERE id = ?`, aliceRecipe).Scan(&dismissed); err != nil || dismissed != 0 {
		t.Fatalf("alice's recipe dismissed=%d err=%v", dismissed, err)
	}
}

func TestPromotingAnUnrecognisedPageFilesItsMail(t *testing.T) {
	st := openRecipeStore(t)
	server := recipeServer(t)
	f := newFixture(t, st, "promote@example.test")
	createMessage(t, st, f.user.ID, f.account.ID, f.inbox.ID, "<p@example.test>", "promote@example.test", "promote@example.test", "Video", server.URL+"/article", time.Now())
	if _, err := saveSettings(context.Background(), st.DB(), f.user.ID, Settings{Enabled: true, OwnRecipientsOnly: true, MoveEnabled: true}); err != nil {
		t.Fatal(err)
	}
	p := testBackend()
	host := &fakeHost{t: t, store: st, userID: f.user.ID}
	drain(t, p, host, f.user.ID)
	if len(host.moved) != 0 {
		t.Fatal("an unrecognised page's mail was filed")
	}
	var linkID int64
	if err := st.DB().QueryRow(`SELECT id FROM plugin_recipe_box_links WHERE user_id = ? AND status = 'page'`, f.user.ID).Scan(&linkID); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("{}"))
	rec := httptest.NewRecorder()
	p.handleAPI(host, fmt.Sprintf("%s/links/%d/recipe", apiPath, linkID), rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("promote = %d %s", rec.Code, rec.Body.String())
	}
	drain(t, p, host, f.user.ID)
	if recipeIDByTitle(t, st.DB(), f.user.ID, "Ein Artikel") == 0 || len(host.moved) != 1 {
		t.Fatalf("promoted page: moved %v", host.moved)
	}
}

func recipeIDByTitle(t *testing.T, db *sql.DB, userID int64, title string) int64 {
	t.Helper()
	var id int64
	_ = db.QueryRowContext(context.Background(), `SELECT id FROM plugin_recipe_box_recipes WHERE user_id = ? AND title = ?`, userID, title).Scan(&id)
	return id
}

func TestDeletedMailAndRefusedMovesDoNotHoldTheQueue(t *testing.T) {
	st := openRecipeStore(t)
	ctx := context.Background()
	server := recipeServer(t)
	f := newFixture(t, st, "queue@example.test")
	gone := createMessage(t, st, f.user.ID, f.account.ID, f.inbox.ID, "<gone@example.test>", "queue@example.test", "queue@example.test", "x", server.URL+"/carbonara", time.Now())
	kept := createMessage(t, st, f.user.ID, f.account.ID, f.inbox.ID, "<kept@example.test>", "queue@example.test", "queue@example.test", "y", server.URL+"/carbonara", time.Now())
	db := st.DB()
	if _, err := saveSettings(ctx, db, f.user.ID, Settings{Enabled: true, OwnRecipientsOnly: true, MoveEnabled: true}); err != nil {
		t.Fatal(err)
	}
	p := testBackend()
	// The server refuses the first moves; the reader deletes one mail meanwhile.
	host := &fakeHost{t: t, store: st, userID: f.user.ID, moveErr: fmt.Errorf("server busy")}
	drain(t, p, host, f.user.ID)
	var waiting, attempts int
	var nextAt int64
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*), MAX(move_attempts), MAX(move_next_at) FROM plugin_recipe_box_mails WHERE user_id = ? AND move_state = ''`, f.user.ID).Scan(&waiting, &attempts, &nextAt); err != nil {
		t.Fatal(err)
	}
	// One refusal each, and then a wait: a busy server does not use up every
	// attempt within the same minute.
	if waiting != 2 || attempts != 1 || nextAt <= time.Now().Unix() {
		t.Fatalf("waiting=%d attempts=%d next=%d", waiting, attempts, nextAt)
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM messages WHERE user_id = ? AND id = ?`, f.user.ID, gone.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE plugin_recipe_box_mails SET move_next_at = 0 WHERE user_id = ?`, f.user.ID); err != nil {
		t.Fatal(err)
	}
	host.moveErr = nil
	drain(t, p, host, f.user.ID)
	states := map[string]string{}
	rows, err := db.QueryContext(ctx, `SELECT message_key, move_state FROM plugin_recipe_box_mails WHERE user_id = ?`, f.user.ID)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var key, state string
		if err := rows.Scan(&key, &state); err != nil {
			t.Fatal(err)
		}
		states[key] = state
	}
	rows.Close()
	if states["mid:<gone@example.test>"] != moveStateSkipped || states["mid:<kept@example.test>"] != moveStateMoved {
		t.Fatalf("states = %v", states)
	}
	if len(host.moved) != 1 || host.moved[0] != kept.ID {
		t.Fatalf("moved = %v", host.moved)
	}
}

func TestPreviewFiledByHandNeverOverwritesAStructuredRecipe(t *testing.T) {
	st := openRecipeStore(t)
	ctx := context.Background()
	f := newFixture(t, st, "keepdata@example.test")
	db := st.DB()
	full := &recipe.Recipe{Name: "Carbonara", Ingredients: []string{"Eigelb"}, Instructions: []recipe.StepSection{{Steps: []string{"Kochen."}}}}
	first, err := upsertRecipe(ctx, db, f.user.ID, "https://example.test/carbonara", recipe.Page{}, full, "structured")
	if err != nil {
		t.Fatal(err)
	}
	second, err := upsertRecipe(ctx, db, f.user.ID, "https://example.test/carbonara", recipe.Page{}, &recipe.Recipe{Name: "Video"}, "manual")
	if err != nil {
		t.Fatal(err)
	}
	detail, err := loadRecipe(ctx, db, f.user.ID, first)
	if err != nil {
		t.Fatal(err)
	}
	if second != first || detail.Title != "Carbonara" || len(detail.Ingredients) != 1 || detail.Origin != "structured" {
		t.Fatalf("recipe after a manual filing = %d/%d %#v", first, second, detail.RecipeSummary)
	}
}

func TestScanDoesNotPassRowsThatHaveNotSettled(t *testing.T) {
	st := openRecipeStore(t)
	ctx := context.Background()
	server := recipeServer(t)
	f := newFixture(t, st, "settle@example.test")
	createMessage(t, st, f.user.ID, f.account.ID, f.inbox.ID, "<s@example.test>", "settle@example.test", "settle@example.test", "x", server.URL+"/carbonara", time.Now())
	db := st.DB()
	settings, err := saveSettings(ctx, db, f.user.ID, Settings{Enabled: true, OwnRecipientsOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	// A row written this second may still have a sibling in an open
	// transaction below it: the cursor stays put until it has settled.
	if _, more, err := scanUser(ctx, db, f.user.ID, settings, time.Now().Add(-time.Minute).Unix()); err != nil || more {
		t.Fatalf("scan = more %v, err %v", more, err)
	}
	after, err := loadSettings(ctx, db, f.user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.ScanCursor != 0 || after.ScanFinishedAt != 0 {
		t.Fatalf("cursor %d finished %d after a scan of only unsettled rows", after.ScanCursor, after.ScanFinishedAt)
	}
	if found, _, err := scanUser(ctx, db, f.user.ID, after, time.Now().Add(time.Minute).Unix()); err != nil || found != 1 {
		t.Fatalf("settled scan found %d, err %v", found, err)
	}
}

func TestHeaderlessNoteDraggedBackIsNotFiledAgain(t *testing.T) {
	st := openRecipeStore(t)
	ctx := context.Background()
	server := recipeServer(t)
	f := newFixture(t, st, "nohdr@example.test")
	createMessageWithSHA(t, st, f.user.ID, f.account.ID, f.inbox.ID, "", "content-sha-1", "nohdr@example.test", "nohdr@example.test", "x", server.URL+"/carbonara", time.Now())
	db := st.DB()
	if _, err := saveSettings(ctx, db, f.user.ID, Settings{Enabled: true, OwnRecipientsOnly: true, MoveEnabled: true}); err != nil {
		t.Fatal(err)
	}
	p := testBackend()
	host := &fakeHost{t: t, store: st, userID: f.user.ID}
	drain(t, p, host, f.user.ID)
	if len(host.moved) != 1 {
		t.Fatalf("moved = %v", host.moved)
	}
	// The reader drags it back into the Inbox.
	var filed int64
	if err := db.QueryRowContext(ctx, `SELECT id FROM messages WHERE user_id = ? AND canonical_sha256 = 'content-sha-1'`, f.user.ID).Scan(&filed); err != nil {
		t.Fatal(err)
	}
	if err := host.MoveMessage(ctx, f.user.ID, filed, f.inbox.ID); err != nil {
		t.Fatal(err)
	}
	drain(t, p, host, f.user.ID)
	if len(host.moved) != 2 {
		t.Fatalf("the box filed a mail the reader took back: moved = %v", host.moved)
	}
	var mails int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM plugin_recipe_box_mails WHERE user_id = ?`, f.user.ID).Scan(&mails); err != nil || mails != 1 {
		t.Fatalf("mails = %d err %v", mails, err)
	}
}
