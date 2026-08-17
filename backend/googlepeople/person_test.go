// File overview: Translation between People resources and Rolltop contacts.
// The asymmetries are what these tests exist for -- Google keeps lists where
// Rolltop keeps one value, and a write that omits a listed field clears it.

package googlepeople

import (
	"testing"

	"rolltop/backend/store"
)

// Google returns several names, organizations and addresses and marks one of
// each primary. Taking the first entry instead would show a contact's old
// employer or maiden name as often as not.
func TestToContactPrefersThePrimaryEntry(t *testing.T) {
	person := Person{
		ResourceName: "people/c1",
		ETag:         "etag-1",
		Names: []Name{
			{GivenName: "Augusta", FamilyName: "Byron"},
			{GivenName: "Ada", FamilyName: "Lovelace", DisplayName: "Ada Lovelace", Metadata: &FieldMetadata{Primary: true}},
		},
		Orgs: []Organization{
			{Name: "Old Employer"},
			{Name: "Analytical Engines", Department: "Research", Title: "Analyst", Metadata: &FieldMetadata{Primary: true}},
		},
		Emails: []EmailAddress{
			{Value: "ada@example.test", Label: "Work", Metadata: &FieldMetadata{Primary: true}},
			{Value: "ada.home@example.test", Type: "home"},
		},
		Birthdays: []Birthday{{Date: &Date{Year: 1815, Month: 12, Day: 10}}},
	}
	contact := ToContact(person)

	if contact.GivenName != "Ada" || contact.FamilyName != "Lovelace" {
		t.Fatalf("name = %s %s, want the primary one", contact.GivenName, contact.FamilyName)
	}
	if contact.Organization != "Analytical Engines" || contact.JobTitle != "Analyst" {
		t.Fatalf("organization = %+v, want the primary one", contact)
	}
	if contact.Birthday != "1815-12-10" {
		t.Fatalf("birthday = %q, want the ISO date", contact.Birthday)
	}
	if len(contact.Emails) != 2 || !contact.Emails[0].IsPrimary || contact.Emails[0].Label != "Work" {
		t.Fatalf("emails = %+v, want both with the primary flag and the formatted label", contact.Emails)
	}
	// A type Google did not format for display still has to become a readable
	// label rather than showing up as lowercase machine text.
	if contact.Emails[1].Label != "Home" {
		t.Fatalf("second email label = %q, want the type capitalized", contact.Emails[1].Label)
	}
	if contact.Source != store.ContactSourceGoogle || contact.ExternalID != "people/c1" || contact.ETag != "etag-1" {
		t.Fatalf("provenance = %+v, want it taken from the resource", contact)
	}
}

// Google allows a birthday without a year and vCard writes that as --MM-DD.
// Dropping it for lack of a year would lose the part people actually use.
func TestBirthdayRoundTripsWithAndWithoutAYear(t *testing.T) {
	for _, tc := range []struct {
		name  string
		date  Date
		value string
	}{
		{"with a year", Date{Year: 1815, Month: 12, Day: 10}, "1815-12-10"},
		{"without a year", Date{Month: 12, Day: 10}, "--12-10"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rendered := birthdayString([]Birthday{{Date: &tc.date}})
			if rendered != tc.value {
				t.Fatalf("rendered = %q, want %q", rendered, tc.value)
			}
			parsed := parseBirthday(rendered)
			if parsed == nil || parsed.Date == nil {
				t.Fatalf("parsed %q = %+v, want a structured date", rendered, parsed)
			}
			if *parsed.Date != tc.date {
				t.Fatalf("parsed = %+v, want %+v", *parsed.Date, tc.date)
			}
		})
	}
}

// Every field named in updatePersonFields is cleared by Google when the payload
// omits it. Emitting empty lists is therefore what makes "I deleted the last
// phone number" mean the same thing at Google as it does here.
func TestFromContactEmitsEveryWritableFieldEvenWhenEmpty(t *testing.T) {
	person := FromContact(store.Contact{DisplayName: "Nobody"})
	for name, list := range map[string]int{
		"names":       len(person.Names),
		"nicknames":   len(person.Nicknames),
		"emails":      len(person.Emails),
		"phones":      len(person.Phones),
		"addresses":   len(person.Addresses),
		"orgs":        len(person.Orgs),
		"biographies": len(person.Biographies),
		"birthdays":   len(person.Birthdays),
		"urls":        len(person.URLs),
	} {
		if list < 0 {
			t.Fatalf("%s is negative", name)
		}
	}
	if person.Emails == nil || person.Phones == nil || person.Addresses == nil || person.URLs == nil {
		t.Fatalf("person = %+v, want empty slices rather than nil so the JSON clears the field", person)
	}
	// A contact with only a display name has no structured name to send, and
	// Google rejects displayName as an input.
	if len(person.Names) != 1 || person.Names[0].UnstructuredNm != "Nobody" {
		t.Fatalf("names = %+v, want the display name sent as an unstructured name", person.Names)
	}
}

// Google's monogram placeholder is not a picture the contact has. Importing it
// would give every contact without a photo a meaningless icon.
func TestPrimaryPhotoURLIgnoresGooglesPlaceholder(t *testing.T) {
	if url := PrimaryPhotoURL(Person{Photos: []Photo{{URL: "https://example.test/default.png", Default: true}}}); url != "" {
		t.Fatalf("photo URL = %q, want the placeholder ignored", url)
	}
	person := Person{Photos: []Photo{
		{URL: "https://example.test/default.png", Default: true},
		{URL: "https://example.test/real.jpg"},
	}}
	if url := PrimaryPhotoURL(person); url != "https://example.test/real.jpg" {
		t.Fatalf("photo URL = %q, want the real picture", url)
	}
}

// A tombstone carries nothing but a resource name. Treating it as an ordinary
// person would replace the contact with a blank row instead of removing it.
func TestDeletedPersonIsRecognizedAsATombstone(t *testing.T) {
	if !(Person{Metadata: &PersonMetadata{Deleted: true}}).IsDeleted() {
		t.Fatal("a deleted person was not recognized")
	}
	if (Person{Metadata: &PersonMetadata{}}).IsDeleted() {
		t.Fatal("a live person was taken for a tombstone")
	}
	if (Person{}).IsDeleted() {
		t.Fatal("a person without metadata was taken for a tombstone")
	}
}
