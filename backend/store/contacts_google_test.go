package store

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
)

func openContactStore(t *testing.T) (*Store, User) {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "rolltop.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	user, err := db.CreateUser(context.Background(), "filter-owner@example.test", "Filter Owner", "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	return db, user
}

func addContact(t *testing.T, db *Store, userID int64, name, email string, connectionID int64, externalID string) Contact {
	t.Helper()
	contact := Contact{
		DisplayName: name,
		Emails:      []ContactEmail{{Label: "Email", Email: email, IsPrimary: true}},
	}
	if connectionID > 0 {
		contact.Source = ContactSourceGoogle
		contact.GoogleConnectionID = connectionID
		contact.ExternalID = externalID
		contact.ETag = "etag-" + externalID
	}
	saved, err := db.CreateContact(context.Background(), userID, contact)
	if err != nil {
		t.Fatal(err)
	}
	return saved
}

// The source filter belongs in the query because the listing is capped. Applied
// after the fact it would only ever see the first page of every contact, and an
// account with more contacts than the cap would appear to hold a fraction of
// what the settings page reports for it.
func TestListContactsFiltersBySourceInsideTheLimit(t *testing.T) {
	db, user := openContactStore(t)
	ctx := context.Background()
	addContact(t, db, user.ID, "Alpha Local", "alpha@example.test", 0, "")
	addContact(t, db, user.ID, "Bravo Google", "bravo@example.test", 7, "people/c1")
	addContact(t, db, user.ID, "Charlie Other", "charlie@example.test", 8, "people/c2")

	names := func(filter ContactListFilter) []string {
		contacts, err := db.ListContactsForUser(ctx, user.ID, filter)
		if err != nil {
			t.Fatal(err)
		}
		out := make([]string, 0, len(contacts))
		for _, contact := range contacts {
			out = append(out, contact.DisplayName)
		}
		return out
	}

	if got := names(ContactListFilter{}); len(got) != 3 {
		t.Fatalf("unfiltered = %v, want all three", got)
	}
	if got := names(ContactListFilter{Source: ContactSourceLocal}); len(got) != 1 || got[0] != "Alpha Local" {
		t.Fatalf("local = %v, want only the local contact", got)
	}
	if got := names(ContactListFilter{Source: ContactSourceGoogle}); len(got) != 2 {
		t.Fatalf("google = %v, want both mirrored contacts", got)
	}
	if got := names(ContactListFilter{Source: ContactSourceGoogle, GoogleConnectionID: 8}); len(got) != 1 || got[0] != "Charlie Other" {
		t.Fatalf("one connection = %v, want only that account's contact", got)
	}
	// The filter has to compose with the search box rather than replace it.
	if got := names(ContactListFilter{Source: ContactSourceGoogle, Query: "charlie"}); len(got) != 1 || got[0] != "Charlie Other" {
		t.Fatalf("google + query = %v, want the single match", got)
	}
}

// The vCard export asks for the whole address book. Clamping its limit down to
// a page wrote a backup that silently stopped at 200 contacts, which is the
// kind of incompleteness nobody notices until they need the file.
func TestListContactsHonoursALimitAboveThePageSize(t *testing.T) {
	db, user := openContactStore(t)
	ctx := context.Background()
	const total = defaultContactListLimit + 5
	for i := 0; i < total; i++ {
		addContact(t, db, user.ID,
			fmt.Sprintf("Person %03d", i), fmt.Sprintf("person%03d@example.test", i), 0, "")
	}

	exported, err := db.ListContactsForUser(ctx, user.ID, ContactListFilter{Limit: 10000})
	if err != nil {
		t.Fatal(err)
	}
	if len(exported) != total {
		t.Fatalf("export returned %d contacts, want all %d", len(exported), total)
	}
	// An unspecified limit still gets a page rather than the whole book.
	paged, err := db.ListContactsForUser(ctx, user.ID, ContactListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(paged) != defaultContactListLimit {
		t.Fatalf("default listing returned %d contacts, want %d", len(paged), defaultContactListLimit)
	}
}

// A contact whose connection was disconnected is local again. Listing it under
// Google would offer a filter for an account that no longer exists.
func TestListContactsCountsDemotedContactsAsLocal(t *testing.T) {
	db, user := openContactStore(t)
	ctx := context.Background()
	addContact(t, db, user.ID, "Bravo Google", "bravo@example.test", 7, "people/c1")
	if _, err := db.DemoteGoogleContactsForConnection(ctx, user.ID, 7); err != nil {
		t.Fatal(err)
	}

	google, err := db.ListContactsForUser(ctx, user.ID, ContactListFilter{Source: ContactSourceGoogle})
	if err != nil {
		t.Fatal(err)
	}
	if len(google) != 0 {
		t.Fatalf("google contacts after disconnect = %d, want none", len(google))
	}
	local, err := db.ListContactsForUser(ctx, user.ID, ContactListFilter{Source: ContactSourceLocal})
	if err != nil {
		t.Fatal(err)
	}
	if len(local) != 1 {
		t.Fatalf("local contacts after disconnect = %d, want the demoted one", len(local))
	}
}

// An ordinary edit goes through UpdateContact, which must not be able to move a
// contact between accounts or quietly detach it from the one that owns it.
func TestUpdateContactLeavesProvenanceAlone(t *testing.T) {
	db, user := openContactStore(t)
	ctx := context.Background()
	contact := addContact(t, db, user.ID, "Bravo Google", "bravo@example.test", 7, "people/c1")

	edited := contact
	edited.JobTitle = "Analyst"
	edited.Source = ContactSourceLocal
	edited.GoogleConnectionID = 0
	edited.ExternalID = ""
	edited.ETag = ""
	if _, err := db.UpdateContact(ctx, user.ID, contact.ID, edited); err != nil {
		t.Fatal(err)
	}

	stored, err := db.GetContactForUser(ctx, user.ID, contact.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !stored.IsGoogleContact() || stored.ExternalID != "people/c1" || stored.ETag != "etag-people/c1" {
		t.Fatalf("contact after edit = %+v, want its Google linkage intact", stored)
	}
	if stored.JobTitle != "Analyst" {
		t.Fatalf("job title = %q, want the edit applied", stored.JobTitle)
	}
}
