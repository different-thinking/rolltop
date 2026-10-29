// File overview: The recipe box's HTTP routes: the cookbook, its settings, and
// the corrections a reader makes to what the box decided.

package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"rolltop/backend/plugins"
	"rolltop/plugins/recipe_box/recipe"
)

const (
	recipeListLimit = 1000
	linkListLimit   = 300
)

func (p *recipeBoxBackend) handleAPI(host plugins.APIHost, path string, w http.ResponseWriter, r *http.Request) {
	cu, ok := host.RequireAPIAuth(w, r)
	if !ok {
		return
	}
	db, err := userDB(r.Context(), host, cu.UserID)
	if err != nil {
		host.ServerError(w, err)
		return
	}
	rest := strings.Trim(strings.TrimPrefix(path, apiPath), "/")
	parts := strings.Split(rest, "/")
	userID := cu.UserID
	switch {
	case rest == "settings" && r.Method == http.MethodGet:
		p.apiSettings(host, db, userID, w, r)
	case rest == "settings" && r.Method == http.MethodPost:
		var in Settings
		if !host.DecodeJSON(w, r, &in) {
			return
		}
		if _, err := saveSettings(r.Context(), db, userID, in); err != nil {
			host.WriteAPIError(w, http.StatusBadRequest, err.Error())
			return
		}
		p.signal()
		p.apiSettings(host, db, userID, w, r)
	case rest == "scan/restart" && r.Method == http.MethodPost:
		if err := restartScan(r.Context(), db, userID); err != nil {
			host.ServerError(w, err)
			return
		}
		p.signal()
		p.apiSettings(host, db, userID, w, r)
	case rest == "folders/hide" && r.Method == http.MethodPost:
		p.apiHideFolder(host, db, userID, w, r)
	case rest == "recipes" && r.Method == http.MethodGet:
		p.apiListRecipes(host, db, userID, w, r)
	case rest == "recipes" && r.Method == http.MethodPost:
		p.apiAddRecipe(host, db, userID, w, r)
	case len(parts) == 2 && parts[0] == "recipes":
		id, ok := parseID(host, w, parts[1])
		if !ok {
			return
		}
		switch r.Method {
		case http.MethodGet:
			p.apiRecipe(host, db, userID, id, w, r)
		case http.MethodPost:
			p.apiUpdateRecipe(host, db, userID, id, w, r)
		default:
			host.WriteAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
		}
	case len(parts) == 3 && parts[0] == "recipes" && parts[2] == "image" && r.Method == http.MethodGet:
		id, ok := parseID(host, w, parts[1])
		if !ok {
			return
		}
		p.apiRecipeImage(host, db, userID, id, w, r)
	case rest == "notes" && r.Method == http.MethodGet:
		p.apiListNotes(host, db, userID, w, r)
	case len(parts) == 2 && parts[0] == "notes" && r.Method == http.MethodPost:
		id, ok := parseID(host, w, parts[1])
		if !ok {
			return
		}
		p.apiUpdateNote(host, db, userID, id, w, r)
	case rest == "links" && r.Method == http.MethodGet:
		p.apiListLinks(host, db, userID, w, r)
	case len(parts) == 3 && parts[0] == "links" && r.Method == http.MethodPost:
		id, ok := parseID(host, w, parts[1])
		if !ok {
			return
		}
		switch parts[2] {
		case "recipe":
			p.apiPromoteLink(host, db, userID, id, w, r)
		case "retry":
			p.apiRetryLink(host, db, userID, id, w, r)
		default:
			host.WriteAPIError(w, http.StatusNotFound, "recipe box route not found")
		}
	default:
		host.WriteAPIError(w, http.StatusNotFound, "recipe box route not found")
	}
}

func parseID(host plugins.APIHost, w http.ResponseWriter, raw string) (int64, bool) {
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		host.WriteAPIError(w, http.StatusBadRequest, "invalid id")
		return 0, false
	}
	return id, true
}

