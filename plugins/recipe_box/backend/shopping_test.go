package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"rolltop/backend/store"
)

// A recipe in two parts, the way sites write parts into recipeIngredient.
const appleCakeMarkup = `<html><head><script type="application/ld+json">{"@type":"Recipe","name":"Apfelkuchen",
"recipeIngredient":["Für den Teig:","250 g Mehl","125 g Butter","Für den Belag:","500 g Äpfel","1 EL Zucker","Für die Streusel:","1 EL Zucker"],"recipeInstructions":"Backen."}</script></head></html>`

// Two more recipes that share ingredients with each other and with the cake.
const tomatoSauceMarkup = `<html><head><script type="application/ld+json">{"@type":"Recipe","name":"Tomatensoße",
"recipeIngredient":["400 g Tomaten","1 Ei","1 kg Mehl"],"recipeInstructions":"Kochen."}</script></head></html>`

const saladMarkup = `<html><head><script type="application/ld+json">{"@type":"Recipe","name":"Salat",
"recipeIngredient":["400 g Tomaten, gewürfelt","2 Eier","Salz"],"recipeInstructions":"Mischen."}</script></head></html>`

func shoppingServer(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	pages := map[string]string{"/carbonara": carbonaraMarkup, "/kuchen": appleCakeMarkup, "/sosse": tomatoSauceMarkup, "/salat": saladMarkup}
	for path, markup := range pages {
		markup := markup
		mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte(markup))
		})
	}
	mux.HandleFunc("/carbonara.png", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("\x89PNG\r\n\x1a\nimage"))
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

func shoppingCall(t *testing.T, p *recipeBoxBackend, host *fakeHost, method, path, body string) (int, []ShoppingItem) {
	t.Helper()
	rec := callAPI(t, p, host, method, apiPath+path, body)
	var out struct {
		Items []ShoppingItem `json:"items"`
	}
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("%s %s: %v %s", method, path, err, rec.Body.String())
		}
	}
	return rec.Code, out.Items
}

// itemTexts prints the list as a reader sees it: amount, unit and name, with
// the recipes behind each item and [x] for what is in the cart.
func itemTexts(items []ShoppingItem) []string {
	out := []string{}
	for _, item := range items {
		line := item.Name
		if item.Amount != nil {
			parts := []string{strconv.FormatFloat(*item.Amount, 'f', -1, 64)}
			if item.Unit != "" {
				parts = append(parts, item.Unit)
			}
			line = strings.Join(append(parts, item.Name), " ")
		}
		var titles []string
		for _, source := range item.Sources {
			if source.RecipeTitle != "" {
				titles = append(titles, source.RecipeTitle)
			}
		}
		if len(titles) > 0 {
			line += " <" + strings.Join(titles, ",") + ">"
		}
		if item.CheckedAt > 0 {
			line += " [x]"
		}
		out = append(out, line)
	}
	return out
}

func shoppingFixture(t *testing.T) (*store.Store, *recipeBoxBackend, *fakeHost, map[string]int64) {
	t.Helper()
	st := openRecipeStore(t)
	server := shoppingServer(t)
	f := newFixture(t, st, "cook@example.test")
	for i, path := range []string{"/carbonara", "/kuchen", "/sosse", "/salat"} {
		createMessage(t, st, f.user.ID, f.account.ID, f.inbox.ID, fmt.Sprintf("<r%d@x>", i), "cook@example.test", "cook@example.test", "x", server.URL+path, time.Now())
	}
	if _, err := saveSettings(context.Background(), st.DB(), f.user.ID, Settings{Enabled: true, OwnRecipientsOnly: true}); err != nil {
		t.Fatal(err)
	}
	p := testBackend()
	host := &fakeHost{t: t, store: st, userID: f.user.ID}
	drain(t, p, host, f.user.ID)
	ids := map[string]int64{}
	for _, title := range []string{"Spaghetti Carbonara", "Apfelkuchen", "Tomatensoße", "Salat"} {
		ids[title] = recipeIDByTitle(t, st.DB(), f.user.ID, title)
	}
	return st, p, host, ids
}

