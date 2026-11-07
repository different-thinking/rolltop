// File overview: The shopping list -- one list of what to buy, gathered from
// the ingredients of one recipe after another and from what the reader types
// in themselves.
//
// The same thing to buy stands on it once: "400 g Tomaten" from one recipe and
// "400 g Tomaten" from another is one item of 800 g. Which lines add up is
// decided by recipe.ParseIngredient's Key (the same name, written loosely, in
// the same kind of unit); lines that do not add up -- "1 EL Zucker" and
// "100 g Zucker" -- stay two items. Each item keeps where it came from as its
// sources: the recipe, its section and the line as the recipe wrote it.
//
// An item taken from a recipe is named by its index into the recipe's stored
// ingredient list, never by text the browser sends: what lands on the list is
// what the recipe says.

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
	// shoppingSourceLimit bounds the lines behind those items: adding to an
	// item that is there already makes no new item, but it does record a line.
	shoppingSourceLimit = 5000
)

// ShoppingItem is one line of the shopping list: what to buy, how much of it
// in total, and the recipe lines it was added up from.
type ShoppingItem struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
	// Amount is the total in Unit, absent when the lines named no amount
	// ("Salz"). Weights are in grams and volumes in millilitres; the page
	// shows them in kilograms and litres from a thousand up.
	Amount *float64 `json:"amount"`
	Unit   string   `json:"unit"`
	// CheckedAt is when the reader ticked the item off, zero while it is still
	// to buy.
	CheckedAt int64            `json:"checked_at"`
	CreatedAt int64            `json:"created_at"`
	Sources   []ShoppingSource `json:"sources"`
}

// ShoppingSource is one line an item was added up from.
type ShoppingSource struct {
	// RecipeID is the recipe the line was taken from, zero for one the reader
	// typed in themselves.
	RecipeID    int64  `json:"recipe_id"`
	RecipeTitle string `json:"recipe_title"`
	Section     string `json:"section"`
	Text        string `json:"text"`
}

var (
	errShoppingListFull = errors.New("The shopping list is full. Clear what you bought first.")
	errShoppingNotFound = errors.New("item not found")
)

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
		// The item's sources go with it (ON DELETE CASCADE).
		n, err := deleteShopping(r.Context(), db, userID, `AND id = ?`, id)
		if err != nil {
			host.ServerError(w, err)
			return
		}
		if n == 0 {
			host.WriteAPIError(w, http.StatusNotFound, "item not found")
			return
		}
		p.writeShopping(host, db, userID, w, r)
	default:
		host.WriteAPIError(w, http.StatusNotFound, "recipe box route not found")
	}
}

func (p *recipeBoxBackend) writeShopping(host plugins.APIHost, db *sql.DB, userID int64, w http.ResponseWriter, r *http.Request) {
	if err := ensureShoppingFolded(r.Context(), db, userID); err != nil {
		host.ServerError(w, err)
		return
	}
	items, err := loadShopping(r.Context(), db, userID)
	if err != nil {
		host.ServerError(w, err)
		return
	}
	host.WriteJSON(w, map[string]any{"items": items})
}

