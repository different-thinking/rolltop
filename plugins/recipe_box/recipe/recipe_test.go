package recipe

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A page shaped like Chefkoch's: one standalone Recipe, ingredients as a list,
// instructions as one paragraph string, the yield as a bare number beside words.
const chefkochPage = `<!doctype html><html><head>
<title>Spaghetti Carbonara von Koch | Chefkoch</title>
<meta property="og:title" content="Spaghetti Carbonara">
<meta property="og:image" content="https://img.chefkoch-cdn.de/og.jpg">
<meta property="og:site_name" content="Chefkoch.de">
<link rel="canonical" href="https://www.chefkoch.de/rezepte/1/Spaghetti-Carbonara.html">
<script type="application/ld+json">
{"@context":"http://schema.org","@type":"Recipe","name":"Spaghetti Carbonara",
 "description":"Spaghetti Carbonara - so wie in Rom. &amp; ganz ohne Sahne.",
 "image":["https://img.chefkoch-cdn.de/1x1.jpg","https://img.chefkoch-cdn.de/4x3.jpg"],
 "recipeYield":["4","4 Portionen"],"prepTime":"P0DT0H15M","cookTime":"PT20M","totalTime":"P0DT0H35M",
 "recipeIngredient":["400 g Spaghetti","150 g Guanciale","4 Eigelb","80 g Pecorino"],
 "recipeInstructions":"Die Nudeln kochen.\nDen Speck auslassen.\n\nAlles vermengen.",
 "recipeCategory":"Hauptspeise","recipeCuisine":["Italien"],"keywords":"Pasta, schnell, Pasta",
 "author":{"@type":"Person","name":"Koch"},
 "aggregateRating":{"@type":"AggregateRating","ratingValue":"4,6","ratingCount":"1234"}}
</script></head><body>…</body></html>`

// A WordPress page with WP Recipe Maker: the Recipe sits inside @graph beside
// the WebPage, and the steps are HowToSections of HowToSteps.
const wordpressPage = `<html><head>
<script type="application/ld+json" class="yoast-schema-graph">{"@context":"https://schema.org","@graph":[
 {"@type":"WebPage","name":"Best Lemon Cake"},
 {"@type":["Recipe","NewsArticle"],"name":"Lemon Cake","image":{"@type":"ImageObject","url":"/wp/cake.webp"},
  "recipeIngredient":["2 lemons","200 g flour"],
  "recipeInstructions":[
   {"@type":"HowToSection","name":"Cake","itemListElement":[{"@type":"HowToStep","text":"Mix <b>everything</b>."},{"@type":"HowToStep","text":"Bake."}]},
   {"@type":"HowToSection","name":"Glaze","itemListElement":[{"@type":"HowToStep","text":"Stir sugar and juice."}]}
  ],
  "totalTime":"PT1H10M","recipeYield":"12"}
]}</script></head><body></body></html>`

const microdataPage = `<html><head><title>Omas Kartoffelsalat</title></head><body>
<div itemscope itemtype="https://schema.org/Recipe">
 <h1 itemprop="name">Omas Kartoffelsalat</h1>
 <img itemprop="image" src="//example.test/salat.jpg">
 <meta itemprop="totalTime" content="PT45M">
 <span itemprop="recipeYield">6 Personen</span>
 <ul><li itemprop="recipeIngredient">1 kg Kartoffeln</li><li itemprop="recipeIngredient">1 Zwiebel</li></ul>
 <div itemprop="recipeInstructions"><p>Kartoffeln kochen.</p><div><p>Schälen und schneiden.</p></div></div>
</div></body></html>`

const articlePage = `<html><head><title>10 Dinge über Pasta</title>
<meta name="description" content="Ein Artikel.">
<meta property="og:image" content="https://example.test/a.jpg"></head><body></body></html>`

func TestParsePageReadsStandaloneRecipe(t *testing.T) {
	page := ParsePage(chefkochPage, "https://www.chefkoch.de/rezepte/1/x.html")
	r := page.Recipe
	if r == nil {
		t.Fatal("recipe not recognised")
	}
	if r.Name != "Spaghetti Carbonara" || r.Yield != "4 Portionen" {
		t.Fatalf("name/yield = %q / %q", r.Name, r.Yield)
	}
	if r.Description != "Spaghetti Carbonara - so wie in Rom. & ganz ohne Sahne." {
		t.Fatalf("description = %q", r.Description)
	}
	if r.TotalMinutes != 35 || r.PrepMinutes != 15 || r.CookMinutes != 20 {
		t.Fatalf("times = %d/%d/%d", r.TotalMinutes, r.PrepMinutes, r.CookMinutes)
	}
	if len(r.Ingredients) != 4 || r.Ingredients[3] != "80 g Pecorino" {
		t.Fatalf("ingredients = %#v", r.Ingredients)
	}
	if len(r.Instructions) != 1 || len(r.Instructions[0].Steps) != 3 {
		t.Fatalf("instructions = %#v", r.Instructions)
	}
	if len(r.Keywords) != 2 || r.Keywords[0] != "Pasta" {
		t.Fatalf("keywords = %#v", r.Keywords)
	}
	if r.RatingValue < 4.59 || r.RatingValue > 4.61 || r.RatingCount != 1234 || r.Author != "Koch" {
		t.Fatalf("rating/author = %v/%d/%q", r.RatingValue, r.RatingCount, r.Author)
	}
	if page.CanonicalURL != "https://www.chefkoch.de/rezepte/1/Spaghetti-Carbonara.html" || page.SiteName != "Chefkoch.de" {
		t.Fatalf("canonical/site = %q / %q", page.CanonicalURL, page.SiteName)
	}
}