func TestShoppingListAddsUpWhatRecipesShare(t *testing.T) {
	st, p, host, ids := shoppingFixture(t)
	add := func(title string, indexes string) []ShoppingItem {
		t.Helper()
		code, items := shoppingCall(t, p, host, http.MethodPost, "/shopping", fmt.Sprintf(`{"recipe_id":%d,"indexes":[%s]}`, ids[title], indexes))
		if code != http.StatusOK {
			t.Fatalf("add %s %s = %d", title, indexes, code)
		}
		return items
	}
	expect := func(items []ShoppingItem, want string) {
		t.Helper()
		if got := strings.Join(itemTexts(items), " | "); got != want {
			t.Fatalf("list =\n  %s\nwant\n  %s", got, want)
		}
	}

	// The recipe page shows the parts with each ingredient's index.
	rec := callAPI(t, p, host, http.MethodGet, fmt.Sprintf("%s/recipes/%d", apiPath, ids["Apfelkuchen"]), "")
	var detail struct {
		Recipe RecipeDetail `json:"recipe"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatal(err)
	}
	if got := detail.Recipe.IngredientSections; len(got) != 3 || got[1].Name != "Für den Belag" || got[1].Items[0].Index != 4 {
		t.Fatalf("sections = %+v", got)
	}

	// Two recipes that each need 400 g of tomatoes need 800 g; a note after a
	// comma does not make them two things, and 1 Ei and 2 Eier are 3 Eier.
	add("Tomatensoße", "0,1,2")
	items := add("Salat", "0,1,2")
	expect(items, "800 g Tomaten <Tomatensoße,Salat> | 3 Eier <Tomatensoße,Salat> | 1000 g Mehl <Tomatensoße> | Salz <Salat>")

	// Kilograms and grams add up; a recipe added twice is not counted twice.
	add("Apfelkuchen", "1")
	items = add("Tomatensoße", "0,1,2")
	expect(items, "800 g Tomaten <Tomatensoße,Salat> | 3 Eier <Tomatensoße,Salat> | 1250 g Mehl <Tomatensoße,Apfelkuchen> | Salz <Salat>")

	// The same line in two sections of one recipe is needed twice.
	items = add("Apfelkuchen", "5,7")
	if last := items[len(items)-1]; last.Name != "Zucker" || last.Amount == nil || *last.Amount != 2 || last.Unit != "EL" || len(last.Sources) != 2 ||
		last.Sources[0].Section != "Für den Belag" || last.Sources[1].Section != "Für die Streusel" {
		t.Fatalf("sugar = %+v", last)
	}

	// What the reader types in adds up the same way, one line each.
	code, items := shoppingCall(t, p, host, http.MethodPost, "/shopping", `{"text":"  500 g\n tomaten "}`)
	if code != http.StatusOK || itemTexts(items)[0] != "1300 g Tomaten <Tomatensoße,Salat>" || items[0].Sources[2].Text != "500 g tomaten" || items[0].Sources[2].RecipeID != 0 {
		t.Fatalf("own tomatoes = %d %v", code, itemTexts(items))
	}
	for _, body := range []string{`{"text":"   "}`, fmt.Sprintf(`{"recipe_id":%d,"indexes":[3]}`, ids["Apfelkuchen"]), fmt.Sprintf(`{"recipe_id":%d,"indexes":[99]}`, ids["Apfelkuchen"]), fmt.Sprintf(`{"recipe_id":%d,"indexes":[]}`, ids["Apfelkuchen"])} {
		if code, _ := shoppingCall(t, p, host, http.MethodPost, "/shopping", body); code != http.StatusBadRequest {
			t.Fatalf("add %s = %d", body, code)
		}
	}

	// What is in the cart was bought for the recipes before: more of it goes
	// on the list as an item to buy, and the cart item put back joins it.
	tomatoes := items[0].ID
	if code, items = shoppingCall(t, p, host, http.MethodPost, fmt.Sprintf("/shopping/%d", tomatoes), `{"checked":true}`); code != http.StatusOK || items[0].CheckedAt == 0 {
		t.Fatalf("check = %d %v", code, itemTexts(items))
	}
	if status, err := loadStatus(context.Background(), st.DB(), host.userID, Settings{}); err != nil || status.Shopping != 4 {
		t.Fatalf("open items = %d %v", status.Shopping, err)
	}
	items = add("Salat", "0")
	if got := itemTexts(items); got[0] != "1300 g Tomaten <Tomatensoße,Salat> [x]" || got[len(got)-1] != "400 g Tomaten <Salat>" {
		t.Fatalf("more tomatoes = %v", got)
	}
	if code, items = shoppingCall(t, p, host, http.MethodPost, fmt.Sprintf("/shopping/%d", tomatoes), `{"checked":false}`); code != http.StatusOK {
		t.Fatalf("uncheck = %d", code)
	}
	if got := itemTexts(items); len(got) != 5 || got[len(got)-1] != "1700 g Tomaten <Tomatensoße,Salat,Salat>" {
		t.Fatalf("tomatoes put back = %v", got)
	}

	// Clearing takes only what was ticked off; clearing all takes the rest,
	// and the lines behind the items go with them.
	shoppingCall(t, p, host, http.MethodPost, fmt.Sprintf("/shopping/%d", items[0].ID), `{"checked":true}`)
	if code, items = shoppingCall(t, p, host, http.MethodPost, "/shopping/clear", `{}`); code != http.StatusOK || len(items) != 4 {
		t.Fatalf("clear checked = %d %v", code, itemTexts(items))
	}
	if code, items = shoppingCall(t, p, host, http.MethodPost, fmt.Sprintf("/shopping/%d/delete", items[0].ID), ``); code != http.StatusOK || len(items) != 3 {
		t.Fatalf("delete = %d %v", code, itemTexts(items))
	}
	if code, items = shoppingCall(t, p, host, http.MethodPost, "/shopping/clear", `{"all":true}`); code != http.StatusOK || len(items) != 0 {
		t.Fatalf("clear all = %d %v", code, itemTexts(items))
	}
	var sources int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM plugin_recipe_box_shopping_sources WHERE user_id = ?`, host.userID).Scan(&sources); err != nil || sources != 0 {
		t.Fatalf("sources left = %d %v", sources, err)
	}
}

