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
	"strings"
	"unicode/utf8"
)

// copyTagRE finds the tags the copy is read by: headings, paragraphs, lists
// and their items. Everything between them is text (with inline markup that
// cleanText strips). The copy is read as a stream of these tags rather than
// by matching elements whole, because HTML lets a page leave out </p> and
// </li>, and an element match then swallows every list that follows it.
var copyTagRE = regexp.MustCompile(`(?is)<(/?)(h[1-6]|p|ul|ol|li)\b[^>]*>`)

var (
	boldOnlyRE   = regexp.MustCompile(`(?is)^\s*<(strong|b)\b[^>]*>(.*?)</(?:strong|b)\s*>\s*:?\s*$`)
	copySkipRE   = regexp.MustCompile(`(?is)<(script|style|noscript|template|nav|header|footer|aside)\b[^>]*>.*?</(?:script|style|noscript|template|nav|header|footer|aside)\s*>`)
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
	// What is being read: a heading (its level), a paragraph, or a list item
	// of the list being read.
	var (
		textKind  string // "h", "p", "li" or ""
		textLevel int
		textStart int
		list      *copyBlock
		listDepth int
	)
	endText := func(at int) {
		raw := body[textStart:at]
		switch textKind {
		case "h":
			blocks = append(blocks, copyBlock{level: textLevel, text: cleanText(raw, maxShortRunes)})
		case "p":
			if bold := boldOnlyRE.FindStringSubmatch(raw); bold != nil {
				blocks = append(blocks, copyBlock{level: 7, text: cleanText(bold[2], maxShortRunes)})
			} else if text := cleanText(raw, maxTextRunes); text != "" {
				blocks = append(blocks, copyBlock{text: text})
			}
		case "li":
			if text := cleanText(raw, maxTextRunes); text != "" && list != nil {
				list.items = append(list.items, text)
			}
		}
		textKind = ""
	}
	endList := func() {
		if list != nil && len(list.items) > 0 {
			blocks = append(blocks, *list)
		}
		list, listDepth = nil, 0
	}
	for _, loc := range copyTagRE.FindAllStringSubmatchIndex(body, -1) {
		closing := loc[3] > loc[2]
		name := strings.ToLower(body[loc[4]:loc[5]])
		switch {
		case name == "li":
			// An item ends at its own end tag or where the next one begins.
			if textKind != "" {
				endText(loc[0])
			}
			if !closing && list != nil {
				textKind, textStart = "li", loc[1]
			}
		case name == "ul" || name == "ol":
			if textKind != "" {
				endText(loc[0])
			}
			if !closing {
				// A nested list's items are read as items of the list around it.
				if list == nil {
					list = &copyBlock{}
				}
				listDepth++
				continue
			}
			if listDepth--; listDepth <= 0 {
				endList()
			}
		default: // a heading or a paragraph ends whatever text was open
			if textKind == "li" && list != nil && listDepth > 0 && name == "p" {
				// A paragraph inside an item is part of the item.
				continue
			}
			if textKind != "" {
				endText(loc[0])
			}
			if list != nil && !(name == "p" && listDepth > 0) {
				endList()
			}
			if closing {
				continue
			}
			if name == "p" {
				textKind, textStart = "p", loc[1]
			} else {
				textKind, textLevel, textStart = "h", int(name[1]-'0'), loc[1]
			}
		}
	}
	if textKind != "" {
		endText(len(body))
	}
	endList()
	return blocks
}

// copyPart returns the blocks between a heading that opens part and the next
// heading at its level or above, or the heading that opens the other part. A
// bold paragraph is a sub-heading and never ends a part, even one a bold
// paragraph opened. The first heading whose part holds a list is the one: a
// recipe's facts line ("Zubereitung: 30 Minuten") names the part too, and
// holds nothing.
func copyPart(blocks []copyBlock, part string) []copyBlock {
	// A sub-heading naming this part ("Zutaten für den Teig") is one of its
	// sections; only the other part's heading ends it.
	other := "instructions"
	if part == "instructions" {
		other = "ingredients"
	}
	for start, opener := range blocks {
		if opener.level == 0 || !copyHeadingNames(opener.text, part) {
			continue
		}
		var out []copyBlock
		lists := false
		for _, block := range blocks[start+1:] {
			if block.level > 0 && ((block.level < 7 && block.level <= opener.level) || copyHeadingNames(block.text, other)) {
				break
			}
			lists = lists || len(block.items) > 0
			out = append(out, block)
		}
		if lists {
			return out
		}
	}
	return nil
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
		if heading != "" && len(out) < maxListItems-1 {
			out = append(out, heading)
		}
		heading = ""
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
		if steps >= maxListItems {
			break
		}
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
