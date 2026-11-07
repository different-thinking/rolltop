// File overview: Reading an ingredient line as amount, unit and name, so the
// shopping list can add up what several recipes ask for.
//
// A recipe writes "400 g Tomaten", "1,5 kg Mehl", "½ TL Salz" or "2-3
// Zwiebeln". ParseIngredient reads the amount at the front, a unit from a
// fixed table, and the rest as the name. Two lines add up only when their
// Key is equal: the same name, written loosely (case, singular and plural,
// umlauts, notes after a comma or in brackets aside), in the same kind of
// unit. Grams and kilograms are one kind and are kept in grams, millilitres
// and litres in millilitres; every other unit only adds up with itself. A
// line whose amount cannot be read is kept as it is and only meets lines
// that read the same.

package recipe

import (
	"strconv"
	"strings"
	"unicode"
)

// Ingredient is one ingredient line read for the shopping list.
type Ingredient struct {
	// Amount is in Unit; for weights in grams, for volumes in millilitres.
	Amount    float64
	HasAmount bool
	// Unit is the label the amount is counted in: "g", "ml", a label from
	// the unit table ("EL", "Dose", "cup"), or empty for a plain count.
	Unit string
	// Name is what to buy, without the amount, the unit and the notes after
	// a comma or in brackets.
	Name string
	// Key says which lines add up: equal keys are the same thing to buy,
	// counted in the same kind of unit.
	Key string
}

type unitInfo struct {
	label     string
	dimension string
	factor    float64
}

// units maps a unit as written (lower case, without a trailing full stop) to
// the label it is counted in. Weights and volumes turn into grams and
// millilitres so that "1 kg" and "500 g" add up; the other units keep the
// spelling of their language and only add up with themselves, so "EL" and
// "tbsp" are one kind but keep the label the first line used.
var units = map[string]unitInfo{}

func init() {
	add := func(label, dimension string, factor float64, spellings ...string) {
		for _, spelling := range spellings {
			units[spelling] = unitInfo{label: label, dimension: dimension, factor: factor}
		}
	}
	add("g", "mass", 1, "g", "gr", "gramm", "gram", "grams", "gramme")
	add("g", "mass", 1000, "kg", "kilo", "kilos", "kilogramm", "kilogram", "kilograms")
	add("g", "mass", 0.001, "mg", "milligramm", "milligram")
	add("ml", "volume", 1, "ml", "milliliter", "millilitre", "milliliters", "millilitres")
	add("ml", "volume", 10, "cl", "centiliter", "zentiliter")
	add("ml", "volume", 100, "dl", "deziliter", "deciliter")
	add("ml", "volume", 1000, "l", "liter", "litre", "liters", "litres", "ltr")
	add("EL", "tablespoon", 1, "el", "essl", "esslöffel", "eßlöffel")
	add("tbsp", "tablespoon", 1, "tbsp", "tbs", "tablespoon", "tablespoons")
	add("TL", "teaspoon", 1, "tl", "teel", "teelöffel")
	add("tsp", "teaspoon", 1, "tsp", "teaspoon", "teaspoons")
	add("Prise", "pinch", 1, "prise", "prisen")
	add("pinch", "pinch", 1, "pinch", "pinches")
	add("Msp.", "knifepoint", 1, "msp", "messerspitze", "messerspitzen")
	add("Bund", "bunch", 1, "bund", "bunde")
	add("bunch", "bunch", 1, "bunch", "bunches")
	add("Dose", "can", 1, "dose", "dosen")
	add("can", "can", 1, "can", "cans", "tin", "tins")
	add("Pck.", "pack", 1, "pck", "pckg", "pkg", "päckchen", "packung", "packungen", "pack", "packs", "packet", "packets", "package", "packages")
	add("Becher", "tub", 1, "becher")
	add("Glas", "jar", 1, "glas", "gläser", "jar", "jars")
	add("Zehe", "clove", 1, "zehe", "zehen")
	add("clove", "clove", 1, "clove", "cloves")
	add("Stück", "piece", 1, "stück", "stk", "stck", "piece", "pieces")
	add("Scheibe", "slice", 1, "scheibe", "scheiben")
	add("slice", "slice", 1, "slice", "slices")
	add("Tasse", "cup", 1, "tasse", "tassen")
	add("cup", "cup", 1, "cup", "cups")
	add("Handvoll", "handful", 1, "handvoll")
	add("handful", "handful", 1, "handful", "handfuls")
	add("Würfel", "cube", 1, "würfel")
	add("Blatt", "leaf", 1, "blatt", "blätter")
	add("Zweig", "sprig", 1, "zweig", "zweige")
	add("sprig", "sprig", 1, "sprig", "sprigs")
	add("Stange", "stalk", 1, "stange", "stangen")
	add("Kopf", "head", 1, "kopf", "köpfe")
	add("Tropfen", "drop", 1, "tropfen")
	add("oz", "ounce", 1, "oz")
	add("lb", "pound", 1, "lb", "lbs")
}

