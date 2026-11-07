package recipe

import (
	"math"
	"testing"
)

func TestParseIngredientReadsAmountUnitAndName(t *testing.T) {
	cases := []struct {
		line   string
		amount float64
		has    bool
		unit   string
		name   string
	}{
		{"400 g Tomaten", 400, true, "g", "Tomaten"},
		{"400g Tomaten", 400, true, "g", "Tomaten"},
		{"1,5 kg Mehl", 1500, true, "g", "Mehl"},
		{"1.5 kg flour", 1500, true, "g", "flour"},
		{"1.000 g Mehl", 1000, true, "g", "Mehl"},
		{"400 g Mehl Type 1.050", 400, true, "g", "Mehl Type 1.050"},
		{"0,5 l Milch", 500, true, "ml", "Milch"},
		{"200 ml Sahne", 200, true, "ml", "Sahne"},
		{"½ TL Salz", 0.5, true, "TL", "Salz"},
		{"1 ½ EL Öl", 1.5, true, "EL", "Öl"},
		{"1½ EL Öl", 1.5, true, "EL", "Öl"},
		{"1/2 Zitrone", 0.5, true, "", "Zitrone"},
		{"1 1/2 cups of sugar", 1.5, true, "cup", "sugar"},
		{"2-3 Zwiebeln", 3, true, "", "Zwiebeln"},
		{"2 bis 3 Zwiebeln", 3, true, "", "Zwiebeln"},
		{"ca. 200 g Feta, zerbröselt", 200, true, "g", "Feta"},
		{"1 Dose (400 g) gehackte Tomaten", 1, true, "Dose", "gehackte Tomaten"},
		{"2 Pck. Vanillezucker", 2, true, "Pck.", "Vanillezucker"},
		{"4 Eigelb", 4, true, "", "Eigelb"},
		{"Salz und Pfeffer", 0, false, "", "Salz und Pfeffer"},
		{"etwas Salz", 0, false, "", "Salz"},
		{"Cashewkerne", 0, false, "", "Cashewkerne"},
		{"Salz, Pfeffer", 0, false, "", "Salz, Pfeffer"},
		{"etwas Salz, Pfeffer (frisch gemahlen)", 0, false, "", "Salz, Pfeffer"},
		{"2 Zwiebeln, fein gehackt", 2, true, "", "Zwiebeln"},
	}
	for _, c := range cases {
		got := ParseIngredient(c.line)
		if got.HasAmount != c.has || math.Abs(got.Amount-c.amount) > 1e-9 || got.Unit != c.unit || got.Name != c.name {
			t.Errorf("%q = %+v, want %v %v %q %q", c.line, got, c.has, c.amount, c.unit, c.name)
		}
	}
}

func TestParseIngredientKeysMeetOnlyWhenTheyAddUp(t *testing.T) {
	same := [][2]string{
		{"400 g Tomaten", "400 g Tomaten"},
		{"1 kg Tomaten", "250 g tomaten, gewürfelt"},
		{"1 Ei", "2 Eier"},
		{"1 Apfel", "3 Äpfel"},
		{"1 Tomate", "2 Tomaten"},
		{"1 Zwiebel", "2 Zwiebeln (rot)"},
		{"200 g Haselnüsse", "50 g Haselnuss"},
		{"2 eggs", "1 egg"},
		{"1 EL Zucker", "2 tbsp zucker"},
		{"Salz", "etwas Salz"},
		{"1 l Milch", "250 ml Milch"},
	}
	for _, pair := range same {
		if a, b := ParseIngredient(pair[0]), ParseIngredient(pair[1]); a.Key != b.Key {
			t.Errorf("%q (%s) and %q (%s) should add up", pair[0], a.Key, pair[1], b.Key)
		}
	}
	apart := [][2]string{
		{"1 EL Zucker", "100 g Zucker"},
		{"Salz", "1 Prise Salz"},
		{"2 Tomaten", "400 g Tomaten"},
		{"200 g Mehl", "200 g Zucker"},
		{"1 Dose Tomaten", "1 Dose Mais"},
		{"Salz", "Salz, Pfeffer"},
	}
	for _, pair := range apart {
		if a, b := ParseIngredient(pair[0]), ParseIngredient(pair[1]); a.Key == b.Key {
			t.Errorf("%q and %q should stay apart (%s)", pair[0], pair[1], a.Key)
		}
	}
}