func TestParsePageFindsRecipeInGraphWithSections(t *testing.T) {
	page := ParsePage(wordpressPage, "https://blog.example.test/lemon-cake/")
	r := page.Recipe
	if r == nil {
		t.Fatal("recipe in @graph not recognised")
	}
	if len(r.ImageURLs) != 1 || r.ImageURLs[0] != "https://blog.example.test/wp/cake.webp" {
		t.Fatalf("image = %#v", r.ImageURLs)
	}
	if len(r.Instructions) != 2 || r.Instructions[0].Name != "Cake" || r.Instructions[0].Steps[0] != "Mix everything." {
		t.Fatalf("sections = %#v", r.Instructions)
	}
	if r.Instructions[1].Name != "Glaze" || len(r.Instructions[1].Steps) != 1 {
		t.Fatalf("second section = %#v", r.Instructions[1])
	}
	if r.TotalMinutes != 70 || r.Yield != "12" {
		t.Fatalf("time/yield = %d / %q", r.TotalMinutes, r.Yield)
	}
}

func TestParsePageReadsMicrodata(t *testing.T) {
	page := ParsePage(microdataPage, "https://example.test/salat")
	r := page.Recipe
	if r == nil {
		t.Fatal("microdata recipe not recognised")
	}
	if r.Name != "Omas Kartoffelsalat" || r.TotalMinutes != 45 || r.Yield != "6 Personen" {
		t.Fatalf("recipe = %#v", r)
	}
	if len(r.Ingredients) != 2 || len(r.ImageURLs) != 1 || r.ImageURLs[0] != "https://example.test/salat.jpg" {
		t.Fatalf("ingredients/image = %#v / %#v", r.Ingredients, r.ImageURLs)
	}
	if len(r.Instructions) != 1 || len(r.Instructions[0].Steps) != 2 {
		t.Fatalf("steps = %#v", r.Instructions)
	}
}

func TestParsePageWithoutRecipeKeepsPreview(t *testing.T) {
	page := ParsePage(articlePage, "https://example.test/artikel")
	if page.Recipe != nil {
		t.Fatal("an article was taken for a recipe")
	}
	if page.Title != "10 Dinge über Pasta" || page.Description != "Ein Artikel." || page.ImageURL != "https://example.test/a.jpg" {
		t.Fatalf("preview = %#v", page)
	}
}

func TestParsePageIgnoresEmptyRecipeTemplate(t *testing.T) {
	markup := `<script type="application/ld+json">{"@type":"Recipe","name":""}</script>`
	if page := ParsePage(markup, "https://example.test/"); page.Recipe != nil {
		t.Fatal("an empty Recipe block was taken for a recipe")
	}
}

func TestParsePageToleratesRawNewlinesInJSON(t *testing.T) {
	markup := "<script type=\"application/ld+json\">{\"@type\":\"Recipe\",\"name\":\"Suppe\",\"recipeIngredient\":[\"1 l\nWasser\"]}</script>"
	page := ParsePage(markup, "https://example.test/")
	if page.Recipe == nil || page.Recipe.Ingredients[0] != "1 l Wasser" {
		t.Fatalf("recipe = %#v", page.Recipe)
	}
}

func TestDurationMinutes(t *testing.T) {
	cases := map[string]int{
		"PT45M": 45, "PT1H30M": 90, "P0DT0H35M": 35, "PT2H": 120, "P1DT2H": 1560, "PT90S": 2,
		"1 Std. 20 Min.": 80, "45 min": 45, "30": 30, "": 0, "P2M": 0, "PT0M": 0, "nonsense": 0,
	}
	for input, want := range cases {
		if got := durationMinutes(input); got != want {
			t.Errorf("durationMinutes(%q) = %d, want %d", input, got, want)
		}
	}
}

