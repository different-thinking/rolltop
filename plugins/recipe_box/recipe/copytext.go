// File overview: The ingredients and steps of a declared recipe, read from
// the page's own copy when its structured block leaves them out.
//
// Some publishers -- the public broadcasters' recipe pages among them -- emit
// a schema.org Recipe that names the dish and its picture and nothing else,
// and print the ingredients and the method only as the article's text, under
// a heading that says what follows ("Zutaten", "Zubereitung"). The block is
// the page's statement that it is a recipe; it is only incomplete. So this
// reader runs for a page that already declared a Recipe and never decides
// that anything is one: it fills in what the declaration left empty, from the
// lists printed under those headings, and finds nothing on a page that prints
// none. A recipe whose block carries ingredients or steps of its own is never
// read here.

package recipe

import (
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// copyBlockRE finds, in document order, the three things the copy is read
// for: headings, paragraphs (a bold one is a sub-heading) and lists. Lists
// are matched lazily, so a nested list ends its parent early -- an ingredient
// list with a nested list is rare enough that losing its tail is the price.
var copyBlockRE = regexp.MustCompile(`(?is)<h([1-6])\b[^>]*>(.*?)</h[1-6]\s*>|<p\b[^>]*>(.*?)</p\s*>|<(ul|ol)\b[^>]*>(.*?)</(?:ul|ol)\s*>`)

var (
	listItemRE   = regexp.MustCompile(`(?is)<li\b[^>]*>(.*?)(?:</li\s*>|$)`)
	boldOnlyRE   = regexp.MustCompile(`(?is)^\s*<(strong|b)\b[^>]*>(.*?)</(?:strong|b)\s*>\s*:?\s*$`)
	copySkipRE   = regexp.MustCompile(`(?is)<(script|style|noscript|template|nav|header|footer|aside|form)\b[^>]*>.*?</(?:script|style|noscript|template|nav|header|footer|aside|form)\s*>`)
	copyKeywords = map[string][]string{
		"ingredients":  {"zutaten", "zutatenliste", "ingredients"},
		"instructions": {"zubereitung", "anleitung", "so geht's", "so geht’s", "instructions", "directions", "method", "preparation"},
	}
)

// maxCopyHeadingRunes is the longest heading that may open a part. "Zutaten
// für 4 Personen" is a heading; a sentence that starts with "Zutaten" is not.
const maxCopyHeadingRunes = 60

type copyBlock struct {
	level int // 1-6 for a heading, 7 for a bold paragraph, 0 otherwise
	text  string
	items []string
}

// fillFromCopy completes a declared recipe whose block carries neither
// ingredients nor steps.
func fillFromCopy(r *Recipe, markup string) {
	if r == nil || len(r.Ingredients) > 0 || len(r.Instructions) > 0 {
		return
	}
	blocks := copyBlocks(markup)
	r.Ingredients = copyIngredients(blocks)
	r.Instructions = copyInstructions(blocks)
}

func copyBlocks(markup string) []copyBlock {
	body := markup
	if loc := headEndRE.FindStringIndex(markup); loc != nil {
		body = markup[loc[1]:]
	}
	body = copySkipRE.ReplaceAllString(body, " ")
	var blocks []copyBlock
	for _, match := range copyBlockRE.FindAllStringSubmatch(body, -1) {
		switch {
		case match[1] != "":
			level, _ := strconv.Atoi(match[1])
			blocks = append(blocks, copyBlock{level: level, text: cleanText(match[2], maxShortRunes)})
		case match[4] != "":
			block := copyBlock{}
			for _, item := range listItemRE.FindAllStringSubmatch(match[5], -1) {
				if text := cleanText(item[1], maxTextRunes); text != "" {
					block.items = append(block.items, text)
				}
			}
			if len(block.items) > 0 {
				blocks = append(blocks, block)
			}
		default:
			if bold := boldOnlyRE.FindStringSubmatch(match[3]); bold != nil {
				blocks = append(blocks, copyBlock{level: 7, text: cleanText(bold[2], maxShortRunes)})
			} else {
				blocks = append(blocks, copyBlock{text: cleanText(match[3], maxTextRunes)})
			}
		}
	}
	return blocks
}

// copyPart returns the blocks between the heading that opens part (the first
// one naming it) and the next heading at its level or above, or the heading
// that opens the other part.
func copyPart(blocks []copyBlock, part string) []copyBlock {
	start, level := -1, 0
	for i, block := range blocks {
		if block.level > 0 && copyHeadingNames(block.text, part) {
			start, level = i, block.level
			break
		}
	}
	if start < 0 {
		return nil
	}
	var out []copyBlock
	for _, block := range blocks[start+1:] {
		if block.level > 0 && (block.level <= level || copyHeadingNames(block.text, "ingredients") || copyHeadingNames(block.text, "instructions")) {
			break
		}
		out = append(out, block)
	}
	return out
}

// copyHeadingNames reports whether a heading opens the given part: it is one
// of the part's words, alone or followed by what it is for, and short.
func copyHeadingNames(text, part string) bool {
	name := strings.ToLower(strings.TrimSpace(strings.TrimRight(strings.TrimSpace(text), ":")))
	if name == "" || utf8.RuneCountInString(name) > maxCopyHeadingRunes {
		return false
	}
	for _, word := range copyKeywords[part] {
		if name == word || strings.HasPrefix(name, word+" ") || strings.HasPrefix(name, word+":") {
			return true
		}
	}
	return false
}

// copyIngredients is every list item of the ingredients part, with the part's
// sub-headings written into the list the way sites write them into
// recipeIngredient, so IngredientSections reads them back as sections. A
// sub-heading IngredientHeading would not read as one -- it carries a number
// -- is left out rather than stored as an ingredient.
func copyIngredients(blocks []copyBlock) []string {
	var out []string
	heading := ""
	for _, block := range copyPart(blocks, "ingredients") {
		if block.level > 0 {
			heading = ""
			if name, ok := IngredientHeading(strings.TrimRight(block.text, ":") + ":"); ok {
				heading = name + ":"
			}
			continue
		}
		if len(block.items) == 0 {
			continue
		}
		if heading != "" {
			out = append(out, heading)
			heading = ""
		}
		for _, item := range block.items {
			if len(out) >= maxListItems {
				return out
			}
			out = append(out, cleanText(item, maxShortRunes))
		}
	}
	return out
}

// copyInstructions is every list item of the instructions part, one section
// per sub-heading. Prose paragraphs are not read as steps: a page's copy
// carries tips and asides between them, and a list is what says "a step".
func copyInstructions(blocks []copyBlock) []StepSection {
	var sections []StepSection
	current := StepSection{}
	steps := 0
	flush := func() {
		if len(current.Steps) > 0 && len(sections) < maxSections {
			sections = append(sections, current)
		}
	}
	for _, block := range copyPart(blocks, "instructions") {
		if block.level > 0 {
			flush()
			current = StepSection{Name: strings.TrimSpace(strings.TrimRight(block.text, ":"))}
			continue
		}
		for _, item := range block.items {
			if steps >= maxListItems {
				break
			}
			current.Steps = append(current.Steps, item)
			steps++
		}
	}
	flush()
	return sections
}
