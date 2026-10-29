// File overview: Asking each recorded link's page what it is, and keeping the
// recipe a page publishes.

package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"time"

	"rolltop/backend/mailparse"
	"rolltop/plugins/recipe_box/recipe"
)

const (
	// fetchesPerRound bounds one reader's share of a round. A backfill can
	// queue hundreds of links, and the other readers' new links should not
	// wait behind all of them.
	fetchesPerRound = 15
	// maxFetchAttempts is how often a link that failed for a passing reason --
	// a timeout, a 503, a challenge page -- is tried before it is reported as
	// failed. A reader can still ask for another try from the settings page.
	maxFetchAttempts = 5
	// sameHostSpacing keeps a backfill from asking one site for many pages in
	// a burst: a reader who saved forty recipes from one blog is the ordinary
	// case, and the blog should not see it as a crawl.
	sameHostSpacing = 3 * time.Second
)

// retryDelays is the wait after each failed attempt.
var retryDelays = []time.Duration{10 * time.Minute, time.Hour, 6 * time.Hour, 24 * time.Hour}

// Link statuses. A link is pending until its page answered, then a recipe, a
// page (it answered, and is not a recipe), or failed.
const (
	linkPending = "pending"
	linkRecipe  = "recipe"
	linkPage    = "page"
	linkFailed  = "failed"
)

type dueLink struct {
	ID     int64
	URL    string
	Forced bool
	Tries  int
}