func TestExtractLinksFromSharedNote(t *testing.T) {
	text := "Für Sonntag!\nhttps://www.chefkoch.de/rezepte/1/Carbonara.html?utm_source=app&portionen=4#comments.\n\nGesendet von meinem iPhone"
	links := ExtractLinks(text, "")
	if len(links) != 1 || links[0] != "https://www.chefkoch.de/rezepte/1/Carbonara.html?portionen=4" {
		t.Fatalf("links = %#v", links)
	}
	if note := NoteText(text, 200); note != "Für Sonntag!" {
		t.Fatalf("note = %q", note)
	}
}

func TestExtractLinksUnwrapsRedirectsAndDropsPlumbing(t *testing.T) {
	markup := `<a href="https://www.google.com/url?q=https%3A%2F%2Fexample.test%2Fcake%3Ffbclid%3Dx&amp;sa=D">Kuchen</a>
<a href="https://news.example.test/unsubscribe?id=1">Abmelden</a>
<a href="mailto:me@example.test">Mail</a>
<a href="https://docs.google.com/document/d/1">Doc</a>
<img src="https://example.test/pic.jpg">`
	links := ExtractLinks("", markup)
	if len(links) != 1 || links[0] != "https://example.test/cake" {
		t.Fatalf("links = %#v", links)
	}
}

func TestNormalizeLinkKeepsPortsThatMatter(t *testing.T) {
	cases := map[string]string{
		"https://example.test:8443/a":  "https://example.test:8443/a",
		"https://Example.TEST:443/a":   "https://example.test/a",
		"http://example.test:80":       "http://example.test/",
		"https://example.test/a?b=1#c": "https://example.test/a?b=1",
	}
	for input, want := range cases {
		if got, ok := NormalizeLink(input); !ok || got != want {
			t.Errorf("NormalizeLink(%q) = %q, %v; want %q", input, got, ok, want)
		}
	}
}

func TestNoteTextKeepsTheReadersOwnLine(t *testing.T) {
	note := NoteText("weniger Zucker nehmen\nhttps://example.test/kuchen\n> zitiert\n--\nSignatur", 100)
	if note != "weniger Zucker nehmen" {
		t.Fatalf("note = %q", note)
	}
}

func TestFetchPageFollowsRedirectsAndDecodesCharset(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/short", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/rezept", http.StatusFound)
	})
	mux.HandleFunc("/rezept", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Cookie") != "" || r.Header.Get("Referer") != "" {
			t.Errorf("request carried identifying headers: %v", r.Header)
		}
		w.Header().Set("Content-Type", "text/html; charset=iso-8859-1")
		// "Käsespätzle" in ISO-8859-1.
		_, _ = w.Write([]byte("<script type=\"application/ld+json\">{\"@type\":\"Recipe\",\"name\":\"K\xe4sesp\xe4tzle\",\"recipeIngredient\":[\"K\xe4se\"]}</script>"))
	})
	mux.HandleFunc("/pic", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("\x89PNG\r\n\x1a\nrest"))
	})
	mux.HandleFunc("/svg", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write([]byte("<svg onload=alert(1)></svg>"))
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	fetcher := NewFetcher((&net.Dialer{}).DialContext)
	page, finalURL, err := fetcher.FetchPage(context.Background(), server.URL+"/short")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(finalURL, "/rezept") {
		t.Fatalf("final url = %q", finalURL)
	}
	if page.Recipe == nil || page.Recipe.Name != "Käsespätzle" {
		t.Fatalf("recipe = %#v", page.Recipe)
	}
	if _, kind, err := fetcher.FetchImage(context.Background(), server.URL+"/pic"); err != nil || kind != "image/png" {
		t.Fatalf("image = %q, %v", kind, err)
	}
	if _, _, err := fetcher.FetchImage(context.Background(), server.URL+"/svg"); err == nil {
		t.Fatal("markup served as an image was accepted")
	}
	if _, _, err := fetcher.FetchPage(context.Background(), server.URL+"/missing"); err == nil {
		t.Fatal("a 404 was read as a page")
	} else if status, ok := err.(HTTPStatusError); !ok || !status.Permanent() {
		t.Fatalf("404 error = %#v", err)
	}
}

func TestParsePageSurvivesUndeclaredLatin1(t *testing.T) {
	// Lowercasing turns every invalid byte into a three-byte U+FFFD, so an
	// offset found in a lowercased copy slices the original out of range.
	markup := "<html><head><title>" + strings.Repeat("\xe9", 100) + "</title></head>"
	_ = ParsePage(markup, "https://example.test/")
	micro := `<div itemscope itemtype="https://schema.org/Recipe"><SPAN itemprop="name">Kuchen <abbr>z.B.</abbr> ` + strings.Repeat("\xe9", 80) + `</span></div>`
	_ = ParsePage(micro, "https://example.test/")
	if got := elementText(`<a>x<abbr>y</abbr>z</a>`, 3, "a"); got != "x<abbr>y</abbr>z" {
		t.Fatalf("elementText = %q", got)
	}
}
