// File overview: schema.org Recipe read out of a page's JSON-LD.
//
// JSON-LD is a loose format in practice: every property may be a string, a
// number, an object, or a list of any of those, and recipe plugins use all of
// them. A Recipe can stand alone, sit in a list, hide in an @graph, or be the
// mainEntity of a WebPage. So the reading here is a walk that accepts every one
// of those shapes and keeps only what it can turn into text.

package recipe

import (
	"encoding/json"
	"math"
	"strconv"
	"strings"
)

func recipeFromJSONLD(markup, pageURL string) *Recipe {
	for _, match := range scriptLDRE.FindAllStringSubmatch(markup, -1) {
		attrs := attributes("<script " + match[1] + ">")
		if !strings.Contains(strings.ToLower(attrs["type"]), "ld+json") {
			continue
		}
		body := strings.TrimSpace(match[2])
		body = strings.TrimPrefix(body, "<!--")
		body = strings.TrimSuffix(body, "-->")
		body = strings.TrimSpace(strings.TrimPrefix(strings.TrimSuffix(body, "]]>"), "//<![CDATA["))
		var doc any
		if err := json.Unmarshal([]byte(body), &doc); err != nil {
			// Raw line breaks inside strings are the common way a theme breaks
			// its own JSON; outside strings a space means the same thing.
			if err := json.Unmarshal([]byte(controlCharReplacer.Replace(body)), &doc); err != nil {
				continue
			}
		}
		if node := findRecipeNode(doc, 0); node != nil {
			return recipeFromNode(node, pageURL)
		}
	}
	return nil
}

// findRecipeNode walks a JSON-LD document for the first object typed Recipe.
// The depth bound is there because a page can nest arbitrarily and a walk is
// not the place to find that out.
var controlCharReplacer = strings.NewReplacer("\n", " ", "\r", " ", "\t", " ")

func findRecipeNode(value any, depth int) map[string]any {
	if depth > 12 {
		return nil
	}
	switch v := value.(type) {
	case map[string]any:
		if isRecipeType(v["@type"]) {
			return v
		}
		// @graph and mainEntity first: they are where a page puts its subject.
		for _, key := range []string{"@graph", "mainEntity", "mainEntityOfPage", "itemListElement", "item", "hasPart"} {
			if found := findRecipeNode(v[key], depth+1); found != nil {
				return found
			}
		}
	case []any:
		for _, item := range v {
			if found := findRecipeNode(item, depth+1); found != nil {
				return found
			}
		}
	}
	return nil
}

func isRecipeType(value any) bool {
	for _, name := range stringsOf(value) {
		name = strings.TrimSpace(name)
		if index := strings.LastIndexAny(name, "/:#"); index >= 0 {
			name = name[index+1:]
		}
		if strings.EqualFold(name, "Recipe") {
			return true
		}
	}
	return false
}

func recipeFromNode(node map[string]any, pageURL string) *Recipe {
	r := &Recipe{
		Name:         cleanText(textOf(node["name"]), maxShortRunes),
		Description:  cleanText(textOf(node["description"]), maxTextRunes),
		URL:          resolveURL(pageURL, textOf(node["url"])),
		Author:       cleanText(nameOf(node["author"]), maxShortRunes),
		Yield:        yieldOf(node["recipeYield"]),
		TotalMinutes: durationMinutes(textOf(node["totalTime"])),
		PrepMinutes:  durationMinutes(textOf(node["prepTime"])),
		CookMinutes:  durationMinutes(textOf(node["cookTime"])),
		Categories:   termList(node["recipeCategory"], maxKeywordList),
		Cuisines:     termList(node["recipeCuisine"], maxKeywordList),
		Keywords:     termList(node["keywords"], maxKeywordList),
	}
	if r.TotalMinutes == 0 {
		r.TotalMinutes = r.PrepMinutes + r.CookMinutes
	}
	for _, image := range imagesOf(node["image"]) {
		if resolved := resolveURL(pageURL, image); resolved != "" {
			r.ImageURLs = append(r.ImageURLs, resolved)
		}
		if len(r.ImageURLs) >= 8 {
			break
		}
	}
	ingredients := node["recipeIngredient"]
	if ingredients == nil {
		ingredients = node["ingredients"]
	}
	for _, item := range stringsOf(ingredients) {
		if text := cleanText(item, maxShortRunes); text != "" {
			r.Ingredients = append(r.Ingredients, text)
		}
		if len(r.Ingredients) >= maxListItems {
			break
		}
	}
	r.Instructions = instructionSections(node["recipeInstructions"])
	if rating, ok := node["aggregateRating"].(map[string]any); ok {
		r.RatingValue = numberOf(rating["ratingValue"])
		count := numberOf(rating["ratingCount"])
		if count == 0 {
			count = numberOf(rating["reviewCount"])
		}
		if best := numberOf(rating["bestRating"]); best > 0 && best != 5 && r.RatingValue > 0 {
			r.RatingValue = r.RatingValue / best * 5
		}
		if r.RatingValue < 0 || r.RatingValue > 5 || math.IsNaN(r.RatingValue) {
			r.RatingValue = 0
		}
		// The count is stored in an INTEGER column: a page claiming more votes
		// than that holds would fail the write on every retry and hold up the
		// reader's whole queue behind one link.
		if count > 0 && count <= math.MaxInt32 {
			r.RatingCount = int(count)
		}
	}
	if nutrition, ok := node["nutrition"].(map[string]any); ok {
		r.Calories = cleanText(textOf(nutrition["calories"]), 40)
	}
	return r
}