// Status is what the settings page shows of the box at work: how far the scan
// has come, what the links turned out to be, and what was filed.
type Status struct {
	MessagesTotal   int64 `json:"messages_total"`
	MessagesScanned int64 `json:"messages_scanned"`
	NotesFound      int64 `json:"notes_found"`
	// Notes counts the open notes: what the Notes tab still has to show.
	Notes   int64          `json:"notes"`
	Links   map[string]int `json:"links"`
	Recipes int64          `json:"recipes"`
	Mails   map[string]int `json:"mails"`
	Folders []FolderStatus `json:"folders"`
}

// FolderStatus is one account's recipe folder, and whether All Mail still
// lists it -- the one setting the box offers to change for the reader.
type FolderStatus struct {
	// Shelf says which folder this is: "recipes" or "notes".
	Shelf string `json:"shelf"`
	// Wanted is the name the folder is created under when it does not exist.
	Wanted        string `json:"wanted"`
	AccountID     int64  `json:"account_id"`
	Account       string `json:"account"`
	MailboxID     int64  `json:"mailbox_id"`
	Name          string `json:"name"`
	ShowInAllMail bool   `json:"show_in_all_mail"`
}

func (p *recipeBoxBackend) apiSettings(host plugins.APIHost, db *sql.DB, userID int64, w http.ResponseWriter, r *http.Request) {
	settings, err := loadSettings(r.Context(), db, userID)
	if err != nil {
		host.ServerError(w, err)
		return
	}
	status, err := loadStatus(r.Context(), db, userID, settings)
	if err != nil {
		host.ServerError(w, err)
		return
	}
	host.WriteJSON(w, map[string]any{"settings": settings, "status": status})
}

func loadStatus(ctx context.Context, db *sql.DB, userID int64, settings Settings) (Status, error) {
	status := Status{Links: map[string]int{}, Mails: map[string]int{}, Folders: []FolderStatus{}}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*), COUNT(*) FILTER (WHERE id <= ?) FROM messages WHERE user_id = ?`, settings.ScanCursor, userID).
		Scan(&status.MessagesTotal, &status.MessagesScanned); err != nil {
		return status, err
	}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM plugin_recipe_box_mails WHERE user_id = ?`, userID).Scan(&status.NotesFound); err != nil {
		return status, err
	}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM plugin_recipe_box_recipes WHERE user_id = ? AND dismissed = 0`, userID).Scan(&status.Recipes); err != nil {
		return status, err
	}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM plugin_recipe_box_mails m WHERE m.user_id = ? AND m.note_dismissed = 0 AND m.done_at = 0 AND NOT `+carriesRecipe, userID).Scan(&status.Notes); err != nil {
		return status, err
	}
	// A pending link whose retry is not due yet is counted apart as
	// "retrying": the pages poll while anything is pending, and a link waiting
	// out a day's back-off is not work the loop is doing now.
	if err := countInto(ctx, db, status.Links, `SELECT CASE WHEN status = 'pending' AND next_attempt_at > ? THEN 'retrying' ELSE status END AS state, COUNT(*)
		FROM plugin_recipe_box_links WHERE user_id = ? GROUP BY state`, time.Now().UTC().Unix(), userID); err != nil {
		return status, err
	}
	if err := countInto(ctx, db, status.Mails, `SELECT move_state, COUNT(*) FROM plugin_recipe_box_mails WHERE user_id = ? GROUP BY move_state`, userID); err != nil {
		return status, err
	}
	shelves := []struct {
		shelf, name string
		on          bool
	}{{"recipes", settings.FolderName, settings.MoveEnabled}, {"notes", settings.NotesFolderName, settings.NotesMoveEnabled}}
	for _, shelf := range shelves {
		if !shelf.on {
			continue
		}
		folders, err := folderStatuses(ctx, db, userID, shelf.shelf, shelf.name)
		if err != nil {
			return status, err
		}
		status.Folders = append(status.Folders, folders...)
	}
	return status, nil
}

