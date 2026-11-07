// File overview: The sections of a recipe's ingredient list.
//
// schema.org has no sections for ingredients: recipeIngredient is one flat
// list of lines. Sites whose recipe has parts ("For the dough", "For the
// sauce") write the part's name into that list as a line of its own, ending
// in a colon. IngredientSections reads those lines back as headings, so the
// cookbook can show the parts and the shopping list can take one part at a
// time. The stored list is not changed: a heading stays a line of it, and
// every ingredient keeps its index into it.

package recipe

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// IngredientLine is one ingredient and its position in the stored list.
type IngredientLine struct {
	Index int    `json:"index"`
	Text  string `json:"text"`
}

// IngredientSection is one part of an ingredient list. A list without
// headings is one section without a name; ingredients before the first
// heading are such a section too.
type IngredientSection struct {
	Name  string           `json:"name"`
	Items []IngredientLine `json:"items"`
}

// maxHeadingRunes is the longest line still read as a heading. A heading
// names a part of the dish; a long line ending in a colon is a sentence.
const maxHeadingRunes = 60

// IngredientSections splits a stored ingredient list at its headings. A
// heading is a line that ends in a colon, carries no digit -- an ingredient
// names an amount, a part of the dish does not -- and is short. A heading
// with no ingredient under it names nothing and is left out, as is any
// section left empty.
func IngredientSections(lines []string) []IngredientSection {
	sections := []IngredientSection{}
	current := IngredientSection{}
	flush := func() {
		if len(current.Items) > 0 {
			sections = append(sections, current)
		}
	}
	for index, line := range lines {
		text := strings.TrimSpace(line)
		if text == "" {
			continue
		}
		if name, ok := IngredientHeading(text); ok {
			flush()
			current = IngredientSection{Name: name}
			continue
		}
		current.Items = append(current.Items, IngredientLine{Index: index, Text: text})
	}
	flush()
	return sections
}

// IngredientHeading reports whether a stored ingredient line is the name of a
// part of the recipe rather than an ingredient, and returns that name without
// its colon.
func IngredientHeading(line string) (string, bool) {
	text := strings.TrimSpace(line)
	if !strings.HasSuffix(text, ":") {
		return "", false
	}
	name := strings.TrimSpace(strings.TrimRight(text, ":"))
	if name == "" || utf8.RuneCountInString(name) > maxHeadingRunes {
		return "", false
	}
	// IsNumber, not IsDigit: an amount is as often written ½ or ⅓ as 1.
	for _, r := range name {
		if unicode.IsNumber(r) {
			return "", false
		}
	}
	return name, true
}