// vulgarFractions are the fraction characters recipes write amounts with.
var vulgarFractions = map[rune]float64{
	'½': 1.0 / 2, '⅓': 1.0 / 3, '⅔': 2.0 / 3, '¼': 1.0 / 4, '¾': 3.0 / 4,
	'⅕': 1.0 / 5, '⅖': 2.0 / 5, '⅗': 3.0 / 5, '⅘': 4.0 / 5, '⅙': 1.0 / 6,
	'⅚': 5.0 / 6, '⅛': 1.0 / 8, '⅜': 3.0 / 8, '⅝': 5.0 / 8, '⅞': 7.0 / 8,
}

// approximateWords stand before an amount and change nothing about what to buy.
var approximateWords = []string{"ca.", "ca", "circa", "etwa", "about", "approx.", "approx", "approximately", "~"}

// vagueWords stand before a name that has no amount: "etwas Salz" is salt.
var vagueWords = []string{"etwas", "ein wenig", "wenig", "einige", "some", "a little", "a few"}

// ParseIngredient reads an ingredient line. A line with no amount at its front
// is kept whole as the name, so "Salz und Pfeffer" stays what it says.
func ParseIngredient(line string) Ingredient {
	text := strings.Join(strings.Fields(line), " ")
	rest := text
	for _, word := range approximateWords {
		if trimmed, ok := cutWord(rest, word); ok {
			rest = trimmed
			break
		}
	}
	out := Ingredient{}
	amountText := rest
	if amount, after, ok := readAmount(rest); ok {
		out.Amount, out.HasAmount, rest = amount, true, after
		// A range ("2-3 Zwiebeln") buys enough for its upper end.
		if upper, after, ok := readRange(rest); ok {
			if upper > out.Amount {
				out.Amount = upper
			}
			rest = after
		}
		if unit, after, ok := readUnit(rest); ok {
			out.Unit = unit.label
			out.Amount *= unit.factor
			rest = after
			if trimmed, ok := cutWord(rest, "of"); ok {
				rest = trimmed
			}
			if amount, ok := thousandsAmount(amountText, unit); ok {
				out.Amount = amount
			}
			out.Key = unit.dimension
		} else {
			out.Key = "count"
		}
	} else {
		rest = text
		for _, word := range vagueWords {
			if trimmed, ok := cutWord(rest, word); ok {
				rest = trimmed
				break
			}
		}
		out.Key = "none"
	}
	out.Name = cleanName(rest, out.HasAmount)
	if out.Name == "" {
		out.Name = text
	}
	out.Key += "|" + nameKey(out.Name)
	return out
}

// cutWord takes a word off the front of a line, ignoring case, when a space
// or the end follows it.
func cutWord(text, word string) (string, bool) {
	if len(text) < len(word) || !strings.EqualFold(text[:len(word)], word) {
		return text, false
	}
	rest := text[len(word):]
	if rest != "" && rest[0] != ' ' {
		// "ca." already ends in a full stop; anything else must end at a space.
		if !strings.HasSuffix(word, ".") && word != "~" {
			return text, false
		}
	}
	return strings.TrimSpace(rest), true
}