// folderStatuses is one shelf's folder in every account of the reader.
func folderStatuses(ctx context.Context, db *sql.DB, userID int64, shelf, name string) ([]FolderStatus, error) {
	rows, err := db.QueryContext(ctx, `SELECT a.id, CASE WHEN a.label <> '' THEN a.label ELSE a.email END, COALESCE(mb.id, 0), COALESCE(mb.name, ''), COALESCE(mb.show_in_all_mail, 0)
		FROM mail_accounts a
		LEFT JOIN mailboxes mb ON mb.account_id = a.id AND mb.user_id = a.user_id AND LOWER(mb.name) = LOWER(?)
		WHERE a.user_id = ? ORDER BY a.id`, name, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FolderStatus
	for rows.Next() {
		folder := FolderStatus{Shelf: shelf, Wanted: name}
		var show int
		if err := rows.Scan(&folder.AccountID, &folder.Account, &folder.MailboxID, &folder.Name, &show); err != nil {
			return nil, err
		}
		folder.ShowInAllMail = show != 0
		out = append(out, folder)
	}
	return out, rows.Err()
}

func countInto(ctx context.Context, db *sql.DB, into map[string]int, query string, args ...any) error {
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var key string
		var n int
		if err := rows.Scan(&key, &n); err != nil {
			return err
		}
		if key == "" {
			key = "waiting"
		}
		into[key] = n
	}
	return rows.Err()
}

// apiHideFolder takes one account's recipe folder out of All Mail and the
// other whole-account lists. It is the reader's own switch on the folder
// settings page, offered here for a folder that existed before the box used
// it, because that one kept whatever the reader had set.
func (p *recipeBoxBackend) apiHideFolder(host plugins.APIHost, db *sql.DB, userID int64, w http.ResponseWriter, r *http.Request) {
	var in struct {
		MailboxID int64 `json:"mailbox_id"`
	}
	if !host.DecodeJSON(w, r, &in) {
		return
	}
	settings, err := loadSettings(r.Context(), db, userID)
	if err != nil {
		host.ServerError(w, err)
		return
	}
	found, err := hideFolderFromAllMail(r.Context(), host, db, userID, in.MailboxID, settings.FolderName)
	if err == nil && !found {
		found, err = hideFolderFromAllMail(r.Context(), host, db, userID, in.MailboxID, settings.NotesFolderName)
	}
	if err != nil {
		host.ServerError(w, err)
		return
	}
	if !found {
		host.WriteAPIError(w, http.StatusNotFound, "recipe folder not found")
		return
	}
	if notifier, ok := host.(plugins.UserChangeHost); ok {
		notifier.NotifyUserChanged(userID)
	}
	p.apiSettings(host, db, userID, w, r)
}

// RecipeSummary is one card of the cookbook.
type RecipeSummary struct {
	ID              int64    `json:"id"`
	Title           string   `json:"title"`
	Description     string   `json:"description"`
	SiteName        string   `json:"site_name"`
	URL             string   `json:"url"`
	Yield           string   `json:"yield"`
	TotalMinutes    int      `json:"total_minutes"`
	RatingValue     float64  `json:"rating_value"`
	RatingCount     int      `json:"rating_count"`
	Categories      []string `json:"categories"`
	Cuisines        []string `json:"cuisines"`
	Keywords        []string `json:"keywords"`
	IngredientCount int      `json:"ingredient_count"`
	ImageURL        string   `json:"image_url"`
	Note            string   `json:"note"`
	Origin          string   `json:"origin"`
	Dismissed       bool     `json:"dismissed"`
	FirstSeenAt     int64    `json:"first_seen_at"`
	// CookedAt is when the reader marked the dish as cooked, zero while it is
	// still one to try.
	CookedAt int64 `json:"cooked_at"`
}

// RecipeDetail is a recipe opened.
type RecipeDetail struct {
	RecipeSummary
	Author       string               `json:"author"`
	PrepMinutes  int                  `json:"prep_minutes"`
	CookMinutes  int                  `json:"cook_minutes"`
	Calories     string               `json:"calories"`
	Ingredients  []string             `json:"ingredients"`
	Instructions []recipe.StepSection `json:"instructions"`
	Mails        []RecipeMail         `json:"mails"`
}

// RecipeMail is one note-to-self that shared the recipe, and where it is now.
type RecipeMail struct {
	MessageID int64  `json:"message_id"`
	Subject   string `json:"subject"`
	DateUnix  int64  `json:"date_unix"`
	Note      string `json:"note"`
	MoveState string `json:"move_state"`
	MoveError string `json:"move_error"`
}

