package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// A recipe in two parts, the way sites write parts into recipeIngredient.
const appleCakeMarkup = `<html><head><script type="application/ld+json">{"@type":"Recipe","name":"Apfelkuchen",
"recipeIngredient":["Für den Teig:","250 g Mehl","125 g Butter","Für den Belag:","500 g Äpfel","1 EL Zucker","Für die Streusel:","1 EL Zucker"],"recipeInstructions":"Backen."}</script></head></html>`

func shoppingServer(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/carbonara", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(carbonaraMarkup))
	})
	mux.HandleFunc("/kuchen", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(appleCakeMarkup))
	})
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

func itemTexts(items []ShoppingItem) []string {
	out := []string{}
	for _, item := range items {
		state := ""
		if item.CheckedAt > 0 {
			state = " [x]"
		}
		out = append(out, item.Section+"|"+item.Text+state)
	}
	return out
}

func TestShoppingListGathersIngredientsFromSeveralRecipes(t *testing.T) {
	st := openRecipeStore(t)
	server := shoppingServer(t)
	f := newFixture(t, st, "cook@example.test")
	createMessage(t, st, f.user.ID, f.account.ID, f.inbox.ID, "<c@x>", "cook@example.test", "cook@example.test", "x", server.URL+"/carbonara", time.Now())
	createMessage(t, st, f.user.ID, f.account.ID, f.inbox.ID, "<k@x>", "cook@example.test", "cook@example.test", "y", server.URL+"/kuchen", time.Now())
	if _, err := saveSettings(context.Background(), st.DB(), f.user.ID, Settings{Enabled: true, OwnRecipientsOnly: true}); err != nil {
		t.Fatal(err)
	}
	p := testBackend()
	host := &fakeHost{t: t, store: st, userID: f.user.ID}
	drain(t, p, host, f.user.ID)
	carbonara := recipeIDByTitle(t, st.DB(), f.user.ID, "Spaghetti Carbonara")
	cake := recipeIDByTitle(t, st.DB(), f.user.ID, "Apfelkuchen")

	// The recipe page shows the parts with each ingredient's index.
	rec := callAPI(t, p, host, http.MethodGet, fmt.Sprintf("%s/recipes/%d", apiPath, cake), "")
	var detail struct {
		Recipe RecipeDetail `json:"recipe"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatal(err)
	}
	if got := detail.Recipe.IngredientSections; len(got) != 3 || got[1].Name != "Für den Belag" || got[1].Items[0].Index != 4 {
		t.Fatalf("sections = %+v", got)
	}

	// One ingredient from the carbonara, then the whole topping of the cake.
	if code, _ := shoppingCall(t, p, host, http.MethodPost, "/shopping", fmt.Sprintf(`{"recipe_id":%d,"indexes":[0]}`, carbonara)); code != http.StatusOK {
		t.Fatalf("add carbonara = %d", code)
	}
	code, items := shoppingCall(t, p, host, http.MethodPost, "/shopping", fmt.Sprintf(`{"recipe_id":%d,"indexes":[4,5]}`, cake))
	if code != http.StatusOK {
		t.Fatalf("add topping = %d", code)
	}
	if got := fmt.Sprint(itemTexts(items)); got != "[|400 g Spaghetti Für den Belag|500 g Äpfel Für den Belag|1 EL Zucker]" {
		t.Fatalf("list = %s", got)
	}
	if items[1].RecipeID != cake || items[1].RecipeTitle != "Apfelkuchen" {
		t.Fatalf("topping item = %+v", items[1])
	}

	// A heading is not something to buy, and neither is a line the recipe lacks.
	for _, body := range []string{fmt.Sprintf(`{"recipe_id":%d,"indexes":[3]}`, cake), fmt.Sprintf(`{"recipe_id":%d,"indexes":[99]}`, cake), fmt.Sprintf(`{"recipe_id":%d,"indexes":[]}`, cake)} {
		if code, _ := shoppingCall(t, p, host, http.MethodPost, "/shopping", body); code != http.StatusBadRequest {
			t.Fatalf("add %s = %d", body, code)
		}
	}

	// Items of the reader's own, one line each.
	code, items = shoppingCall(t, p, host, http.MethodPost, "/shopping", `{"text":"  Spülmittel\n und Schwämme "}`)
	if code != http.StatusOK || items[len(items)-1].Text != "Spülmittel und Schwämme" || items[len(items)-1].RecipeID != 0 {
		t.Fatalf("own item = %d %+v", code, items)
	}
	if code, _ := shoppingCall(t, p, host, http.MethodPost, "/shopping", `{"text":"   "}`); code != http.StatusBadRequest {
		t.Fatalf("empty own item = %d", code)
	}

	// Ticking off, then adding the same ingredient again puts it back to buy
	// rather than on the list twice.
	apples := items[1].ID
	if code, items = shoppingCall(t, p, host, http.MethodPost, fmt.Sprintf("/shopping/%d", apples), `{"checked":true}`); code != http.StatusOK || items[1].CheckedAt == 0 {
		t.Fatalf("check = %d %+v", code, items)
	}
	if status, err := loadStatus(context.Background(), st.DB(), f.user.ID, Settings{}); err != nil || status.Shopping != 3 {
		t.Fatalf("open items = %d %v", status.Shopping, err)
	}
	code, items = shoppingCall(t, p, host, http.MethodPost, "/shopping", fmt.Sprintf(`{"recipe_id":%d,"indexes":[1,2,4,5]}`, cake))
	if code != http.StatusOK || len(items) != 6 || items[1].ID != apples || items[1].CheckedAt != 0 {
		t.Fatalf("whole cake = %d %v", code, itemTexts(items))
	}

	// The same line in another section is needed a second time, so it is an
	// item of its own rather than the first one put back.
	code, items = shoppingCall(t, p, host, http.MethodPost, "/shopping", fmt.Sprintf(`{"recipe_id":%d,"indexes":[7]}`, cake))
	if code != http.StatusOK || len(items) != 7 || items[6].Section != "Für die Streusel" || items[2].Section != "Für den Belag" {
		t.Fatalf("sugar for the crumble = %d %v", code, itemTexts(items))
	}

	// Clearing takes only what was ticked off; clearing all takes the rest.
	shoppingCall(t, p, host, http.MethodPost, fmt.Sprintf("/shopping/%d", items[0].ID), `{"checked":true}`)
	if code, items = shoppingCall(t, p, host, http.MethodPost, "/shopping/clear", `{}`); code != http.StatusOK || len(items) != 6 {
		t.Fatalf("clear checked = %d %v", code, itemTexts(items))
	}
	if code, items = shoppingCall(t, p, host, http.MethodPost, fmt.Sprintf("/shopping/%d/delete", items[0].ID), ``); code != http.StatusOK || len(items) != 5 {
		t.Fatalf("delete = %d %v", code, itemTexts(items))
	}
	if code, items = shoppingCall(t, p, host, http.MethodPost, "/shopping/clear", `{"all":true}`); code != http.StatusOK || len(items) != 0 {
		t.Fatalf("clear all = %d %v", code, itemTexts(items))
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
