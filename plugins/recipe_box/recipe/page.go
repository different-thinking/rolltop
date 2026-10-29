// File overview: What a fetched page says about itself.
//
// A page is a recipe only when it says so in the structured data search
// engines read -- schema.org's Recipe, as JSON-LD or as microdata. That is the
// whole test, and it is deliberately the page's own statement rather than a
// guess from the site's name or the words on it: nearly every recipe site
// publishes it, because Google's recipe cards are built from it, and nothing
// that is not a recipe has a reason to. Everything else a page offers -- its
// OpenGraph title, picture and description -- is kept as a preview, so a link
// that was not recognised can still be shown and filed as a recipe by hand.

package recipe

import (
	"html"
	"regexp"
	"strings"
)

// Page is one fetched page, read for what Rolltop shows of it.
type Page struct {
	// Recipe is the page's own structured description, nil when it has none.
	Recipe *Recipe
	// The preview fields come from OpenGraph and the document head, and fill
	// in what the structured data leaves out.
	Title        string
	Description  string
	ImageURL     string
	SiteName     string
	CanonicalURL string
	// Facts is what the page says about its own kind, read for the notes
	// overview when the page is not a recipe.
	Facts PageFacts
}

// Recipe is a schema.org Recipe as the plugin stores it: plain text, minutes
// and lists, every value already stripped of markup.
type Recipe struct {
	Name         string        `json:"name"`
	Description  string        `json:"description"`
	ImageURLs    []string      `json:"image_urls"`
	URL          string        `json:"url"`
	Author       string        `json:"author"`
	Yield        string        `json:"yield"`
	TotalMinutes int           `json:"total_minutes"`
	PrepMinutes  int           `json:"prep_minutes"`
	CookMinutes  int           `json:"cook_minutes"`
	Ingredients  []string      `json:"ingredients"`
	Instructions []StepSection `json:"instructions"`
	Categories   []string      `json:"categories"`
	Cuisines     []string      `json:"cuisines"`
	Keywords     []string      `json:"keywords"`
	RatingValue  float64       `json:"rating_value"`
	RatingCount  int           `json:"rating_count"`
	Calories     string        `json:"calories"`
}

// StepSection is one titled run of steps. Most recipes have one untitled
// section; a cake with a base and a frosting has two.
type StepSection struct {
	Name  string   `json:"name"`
	Steps []string `json:"steps"`
}

// Limits on what one recipe may contribute. A page is hostile input, and a
// structured block that lists ten thousand ingredients is not describing a
// dish.
const (
	maxListItems   = 120
	maxSections    = 20
	maxTextRunes   = 4000
	maxShortRunes  = 300
	maxKeywordList = 30
)

var (
	scriptLDRE   = regexp.MustCompile(`(?is)<script\b([^>]*)>(.*?)</script>`)
	metaTagRE    = regexp.MustCompile(`(?is)<meta\b[^>]*>`)
	linkTagRE    = regexp.MustCompile(`(?is)<link\b[^>]*>`)
	titleRE      = regexp.MustCompile(`(?is)<title\b[^>]*>(.*?)</title>`)
	attrRE       = regexp.MustCompile(`(?is)([a-zA-Z_:][-a-zA-Z0-9_:.]*)\s*=\s*("([^"]*)"|'([^']*)'|([^\s"'>]+))`)
	tagRE        = regexp.MustCompile(`(?s)<[^>]*>`)
	blockTagRE   = regexp.MustCompile(`(?is)</?(p|div|br|li|ul|ol|h[1-6]|tr|td|th|dt|dd|table|section|article|header|footer|blockquote)\b[^>]*>`)
	scriptBodyRE = regexp.MustCompile(`(?is)<(script|style|noscript|template)\b[^>]*>.*?</(script|style|noscript|template)>`)
	spaceRE      = regexp.MustCompile(`[ \t\f\v\r\x{00a0}]+`)
	headEndRE    = regexp.MustCompile(`(?i)</head>`)
)