const recipeColumns = `r.id, r.title, r.description, r.site_name, r.canonical_url, r.yield, r.total_minutes, r.rating_value, r.rating_count,
	r.categories_json, r.cuisines_json, r.keywords_json, r.ingredients_json, r.image_status, r.updated_at, r.origin, r.dismissed, r.first_seen_at, r.cooked_at,
	COALESCE((SELECT m.note FROM plugin_recipe_box_mails m
		JOIN plugin_recipe_box_mail_links ml ON ml.mail_id = m.id AND ml.user_id = m.user_id
		JOIN plugin_recipe_box_links l ON l.id = ml.link_id AND l.user_id = ml.user_id
		WHERE l.user_id = r.user_id AND l.recipe_id = r.id AND m.note <> '' ORDER BY m.date_unix LIMIT 1), '')`

type rowScanner interface {
	Scan(dest ...any) error
}

func scanSummary(row rowScanner, extra ...any) (RecipeSummary, []string, error) {
	var s RecipeSummary
	var categories, cuisines, keywords, ingredients, imageStatus string
	var updatedAt int64
	var dismissed int
	dest := []any{&s.ID, &s.Title, &s.Description, &s.SiteName, &s.URL, &s.Yield, &s.TotalMinutes, &s.RatingValue, &s.RatingCount,
		&categories, &cuisines, &keywords, &ingredients, &imageStatus, &updatedAt, &s.Origin, &dismissed, &s.FirstSeenAt, &s.CookedAt, &s.Note}
	if err := row.Scan(append(dest, extra...)...); err != nil {
		return s, nil, err
	}
	s.Categories, s.Cuisines, s.Keywords = jsonList(categories), jsonList(cuisines), jsonList(keywords)
	list := jsonList(ingredients)
	s.IngredientCount = len(list)
	s.Dismissed = dismissed != 0
	if imageStatus == "ok" {
		s.ImageURL = "/api/" + apiPath + "/recipes/" + strconv.FormatInt(s.ID, 10) + "/image?v=" + strconv.FormatInt(updatedAt, 10)
	}
	return s, list, nil
}

func jsonList(raw string) []string {
	var out []string
	if err := json.Unmarshal([]byte(raw), &out); err != nil || out == nil {
		return []string{}
	}
	return out
}

// apiListRecipes answers the cookbook, newest find first. A search matches
// every word against what the dish is called, what goes in it and how the site
// files it, and against the reader's own note on the mail that shared it.
func (p *recipeBoxBackend) apiListRecipes(host plugins.APIHost, db *sql.DB, userID int64, w http.ResponseWriter, r *http.Request) {
	query := `SELECT ` + recipeColumns + ` FROM plugin_recipe_box_recipes r WHERE r.user_id = ? `
	view := r.URL.Query().Get("view")
	switch view {
	case "dismissed":
		query += `AND r.dismissed = 1`
	case "cooked":
		query += `AND r.dismissed = 0 AND r.cooked_at > 0`
	default:
		view = ""
		query += `AND r.dismissed = 0 AND r.cooked_at = 0`
	}
	args := []any{userID}
	for _, term := range strings.Fields(strings.ToLower(r.URL.Query().Get("q"))) {
		if len(args) > 12 {
			break
		}
		pattern := "%" + likeEscaper.Replace(term) + "%"
		query += ` AND (r.search_text LIKE ? ESCAPE '\' OR EXISTS (
			SELECT 1 FROM plugin_recipe_box_mails m
			JOIN plugin_recipe_box_mail_links ml ON ml.mail_id = m.id AND ml.user_id = m.user_id
			JOIN plugin_recipe_box_links l ON l.id = ml.link_id AND l.user_id = ml.user_id
			WHERE l.user_id = r.user_id AND l.recipe_id = r.id AND LOWER(m.note || ' ' || m.subject) LIKE ? ESCAPE '\'))`
		args = append(args, pattern, pattern)
	}
	if view == "cooked" {
		// What was made most recently is what a reader looks for there.
		query += ` ORDER BY r.cooked_at DESC, r.id DESC LIMIT ?`
	} else {
		query += ` ORDER BY r.first_seen_at DESC, r.id DESC LIMIT ?`
	}
	args = append(args, recipeListLimit)
	rows, err := db.QueryContext(r.Context(), query, args...)
	if err != nil {
		host.ServerError(w, err)
		return
	}
	defer rows.Close()
	out := []RecipeSummary{}
	for rows.Next() {
		summary, _, err := scanSummary(rows)
		if err != nil {
			host.ServerError(w, err)
			return
		}
		out = append(out, summary)
	}
	if err := rows.Err(); err != nil {
		host.ServerError(w, err)
		return
	}
	counts := map[string]int{"to_cook": 0, "cooked": 0, "dismissed": 0}
	if err := countInto(r.Context(), db, counts, `SELECT CASE WHEN dismissed <> 0 THEN 'dismissed' WHEN cooked_at > 0 THEN 'cooked' ELSE 'to_cook' END AS shelf, COUNT(*)
		FROM plugin_recipe_box_recipes WHERE user_id = ? GROUP BY shelf`, userID); err != nil {
		host.ServerError(w, err)
		return
	}
	host.WriteJSON(w, map[string]any{"recipes": out, "counts": counts})
}