// readAmount reads a number at the front: "400", "1,5", "1.5", "½", "1 ½",
// "1½", "1/2", "1 1/2".
func readAmount(text string) (float64, string, bool) {
	value, rest, ok := readNumber(text)
	if !ok {
		return 0, text, false
	}
	// A whole number followed by a fraction is one amount: "1 ½", "1 1/2".
	if value == float64(int(value)) {
		trimmed := strings.TrimLeft(rest, " ")
		if r := firstRune(trimmed); vulgarFractions[r] > 0 {
			return value + vulgarFractions[r], strings.TrimSpace(trimmed[len(string(r)):]), true
		}
		if trimmed != rest || rest == "" {
			if numerator, denominator, after, ok := readSlash(trimmed); ok && numerator < denominator {
				return value + numerator/denominator, after, true
			}
		}
	}
	return value, strings.TrimSpace(rest), true
}

// readNumber reads one number: digits with an optional decimal part, a
// fraction character, or a fraction written with a slash.
func readNumber(text string) (float64, string, bool) {
	if r := firstRune(text); vulgarFractions[r] > 0 {
		return vulgarFractions[r], text[len(string(r)):], true
	}
	if numerator, denominator, after, ok := readSlash(text); ok {
		return numerator / denominator, after, true
	}
	end := 0
	for end < len(text) && text[end] >= '0' && text[end] <= '9' {
		end++
	}
	if end == 0 {
		return 0, text, false
	}
	if end+1 < len(text) && (text[end] == ',' || text[end] == '.') && text[end+1] >= '0' && text[end+1] <= '9' {
		end++
		for end < len(text) && text[end] >= '0' && text[end] <= '9' {
			end++
		}
	}
	value, err := strconv.ParseFloat(strings.Replace(text[:end], ",", ".", 1), 64)
	if err != nil {
		return 0, text, false
	}
	// "1½": a fraction character right after the digits belongs to them.
	if r := firstRune(text[end:]); vulgarFractions[r] > 0 && !strings.ContainsAny(text[:end], ".,") {
		return value + vulgarFractions[r], text[end+len(string(r)):], true
	}
	return value, text[end:], true
}

// readSlash reads "1/2" at the front.
func readSlash(text string) (float64, float64, string, bool) {
	i := 0
	for i < len(text) && text[i] >= '0' && text[i] <= '9' {
		i++
	}
	if i == 0 || i >= len(text) || text[i] != '/' {
		return 0, 0, text, false
	}
	j := i + 1
	for j < len(text) && text[j] >= '0' && text[j] <= '9' {
		j++
	}
	if j == i+1 {
		return 0, 0, text, false
	}
	numerator, _ := strconv.ParseFloat(text[:i], 64)
	denominator, _ := strconv.ParseFloat(text[i+1:j], 64)
	if denominator == 0 {
		return 0, 0, text, false
	}
	return numerator, denominator, strings.TrimSpace(text[j:]), true
}

// readRange reads the upper end of "2-3", "2 – 3", "2 bis 3" or "2 to 3".
func readRange(text string) (float64, string, bool) {
	rest := strings.TrimLeft(text, " ")
	switch {
	case strings.HasPrefix(rest, "-"):
		rest = rest[1:]
	case strings.HasPrefix(rest, "–"):
		rest = rest[len("–"):]
	default:
		trimmed, ok := cutWord(rest, "bis")
		if !ok {
			trimmed, ok = cutWord(rest, "to")
		}
		if !ok {
			return 0, text, false
		}
		rest = trimmed
	}
	value, after, ok := readNumber(strings.TrimLeft(rest, " "))
	if !ok {
		return 0, text, false
	}
	return value, strings.TrimSpace(after), true
}