// loadShopping lists the reader's shopping list in the order its items were
// first written, each with the lines it was added up from.
func loadShopping(ctx context.Context, db *sql.DB, userID int64) ([]ShoppingItem, error) {
	rows, err := db.QueryContext(ctx, `SELECT id, name, amount, has_amount, unit, checked_at, created_at
		FROM plugin_recipe_box_shopping_items WHERE user_id = ? ORDER BY id LIMIT ?`, userID, shoppingListLimit)
	if err != nil {
		return nil, err
	}
	// Closed explicitly below before the next query; the deferred Close is
	// the guard for an early return (Close is safe to call twice).
	defer rows.Close()
	out := []ShoppingItem{}
	index := map[int64]int{}
	for rows.Next() {
		var item ShoppingItem
		var amount float64
		var hasAmount int
		if err := rows.Scan(&item.ID, &item.Name, &amount, &hasAmount, &item.Unit, &item.CheckedAt, &item.CreatedAt); err != nil {
			rows.Close()
			return nil, err
		}
		if hasAmount != 0 {
			item.Amount = &amount
		}
		item.Sources = []ShoppingSource{}
		index[item.ID] = len(out)
		out = append(out, item)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sources, err := db.QueryContext(ctx, `SELECT item_id, recipe_id, recipe_title, section, text
		FROM plugin_recipe_box_shopping_sources WHERE user_id = ? ORDER BY id`, userID)
	if err != nil {
		return nil, err
	}
	defer sources.Close()
	for sources.Next() {
		var itemID int64
		var source ShoppingSource
		if err := sources.Scan(&itemID, &source.RecipeID, &source.RecipeTitle, &source.Section, &source.Text); err != nil {
			return nil, err
		}
		if i, ok := index[itemID]; ok {
			out[i].Sources = append(out[i].Sources, source)
		}
	}
	return out, sources.Err()
}

// shoppingEntry is one line about to be added.
type shoppingEntry struct {
	section, text string
}

// apiAddShopping puts ingredients of one recipe, or one item of the reader's
// own, on the list. Ingredients are named by their index in the recipe; a
// heading is not an ingredient and is refused.
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
		in.RecipeID = 0
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

// beginShopping opens a transaction holding the reader's shopping lock. Every
// change that adds up items reads before it writes, and two devices changing
// the list at once would otherwise both miss each other's row and write the
// same item twice, or both pass the limits. The two-key form keeps clear of
// the one-key locks the store takes; two readers whose ids share a key only
// wait on each other.
func beginShopping(ctx context.Context, db *sql.DB, userID int64) (*sql.Tx, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtext('plugin_recipe_box_shopping'), (?::bigint % 2147483647)::integer)`, userID); err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	return tx, nil
}

// addShoppingEntries adds lines to the list in one transaction. A recipe's
// line already behind an item still to buy -- the same recipe, section and
// text, case aside -- is not counted a second time: adding a recipe twice does
// not double it. A line the reader types in always counts. Otherwise the line adds to the item still to buy that it adds up with,
// or becomes an item of its own. Items already bought are never added to:
// what is in the cart was bought for the recipes before.
func addShoppingEntries(ctx context.Context, db *sql.DB, userID, recipeID int64, title string, entries []shoppingEntry) error {
	tx, err := beginShopping(ctx, db, userID)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().UTC().Unix()
	if err := foldLegacyShopping(ctx, tx, userID); err != nil {
		return err
	}
	var items, sources int
	if err := tx.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM plugin_recipe_box_shopping_items WHERE user_id = ?),
		(SELECT COUNT(*) FROM plugin_recipe_box_shopping_sources WHERE user_id = ?)`, userID, userID).Scan(&items, &sources); err != nil {
		return err
	}
	for _, entry := range entries {
		// Only a recipe's line counts once: what the reader types in again
		// is more of it ("500 g Tomaten" twice is a kilo).
		if recipeID > 0 {
			var listed bool
			if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM plugin_recipe_box_shopping_sources s
				JOIN plugin_recipe_box_shopping_items i ON i.id = s.item_id AND i.user_id = s.user_id
				WHERE s.user_id = ? AND s.recipe_id = ? AND s.section = ? AND LOWER(s.text) = LOWER(?) AND i.checked_at = 0)`,
				userID, recipeID, entry.section, entry.text).Scan(&listed); err != nil {
				return err
			}
			if listed {
				continue
			}
		}
		if sources >= shoppingSourceLimit {
			return errShoppingListFull
		}
		ingredient := recipe.ParseIngredient(entry.text)
		itemID, found, err := addToMatchingItem(ctx, tx, userID, ingredient, false, now)
		if err != nil {
			return err
		}
		if !found {
			if items >= shoppingListLimit {
				return errShoppingListFull
			}
			if err := tx.QueryRowContext(ctx, `INSERT INTO plugin_recipe_box_shopping_items (user_id, text, name, amount, has_amount, unit, merge_key, created_at, updated_at)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?) RETURNING id`, userID, ingredient.Name, ingredient.Name, ingredient.Amount, boolInt(ingredient.HasAmount),
				ingredient.Unit, ingredient.Key, now, now).Scan(&itemID); err != nil {
				return err
			}
			items++
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO plugin_recipe_box_shopping_sources (user_id, item_id, recipe_id, recipe_title, section, text, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)`, userID, itemID, recipeID, title, entry.section, entry.text, now); err != nil {
			return err
		}
		sources++
	}
	// Putting a recipe's ingredients on the list plans it: it leads the
	// cookbook until it is cooked, and a cooked recipe shopped for again
	// leaves the cooked shelf. This holds even when every line was on the
	// list already -- the reader still means to cook it.
	if recipeID > 0 {
		if _, err := tx.ExecContext(ctx, `UPDATE plugin_recipe_box_recipes SET planned_at = ?, cooked_at = 0 WHERE user_id = ? AND id = ?`, now, userID, recipeID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// addToMatchingItem adds an ingredient's amount to the item it adds up with,
// among the items still to buy or, with bought, among those in the cart. It
// reports false when there is none.
func addToMatchingItem(ctx context.Context, tx *sql.Tx, userID int64, ingredient recipe.Ingredient, bought bool, now int64) (int64, bool, error) {
	state := `checked_at = 0`
	if bought {
		state = `checked_at > 0`
	}
	var id int64
	var amount float64
	var name string
	err := tx.QueryRowContext(ctx, `SELECT id, amount, name FROM plugin_recipe_box_shopping_items
		WHERE user_id = ? AND merge_key = ? AND `+state+` ORDER BY id LIMIT 1`, userID, ingredient.Key).Scan(&id, &amount, &name)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	total, name := addUp(amount, name, ingredient.Amount, ingredient.Name, ingredient.HasAmount)
	if _, err := tx.ExecContext(ctx, `UPDATE plugin_recipe_box_shopping_items SET amount = ?, name = ?, text = ?, updated_at = ? WHERE user_id = ? AND id = ?`,
		total, name, name, now, userID, id); err != nil {
		return 0, false, err
	}
	return id, true, nil
}

// addUp adds an amount to an item's total and decides which name the item
// keeps. Two names that add up differ by singular and plural ("Ei"/"Eier",
// "Tomate"/"Tomaten"), so for a total above one the longer name wins -- the
// plural, mostly -- and for one or less the name of whichever side brings
// more. "1 Ei" and "2 Eier" read "3 Eier" whichever came first. Without
// amounts the item keeps its name.
func addUp(total float64, name string, amount float64, otherName string, hasAmount bool) (float64, string) {
	if !hasAmount {
		return total, name
	}
	sum := total + amount
	switch length, other := utf8.RuneCountInString(name), utf8.RuneCountInString(otherName); {
	case sum > 1 && other > length:
		name = otherName
	case sum <= 1 && amount > total:
		name = otherName
	case other == length && startsLower(name) && !startsLower(otherName):
		// The same word, once typed in lower case: the recipe's spelling wins.
		name = otherName
	}
	return sum, name
}

func startsLower(text string) bool {
	for _, r := range text {
		return unicode.IsLower(r)
	}
	return false
}

// ensureShoppingFolded brings a list written before items were added up into
// the shape this build reads, once. It costs one EXISTS when there is nothing
// to fold.
func ensureShoppingFolded(ctx context.Context, db *sql.DB, userID int64) error {
	var pending bool
	if err := db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM plugin_recipe_box_shopping_items WHERE user_id = ? AND merge_key = '')`, userID).Scan(&pending); err != nil {
		return err
	}
	if !pending {
		return nil
	}
	tx, err := beginShopping(ctx, db, userID)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := foldLegacyShopping(ctx, tx, userID); err != nil {
		return err
	}
	return tx.Commit()
}

// foldLegacyShopping is the second half of migration 004. Every item written
// before items were added up has an empty merge_key and carries its one line
// in the old columns. Each becomes a source of its own, is read with
// recipe.ParseIngredient, and is merged into the item it adds up with -- open
// with open, bought with bought -- or keeps its row in the new shape. The
// items are taken in the order they were written, so the first of a group is
// the one that stays. The caller holds the reader's shopping lock.
func foldLegacyShopping(ctx context.Context, tx *sql.Tx, userID int64) error {
	type legacy struct {
		id, recipeID, checkedAt, createdAt int64
		title, section, text               string
	}
	rows, err := tx.QueryContext(ctx, `SELECT id, recipe_id, recipe_title, section, text, checked_at, created_at
		FROM plugin_recipe_box_shopping_items WHERE user_id = ? AND merge_key = '' ORDER BY id`, userID)
	if err != nil {
		return err
	}
	// Closed explicitly before the writes below, which need the
	// transaction's connection; the deferred Close guards early returns.
	defer rows.Close()
	var pending []legacy
	for rows.Next() {
		var item legacy
		if err := rows.Scan(&item.id, &item.recipeID, &item.title, &item.section, &item.text, &item.checkedAt, &item.createdAt); err != nil {
			rows.Close()
			return err
		}
		pending = append(pending, item)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if err := rows.Err(); err != nil {
		return err
	}
	now := time.Now().UTC().Unix()
	for _, item := range pending {
		if _, err := tx.ExecContext(ctx, `INSERT INTO plugin_recipe_box_shopping_sources (user_id, item_id, recipe_id, recipe_title, section, text, created_at)
			SELECT ?, ?, ?, ?, ?, ?, ? WHERE NOT EXISTS (SELECT 1 FROM plugin_recipe_box_shopping_sources WHERE user_id = ? AND item_id = ?)`,
			userID, item.id, item.recipeID, item.title, item.section, item.text, item.createdAt, userID, item.id); err != nil {
			return err
		}
		ingredient := recipe.ParseIngredient(item.text)
		target, found, err := addToMatchingItem(ctx, tx, userID, ingredient, item.checkedAt > 0, now)
		if err != nil {
			return err
		}
		if found {
			if err := moveSourcesAndDelete(ctx, tx, userID, item.id, target); err != nil {
				return err
			}
			continue
		}
		if _, err := tx.ExecContext(ctx, `UPDATE plugin_recipe_box_shopping_items SET name = ?, amount = ?, has_amount = ?, unit = ?, merge_key = ?, updated_at = ?
			WHERE user_id = ? AND id = ?`, ingredient.Name, ingredient.Amount, boolInt(ingredient.HasAmount), ingredient.Unit, ingredient.Key, now, userID, item.id); err != nil {
			return err
		}
	}
	return nil
}

// moveSourcesAndDelete hands an item's lines to the item it was merged into
// and removes it.
func moveSourcesAndDelete(ctx context.Context, tx *sql.Tx, userID, from, to int64) error {
	if _, err := tx.ExecContext(ctx, `UPDATE plugin_recipe_box_shopping_sources SET item_id = ? WHERE user_id = ? AND item_id = ?`, to, userID, from); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `DELETE FROM plugin_recipe_box_shopping_items WHERE user_id = ? AND id = ?`, userID, from)
	return err
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

// apiUpdateShopping ticks an item off or puts it back. An item that changes
// sides joins the item it adds up with on its new side, so the list never
// shows the same thing to buy twice -- a bought item put back next to more of
// the same added since stands there once, with both amounts.
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
	err := setShoppingChecked(r.Context(), db, userID, id, *in.Checked)
	if errors.Is(err, errShoppingNotFound) {
		host.WriteAPIError(w, http.StatusNotFound, "item not found")
		return
	}
	if err != nil {
		host.ServerError(w, err)
		return
	}
	p.writeShopping(host, db, userID, w, r)
}

func setShoppingChecked(ctx context.Context, db *sql.DB, userID, id int64, checked bool) error {
	tx, err := beginShopping(ctx, db, userID)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := foldLegacyShopping(ctx, tx, userID); err != nil {
		return err
	}
	var key, name string
	var amount float64
	var hasAmount int
	err = tx.QueryRowContext(ctx, `SELECT merge_key, name, amount, has_amount FROM plugin_recipe_box_shopping_items WHERE user_id = ? AND id = ?`, userID, id).
		Scan(&key, &name, &amount, &hasAmount)
	if errors.Is(err, sql.ErrNoRows) {
		// A fold can have merged the item the page still shows into another.
		return errShoppingNotFound
	}
	if err != nil {
		return err
	}
	now := time.Now().UTC().Unix()
	checkedAt := int64(0)
	if checked {
		checkedAt = now
	}
	moved := recipe.Ingredient{Key: key, Name: name, Amount: amount, HasAmount: hasAmount != 0}
	// Leave the item out of the search first, so it never finds itself.
	if _, err := tx.ExecContext(ctx, `UPDATE plugin_recipe_box_shopping_items SET checked_at = -1 WHERE user_id = ? AND id = ?`, userID, id); err != nil {
		return err
	}
	target, found, err := addToMatchingItem(ctx, tx, userID, moved, checked, now)
	if err != nil {
		return err
	}
	if found {
		if err := moveSourcesAndDelete(ctx, tx, userID, id, target); err != nil {
			return err
		}
	} else if _, err := tx.ExecContext(ctx, `UPDATE plugin_recipe_box_shopping_items SET checked_at = ?, updated_at = ? WHERE user_id = ? AND id = ?`,
		checkedAt, now, userID, id); err != nil {
		return err
	}
	return tx.Commit()
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
	filter := ``
	if !in.All {
		filter = `AND checked_at > 0`
	}
	if _, err := deleteShopping(r.Context(), db, userID, filter); err != nil {
		host.ServerError(w, err)
		return
	}
	p.writeShopping(host, db, userID, w, r)
}

// deleteShopping removes items under the reader's shopping lock: an add that
// is merging into an item must not have it deleted between finding it and
// recording its line there. The lines go with their items (ON DELETE CASCADE).
func deleteShopping(ctx context.Context, db *sql.DB, userID int64, filter string, args ...any) (int64, error) {
	tx, err := beginShopping(ctx, db, userID)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `DELETE FROM plugin_recipe_box_shopping_items WHERE user_id = ? `+filter, append([]any{userID}, args...)...)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, tx.Commit()
}