var likeEscaper = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)

func (p *recipeBoxBackend) apiRecipe(host plugins.APIHost, db *sql.DB, userID, id int64, w http.ResponseWriter, r *http.Request) {
	detail, err := loadRecipe(r.Context(), db, userID, id)
	if errors.Is(err, sql.ErrNoRows) {
		host.WriteAPIError(w, http.StatusNotFound, "recipe not found")
		return
	}
	if err != nil {
		host.ServerError(w, err)
		return
	}
	host.WriteJSON(w, map[string]any{"recipe": detail})
}

func loadRecipe(ctx context.Context, db *sql.DB, userID, id int64) (RecipeDetail, error) {
	var d RecipeDetail
	var instructions string
	row := db.QueryRowContext(ctx, `SELECT `+recipeColumns+`, r.author, r.prep_minutes, r.cook_minutes, r.calories, r.instructions_json
		FROM plugin_recipe_box_recipes r WHERE r.user_id = ? AND r.id = ?`, userID, id)
	summary, ingredients, err := scanSummary(row, &d.Author, &d.PrepMinutes, &d.CookMinutes, &d.Calories, &instructions)
	if err != nil {
		return d, err
	}
	d.RecipeSummary = summary
	d.Ingredients = ingredients
	if err := json.Unmarshal([]byte(instructions), &d.Instructions); err != nil || d.Instructions == nil {
		d.Instructions = []recipe.StepSection{}
	}
	d.Mails, err = recipeMails(ctx, db, userID, id)
	return d, err
}

