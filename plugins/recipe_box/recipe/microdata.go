// File overview: schema.org Recipe read out of a page's microdata.
//
// Microdata is what recipe sites published before JSON-LD and what some still
// do. It is read here without a DOM: from the element that declares the Recipe
// type onwards, every element carrying an itemprop contributes the value the
// microdata rules give it -- an attribute for the void and link-like elements,
// the text content for everything else. That is approximate where a recipe
// nests another item (an author's own name, a nutrition block), which is why
// only the properties a card needs are read and the first one found wins.

package recipe

import (
	"regexp"
	"strings"
)

var (
	recipeItemTypeRE = regexp.MustCompile(`(?is)<[a-z][a-z0-9]*\b[^>]*\bitemtype\s*=\s*["']?https?://schema\.org/Recipe\b[^>]*>`)
	itemPropTagRE    = regexp.MustCompile(`(?is)<([a-z][a-z0-9]*)\b[^>]*\bitemprop\s*=[^>]*>`)
)

const (
	// maxMicrodataProps bounds how many itemprop elements one page is read
	// for. A recipe uses a few dozen; a page with thousands is listing
	// something else, or is built to be expensive to read.
	maxMicrodataProps = 2000
	// maxMicrodataTextBytes bounds the text collected from them in all.
	maxMicrodataTextBytes = 2 * 1024 * 1024
)

// microdataProps are the properties a recipe is read for, each with how many
// values of it can be used: one for a scalar (a few, since the first may be
// empty), a list's length for a list.
var microdataProps = map[string]int{
	"name": 3, "description": 3, "recipeYield": 3, "totalTime": 3, "prepTime": 3, "cookTime": 3,
	"recipeCategory": 3, "recipeCuisine": 3, "keywords": 3, "image": 3,
	"recipeIngredient": maxListItems, "ingredients": maxListItems, "recipeInstructions": maxListItems,
}

func recipeFromMicrodata(markup, pageURL string) *Recipe {
	loc := recipeItemTypeRE.FindStringIndex(markup)
	if loc == nil {
		return nil
	}
	region := markup[loc[0]:]
	values := map[string][]string{}
	budget := maxMicrodataTextBytes
	for _, match := range itemPropTagRE.FindAllStringSubmatchIndex(region, maxMicrodataProps) {
		tag := region[match[0]:match[1]]
		name := strings.ToLower(region[match[2]:match[3]])
		attrs := attributes(tag)
		// Only the properties a card is built from are read, each only as
		// often as it can be used. Reading the text of every other itemprop -- a
		// review's, a breadcrumb's -- is work a hostile page could multiply by
		// the thousand, in the one loop every reader's recipes wait on.
		var wanted []string
		for _, prop := range strings.Fields(attrs["itemprop"]) {
			if limit, ok := microdataProps[prop]; ok && len(values[prop]) < limit {
				wanted = append(wanted, prop)
			}
		}
		if len(wanted) == 0 {
			continue
		}
		var value string
		switch name {
		case "meta":
			value = attrs["content"]
		case "img", "source":
			value = firstNonEmpty(attrs["src"], attrs["data-src"], attrs["content"])
		case "a", "link":
			value = firstNonEmpty(attrs["href"], attrs["content"])
		case "time":
			value = firstNonEmpty(attrs["datetime"], attrs["content"], elementText(region, match[1], name))
		case "data", "meter":
			value = firstNonEmpty(attrs["value"], attrs["content"])
		default:
			value = firstNonEmpty(attrs["content"], elementText(region, match[1], name))
		}
		for _, prop := range wanted {
			values[prop] = append(values[prop], value)
		}
		if budget -= len(value); budget <= 0 {
			break
		}
	}
	first := func(prop string) string {
		for _, value := range values[prop] {
			if strings.TrimSpace(value) != "" {
				return value
			}
		}
		return ""
	}
	r := &Recipe{
		Name:         cleanText(first("name"), maxShortRunes),
		Description:  cleanText(first("description"), maxTextRunes),
		Yield:        cleanText(first("recipeYield"), 80),
		TotalMinutes: durationMinutes(first("totalTime")),
		PrepMinutes:  durationMinutes(first("prepTime")),
		CookMinutes:  durationMinutes(first("cookTime")),
		Categories:   termList(first("recipeCategory"), maxKeywordList),
		Cuisines:     termList(first("recipeCuisine"), maxKeywordList),
		Keywords:     termList(first("keywords"), maxKeywordList),
	}
	if r.TotalMinutes == 0 {
		r.TotalMinutes = r.PrepMinutes + r.CookMinutes
	}
	if image := resolveURL(pageURL, first("image")); image != "" {
		r.ImageURLs = []string{image}
	}
	ingredients := values["recipeIngredient"]
	if len(ingredients) == 0 {
		ingredients = values["ingredients"]
	}
	for _, item := range ingredients {
		if len(r.Ingredients) >= maxListItems {
			break
		}
		if text := cleanText(item, maxShortRunes); text != "" {
			r.Ingredients = append(r.Ingredients, text)
		}
	}
	section := StepSection{}
	for _, item := range values["recipeInstructions"] {
		if len(section.Steps) >= maxListItems {
			break
		}
		for _, line := range cleanLines(item) {
			if len(section.Steps) < maxListItems {
				section.Steps = append(section.Steps, line)
			}
		}
	}
	if len(section.Steps) > 0 {
		r.Instructions = []StepSection{section}
	}
	return r
}

// elementText returns the markup between an opening tag ending at start and
// the closing tag that balances it, bounded so an unclosed element cannot make
// one property the rest of the page.
func elementText(markup string, start int, name string) string {
	switch name {
	case "br", "hr", "input", "area", "base", "col", "embed", "param", "track", "wbr":
		return ""
	}
	const limit = 64 * 1024
	end := len(markup)
	if end-start > limit {
		end = start + limit
	}
	// The tags are matched in the markup itself, ASCII case folded per byte:
	// lowercasing a copy would cost a 64 KiB allocation per property, and its
	// offsets need not be the original's (an invalid byte becomes U+FFFD).
	segment := markup[start:end]
	open := "<" + name
	closing := "</" + name
	depth := 1
	for i := 0; i < len(segment); {
		next := strings.IndexByte(segment[i:], '<')
		if next < 0 {
			break
		}
		i += next
		rest := segment[i:]
		switch {
		case tagNameAt(rest, closing):
			depth--
			if depth == 0 {
				return segment[:i]
			}
		case tagNameAt(rest, open):
			depth++
		}
		i++
	}
	return segment
}

// tagNameAt reports whether s starts with prefix, ASCII case-insensitively,
// and the tag name ends there -- so "</a" does not match "</abbr>".
func tagNameAt(s, prefix string) bool {
	if len(s) <= len(prefix) || !strings.EqualFold(s[:len(prefix)], prefix) {
		return false
	}
	return !isNameChar(s[len(prefix)])
}

func isNameChar(b byte) bool {
	if b >= 'A' && b <= 'Z' {
		b += 'a' - 'A'
	}
	return (b >= 'a' && b <= 'z') || (b >= '0' && b <= '9') || b == '-'
}