// A list written before items were added up (one row per recipe line, the
// line in the old columns, no merge key) is folded into the new shape the
// first time it is read: open with open, bought with bought.
func TestShoppingListWrittenBeforeAddingUpIsFolded(t *testing.T) {
	st, p, host, ids := shoppingFixture(t)
	legacy := []struct {
		recipeID int64
		title    string
		section  string
		text     string
		checked  int64
	}{
		{11, "Tomatensoße", "", "400 g Tomaten", 0},
		{12, "Salat", "", "400 g Tomaten", 0},
		{12, "Salat", "", "2 Eier", 0},
		{13, "Kuchen", "Für den Teig", "1 Ei", 50},
		{11, "Tomatensoße", "", "1 Ei", 60},
		{0, "", "", "Spülmittel", 0},
	}
	for i, row := range legacy {
		if _, err := st.DB().Exec(`INSERT INTO plugin_recipe_box_shopping_items (user_id, recipe_id, recipe_title, section, text, checked_at, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, host.userID, row.recipeID, row.title, row.section, row.text, row.checked, 10+i, 10+i); err != nil {
			t.Fatal(err)
		}
	}
	if status, err := loadStatus(context.Background(), st.DB(), host.userID, Settings{}); err != nil || status.Shopping != 3 {
		t.Fatalf("open items after the fold = %d %v", status.Shopping, err)
	}
	code, items := shoppingCall(t, p, host, http.MethodGet, "/shopping", "")
	want := "800 g Tomaten <Tomatensoße,Salat> | 2 Eier <Salat> | 2 Ei <Kuchen,Tomatensoße> [x] | Spülmittel"
	if got := strings.Join(itemTexts(items), " | "); code != http.StatusOK || got != want {
		t.Fatalf("folded list = %d\n  %s\nwant\n  %s", code, got, want)
	}
	if items[2].Sources[0].Section != "Für den Teig" || items[2].Sources[0].RecipeID != 13 || items[2].CheckedAt != 50 {
		t.Fatalf("folded cart item = %+v", items[2])
	}
	var unfolded int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM plugin_recipe_box_shopping_items WHERE user_id = ? AND merge_key = ''`, host.userID).Scan(&unfolded); err != nil || unfolded != 0 {
		t.Fatalf("unfolded rows = %d %v", unfolded, err)
	}
	// Folding again changes nothing, and the folded list takes new lines.
	if _, again := shoppingCall(t, p, host, http.MethodGet, "/shopping", ""); strings.Join(itemTexts(again), " | ") != want {
		t.Fatalf("second read = %v", itemTexts(again))
	}
	if _, more := shoppingCall(t, p, host, http.MethodPost, "/shopping", `{"text":"200 g Tomaten"}`); itemTexts(more)[0] != "1000 g Tomaten <Tomatensoße,Salat>" {
		t.Fatalf("after the fold = %v", itemTexts(more))
	}
	// A name typed in lower case takes the spelling of a recipe that adds to it.
	shoppingCall(t, p, host, http.MethodPost, "/shopping", `{"text":"200 g mehl"}`)
	if _, more := shoppingCall(t, p, host, http.MethodPost, "/shopping", fmt.Sprintf(`{"recipe_id":%d,"indexes":[2]}`, ids["Tomatensoße"])); itemTexts(more)[len(more)-1] != "1200 g Mehl <Tomatensoße>" {
		t.Fatalf("flour = %v", itemTexts(more))
	}
	// What the reader types in twice is more of it, not the same line again.
	shoppingCall(t, p, host, http.MethodPost, "/shopping", `{"text":"500 g Reis"}`)
	if _, more := shoppingCall(t, p, host, http.MethodPost, "/shopping", `{"text":"500 g Reis"}`); itemTexts(more)[len(more)-1] != "1000 g Reis" {
		t.Fatalf("rice = %v", itemTexts(more))
	}
	// Singular and plural meet, and above one the plural names the total.
	shoppingCall(t, p, host, http.MethodPost, "/shopping", `{"text":"2 Ei"}`)
	if _, more := shoppingCall(t, p, host, http.MethodPost, "/shopping", `{"text":"1 Ei"}`); itemTexts(more)[1] != "5 Eier <Salat>" {
		t.Fatalf("eggs = %v", itemTexts(more))
	}
}

func TestShoppingListStaysInsideTheTenant(t *testing.T) {
	st := openRecipeStore(t)
	server := shoppingServer(t)
	alice := newFixture(t, st, "alice@example.test")
	bob := newFixture(t, st, "bob@example.test")
	createMessage(t, st, alice.user.ID, alice.account.ID, alice.inbox.ID, "<a@x>", "alice@example.test", "alice@example.test", "x", server.URL+"/carbonara", time.Now())
	if _, err := saveSettings(context.Background(), st.DB(), alice.user.ID, Settings{Enabled: true, OwnRecipientsOnly: true}); err != nil {
		t.Fatal(err)
	}
	p := testBackend()
	aliceHost := &fakeHost{t: t, store: st, userID: alice.user.ID}
	bobHost := &fakeHost{t: t, store: st, userID: bob.user.ID}
	drain(t, p, aliceHost, alice.user.ID)
	carbonara := recipeIDByTitle(t, st.DB(), alice.user.ID, "Spaghetti Carbonara")
	_, items := shoppingCall(t, p, aliceHost, http.MethodPost, "/shopping", fmt.Sprintf(`{"recipe_id":%d,"indexes":[0,1]}`, carbonara))
	if len(items) != 2 {
		t.Fatalf("alice's list = %v", itemTexts(items))
	}

	// Bob can neither take from Alice's recipe nor see, tick or drop her items.
	if code, _ := shoppingCall(t, p, bobHost, http.MethodPost, "/shopping", fmt.Sprintf(`{"recipe_id":%d,"indexes":[0]}`, carbonara)); code != http.StatusNotFound {
		t.Fatalf("bob adding from alice's recipe = %d", code)
	}
	if code, got := shoppingCall(t, p, bobHost, http.MethodGet, "/shopping", ""); code != http.StatusOK || len(got) != 0 {
		t.Fatalf("bob's list = %d %v", code, itemTexts(got))
	}
	if code, _ := shoppingCall(t, p, bobHost, http.MethodPost, fmt.Sprintf("/shopping/%d", items[0].ID), `{"checked":true}`); code != http.StatusNotFound {
		t.Fatalf("bob ticking alice's item = %d", code)
	}
	if code, _ := shoppingCall(t, p, bobHost, http.MethodPost, fmt.Sprintf("/shopping/%d/delete", items[0].ID), ``); code != http.StatusNotFound {
		t.Fatalf("bob deleting alice's item = %d", code)
	}
	shoppingCall(t, p, bobHost, http.MethodPost, "/shopping/clear", `{"all":true}`)
	if _, got := shoppingCall(t, p, aliceHost, http.MethodGet, "/shopping", ""); len(got) != 2 || got[0].CheckedAt != 0 {
		t.Fatalf("alice's list after bob = %v", itemTexts(got))
	}
}