func dueLinks(ctx context.Context, db *sql.DB, userID int64, limit int) ([]dueLink, error) {
	rows, err := db.QueryContext(ctx, `SELECT id, url, forced, attempts FROM plugin_recipe_box_links
		WHERE user_id = ? AND status = 'pending' AND next_attempt_at <= ?
		ORDER BY next_attempt_at, id LIMIT ?`, userID, time.Now().UTC().Unix(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []dueLink
	for rows.Next() {
		var link dueLink
		var forced int
		if err := rows.Scan(&link.ID, &link.URL, &forced, &link.Tries); err != nil {
			return nil, err
		}
		link.Forced = forced != 0
		out = append(out, link)
	}
	return out, rows.Err()
}

// resolveLinks fetches up to fetchesPerRound due links and reports how many it
// answered and whether more are due.
func (p *recipeBoxBackend) resolveLinks(ctx context.Context, db *sql.DB, userID int64) (int, bool, error) {
	links, err := dueLinks(ctx, db, userID, fetchesPerRound+1)
	if err != nil {
		return 0, false, err
	}
	more := len(links) > fetchesPerRound
	if more {
		links = links[:fetchesPerRound]
	}
	done := 0
	for _, link := range links {
		if ctx.Err() != nil {
			return done, true, ctx.Err()
		}
		if recipe.NoFetch(link.URL) && !link.Forced {
			// A document or a map answers a fetch with a sign-in page or a
			// script; its platform is all that can be said about it.
			if err := recordUnfetched(ctx, db, userID, link); err != nil {
				return done, more, err
			}
			done++
			continue
		}
		p.waitForHost(ctx, link.URL)
		page, finalURL, fetchErr := p.fetcher.FetchPage(ctx, link.URL)
		if fetchErr != nil && ctx.Err() != nil {
			return done, true, ctx.Err()
		}
		if err := p.recordAnswer(ctx, db, userID, link, page, finalURL, fetchErr); err != nil {
			return done, more, err
		}
		done++
	}
	return done, more, nil
}

// waitForHost spaces requests to one site out by sameHostSpacing.
func (p *recipeBoxBackend) waitForHost(ctx context.Context, link string) {
	host := hostLabel(link)
	if host == "" {
		return
	}
	p.mu.Lock()
	if p.lastFetch == nil {
		p.lastFetch = map[string]time.Time{}
	}
	last := p.lastFetch[host]
	wait := time.Until(last.Add(p.hostSpacing))
	p.lastFetch[host] = time.Now().Add(max(wait, 0))
	if len(p.lastFetch) > 512 {
		p.lastFetch = map[string]time.Time{host: p.lastFetch[host]}
	}
	p.mu.Unlock()
	if wait <= 0 {
		return
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}

func (p *recipeBoxBackend) recordAnswer(ctx context.Context, db *sql.DB, userID int64, link dueLink, page recipe.Page, finalURL string, fetchErr error) error {
	now := time.Now().UTC()
	if fetchErr != nil {
		var status recipe.HTTPStatusError
		permanent := errors.Is(fetchErr, recipe.ErrNotHTML) || (errors.As(fetchErr, &status) && status.Permanent())
		attempts := link.Tries + 1
		if permanent || attempts >= maxFetchAttempts {
			_, err := db.ExecContext(ctx, `UPDATE plugin_recipe_box_links SET status = 'failed', kind = ?, attempts = ?, error = ?, fetched_at = ?, updated_at = ? WHERE user_id = ? AND id = ?`,
				hostKindOrLink(link.URL), attempts, fetchErrorText(fetchErr), now.Unix(), now.Unix(), userID, link.ID)
			return err
		}
		next := now.Add(retryDelays[min(attempts-1, len(retryDelays)-1)])
		_, err := db.ExecContext(ctx, `UPDATE plugin_recipe_box_links SET attempts = ?, next_attempt_at = ?, error = ?, updated_at = ? WHERE user_id = ? AND id = ?`,
			attempts, next.Unix(), fetchErrorText(fetchErr), now.Unix(), userID, link.ID)
		return err
	}
	preview := previewOf(page)
	if _, err := db.ExecContext(ctx, `UPDATE plugin_recipe_box_links SET final_url = ?, title = ?, description = ?, image_url = ?, site_name = ?, fetched_at = ?, error = '', updated_at = ?
		WHERE user_id = ? AND id = ?`, clean(finalURL), preview.Title, preview.Description, preview.ImageURL, preview.SiteName, now.Unix(), now.Unix(), userID, link.ID); err != nil {
		return err
	}
	var found *recipe.Recipe
	origin := "structured"
	switch {
	case page.Recipe != nil:
		found = page.Recipe
	case link.Forced:
		// The reader said this page is a recipe. What the page offers about
		// itself is all there is to show, and it is shown.
		found = &recipe.Recipe{Name: firstText(preview.Title, link.URL), Description: preview.Description}
		if preview.ImageURL != "" {
			found.ImageURLs = []string{preview.ImageURL}
		}
		origin = "manual"
	default:
		kind, detail := recipe.Classify(page, firstText(finalURL, link.URL))
		_, err := db.ExecContext(ctx, `UPDATE plugin_recipe_box_links SET status = 'page', kind = ?, detail = ?, updated_at = ? WHERE user_id = ? AND id = ?`,
			kind, clean(detail), now.Unix(), userID, link.ID)
		return err
	}
	canonical := canonicalURL(link.URL, finalURL, page)
	recipeID, err := upsertRecipe(ctx, db, userID, canonical, page, found, origin)
	if err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, `UPDATE plugin_recipe_box_links SET status = 'recipe', kind = 'recipe', recipe_id = ?, updated_at = ? WHERE user_id = ? AND id = ?`, recipeID, now.Unix(), userID, link.ID); err != nil {
		return err
	}
	if err := refreshFirstSeen(ctx, db, userID, recipeID); err != nil {
		return err
	}
	return p.ensureImage(ctx, db, userID, recipeID)
}

// recordUnfetched files a link that is not fetched by its platform.
func recordUnfetched(ctx context.Context, db *sql.DB, userID int64, link dueLink) error {
	now := time.Now().UTC().Unix()
	_, err := db.ExecContext(ctx, `UPDATE plugin_recipe_box_links SET status = 'page', kind = ?, detail = '', site_name = CASE WHEN site_name = '' THEN ? ELSE site_name END,
			error = '', fetched_at = ?, updated_at = ? WHERE user_id = ? AND id = ?`,
		hostKindOrLink(link.URL), hostLabel(link.URL), now, now, userID, link.ID)
	return err
}

func hostKindOrLink(link string) string {
	if kind := recipe.HostKind(link); kind != "" {
		return kind
	}
	return recipe.KindLink
}

type preview struct {
	Title, Description, ImageURL, SiteName string
}

func previewOf(page recipe.Page) preview {
	return preview{
		Title:       clean(page.Title),
		Description: clean(page.Description),
		ImageURL:    clean(page.ImageURL),
		SiteName:    clean(page.SiteName),
	}
}

// canonicalURL is what one recipe is known by, so the same dish reached
// through a short link, a tracking redirect and the plain address is one row.
// The page's own canonical link decides when it names the site the fetch
// ended on; a canonical pointing elsewhere is a syndicated copy, and the page
// the reader actually read is kept instead.
func canonicalURL(link, finalURL string, page recipe.Page) string {
	final := firstText(finalURL, link)
	if page.CanonicalURL != "" && sameSite(page.CanonicalURL, final) {
		if normalized, ok := recipe.NormalizeLink(page.CanonicalURL); ok {
			return normalized
		}
	}
	if normalized, ok := recipe.NormalizeLink(final); ok {
		return normalized
	}
	return link
}

func sameSite(a, b string) bool {
	ua, errA := url.Parse(a)
	ub, errB := url.Parse(b)
	if errA != nil || errB != nil {
		return false
	}
	return strings.TrimPrefix(strings.ToLower(ua.Hostname()), "www.") == strings.TrimPrefix(strings.ToLower(ub.Hostname()), "www.")
}

func upsertRecipe(ctx context.Context, db *sql.DB, userID int64, canonical string, page recipe.Page, r *recipe.Recipe, origin string) (int64, error) {
	now := time.Now().UTC().Unix()
	site := firstText(page.SiteName, hostLabel(canonical))
	images := r.ImageURLs
	if len(images) == 0 && page.ImageURL != "" {
		images = []string{page.ImageURL}
	}
	var id int64
	// A recipe the reader already dismissed stays dismissed, and a picture
	// already stored is kept: the page is read again only to refresh the text.
	// A recipe read from structured data is never overwritten by one filed by
	// hand from a preview of the same page.
	err := db.QueryRowContext(ctx, `INSERT INTO plugin_recipe_box_recipes (user_id, canonical_url, title, description, site_name, author, yield,
			total_minutes, prep_minutes, cook_minutes, calories, rating_value, rating_count, ingredients_json, instructions_json,
			categories_json, cuisines_json, keywords_json, image_urls_json, origin, search_text, fetched_at, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (user_id, canonical_url) DO UPDATE SET title = excluded.title, description = excluded.description,
			site_name = excluded.site_name, author = excluded.author, yield = excluded.yield, total_minutes = excluded.total_minutes,
			prep_minutes = excluded.prep_minutes, cook_minutes = excluded.cook_minutes, calories = excluded.calories,
			rating_value = excluded.rating_value, rating_count = excluded.rating_count, ingredients_json = excluded.ingredients_json,
			instructions_json = excluded.instructions_json, categories_json = excluded.categories_json, cuisines_json = excluded.cuisines_json,
			keywords_json = excluded.keywords_json, image_urls_json = excluded.image_urls_json,
			origin = excluded.origin, search_text = excluded.search_text, fetched_at = excluded.fetched_at, updated_at = excluded.updated_at
		WHERE excluded.origin = 'structured' OR plugin_recipe_box_recipes.origin = 'manual'
		RETURNING id`,
		userID, clean(canonical), clean(r.Name), clean(r.Description), clean(site), clean(r.Author), clean(r.Yield),
		r.TotalMinutes, r.PrepMinutes, r.CookMinutes, clean(r.Calories), r.RatingValue, r.RatingCount,
		jsonText(cleanList(r.Ingredients)), jsonText(cleanSections(r.Instructions)), jsonText(cleanList(r.Categories)),
		jsonText(cleanList(r.Cuisines)), jsonText(cleanList(r.Keywords)), jsonText(cleanList(images)), origin,
		searchText(r, site), now, now, now).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		// The page is already in the cookbook from its own structured data, and
		// a preview filed by hand knows less than that: the row stays as it is.
		err = db.QueryRowContext(ctx, `SELECT id FROM plugin_recipe_box_recipes WHERE user_id = ? AND canonical_url = ?`, userID, clean(canonical)).Scan(&id)
	}
	return id, err
}

// refreshFirstSeen dates a recipe by the oldest mail that shared it.
func refreshFirstSeen(ctx context.Context, db *sql.DB, userID, recipeID int64) error {
	_, err := db.ExecContext(ctx, `UPDATE plugin_recipe_box_recipes r SET first_seen_at = COALESCE((
			SELECT MIN(m.date_unix) FROM plugin_recipe_box_mails m
			JOIN plugin_recipe_box_mail_links ml ON ml.mail_id = m.id AND ml.user_id = m.user_id
			JOIN plugin_recipe_box_links l ON l.id = ml.link_id AND l.user_id = ml.user_id
			WHERE l.user_id = r.user_id AND l.recipe_id = r.id AND m.date_unix > 0), r.created_at)
		WHERE r.user_id = ? AND r.id = ?`, userID, recipeID)
	return err
}

// ensureImage stores the recipe's picture when it has none yet. A picture is
// kept rather than linked so the card shows offline, survives the page, and
// opening the cookbook does not tell every recipe site the reader looked.
func (p *recipeBoxBackend) ensureImage(ctx context.Context, db *sql.DB, userID, recipeID int64) error {
	var status, urlsJSON string
	if err := db.QueryRowContext(ctx, `SELECT image_status, image_urls_json FROM plugin_recipe_box_recipes WHERE user_id = ? AND id = ?`, userID, recipeID).Scan(&status, &urlsJSON); err != nil {
		return err
	}
	if status == "ok" {
		return nil
	}
	var urls []string
	_ = json.Unmarshal([]byte(urlsJSON), &urls)
	now := time.Now().UTC().Unix()
	for i, link := range urls {
		if i >= 3 {
			break
		}
		p.waitForHost(ctx, link)
		data, contentType, err := p.fetcher.FetchImage(ctx, link)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			continue
		}
		_, err = db.ExecContext(ctx, `UPDATE plugin_recipe_box_recipes SET image_data = ?, image_type = ?, image_status = 'ok', updated_at = ? WHERE user_id = ? AND id = ?`,
			data, contentType, now, userID, recipeID)
		return err
	}
	_, err := db.ExecContext(ctx, `UPDATE plugin_recipe_box_recipes SET image_status = 'failed', updated_at = ? WHERE user_id = ? AND id = ?`, now, userID, recipeID)
	return err
}