// recipeMails lists the mails that shared a recipe, each resolved to the row
// it is stored as now. A mail is followed by its Message-ID, because the row
// it was found as is gone the moment it is filed and the folder's sync stores
// it again under a new one.
func recipeMails(ctx context.Context, db *sql.DB, userID, recipeID int64) ([]RecipeMail, error) {
	rows, err := db.QueryContext(ctx, `SELECT DISTINCT m.id, m.message_id, m.message_id_header, m.subject, m.date_unix, m.note, m.move_state, m.move_error,
			COALESCE((SELECT MAX(x.id) FROM messages x WHERE x.user_id = m.user_id AND m.message_id_header <> '' AND x.message_id_header = m.message_id_header), 0),
			COALESCE((SELECT x.id FROM messages x WHERE x.user_id = m.user_id AND x.id = m.message_id), 0)
		FROM plugin_recipe_box_mails m
		JOIN plugin_recipe_box_mail_links ml ON ml.mail_id = m.id AND ml.user_id = m.user_id
		JOIN plugin_recipe_box_links l ON l.id = ml.link_id AND l.user_id = ml.user_id
		WHERE m.user_id = ? AND l.recipe_id = ?
		ORDER BY m.date_unix DESC, m.id DESC LIMIT 50`, userID, recipeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []RecipeMail{}
	for rows.Next() {
		var mail RecipeMail
		var id, stored, byHeader, byRow int64
		var header string
		if err := rows.Scan(&id, &stored, &header, &mail.Subject, &mail.DateUnix, &mail.Note, &mail.MoveState, &mail.MoveError, &byHeader, &byRow); err != nil {
			return nil, err
		}
		mail.MessageID = byHeader
		if mail.MessageID == 0 {
			mail.MessageID = byRow
		}
		out = append(out, mail)
	}
	return out, rows.Err()
}

// apiUpdateRecipe marks a recipe cooked or not, and takes it out of the
// cookbook or puts it back. Taking it out also stops the box filing the mail
// behind it; mail it already filed stays where it is, because moving mail back
// is the reader's own act.
func (p *recipeBoxBackend) apiUpdateRecipe(host plugins.APIHost, db *sql.DB, userID, id int64, w http.ResponseWriter, r *http.Request) {
	var in struct {
		Dismissed *bool `json:"dismissed"`
		Cooked    *bool `json:"cooked"`
	}
	if !host.DecodeJSON(w, r, &in) {
		return
	}
	if in.Cooked != nil {
		// updated_at is left alone: it versions the picture URL, and marking a
		// dish cooked does not change its picture.
		cookedAt := int64(0)
		if *in.Cooked {
			cookedAt = time.Now().UTC().Unix()
		}
		res, err := db.ExecContext(r.Context(), `UPDATE plugin_recipe_box_recipes SET cooked_at = ? WHERE user_id = ? AND id = ?`, cookedAt, userID, id)
		if err != nil {
			host.ServerError(w, err)
			return
		}
		if n, _ := res.RowsAffected(); n == 0 {
			host.WriteAPIError(w, http.StatusNotFound, "recipe not found")
			return
		}
	}
	if in.Dismissed != nil {
		res, err := db.ExecContext(r.Context(), `UPDATE plugin_recipe_box_recipes SET dismissed = ?, updated_at = ? WHERE user_id = ? AND id = ?`,
			boolInt(*in.Dismissed), time.Now().UTC().Unix(), userID, id)
		if err != nil {
			host.ServerError(w, err)
			return
		}
		if n, _ := res.RowsAffected(); n == 0 {
			host.WriteAPIError(w, http.StatusNotFound, "recipe not found")
			return
		}
		if !*in.Dismissed {
			p.signal()
		}
	}
	p.apiRecipe(host, db, userID, id, w, r)
}

// apiRecipeImage serves a stored picture. It is served only as the raster type
// its bytes are, sniffed again here rather than trusted from the row, with
// sniffing and scripts switched off: these bytes came from somebody else's
// server and are now answered from Rolltop's own origin.
func (p *recipeBoxBackend) apiRecipeImage(host plugins.APIHost, db *sql.DB, userID, id int64, w http.ResponseWriter, r *http.Request) {
	var data []byte
	err := db.QueryRowContext(r.Context(), `SELECT image_data FROM plugin_recipe_box_recipes WHERE user_id = ? AND id = ? AND image_status = 'ok'`, userID, id).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && len(data) == 0) {
		host.WriteAPIError(w, http.StatusNotFound, "picture not found")
		return
	}
	if err != nil {
		host.ServerError(w, err)
		return
	}
	contentType := recipe.SniffImageType(data)
	if contentType == "" {
		host.WriteAPIError(w, http.StatusNotFound, "picture not found")
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
	w.Header().Set("Cache-Control", "private, max-age=604800")
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	_, _ = w.Write(data)
}

// apiAddRecipe files a page the reader names as a recipe without mailing it to
// themselves first. It is the same link queue the scan feeds, with the reader's
// word already on it: a page that publishes no recipe still becomes one, from
// what it says about itself.
func (p *recipeBoxBackend) apiAddRecipe(host plugins.APIHost, db *sql.DB, userID int64, w http.ResponseWriter, r *http.Request) {
	var in struct {
		URL string `json:"url"`
	}
	if !host.DecodeJSON(w, r, &in) {
		return
	}
	link, ok := recipe.NormalizeLink(in.URL)
	if !ok {
		host.WriteAPIError(w, http.StatusBadRequest, "That is not a web address the recipe box can read.")
		return
	}
	now := time.Now().UTC().Unix()
	var id int64
	var status string
	var recipeID int64
	err := db.QueryRowContext(r.Context(), `INSERT INTO plugin_recipe_box_links (user_id, url, forced, created_at, updated_at) VALUES (?, ?, 1, ?, ?)
		ON CONFLICT (user_id, url) DO UPDATE SET forced = 1,
			status = CASE WHEN plugin_recipe_box_links.status = 'recipe' THEN 'recipe' ELSE 'pending' END,
			attempts = CASE WHEN plugin_recipe_box_links.status = 'recipe' THEN plugin_recipe_box_links.attempts ELSE 0 END,
			next_attempt_at = 0, updated_at = excluded.updated_at
		RETURNING id, status, recipe_id`, userID, clean(link), now, now).Scan(&id, &status, &recipeID)
	if err != nil {
		host.ServerError(w, err)
		return
	}
	if status == linkRecipe && recipeID > 0 {
		// Already in the cookbook. A dismissed recipe the reader adds again is
		// one they changed their mind about.
		if _, err := db.ExecContext(r.Context(), `UPDATE plugin_recipe_box_recipes SET dismissed = 0, updated_at = ? WHERE user_id = ? AND id = ?`, now, userID, recipeID); err != nil {
			host.ServerError(w, err)
			return
		}
	}
	p.signal()
	host.WriteJSON(w, map[string]any{"ok": true, "link_id": id, "status": status, "recipe_id": recipeID})
}

// LinkSummary is one shared link that did not become a recipe, with the
// preview its page gave and the mail it came in.
type LinkSummary struct {
	ID          int64  `json:"id"`
	URL         string `json:"url"`
	FinalURL    string `json:"final_url"`
	Status      string `json:"status"`
	Error       string `json:"error"`
	Attempts    int    `json:"attempts"`
	NextAttempt int64  `json:"next_attempt_at"`
	Title       string `json:"title"`
	Description string `json:"description"`
	ImageURL    string `json:"image_url"`
	SiteName    string `json:"site_name"`
	Subject     string `json:"subject"`
	DateUnix    int64  `json:"date_unix"`
	MessageID   int64  `json:"message_id"`
}

func (p *recipeBoxBackend) apiListLinks(host plugins.APIHost, db *sql.DB, userID int64, w http.ResponseWriter, r *http.Request) {
	status := r.URL.Query().Get("status")
	if status != linkPage && status != linkFailed && status != linkPending {
		host.WriteAPIError(w, http.StatusBadRequest, "unknown link status")
		return
	}
	rows, err := db.QueryContext(r.Context(), `SELECT l.id, l.url, l.final_url, l.status, l.error, l.attempts, l.next_attempt_at, l.title, l.description, l.image_url, l.site_name,
			COALESCE(m.subject, ''), COALESCE(m.date_unix, 0),
			COALESCE((SELECT MAX(x.id) FROM messages x WHERE x.user_id = l.user_id AND m.message_id_header <> '' AND x.message_id_header = m.message_id_header),
				(SELECT x.id FROM messages x WHERE x.user_id = l.user_id AND x.id = m.message_id), 0)
		FROM plugin_recipe_box_links l
		LEFT JOIN LATERAL (
			SELECT pm.subject, pm.date_unix, pm.message_id_header, pm.message_id FROM plugin_recipe_box_mails pm
			JOIN plugin_recipe_box_mail_links ml ON ml.mail_id = pm.id AND ml.user_id = pm.user_id
			WHERE ml.link_id = l.id AND ml.user_id = l.user_id ORDER BY pm.date_unix DESC LIMIT 1
		) m ON TRUE
		WHERE l.user_id = ? AND l.status = ?
		ORDER BY COALESCE(m.date_unix, 0) DESC, l.id DESC LIMIT ?`, userID, status, linkListLimit)
	if err != nil {
		host.ServerError(w, err)
		return
	}
	defer rows.Close()
	out := []LinkSummary{}
	for rows.Next() {
		var link LinkSummary
		if err := rows.Scan(&link.ID, &link.URL, &link.FinalURL, &link.Status, &link.Error, &link.Attempts, &link.NextAttempt, &link.Title,
			&link.Description, &link.ImageURL, &link.SiteName, &link.Subject, &link.DateUnix, &link.MessageID); err != nil {
			host.ServerError(w, err)
			return
		}
		out = append(out, link)
	}
	if err := rows.Err(); err != nil {
		host.ServerError(w, err)
		return
	}
	host.WriteJSON(w, map[string]any{"links": out})
}

// apiPromoteLink is the reader saying a page the box did not recognise is a
// recipe after all. The preview the page already gave is enough to file it at
// once; a page that never answered is asked again with the reader's word on it.
func (p *recipeBoxBackend) apiPromoteLink(host plugins.APIHost, db *sql.DB, userID, id int64, w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var link dueLink
	var status, finalURL string
	var pv preview
	err := db.QueryRowContext(ctx, `SELECT id, url, attempts, status, final_url, title, description, image_url, site_name FROM plugin_recipe_box_links WHERE user_id = ? AND id = ?`, userID, id).
		Scan(&link.ID, &link.URL, &link.Tries, &status, &finalURL, &pv.Title, &pv.Description, &pv.ImageURL, &pv.SiteName)
	if errors.Is(err, sql.ErrNoRows) {
		host.WriteAPIError(w, http.StatusNotFound, "link not found")
		return
	}
	if err != nil {
		host.ServerError(w, err)
		return
	}
	link.Forced = true
	now := time.Now().UTC().Unix()
	if status == linkPage {
		if _, err := db.ExecContext(ctx, `UPDATE plugin_recipe_box_links SET forced = 1, updated_at = ? WHERE user_id = ? AND id = ?`, now, userID, id); err != nil {
			host.ServerError(w, err)
			return
		}
		page := recipe.Page{Title: pv.Title, Description: pv.Description, ImageURL: pv.ImageURL, SiteName: pv.SiteName}
		// The picture is fetched by the loop, not inside this request.
		if err := p.recordPromoted(ctx, db, userID, link, page, finalURL); err != nil {
			host.ServerError(w, err)
			return
		}
	} else {
		if _, err := db.ExecContext(ctx, `UPDATE plugin_recipe_box_links SET forced = 1, status = 'pending', attempts = 0, next_attempt_at = 0, error = '', updated_at = ?
			WHERE user_id = ? AND id = ? AND status <> 'recipe'`, now, userID, id); err != nil {
			host.ServerError(w, err)
			return
		}
	}
	p.signal()
	host.WriteJSON(w, map[string]any{"ok": true})
}

// recordPromoted files a page as a manual recipe from its stored preview.
func (p *recipeBoxBackend) recordPromoted(ctx context.Context, db *sql.DB, userID int64, link dueLink, page recipe.Page, finalURL string) error {
	found := &recipe.Recipe{Name: firstText(page.Title, link.URL), Description: page.Description}
	if page.ImageURL != "" {
		found.ImageURLs = []string{page.ImageURL}
	}
	recipeID, err := upsertRecipe(ctx, db, userID, canonicalURL(link.URL, finalURL, page), page, found, "manual")
	if err != nil {
		return err
	}
	now := time.Now().UTC().Unix()
	if _, err := db.ExecContext(ctx, `UPDATE plugin_recipe_box_links SET status = 'recipe', recipe_id = ?, updated_at = ? WHERE user_id = ? AND id = ?`, recipeID, now, userID, link.ID); err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, `UPDATE plugin_recipe_box_recipes SET dismissed = 0 WHERE user_id = ? AND id = ?`, userID, recipeID); err != nil {
		return err
	}
	return refreshFirstSeen(ctx, db, userID, recipeID)
}

func (p *recipeBoxBackend) apiRetryLink(host plugins.APIHost, db *sql.DB, userID, id int64, w http.ResponseWriter, r *http.Request) {
	res, err := db.ExecContext(r.Context(), `UPDATE plugin_recipe_box_links SET status = 'pending', attempts = 0, next_attempt_at = 0, error = '', updated_at = ?
		WHERE user_id = ? AND id = ? AND status IN ('failed', 'page', 'pending')`, time.Now().UTC().Unix(), userID, id)
	if err != nil {
		host.ServerError(w, err)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		host.WriteAPIError(w, http.StatusNotFound, "link not found")
		return
	}
	p.signal()
	host.WriteJSON(w, map[string]any{"ok": true})
}