// readUnit reads a unit from the table at the front, attached to the number
// ("400g") or as the next word.
func readUnit(text string) (unitInfo, string, bool) {
	end := 0
	for end < len(text) {
		r := firstRune(text[end:])
		if !unicode.IsLetter(r) {
			break
		}
		end += len(string(r))
	}
	if end == 0 {
		return unitInfo{}, text, false
	}
	word := strings.ToLower(text[:end])
	after := text[end:]
	if strings.HasPrefix(after, ".") {
		after = after[1:]
	}
	if after != "" && after[0] != ' ' && after[0] != ',' && after[0] != '(' {
		return unitInfo{}, text, false
	}
	unit, ok := units[word]
	if !ok {
		return unitInfo{}, text, false
	}
	return unit, strings.TrimSpace(after), true
}

// thousandsAmount reads "1.000 g" as a thousand grams. A full stop followed by
// exactly three digits before grams or millilitres is a thousands separator:
// nobody weighs out 1.000 grams to the milligram. Only the amount at the
// front is looked at -- "Mehl Type 1.050" names a flour, not an amount.
func thousandsAmount(amountText string, unit unitInfo) (float64, bool) {
	if unit.factor != 1 || (unit.label != "g" && unit.label != "ml") {
		return 0, false
	}
	fields := strings.Fields(amountText)
	if len(fields) == 0 {
		return 0, false
	}
	digits := strings.TrimRightFunc(fields[0], unicode.IsLetter)
	dot := strings.IndexByte(digits, '.')
	if dot < 1 || dot > 3 || len(digits)-dot-1 != 3 {
		return 0, false
	}
	whole, wholeErr := strconv.Atoi(digits[:dot])
	frac, fracErr := strconv.Atoi(digits[dot+1:])
	if wholeErr != nil || fracErr != nil {
		return 0, false
	}
	return float64(whole*1000 + frac), true
}

// cleanName drops what is a note rather than the thing to buy: anything in
// brackets and, on a line with an amount, anything after the first comma
// ("400 g Tomaten, gewürfelt"). A line without an amount keeps its commas:
// there they list things ("Salz, Pfeffer"), and cutting would drop one.
func cleanName(text string, hasAmount bool) string {
	var b strings.Builder
	depth := 0
	for _, r := range text {
		switch {
		case r == '(':
			depth++
		case r == ')' && depth > 0:
			depth--
		case depth == 0:
			b.WriteRune(r)
		}
	}
	name := b.String()
	if i := strings.IndexByte(name, ','); i >= 0 && hasAmount {
		name = name[:i]
	}
	return strings.Join(strings.Fields(name), " ")
}

// nameKey is the name as two lines are compared: lower case, umlauts folded,
// and each word cut back to a common stem so that singular and plural meet
// ("Tomate"/"Tomaten", "Ei"/"Eier", "Apfel"/"Äpfel", "egg"/"eggs").
func nameKey(name string) string {
	replacer := strings.NewReplacer("ä", "a", "ö", "o", "ü", "u", "ß", "ss")
	words := strings.FieldsFunc(replacer.Replace(strings.ToLower(name)), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsNumber(r)
	})
	for i, word := range words {
		words[i] = stem(word)
	}
	return strings.Join(words, " ")
}

// stem takes one plural or inflection ending off a word, keeping at least two
// letters. "ss" keeps its last s: "Nuss" and "Nüsse" meet at "nuss".
func stem(word string) string {
	for _, suffix := range []string{"en", "er", "es", "n", "e", "s"} {
		if !strings.HasSuffix(word, suffix) {
			continue
		}
		base := word[:len(word)-len(suffix)]
		if len([]rune(base)) < 2 {
			continue
		}
		if suffix == "s" && strings.HasSuffix(base, "s") {
			continue
		}
		return base
	}
	return word
}

func firstRune(text string) rune {
	for _, r := range text {
		return r
	}
	return 0
}