// searchText is what the cookbook's search box reads: the words a reader
// remembers a dish by.
func searchText(r *recipe.Recipe, site string) string {
	parts := []string{r.Name, r.Description, site, r.Author}
	parts = append(parts, r.Ingredients...)
	parts = append(parts, r.Categories...)
	parts = append(parts, r.Cuisines...)
	parts = append(parts, r.Keywords...)
	return clean(strings.ToLower(strings.Join(parts, " \n")))
}

func fetchErrorText(err error) string {
	var status recipe.HTTPStatusError
	switch {
	case errors.As(err, &status):
		return status.Error()
	case errors.Is(err, recipe.ErrNotHTML):
		return err.Error()
	case errors.Is(err, context.DeadlineExceeded) || strings.Contains(err.Error(), "Client.Timeout"):
		return "the page did not answer in time"
	case strings.Contains(err.Error(), "private addresses"):
		return "the link points into a private network"
	}
	return "the page could not be reached"
}

func hostLabel(link string) string {
	u, err := url.Parse(link)
	if err != nil {
		return ""
	}
	return strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.")
}

func clean(value string) string {
	return mailparse.SanitizeText(strings.TrimSpace(value))
}

func cleanList(values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		if value = clean(value); value != "" {
			out = append(out, value)
		}
	}
	return out
}

func cleanSections(sections []recipe.StepSection) []recipe.StepSection {
	out := make([]recipe.StepSection, 0, len(sections))
	for _, section := range sections {
		steps := cleanList(section.Steps)
		if len(steps) > 0 {
			out = append(out, recipe.StepSection{Name: clean(section.Name), Steps: steps})
		}
	}
	return out
}

func jsonText(value any) string {
	raw, err := json.Marshal(value)
	if err != nil {
		return "[]"
	}
	return string(raw)
}

func firstText(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