// instructionSections reads recipeInstructions in every shape it is written
// in: one string (paragraphs or a list in markup), a list of strings, a list of
// HowToStep, or HowToSections holding steps.
func instructionSections(value any) []StepSection {
	var sections []StepSection
	loose := StepSection{}
	addStep := func(section *StepSection, text string) {
		for _, line := range cleanLines(text) {
			if len(section.Steps) < maxListItems {
				section.Steps = append(section.Steps, line)
			}
		}
	}
	var walk func(value any, section *StepSection, depth int)
	walk = func(value any, section *StepSection, depth int) {
		if depth > 6 {
			return
		}
		switch v := value.(type) {
		case string:
			addStep(section, v)
		case []any:
			for _, item := range v {
				walk(item, section, depth+1)
			}
		case map[string]any:
			types := strings.ToLower(strings.Join(stringsOf(v["@type"]), " "))
			if strings.Contains(types, "howtosection") || (v["itemListElement"] != nil && !strings.Contains(types, "howtostep")) {
				if len(sections) >= maxSections {
					return
				}
				next := StepSection{Name: cleanText(textOf(v["name"]), maxShortRunes)}
				walk(v["itemListElement"], &next, depth+1)
				if len(next.Steps) > 0 {
					sections = append(sections, next)
				}
				return
			}
			text := textOf(v["text"])
			if text == "" {
				text = textOf(v["name"])
			}
			if text == "" {
				text = textOf(v["description"])
			}
			addStep(section, text)
		}
	}
	walk(value, &loose, 0)
	if len(loose.Steps) > 0 {
		sections = append([]StepSection{loose}, sections...)
	}
	return sections
}

// textOf reads a value that should be one piece of text: a string, a number,
// an {"@value": ...} object, or the first of a list.
func textOf(value any) string {
	switch v := value.(type) {
	case string:
		return strings.TrimSpace(v)
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	case map[string]any:
		if inner := textOf(v["@value"]); inner != "" {
			return inner
		}
		return textOf(v["text"])
	case []any:
		for _, item := range v {
			if text := textOf(item); text != "" {
				return text
			}
		}
	}
	return ""
}

// stringsOf reads a value that may be one string or a list of them.
func stringsOf(value any) []string {
	switch v := value.(type) {
	case string:
		return []string{v}
	case float64:
		return []string{strconv.FormatFloat(v, 'f', -1, 64)}
	case map[string]any:
		if text := textOf(v); text != "" {
			return []string{text}
		}
	case []any:
		var out []string
		for _, item := range v {
			out = append(out, stringsOf(item)...)
		}
		return out
	}
	return nil
}

// nameOf reads a Person or Organization -- or a list of them, or a bare string.
func nameOf(value any) string {
	switch v := value.(type) {
	case map[string]any:
		return textOf(v["name"])
	case []any:
		var names []string
		for _, item := range v {
			if name := nameOf(item); name != "" {
				names = append(names, name)
			}
			if len(names) == 3 {
				break
			}
		}
		return strings.Join(names, ", ")
	}
	return textOf(value)
}

// imagesOf reads image as a URL, an ImageObject, or a list of either.
func imagesOf(value any) []string {
	switch v := value.(type) {
	case string:
		return []string{strings.TrimSpace(v)}
	case map[string]any:
		if u := textOf(v["url"]); u != "" {
			return []string{u}
		}
		return []string{textOf(v["contentUrl"])}
	case []any:
		var out []string
		for _, item := range v {
			out = append(out, imagesOf(item)...)
		}
		return out
	}
	return nil
}

// yieldOf picks the servings line a reader understands. Plugins often publish
// a bare number beside the same number in words ("4", "4 Portionen"); the one
// with words is the one to print.
func yieldOf(value any) string {
	options := stringsOf(value)
	best := ""
	for _, option := range options {
		option = cleanText(option, 80)
		if option == "" {
			continue
		}
		if best == "" || (!hasLetter(best) && hasLetter(option)) {
			best = option
		}
	}
	return best
}

func hasLetter(value string) bool {
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || r > 127 {
			return true
		}
	}
	return false
}

// termList reads keywords and categories, which arrive as a list or as one
// comma-separated string, into distinct, trimmed terms.
func termList(value any, limit int) []string {
	seen := map[string]bool{}
	var out []string
	for _, raw := range stringsOf(value) {
		for _, part := range strings.Split(raw, ",") {
			term := cleanText(part, 60)
			key := strings.ToLower(term)
			if term == "" || seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, term)
			if len(out) >= limit {
				return out
			}
		}
	}
	return out
}

func numberOf(value any) float64 {
	switch v := value.(type) {
	case float64:
		return v
	case string:
		f, err := strconv.ParseFloat(strings.ReplaceAll(strings.TrimSpace(v), ",", "."), 64)
		if err == nil {
			return f
		}
	case map[string]any:
		return numberOf(v["@value"])
	case []any:
		if len(v) > 0 {
			return numberOf(v[0])
		}
	}
	return 0
}
