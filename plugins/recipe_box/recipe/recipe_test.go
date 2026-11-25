package recipe

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
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
<a href="https://meet.google.com/abc-defg-hij">Call</a>
<a href="https://docs.google.com/document/d/1">Doc</a>
<img src="https://example.test/pic.jpg">`
	links := ExtractLinks("", markup)
	// A call to join is plumbing; a document the reader mailed themselves is
	// something they kept, for the notes overview.
	if len(links) != 2 || links[0] != "https://example.test/cake" || links[1] != "https://docs.google.com/document/d/1" {
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

func TestMicrodataReadIsBoundedOnHostilePages(t *testing.T) {
	var b strings.Builder
	b.WriteString(`<div itemscope itemtype="https://schema.org/Recipe"><h1 itemprop="name">Teig</h1>`)
	// Unclosed list items, each of which would otherwise be read up to the
	// element-text bound, and reviews nothing on a card uses.
	for b.Len() < MaxPageBytes-64 {
		b.WriteString(`<li itemprop="recipeIngredient">Mehl <span itemprop="reviewBody">x`)
	}
	start := time.Now()
	page := ParsePage(b.String(), "https://example.test/")
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("hostile page took %v to read", elapsed)
	}
	// The text budget ends the read before the list cap would: each unclosed
	// item is read to the element bound, so fewer of them fit.
	if page.Recipe == nil || len(page.Recipe.Ingredients) == 0 || len(page.Recipe.Ingredients) > maxListItems {
		t.Fatalf("recipe = %v ingredients", page.Recipe != nil)
	}
}

func TestClassifyReadsWhatThePageSaysAboutItself(t *testing.T) {
	cases := []struct {
		name, markup, link, kind, detail string
	}{
		{"recipe", chefkochPage, "https://www.chefkoch.de/rezepte/1/x.html", KindRecipe, ""},
		{"product with price", `<script type="application/ld+json">{"@graph":[{"@type":"WebPage"},{"@type":"Product","name":"Pfanne","offers":{"@type":"Offer","price":"49.90","priceCurrency":"eur"}}]}</script>`,
			"https://shop.example.test/pfanne", KindProduct, "49.90 EUR"},
		{"og product", `<head><meta property="og:type" content="product"><meta property="product:price:amount" content="19,99"><meta property="product:price:currency" content="EUR"></head>`,
			"https://shop.example.test/x", KindProduct, "19,99 EUR"},
		{"event", `<script type="application/ld+json">{"@type":"MusicEvent","name":"Konzert","startDate":"2026-11-03T20:00"}</script>`,
			"https://club.example.test/konzert", KindEvent, "2026-11-03T20:00"},
		{"news article", `<script type="application/ld+json">{"@type":["NewsArticle"],"headline":"x"}</script>`, "https://news.example.test/a", KindArticle, ""},
		{"og article", `<head><meta property="og:type" content="article"></head>`, "https://blog.example.test/a", KindArticle, ""},
		{"youtube is a video whatever it says", `<script type="application/ld+json">{"@type":"Person"}</script>`, "https://www.youtube.com/watch?v=x", KindVideo, ""},
		{"youtube music is music", ``, "https://music.youtube.com/watch?v=x", KindMusic, ""},
		{"a ticket platform keeps the event's date", `<script type="application/ld+json">{"@type":"Event","name":"Lesung","startDate":"2026-12-01"}</script>`,
			"https://www.eventbrite.de/e/lesung-1", KindEvent, "2026-12-01"},
		{"microdata place", `<div itemscope itemtype="https://schema.org/Restaurant"></div>`, "https://trattoria.example.test/", KindPlace, ""},
		{"amazon page without data", ``, "https://www.amazon.de/dp/B0", KindProduct, ""},
		{"nothing said", `<html><head><title>x</title></head></html>`, "https://example.test/x", KindLink, ""},
	}
	for _, c := range cases {
		page := ParsePage(c.markup, c.link)
		kind, detail := Classify(page, c.link)
		if kind != c.kind || detail != c.detail {
			t.Errorf("%s: Classify = %q %q, want %q %q", c.name, kind, detail, c.kind, c.detail)
		}
	}
}

func TestHostKindAndNoFetch(t *testing.T) {
	if got := HostKind("https://www.google.com/maps/place/x"); got != KindPlace {
		t.Fatalf("google maps = %q", got)
	}
	if got := HostKind("https://github.com/acme/tool"); got != KindTech {
		t.Fatalf("github = %q", got)
	}
	if !NoFetch("https://docs.google.com/document/d/1") || !NoFetch("https://maps.app.goo.gl/abc") {
		t.Fatal("a document or a map link would be fetched")
	}
	if NoFetch("https://www.booking.com/hotel/x") || NoFetch("https://github.com/acme/tool") {
		t.Fatal("a public page is not fetched")
	}
}

// hr-fernsehen.de publishes a Recipe block holding only the dish's name and
// picture and prints the ingredients and the method as the article's text.
// The page is fetched as it was served on 2026-11-25.
func TestParsePageCompletesNameOnlyRecipeFromItsCopy(t *testing.T) {
	markup, err := os.ReadFile("testdata/hr-fernsehen-rote-linsen-suppe.html")
	if err != nil {
		t.Fatal(err)
	}
	page := ParsePage(string(markup), "https://www.hr-fernsehen.de/sendungen-a-z/hallo-hessen/rezepte/rezept-tuerkische-rote-linsen-suppe-v1,kochen-11880.html")
	if page.Recipe == nil {
		t.Fatal("a page declaring a Recipe was not taken for one")
	}
	r := page.Recipe
	if r.Name != "Türkische Rote-Linsen-Suppe" || len(r.ImageURLs) == 0 {
		t.Fatalf("recipe = %#v", r)
	}
	sections := IngredientSections(r.Ingredients)
	if len(sections) != 3 || sections[0].Name != "Rote-Linsen-Suppe" || len(sections[0].Items) != 13 ||
		sections[1].Name != "Toppings (je nach persönlichem Geschmack)" || sections[2].Name != "Beilagen" {
		t.Fatalf("sections = %#v", sections)
	}
	if sections[0].Items[0].Text != "1 Zwiebel, mittelgroß" || sections[0].Items[11].Text != "je 1 Prise Salz & Pfeffer, zum Abschmecken" {
		t.Fatalf("ingredients = %#v", r.Ingredients)
	}
	if len(r.Instructions) != 1 || r.Instructions[0].Name != "Rote-Linsen-Suppe" || len(r.Instructions[0].Steps) != 12 ||
		r.Instructions[0].Steps[0] != "die roten Linsen unter fließendem, kaltem Wasser waschen" {
		t.Fatalf("instructions = %#v", r.Instructions)
	}
}

func TestParsePageReadsCopyOnlyForADeclaredRecipe(t *testing.T) {
	copyText := `<h2>Zutaten für 4 Personen</h2><ul><li>200 g Mehl</li><li>1 Ei</li></ul>
		<p>Ein Tipp vom Koch.</p><h2>Zubereitung</h2><ol><li>Mischen.</li><li>Backen.</li></ol><h2>Mehr Rezepte</h2><ul><li>Kuchen</li></ul>`
	if page := ParsePage(`<html><head><title>Kuchen</title></head><body>`+copyText+`</body></html>`, "https://example.test/"); page.Recipe != nil {
		t.Fatal("a page without a Recipe block was taken for a recipe because of its headings")
	}
	declared := `<html><head><script type="application/ld+json">{"@type":"Recipe","name":"Kuchen"}</script></head><body>` + copyText + `</body></html>`
	page := ParsePage(declared, "https://example.test/")
	if page.Recipe == nil || strings.Join(page.Recipe.Ingredients, "|") != "200 g Mehl|1 Ei" ||
		len(page.Recipe.Instructions) != 1 || strings.Join(page.Recipe.Instructions[0].Steps, "|") != "Mischen.|Backen." {
		t.Fatalf("recipe = %#v", page.Recipe)
	}
	nameOnly := `<html><head><script type="application/ld+json">{"@type":"Recipe","name":"Kuchen"}</script></head><body><p>Lecker.</p></body></html>`
	if page := ParsePage(nameOnly, "https://example.test/"); page.Recipe != nil {
		t.Fatal("a Recipe block naming nothing to cook was taken for a recipe")
	}
	structured := `<html><head><script type="application/ld+json">{"@type":"Recipe","name":"Kuchen","recipeIngredient":["250 g Mehl"]}</script></head><body>` + copyText + `</body></html>`
	page = ParsePage(structured, "https://example.test/")
	if page.Recipe == nil || strings.Join(page.Recipe.Ingredients, "|") != "250 g Mehl" || len(page.Recipe.Instructions) != 0 {
		t.Fatalf("the copy was read beside the page's own structured ingredients: %#v", page.Recipe)
	}
}

func TestCopyIsReadAsHTMLWritesIt(t *testing.T) {
	declared := func(body string) *Recipe {
		t.Helper()
		page := ParsePage(`<html><head><script type="application/ld+json">{"@type":"Recipe","name":"Kuchen"}</script></head><body>`+body+`</body></html>`, "https://example.test/")
		if page.Recipe == nil {
			t.Fatalf("no recipe read from %s", body)
		}
		return page.Recipe
	}
	// </li> and </p> are optional in HTML.
	r := declared(`<p>Vorweg<h2>Zutaten</h2><ul><li>1 Ei<li>2 g Salz</ul><h2>Zubereitung</h2><ol><li>Mischen<li>Backen</ol>`)
	if strings.Join(r.Ingredients, "|") != "1 Ei|2 g Salz" || strings.Join(r.Instructions[0].Steps, "|") != "Mischen|Backen" {
		t.Fatalf("unclosed items: %q %#v", r.Ingredients, r.Instructions)
	}
	// A part opened by a bold paragraph keeps its bold sub-headings.
	r = declared(`<p><strong>Zutaten</strong></p><p><strong>Teig:</strong></p><ul><li>1 Ei</li></ul><p><b>Belag:</b></p><ul><li>2 Äpfel</li></ul><p><b>Zubereitung</b></p><ol><li>Backen</li></ol>`)
	if strings.Join(r.Ingredients, "|") != "Teig:|1 Ei|Belag:|2 Äpfel" || len(r.Instructions) != 1 || r.Instructions[0].Steps[0] != "Backen" {
		t.Fatalf("bold parts: %q %#v", r.Ingredients, r.Instructions)
	}
	// A sub-heading naming the part is a section of it, not its end.
	r = declared(`<h2>Zutaten</h2><h3>Zutaten für den Teig</h3><ul><li>1 Ei</li></ul><h2>Zubereitung</h2><h3>Zubereitung des Teigs</h3><ol><li>Kneten</li></ol>`)
	if strings.Join(r.Ingredients, "|") != "Zutaten für den Teig:|1 Ei" || len(r.Instructions) != 1 || r.Instructions[0].Name != "Zubereitung des Teigs" {
		t.Fatalf("sub-headings naming the part: %q %#v", r.Ingredients, r.Instructions)
	}
	// A facts line naming the part holds nothing; the heading after it is the part.
	r = declared(`<p><strong>Zubereitung: 30 Minuten</strong></p><h2>Zutaten</h2><ul><li>1 Ei</li></ul><h2>Zubereitung</h2><ol><li>Backen</li></ol>`)
	if len(r.Instructions) != 1 || r.Instructions[0].Steps[0] != "Backen" {
		t.Fatalf("facts line took the part: %#v", r.Instructions)
	}
	// A heading never stands at the end of a full list with nothing under it.
	items := strings.Repeat("<li>1 g Salz</li>", maxListItems-1)
	r = declared(`<h2>Zutaten</h2><ul>` + items + `</ul><p><b>Deko:</b></p><ul><li>1 Blatt</li></ul><h2>Zubereitung</h2><ol><li>x</li></ol>`)
	if len(r.Ingredients) > maxListItems || strings.HasSuffix(r.Ingredients[len(r.Ingredients)-1], ":") {
		t.Fatalf("capped list ends %q (%d lines)", r.Ingredients[len(r.Ingredients)-1], len(r.Ingredients))
	}
}
