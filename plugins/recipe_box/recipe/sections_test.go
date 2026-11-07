package recipe

import (
	"reflect"
	"testing"
)

func TestIngredientSectionsSplitAtHeadingLines(t *testing.T) {
	got := IngredientSections([]string{
		"Für den Teig:", "250 g Mehl", "125 g Butter",
		"Für den Belag:", "500 g Äpfel", "Zum Bestreuen: Puderzucker",
		"Deko:",
	})
	want := []IngredientSection{
		{Name: "Für den Teig", Items: []IngredientLine{{1, "250 g Mehl"}, {2, "125 g Butter"}}},
		{Name: "Für den Belag", Items: []IngredientLine{{4, "500 g Äpfel"}, {5, "Zum Bestreuen: Puderzucker"}}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("sections = %#v", got)
	}
}

func TestIngredientSectionsWithoutHeadingsIsOneUnnamedSection(t *testing.T) {
	got := IngredientSections([]string{"400 g Spaghetti", " ", "4 Eigelb"})
	want := []IngredientSection{{Items: []IngredientLine{{0, "400 g Spaghetti"}, {2, "4 Eigelb"}}}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("sections = %#v", got)
	}
	if got := IngredientSections(nil); got == nil || len(got) != 0 {
		t.Fatalf("empty list = %#v", got)
	}
}

func TestIngredientHeadingRefusesAmountsAndSentences(t *testing.T) {
	for _, line := range []string{"2 Eier:", "½ Zitrone:", "Salz", ":", "Alles, was man für diesen wunderbaren und sehr langen Kuchen braucht:"} {
		if name, ok := IngredientHeading(line); ok {
			t.Fatalf("%q read as heading %q", line, name)
		}
	}
	if name, ok := IngredientHeading("  Sauce:: "); !ok || name != "Sauce" {
		t.Fatalf("Sauce = %q %v", name, ok)
	}
}
