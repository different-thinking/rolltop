// File overview: The shopping list -- ingredients the reader picked from one
// recipe after another, and whatever else they typed in themselves.
//
// An item taken from a recipe is named by its index into the recipe's stored
// ingredient list, never by text the browser sends: what lands on the list is
// what the recipe says, under the section it stands in. The recipe's name and
// the section are copied onto the item, so the list keeps saying where an
// item came from after the recipe left the cookbook.

package main

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"rolltop/backend/plugins"
	"rolltop/plugins/recipe_box/recipe"
)

const (
	// shoppingItemRunes bounds one item the reader types in.
	shoppingItemRunes = 300
	// shoppingListLimit bounds how many items the list holds at all, ticked
	// off or not, so a runaway client cannot grow it without end.
	shoppingListLimit = 1000
)

// ShoppingItem is one line of the shopping list.
type ShoppingItem struct {
	ID int64 `json:"id"`
	// RecipeID is the recipe the item was taken from, zero for an item the
	// reader typed in themselves.
	RecipeID    int64  `json:"recipe_id"`
	RecipeTitle string `json:"recipe_title"`
	Section     string `json:"section"`
	Text        string `json:"text"`
	// CheckedAt is when the reader ticked the item off, zero while it is still
	// to buy.
	CheckedAt int64 `json:"checked_at"`
	CreatedAt int64 `json:"created_at"`
}

var errShoppingListFull = errors.New("The shopping list is full. Clear what you bought first.")

func (p *recipeBoxBackend) apiShopping(host plugins.APIHost, db *sql.DB, userID int64, parts []string, w http.ResponseWriter, r *http.Request) {
	switch {
	case len(parts) == 1 && r.Method == http.MethodGet:
		p.writeShopping(host, db, userID, w, r)
	case len(parts) == 1 && r.Method == http.MethodPost:
		p.apiAddShopping(host, db, userID, w, r)
	case len(parts) == 2 && parts[1] == "clear" && r.Method == http.MethodPost:
		p.apiClearShopping(host, db, userID, w, r)
	case len(parts) == 2 && r.Method == http.MethodPost:
		id, ok := parseID(host, w, parts[1])
		if !ok {
			return
		}
		p.apiUpdateShopping(host, db, userID, id, w, r)
	case len(parts) == 3 && parts[2] == "delete" && r.Method == http.MethodPost:
		id, ok := parseID(host, w, parts[1])
		if !ok {
			return
		}
		res, err := db.ExecContext(r.Context(), `DELETE FROM plugin_recipe_box_shopping_items WHERE user_id = ? AND id = ?`, userID, id)
		if err != nil {
			host.ServerError(w, err)
			return
		}
		if n, _ := res.RowsAffected(); n == 0 {
			host.WriteAPIError(w, http.StatusNotFound, "item not found")
			return
		}
		p.writeShopping(host, db, userID, w, r)
	default:
		host.WriteAPIError(w, http.StatusNotFound, "recipe box route not found")
	}
}

func (p *recipeBoxBackend) writeShopping(host plugins.APIHost, db *sql.DB, userID int64, w http.ResponseWriter, r *http.Request) {
	items, err := loadShopping(r.Context(), db, userID)
	if err != nil {
		host.ServerError(w, err)
		return
	}
	host.WriteJSON(w, map[string]any{"items": items})
}

