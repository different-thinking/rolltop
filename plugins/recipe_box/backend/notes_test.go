package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func notesServer(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/carbonara", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(carbonaraMarkup))
	})
	mux.HandleFunc("/pfanne", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<html><head><title>Gusspfanne</title></head><script type="application/ld+json">{"@type":"Product","name":"Gusspfanne","offers":{"price":"49.90","priceCurrency":"EUR"}}</script></html>`))
	})
	mux.HandleFunc("/bericht", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<html><head><meta property="og:type" content="article"><meta property="og:title" content="Ein Bericht"></head></html>`))
	})
	mux.HandleFunc("/carbonara.png", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("\x89PNG\r\n\x1a\nimage"))
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

type notesAnswer struct {
	Notes  []Note         `json:"notes"`
	Counts map[string]int `json:"counts"`
	Totals map[string]int `json:"totals"`
}

func callAPI(t *testing.T, p *recipeBoxBackend, host *fakeHost, method, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, "/api/"+target, strings.NewReader(body))
	rec := httptest.NewRecorder()
	p.handleAPI(host, req.URL.Path[len("/api/"):], rec, req)
	return rec
}

func listNotes(t *testing.T, p *recipeBoxBackend, host *fakeHost, query string) notesAnswer {
	t.Helper()
	rec := callAPI(t, p, host, http.MethodGet, apiPath+"/notes"+query, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("notes = %d %s", rec.Code, rec.Body.String())
	}
	var out notesAnswer
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestNotesAreSortedByWhatTheirPagesSay(t *testing.T) {
	st := openRecipeStore(t)
	ctx := context.Background()
	server := notesServer(t)
	f := newFixture(t, st, "notes@example.test")
	date := time.Date(2026, 5, 1, 9, 0, 0, 0, time.UTC)
	me := "notes@example.test"
	createMessage(t, st, f.user.ID, f.account.ID, f.inbox.ID, "<r@x>", me, me, "Rezept", server.URL+"/carbonara", date)
	createMessage(t, st, f.user.ID, f.account.ID, f.inbox.ID, "<p@x>", me, me, "Pfanne", "die hier?\n"+server.URL+"/pfanne", date.Add(time.Hour))
	createMessage(t, st, f.user.ID, f.account.ID, f.inbox.ID, "<a@x>", me, me, "Lesen", server.URL+"/bericht", date.Add(2*time.Hour))
	createMessage(t, st, f.user.ID, f.account.ID, f.inbox.ID, "<t@x>", me, me, "Einkauf", "Milch\nEier\n\n\nBrot", date.Add(3*time.Hour))
	createMessage(t, st, f.user.ID, f.account.ID, f.inbox.ID, "<d@x>", me, me, "Doc", "https://docs.google.com/document/d/abc", date.Add(4*time.Hour))
	// Somebody else's mail is no note of the reader's, whatever it says.
	createMessage(t, st, f.user.ID, f.account.ID, f.inbox.ID, "<n@x>", "news@shop.test", me, "Angebot", "Milch billiger", date)
	if _, err := saveSettings(ctx, st.DB(), f.user.ID, Settings{Enabled: true, OwnRecipientsOnly: true, MoveEnabled: true}); err != nil {
		t.Fatal(err)
	}
	p := testBackend()
	host := &fakeHost{t: t, store: st, userID: f.user.ID}
	drain(t, p, host, f.user.ID)

	answer := listNotes(t, p, host, "")
	kinds := map[string]Note{}
	for _, note := range answer.Notes {
		kinds[note.Kind] = note
	}
	if len(answer.Notes) != 4 {
		t.Fatalf("notes = %d (%v), want 4 -- the recipe mail is no note, the newsletter no mail of the reader's", len(answer.Notes), answer.Counts)
	}
	if note := kinds["product"]; len(note.Links) != 1 || note.Links[0].Detail != "49.90 EUR" || note.Body != "die hier?" {
		t.Fatalf("product note = %#v", note)
	}
	if note := kinds["article"]; len(note.Links) != 1 || note.Links[0].Title != "Ein Bericht" {
		t.Fatalf("article note = %#v", note)
	}
	if note := kinds["text"]; note.Body != "Milch\nEier\n\nBrot" || note.Subject != "Einkauf" {
		t.Fatalf("text note = %#v", note)
	}
	if note := kinds["docs"]; len(note.Links) != 1 || note.Links[0].Status != "page" {
		t.Fatalf("document note = %#v", note)
	}
	if answer.Counts["product"] != 1 || answer.Totals["open"] != 4 {
		t.Fatalf("counts = %v totals = %v", answer.Counts, answer.Totals)
	}
	// The recipe mail was filed; no note was, since filing notes is off.
	if len(host.moved) != 1 {
		t.Fatalf("moved = %v", host.moved)
	}

	// A filter and a search narrow the list but not the counts per kind
	// beyond the search.
	if got := listNotes(t, p, host, "?kind=text"); len(got.Notes) != 1 || got.Counts["article"] != 1 {
		t.Fatalf("kind filter = %#v", got)
	}
	if got := listNotes(t, p, host, "?q=gusspfanne"); len(got.Notes) != 1 || got.Notes[0].Kind != "product" {
		t.Fatalf("search = %#v", got)
	}

	// Done moves a note to the other state, and only for its owner.
	textID := kinds["text"].ID
	stranger := newFixture(t, st, "stranger@example.test")
	if rec := callAPI(t, p, &fakeHost{t: t, store: st, userID: stranger.user.ID}, http.MethodPost, fmt.Sprintf("%s/notes/%d", apiPath, textID), `{"done":true}`); rec.Code != http.StatusNotFound {
		t.Fatalf("a stranger marked the note done: %d", rec.Code)
	}
	if rec := callAPI(t, p, host, http.MethodPost, fmt.Sprintf("%s/notes/%d", apiPath, textID), `{"done":true}`); rec.Code != http.StatusOK {
		t.Fatalf("done = %d %s", rec.Code, rec.Body.String())
	}
	if got := listNotes(t, p, host, "?state=done"); len(got.Notes) != 1 || got.Notes[0].ID != textID || got.Totals["done"] != 1 {
		t.Fatalf("done list = %#v", got)
	}
}

func TestNotesAreFiledOnlyWhenAskedAndOnlyOnceTheirPagesAnswered(t *testing.T) {
	st := openRecipeStore(t)
	ctx := context.Background()
	server := notesServer(t)
	f := newFixture(t, st, "file@example.test")
	me := "file@example.test"
	createMessage(t, st, f.user.ID, f.account.ID, f.inbox.ID, "<t@y>", me, me, "Idee", "Garten umgraben", time.Now())
	createMessage(t, st, f.user.ID, f.account.ID, f.inbox.ID, "<r@y>", me, me, "Rezept", server.URL+"/carbonara", time.Now())
	db := st.DB()
	if _, err := saveSettings(ctx, db, f.user.ID, Settings{Enabled: true, OwnRecipientsOnly: true, MoveEnabled: true, NotesMoveEnabled: true, NotesFolderName: "Notizen"}); err != nil {
		t.Fatal(err)
	}
	if _, err := saveSettings(ctx, db, f.user.ID, Settings{Enabled: true, MoveEnabled: true, NotesMoveEnabled: true, FolderName: "Ablage", NotesFolderName: "ablage"}); err == nil {
		t.Fatal("one folder was accepted for both shelves")
	}
	// While notes are not filed the notes folder is not asked about: a recipe
	// folder that carries the default notes name must not block every save.
	if _, err := saveSettings(ctx, db, f.user.ID, Settings{Enabled: true, MoveEnabled: true, FolderName: "Notes"}); err != nil {
		t.Fatalf("a recipe folder named like the unused notes folder was refused: %v", err)
	}
	if _, err := saveSettings(ctx, db, f.user.ID, Settings{Enabled: true, OwnRecipientsOnly: true, MoveEnabled: true, NotesMoveEnabled: true, NotesFolderName: "Notizen"}); err != nil {
		t.Fatal(err)
	}
	p := testBackend()
	host := &fakeHost{t: t, store: st, userID: f.user.ID}
	drain(t, p, host, f.user.ID)
	if len(host.moved) != 2 {
		t.Fatalf("moved = %v", host.moved)
	}
	var folder string
	if err := db.QueryRowContext(ctx, `SELECT mb.name FROM messages m JOIN mailboxes mb ON mb.id = m.mailbox_id WHERE m.user_id = ? AND m.message_id_header = '<t@y>'`, f.user.ID).Scan(&folder); err != nil {
		t.Fatal(err)
	}
	if folder != "Notizen" {
		t.Fatalf("the text note was filed into %q", folder)
	}
	var recipeFolder string
	if err := db.QueryRowContext(ctx, `SELECT mb.name FROM messages m JOIN mailboxes mb ON mb.id = m.mailbox_id WHERE m.user_id = ? AND m.message_id_header = '<r@y>'`, f.user.ID).Scan(&recipeFolder); err != nil {
		t.Fatal(err)
	}
	if recipeFolder != "Recipes" {
		t.Fatalf("the recipe mail was filed into %q", recipeFolder)
	}
}

func TestPendingLinkKeepsANoteFromBeingFiled(t *testing.T) {
	st := openRecipeStore(t)
	ctx := context.Background()
	f := newFixture(t, st, "wait@example.test")
	me := "wait@example.test"
	createMessage(t, st, f.user.ID, f.account.ID, f.inbox.ID, "<w@z>", me, me, "Später", "https://unreachable.example.test/x", time.Now())
	db := st.DB()
	if _, err := saveSettings(ctx, db, f.user.ID, Settings{Enabled: true, OwnRecipientsOnly: true, NotesMoveEnabled: true}); err != nil {
		t.Fatal(err)
	}
	settings, err := loadSettings(ctx, db, f.user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := scanUser(ctx, db, f.user.ID, settings, time.Now().Add(time.Minute).Unix()); err != nil {
		t.Fatal(err)
	}
	host := &fakeHost{t: t, store: st, userID: f.user.ID}
	if _, _, err := fileMails(ctx, host, db, f.user.ID, settings); err != nil {
		t.Fatal(err)
	}
	// The page may still turn out to be a recipe: the note waits for it.
	if len(host.moved) != 0 {
		t.Fatalf("a note with an unanswered link was filed: %v", host.moved)
	}
}

func TestRecipesCanBeMarkedCooked(t *testing.T) {
	st := openRecipeStore(t)
	server := notesServer(t)
	f := newFixture(t, st, "cook@example.test")
	createMessage(t, st, f.user.ID, f.account.ID, f.inbox.ID, "<c@x>", "cook@example.test", "cook@example.test", "x", server.URL+"/carbonara", time.Now())
	if _, err := saveSettings(context.Background(), st.DB(), f.user.ID, Settings{Enabled: true, OwnRecipientsOnly: true}); err != nil {
		t.Fatal(err)
	}
	p := testBackend()
	host := &fakeHost{t: t, store: st, userID: f.user.ID}
	drain(t, p, host, f.user.ID)
	id := recipeIDByTitle(t, st.DB(), f.user.ID, "Spaghetti Carbonara")
	list := func(view string) (int, map[string]int) {
		rec := callAPI(t, p, host, http.MethodGet, apiPath+"/recipes"+view, "")
		var out struct {
			Recipes []RecipeSummary `json:"recipes"`
			Counts  map[string]int  `json:"counts"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return len(out.Recipes), out.Counts
	}
	if n, counts := list(""); n != 1 || counts["to_cook"] != 1 {
		t.Fatalf("to cook = %d %v", n, counts)
	}
	if rec := callAPI(t, p, host, http.MethodPost, fmt.Sprintf("%s/recipes/%d", apiPath, id), `{"cooked":true}`); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"cooked_at":`) {
		t.Fatalf("cooked = %d %s", rec.Code, rec.Body.String())
	}
	if n, counts := list(""); n != 0 || counts["cooked"] != 1 {
		t.Fatalf("to cook after cooking = %d %v", n, counts)
	}
	if n, _ := list("?view=cooked"); n != 1 {
		t.Fatalf("cooked list = %d", n)
	}
	if rec := callAPI(t, p, host, http.MethodPost, fmt.Sprintf("%s/recipes/%d", apiPath, id), `{"cooked":false}`); rec.Code != http.StatusOK {
		t.Fatalf("uncook = %d", rec.Code)
	}
	if n, _ := list(""); n != 1 {
		t.Fatalf("to cook after uncooking = %d", n)
	}
}

func TestDeletingANoteMovesItsMailToTrash(t *testing.T) {
	st := openRecipeStore(t)
	ctx := context.Background()
	f := newFixture(t, st, "delete@example.test")
	trash, err := st.GetOrCreateMailboxWithRole(ctx, f.user.ID, f.account.ID, "Trash", "trash")
	if err != nil {
		t.Fatal(err)
	}
	me := "delete@example.test"
	date := time.Date(2026, 5, 1, 9, 0, 0, 0, time.UTC)
	createMessage(t, st, f.user.ID, f.account.ID, f.inbox.ID, "<del@x>", me, me, "Einkauf", "Milch", date)
	sentCopy := createMessage(t, st, f.user.ID, f.account.ID, f.sent.ID, "<del@x>", me, me, "Einkauf", "Milch", date)
	if _, err := saveSettings(ctx, st.DB(), f.user.ID, Settings{Enabled: true, OwnRecipientsOnly: true}); err != nil {
		t.Fatal(err)
	}
	p := testBackend()
	host := &fakeHost{t: t, store: st, userID: f.user.ID}
	drain(t, p, host, f.user.ID)
	answer := listNotes(t, p, host, "")
	if len(answer.Notes) != 1 {
		t.Fatalf("notes = %#v", answer.Notes)
	}
	noteID := answer.Notes[0].ID
	target := fmt.Sprintf("%s/notes/%d/delete", apiPath, noteID)

	// Somebody else cannot delete the reader's note, nor move their mail.
	stranger := newFixture(t, st, "other@example.test")
	if rec := callAPI(t, p, &fakeHost{t: t, store: st, userID: stranger.user.ID}, http.MethodPost, target, `{}`); rec.Code != http.StatusNotFound {
		t.Fatalf("a stranger deleted the note: %d", rec.Code)
	}
	if len(host.moved) != 0 {
		t.Fatalf("moved before the delete = %v", host.moved)
	}

	if rec := callAPI(t, p, host, http.MethodPost, target, `{}`); rec.Code != http.StatusOK {
		t.Fatalf("delete = %d %s", rec.Code, rec.Body.String())
	}
	if len(host.moved) != 1 {
		t.Fatalf("moved = %v, want the Inbox copy only", host.moved)
	}
	var inTrash, inSent int
	if err := st.DB().QueryRowContext(ctx, `SELECT COUNT(*) FILTER (WHERE mailbox_id = ?), COUNT(*) FILTER (WHERE id = ?) FROM messages WHERE user_id = ?`,
		trash.ID, sentCopy.ID, f.user.ID).Scan(&inTrash, &inSent); err != nil {
		t.Fatal(err)
	}
	if inTrash != 1 || inSent != 1 {
		t.Fatalf("in Trash = %d, Sent copy kept = %d", inTrash, inSent)
	}
	if got := listNotes(t, p, host, ""); len(got.Notes) != 0 || got.Totals["open"] != 0 {
		t.Fatalf("notes after delete = %#v", got)
	}
	// The mail in Trash is not read back into a note.
	drain(t, p, host, f.user.ID)
	if got := listNotes(t, p, host, ""); len(got.Notes) != 0 {
		t.Fatalf("notes after the next round = %#v", got.Notes)
	}
	if rec := callAPI(t, p, host, http.MethodPost, target, `{}`); rec.Code != http.StatusNotFound {
		t.Fatalf("second delete = %d", rec.Code)
	}
}

func TestDeletingANoteRefusesAnAccountWithoutTrash(t *testing.T) {
	st := openRecipeStore(t)
	ctx := context.Background()
	f := newFixture(t, st, "notrash@example.test")
	me := "notrash@example.test"
	createMessage(t, st, f.user.ID, f.account.ID, f.inbox.ID, "<nt@x>", me, me, "Notiz", "Text", time.Date(2026, 5, 1, 9, 0, 0, 0, time.UTC))
	if _, err := saveSettings(ctx, st.DB(), f.user.ID, Settings{Enabled: true, OwnRecipientsOnly: true}); err != nil {
		t.Fatal(err)
	}
	p := testBackend()
	host := &fakeHost{t: t, store: st, userID: f.user.ID}
	drain(t, p, host, f.user.ID)
	answer := listNotes(t, p, host, "")
	if len(answer.Notes) != 1 {
		t.Fatalf("notes = %#v", answer.Notes)
	}
	if rec := callAPI(t, p, host, http.MethodPost, fmt.Sprintf("%s/notes/%d/delete", apiPath, answer.Notes[0].ID), `{}`); rec.Code != http.StatusConflict {
		t.Fatalf("delete without Trash = %d %s", rec.Code, rec.Body.String())
	}
	if len(host.moved) != 0 || len(listNotes(t, p, host, "").Notes) != 1 {
		t.Fatalf("a refused delete still moved %v", host.moved)
	}
}

func TestDeletingANoteKeepsItListedWhenAMoveFails(t *testing.T) {
	st := openRecipeStore(t)
	ctx := context.Background()
	f := newFixture(t, st, "failing@example.test")
	if _, err := st.GetOrCreateMailboxWithRole(ctx, f.user.ID, f.account.ID, "Trash", "trash"); err != nil {
		t.Fatal(err)
	}
	me := "failing@example.test"
	createMessage(t, st, f.user.ID, f.account.ID, f.inbox.ID, "<fail@x>", me, me, "Notiz", "Text", time.Date(2026, 5, 1, 9, 0, 0, 0, time.UTC))
	if _, err := saveSettings(ctx, st.DB(), f.user.ID, Settings{Enabled: true, OwnRecipientsOnly: true}); err != nil {
		t.Fatal(err)
	}
	p := testBackend()
	host := &fakeHost{t: t, store: st, userID: f.user.ID}
	drain(t, p, host, f.user.ID)
	answer := listNotes(t, p, host, "")
	if len(answer.Notes) != 1 {
		t.Fatalf("notes = %#v", answer.Notes)
	}
	target := fmt.Sprintf("%s/notes/%d/delete", apiPath, answer.Notes[0].ID)
	host.moveErr = fmt.Errorf("server busy")
	if rec := callAPI(t, p, host, http.MethodPost, target, `{}`); rec.Code != http.StatusBadGateway {
		t.Fatalf("delete with a failing move = %d %s", rec.Code, rec.Body.String())
	}
	if got := listNotes(t, p, host, ""); len(got.Notes) != 1 {
		t.Fatalf("a failed delete took the note away: %#v", got.Notes)
	}
	host.moveErr = nil
	rec := callAPI(t, p, host, http.MethodPost, target, `{}`)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"moved":1`) {
		t.Fatalf("delete after the server recovered = %d %s", rec.Code, rec.Body.String())
	}
}

func TestDeletingANoteTakesCopiesTheScanNeverReads(t *testing.T) {
	st := openRecipeStore(t)
	ctx := context.Background()
	f := newFixture(t, st, "gmail@example.test")
	trash, err := st.GetOrCreateMailboxWithRole(ctx, f.user.ID, f.account.ID, "[Gmail]/Trash", "trash")
	if err != nil {
		t.Fatal(err)
	}
	allMail, err := st.GetOrCreateMailboxWithRole(ctx, f.user.ID, f.account.ID, "[Gmail]/All Mail", "all")
	if err != nil {
		t.Fatal(err)
	}
	junk, err := st.GetOrCreateMailboxWithRole(ctx, f.user.ID, f.account.ID, "[Gmail]/Spam", "junk")
	if err != nil {
		t.Fatal(err)
	}
	me := "gmail@example.test"
	date := time.Date(2026, 5, 1, 9, 0, 0, 0, time.UTC)
	inboxCopy := createMessage(t, st, f.user.ID, f.account.ID, f.inbox.ID, "<arch@x>", me, me, "Notiz", "Text", date)
	if _, err := saveSettings(ctx, st.DB(), f.user.ID, Settings{Enabled: true, OwnRecipientsOnly: true}); err != nil {
		t.Fatal(err)
	}
	p := testBackend()
	host := &fakeHost{t: t, store: st, userID: f.user.ID}
	drain(t, p, host, f.user.ID)
	answer := listNotes(t, p, host, "")
	if len(answer.Notes) != 1 {
		t.Fatalf("notes = %#v", answer.Notes)
	}
	// Archived in Gmail: the Inbox copy is gone and All Mail holds the
	// message; another copy was filed as spam.
	if _, err := st.DB().ExecContext(ctx, `DELETE FROM messages WHERE user_id = ? AND id = ?`, f.user.ID, inboxCopy.ID); err != nil {
		t.Fatal(err)
	}
	createMessage(t, st, f.user.ID, f.account.ID, allMail.ID, "<arch@x>", me, me, "Notiz", "Text", date)
	createMessage(t, st, f.user.ID, f.account.ID, junk.ID, "<arch@x>", me, me, "Notiz", "Text", date)
	sentCopy := createMessage(t, st, f.user.ID, f.account.ID, f.sent.ID, "<arch@x>", me, me, "Notiz", "Text", date)

	rec := callAPI(t, p, host, http.MethodPost, fmt.Sprintf("%s/notes/%d/delete", apiPath, answer.Notes[0].ID), `{}`)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"moved":2`) {
		t.Fatalf("delete = %d %s", rec.Code, rec.Body.String())
	}
	var inTrash, outside, inSent int
	if err := st.DB().QueryRowContext(ctx, `SELECT COUNT(*) FILTER (WHERE mailbox_id = ?), COUNT(*) FILTER (WHERE mailbox_id IN (?, ?)), COUNT(*) FILTER (WHERE id = ?)
		FROM messages WHERE user_id = ?`, trash.ID, allMail.ID, junk.ID, sentCopy.ID, f.user.ID).Scan(&inTrash, &outside, &inSent); err != nil {
		t.Fatal(err)
	}
	if inTrash != 2 || outside != 0 || inSent != 1 {
		t.Fatalf("in Trash = %d, left in All Mail or Junk = %d, Sent copy kept = %d", inTrash, outside, inSent)
	}
}

func TestDeletingAnArchivedGmailNoteTrashesItsSentLabel(t *testing.T) {
	st := openRecipeStore(t)
	ctx := context.Background()
	f := newFixture(t, st, "archived@example.test")
	trash, err := st.GetOrCreateMailboxWithRole(ctx, f.user.ID, f.account.ID, "[Gmail]/Trash", "trash")
	if err != nil {
		t.Fatal(err)
	}
	gmailSent, err := st.GetOrCreateMailboxWithRole(ctx, f.user.ID, f.account.ID, "[Gmail]/Sent Mail", "sent")
	if err != nil {
		t.Fatal(err)
	}
	me := "archived@example.test"
	date := time.Date(2026, 5, 1, 9, 0, 0, 0, time.UTC)
	inboxCopy := createMessage(t, st, f.user.ID, f.account.ID, f.inbox.ID, "<gs@x>", me, me, "Notiz", "Text", date)
	if _, err := saveSettings(ctx, st.DB(), f.user.ID, Settings{Enabled: true, OwnRecipientsOnly: true}); err != nil {
		t.Fatal(err)
	}
	p := testBackend()
	host := &fakeHost{t: t, store: st, userID: f.user.ID}
	drain(t, p, host, f.user.ID)
	answer := listNotes(t, p, host, "")
	if len(answer.Notes) != 1 {
		t.Fatalf("notes = %#v", answer.Notes)
	}
	// Archived in Gmail with All Mail not mirrored -- the default: the Sent
	// label is the only copy left, and it is the note itself.
	if _, err := st.DB().ExecContext(ctx, `DELETE FROM messages WHERE user_id = ? AND id = ?`, f.user.ID, inboxCopy.ID); err != nil {
		t.Fatal(err)
	}
	createMessage(t, st, f.user.ID, f.account.ID, gmailSent.ID, "<gs@x>", me, me, "Notiz", "Text", date)

	rec := callAPI(t, p, host, http.MethodPost, fmt.Sprintf("%s/notes/%d/delete", apiPath, answer.Notes[0].ID), `{}`)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"moved":1`) {
		t.Fatalf("delete = %d %s", rec.Code, rec.Body.String())
	}
	var inTrash int
	if err := st.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM messages WHERE user_id = ? AND mailbox_id = ?`, f.user.ID, trash.ID).Scan(&inTrash); err != nil {
		t.Fatal(err)
	}
	if inTrash != 1 {
		t.Fatalf("in Trash = %d, want the Gmail Sent label's copy", inTrash)
	}
}