// ParsePage reads a page's markup. It never fails: a page that says nothing
// about itself is a Page with no recipe and an empty preview.
func ParsePage(markup string, pageURL string) Page {
	page := Page{}
	head := markup
	// The end of the head is found in the markup itself, not in a lowercased
	// copy: lowercasing can change a string's length (every invalid byte of an
	// undeclared Latin-1 page becomes a three-byte U+FFFD), and an offset into
	// the copy then slices the original out of range.
	if loc := headEndRE.FindStringIndex(markup); loc != nil && loc[0] > 0 {
		head = markup[:loc[0]]
	}
	meta := metaValues(head)
	page.Title = firstNonEmpty(meta["og:title"], meta["twitter:title"], cleanText(titleText(head), maxShortRunes))
	page.Description = cleanText(firstNonEmpty(meta["og:description"], meta["twitter:description"], meta["description"]), maxTextRunes)
	page.ImageURL = resolveURL(pageURL, firstNonEmpty(meta["og:image:secure_url"], meta["og:image"], meta["og:image:url"], meta["twitter:image"]))
	page.SiteName = cleanText(meta["og:site_name"], maxShortRunes)
	page.CanonicalURL = resolveURL(pageURL, firstNonEmpty(canonicalLink(head), meta["og:url"]))
	page.Title = cleanText(page.Title, maxShortRunes)
	page.Facts = pageFacts(markup, meta)

	recipe := recipeFromJSONLD(markup, pageURL)
	if recipe == nil {
		recipe = recipeFromMicrodata(markup, pageURL)
	}
	if recipe != nil {
		if recipe.Name == "" {
			recipe.Name = page.Title
		}
		if recipe.Description == "" {
			recipe.Description = page.Description
		}
		if len(recipe.ImageURLs) == 0 && page.ImageURL != "" {
			recipe.ImageURLs = []string{page.ImageURL}
		}
		// A structured block with neither a name nor an ingredient is a theme's
		// empty template, not a recipe the page publishes.
		if recipe.Name != "" && (len(recipe.Ingredients) > 0 || len(recipe.Instructions) > 0) {
			page.Recipe = recipe
		}
	}
	return page
}

// metaValues reads the head's meta tags into one map keyed by property or
// name, lowercased. The first tag naming a key wins, which is the one a page
// means: themes that print a second og:image do it for a fallback.
func metaValues(head string) map[string]string {
	values := map[string]string{}
	for _, tag := range metaTagRE.FindAllString(head, -1) {
		attrs := attributes(tag)
		key := strings.ToLower(firstNonEmpty(attrs["property"], attrs["name"], attrs["itemprop"]))
		content := attrs["content"]
		if key == "" || strings.TrimSpace(content) == "" {
			continue
		}
		if _, ok := values[key]; !ok {
			values[key] = strings.TrimSpace(content)
		}
	}
	return values
}

func canonicalLink(head string) string {
	for _, tag := range linkTagRE.FindAllString(head, -1) {
		attrs := attributes(tag)
		for _, rel := range strings.Fields(strings.ToLower(attrs["rel"])) {
			if rel == "canonical" {
				return attrs["href"]
			}
		}
	}
	return ""
}

func titleText(head string) string {
	match := titleRE.FindStringSubmatch(head)
	if match == nil {
		return ""
	}
	return match[1]
}

// attributes reads one tag's attributes, names lowercased and values
// entity-decoded.
func attributes(tag string) map[string]string {
	out := map[string]string{}
	for _, match := range attrRE.FindAllStringSubmatch(tag, -1) {
		name := strings.ToLower(match[1])
		if _, ok := out[name]; ok {
			continue
		}
		out[name] = html.UnescapeString(firstNonEmpty(match[3], match[4], match[5]))
	}
	return out
}

// stripTags turns markup into text, keeping a line break where a block ended
// and dropping scripts and styles with their content.
func stripTags(markup string) string {
	markup = scriptBodyRE.ReplaceAllString(markup, " ")
	markup = blockTagRE.ReplaceAllString(markup, "\n")
	// What is left is inline markup, which runs inside a word as often as
	// between two ("<b>Zucker</b>mischung"), so it leaves nothing behind.
	return tagRE.ReplaceAllString(markup, "")
}

// cleanText is how every value a page supplies is stored: markup removed,
// entities decoded (twice, because recipe plugins routinely double-encode),
// whitespace collapsed, bounded.
func cleanText(value string, limit int) string {
	if value == "" {
		return ""
	}
	value = html.UnescapeString(value)
	if strings.Contains(value, "<") {
		value = stripTags(value)
	}
	value = html.UnescapeString(value)
	value = spaceRE.ReplaceAllString(value, " ")
	var lines []string
	for _, line := range strings.Split(value, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			lines = append(lines, line)
		}
	}
	return truncateRunes(strings.Join(lines, " "), limit)
}

// cleanLines is cleanText for a value whose line breaks separate items, such
// as instructions written as one paragraph per step.
func cleanLines(value string) []string {
	value = html.UnescapeString(value)
	if strings.Contains(value, "<") {
		value = stripTags(value)
	}
	var out []string
	for _, line := range strings.Split(value, "\n") {
		if line = cleanText(line, maxTextRunes); line != "" {
			out = append(out, line)
		}
	}
	return out
}