// loadShopping lists the reader's shopping list in the order it was written.
func loadShopping(ctx context.Context, db *sql.DB, userID int64) ([]ShoppingItem, error) {
	rows, err := db.QueryContext(ctx, `SELECT id, recipe_id, recipe_title, section, text, checked_at, created_at
		FROM plugin_recipe_box_shopping_items WHERE user_id = ? ORDER BY id LIMIT ?`, userID, shoppingListLimit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ShoppingItem{}
	for rows.Next() {
		var item ShoppingItem
		if err := rows.Scan(&item.ID, &item.RecipeID, &item.RecipeTitle, &item.Section, &item.Text, &item.CheckedAt, &item.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

// shoppingEntry is one item about to be written.
type shoppingEntry struct {
	section, text string
}

// apiAddShopping puts ingredients of one recipe, or one item of the reader's
// own, on the list. Ingredients are named by their index in the recipe; a
// heading is not an ingredient and is refused. Adding what is on the list
// already does not write it twice: an item ticked off is put back to buy.
func (p *recipeBoxBackend) apiAddShopping(host plugins.APIHost, db *sql.DB, userID int64, w http.ResponseWriter, r *http.Request) {
	var in struct {
		RecipeID int64  `json:"recipe_id"`
		Indexes  []int  `json:"indexes"`
		Text     string `json:"text"`
	}
	if !host.DecodeJSON(w, r, &in) {
		return
	}
	ctx := r.Context()
	var title string
	var entries []shoppingEntry
	if in.RecipeID > 0 {
		var ingredientsJSON string
		err := db.QueryRowContext(ctx, `SELECT title, ingredients_json FROM plugin_recipe_box_recipes WHERE user_id = ? AND id = ?`, userID, in.RecipeID).
			Scan(&title, &ingredientsJSON)
		if errors.Is(err, sql.ErrNoRows) {
			host.WriteAPIError(w, http.StatusNotFound, "recipe not found")
			return
		}
		if err != nil {
			host.ServerError(w, err)
			return
		}
		entries, err = recipeShoppingEntries(jsonList(ingredientsJSON), in.Indexes)
		if err != nil {
			host.WriteAPIError(w, http.StatusBadRequest, err.Error())
			return
		}
	} else {
		text := cleanShoppingText(in.Text)
		if text == "" {
			host.WriteAPIError(w, http.StatusBadRequest, "Write what you want to buy.")
			return
		}
		if utf8.RuneCountInString(text) > shoppingItemRunes {
			host.WriteAPIError(w, http.StatusBadRequest, "That item is too long for the shopping list.")
			return
		}
		entries = []shoppingEntry{{text: text}}
	}
	if err := addShoppingEntries(ctx, db, userID, in.RecipeID, title, entries); err != nil {
		if errors.Is(err, errShoppingListFull) {
			host.WriteAPIError(w, http.StatusBadRequest, err.Error())
			return
		}
		host.ServerError(w, err)
		return
	}
	p.writeShopping(host, db, userID, w, r)
}

// recipeShoppingEntries reads the chosen ingredients out of a recipe's stored
// list, each with the section it stands in. An index out of range or naming a
// heading is refused rather than skipped: the browser asked for something the
// recipe does not have, and adding the rest would hide that.
func recipeShoppingEntries(lines []string, indexes []int) ([]shoppingEntry, error) {
	if len(indexes) == 0 {
		return nil, errors.New("Choose the ingredients to put on the shopping list.")
	}
	byIndex := map[int]shoppingEntry{}
	for _, section := range recipe.IngredientSections(lines) {
		for _, item := range section.Items {
			byIndex[item.Index] = shoppingEntry{section: section.Name, text: cleanShoppingText(item.Text)}
		}
	}
	seen := map[int]bool{}
	var entries []shoppingEntry
	for _, index := range indexes {
		entry, ok := byIndex[index]
		if !ok {
			return nil, errors.New("That ingredient is not part of the recipe.")
		}
		if seen[index] {
			continue
		}
		seen[index] = true
		entries = append(entries, entry)
	}
	return entries, nil
}

// addShoppingEntries writes items in one transaction. An item counts as on the
// list already when the same source gave the same text in the same section,
// compared without case; that one is put back to buy instead of written a
// second time. The section is part of it because a recipe can need the same
// line twice ("1 pinch of salt" for the dough and again for the topping).
//
// Adds of one reader are serialized on an advisory lock: the check and the
// insert are two statements, and two devices adding at once would otherwise
// both miss the other's row and write the item twice, or both pass the limit.
func addShoppingEntries(ctx context.Context, db *sql.DB, userID, recipeID int64, title string, entries []shoppingEntry) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// The two-key form keeps clear of the one-key locks the store takes; two
	// readers whose ids share a key only wait on each other.
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtext('plugin_recipe_box_shopping'), (?::bigint % 2147483647)::integer)`, userID); err != nil {
		return err
	}
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM plugin_recipe_box_shopping_items WHERE user_id = ?`, userID).Scan(&count); err != nil {
		return err
	}
	now := time.Now().UTC().Unix()
	for _, entry := range entries {
		res, err := tx.ExecContext(ctx, `UPDATE plugin_recipe_box_shopping_items SET checked_at = 0, recipe_title = ?, updated_at = ?
			WHERE user_id = ? AND recipe_id = ? AND section = ? AND LOWER(text) = LOWER(?)`, title, now, userID, recipeID, entry.section, entry.text)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			continue
		}
		if count >= shoppingListLimit {
			return errShoppingListFull
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO plugin_recipe_box_shopping_items (user_id, recipe_id, recipe_title, section, text, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)`, userID, recipeID, title, entry.section, entry.text, now, now); err != nil {
			return err
		}
		count++
	}
	return tx.Commit()
}

// cleanShoppingText keeps an item on one line: whitespace runs, line breaks
// included, become one space, and control characters are dropped.
func cleanShoppingText(text string) string {
	text = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) && !unicode.IsSpace(r) {
			return -1
		}
		return r
	}, text)
	return strings.Join(strings.Fields(text), " ")
}

func (p *recipeBoxBackend) apiUpdateShopping(host plugins.APIHost, db *sql.DB, userID, id int64, w http.ResponseWriter, r *http.Request) {
	var in struct {
		Checked *bool `json:"checked"`
	}
	if !host.DecodeJSON(w, r, &in) {
		return
	}
	if in.Checked == nil {
		host.WriteAPIError(w, http.StatusBadRequest, "nothing to change")
		return
	}
	now := time.Now().UTC().Unix()
	checkedAt := int64(0)
	if *in.Checked {
		checkedAt = now
	}
	res, err := db.ExecContext(r.Context(), `UPDATE plugin_recipe_box_shopping_items SET checked_at = ?, updated_at = ? WHERE user_id = ? AND id = ?`, checkedAt, now, userID, id)
	if err != nil {
		host.ServerError(w, err)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		host.WriteAPIError(w, http.StatusNotFound, "item not found")
		return
	}
	p.writeShopping(host, db, userID, w, r)
}

// apiClearShopping takes the items ticked off the list, or with `all` every
// item.
func (p *recipeBoxBackend) apiClearShopping(host plugins.APIHost, db *sql.DB, userID int64, w http.ResponseWriter, r *http.Request) {
	var in struct {
		All bool `json:"all"`
	}
	if !host.DecodeJSON(w, r, &in) {
		return
	}
	query := `DELETE FROM plugin_recipe_box_shopping_items WHERE user_id = ?`
	if !in.All {
		query += ` AND checked_at > 0`
	}
	if _, err := db.ExecContext(r.Context(), query, userID); err != nil {
		host.ServerError(w, err)
		return
	}
	p.writeShopping(host, db, userID, w, r)
}
