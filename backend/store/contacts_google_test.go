package store

import (
	"context"
	"fmt"
	"testing"
)

// openContactStore returns a store plus two users. Every listing and every
// bulk update below is user-owned, and a missing user_id predicate would pass
// unnoticed against a single-tenant fixture.
func openContactStore(t *testing.T) (*Store, User, User) {
	t.Helper()
	db, err := openTestStore(t)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	ctx := context.Background()
	user, err := db.CreateUser(ctx, "filter-owner@example.test", "Filter Owner", "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	neighbour, err := db.CreateUser(ctx, "filter-neighbour@example.test", "Filter Neighbour", "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	return db, user, neighbour
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
	db, user, neighbour := openContactStore(t)
	ctx := context.Background()
	addContact(t, db, user.ID, "Alpha Local", "alpha@example.test", 0, "")
	addContact(t, db, user.ID, "Bravo Google", "bravo@example.test", 7, "people/c1")
	addContact(t, db, user.ID, "Charlie Other", "charlie@example.test", 8, "people/c2")
	// Same connection id, same resource name, different tenant. A filter or a
	// bulk update missing its user_id predicate would pick this up.
	addContact(t, db, neighbour.ID, "Neighbour Google", "bravo@example.test", 7, "people/c1")

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
	neighbourContacts, err := db.ListContactsForUser(ctx, neighbour.ID, ContactListFilter{Source: ContactSourceGoogle})
	if err != nil {
		t.Fatal(err)
	}
	if len(neighbourContacts) != 1 || neighbourContacts[0].DisplayName != "Neighbour Google" {
		t.Fatalf("neighbour listing = %+v, want only their own contact", neighbourContacts)
	}
}

// The vCard export asks for the whole address book. Clamping its limit down to
// a page wrote a backup that silently stopped at 200 contacts, which is the
// kind of incompleteness nobody notices until they need the file.
func TestListContactsHonoursALimitAboveThePageSize(t *testing.T) {
	db, user, neighbour := openContactStore(t)
	ctx := context.Background()
	const total = defaultContactListLimit + 5
	for i := 0; i < total; i++ {
		addContact(t, db, user.ID,
			fmt.Sprintf("Person %03d", i), fmt.Sprintf("person%03d@example.test", i), 0, "")
	}

	addContact(t, db, neighbour.ID, "Neighbour Person", "neighbour@example.test", 0, "")

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

// The listing loads detail rows in batches while a single read loads them one
// contact at a time. Two code paths for the same data have to agree, ordering
// included: the first email is what compose and the avatar fall back to.
func TestBatchedListingMatchesASingleContactRead(t *testing.T) {
	db, user, _ := openContactStore(t)
	ctx := context.Background()
	rich, err := db.CreateContact(ctx, user.ID, Contact{
		DisplayName: "Rich Person",
		Emails: []ContactEmail{
			{Label: "Work", Email: "work@example.test"},
			{Label: "Home", Email: "home@example.test", IsPrimary: true},
		},
		Phones:    []ContactPhone{{Label: "Mobile", Number: "+1 555 0100"}, {Label: "Home", Number: "+1 555 0199", IsPrimary: true}},
		Addresses: []ContactAddress{{Label: "Home", Street: "1 Main St", Locality: "Springfield"}},
		URLs:      []ContactURL{{Label: "Blog", URL: "https://example.test/blog"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	// A second contact so the batch query has more than one id to group by.
	addContact(t, db, user.ID, "Plain Person", "plain@example.test", 0, "")

	listed, err := db.ListContactsForUser(ctx, user.ID, ContactListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	var batched Contact
	for _, contact := range listed {
		if contact.ID == rich.ID {
			batched = contact
		}
	}
	single, err := db.GetContactForUser(ctx, user.ID, rich.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(batched.Emails) != len(single.Emails) || len(batched.Phones) != len(single.Phones) ||
		len(batched.Addresses) != len(single.Addresses) || len(batched.URLs) != len(single.URLs) {
		t.Fatalf("batched = %+v\nsingle = %+v", batched, single)
	}
	for i := range single.Emails {
		if batched.Emails[i] != single.Emails[i] {
			t.Fatalf("email %d: batched = %+v, single = %+v", i, batched.Emails[i], single.Emails[i])
		}
	}
	for i := range single.Phones {
		if batched.Phones[i] != single.Phones[i] {
			t.Fatalf("phone %d: batched = %+v, single = %+v", i, batched.Phones[i], single.Phones[i])
		}
	}
	// The explicitly marked entry leads, not the one that was listed first.
	if !batched.Emails[0].IsPrimary || batched.Emails[0].Email != "home@example.test" {
		t.Fatalf("first email = %+v, want the primary one", batched.Emails[0])
	}
}

// Merging two versions of a person appends entries that were each primary on
// their own side. Storing both leaves the address book with two primary emails,
// which nothing downstream is prepared for.
func TestSavingAContactKeepsExactlyOnePrimaryPerList(t *testing.T) {
	db, user, _ := openContactStore(t)
	ctx := context.Background()
	saved, err := db.CreateContact(ctx, user.ID, Contact{
		DisplayName: "Two Primaries",
		Emails: []ContactEmail{
			{Label: "Work", Email: "work@example.test", IsPrimary: true},
			{Label: "Home", Email: "home@example.test", IsPrimary: true},
		},
		Phones: []ContactPhone{
			{Label: "Mobile", Number: "+1 555 0100", IsPrimary: true},
			{Label: "Home", Number: "+1 555 0199", IsPrimary: true},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	countPrimary := func(contact Contact) (int, int) {
		emails, phones := 0, 0
		for _, item := range contact.Emails {
			if item.IsPrimary {
				emails++
			}
		}
		for _, item := range contact.Phones {
			if item.IsPrimary {
				phones++
			}
		}
		return emails, phones
	}
	if emails, phones := countPrimary(saved); emails != 1 || phones != 1 {
		t.Fatalf("after create: %d primary emails, %d primary phones, want one each", emails, phones)
	}
	if saved.Emails[0].Email != "work@example.test" {
		t.Fatalf("primary email = %q, want the first one marked", saved.Emails[0].Email)
	}

	// A list with nothing marked still gets a primary, so the stored rows and
	// the struct never disagree about which entry leads.
	unmarked := saved
	unmarked.Emails = []ContactEmail{{Label: "Other", Email: "other@example.test"}}
	updated, err := db.UpdateContact(ctx, user.ID, saved.ID, unmarked)
	if err != nil {
		t.Fatal(err)
	}
	if len(updated.Emails) != 1 || !updated.Emails[0].IsPrimary {
		t.Fatalf("emails after update = %+v, want the only entry promoted to primary", updated.Emails)
	}
}

// A contact whose connection was disconnected is local again. Listing it under
// Google would offer a filter for an account that no longer exists.
func TestListContactsCountsDemotedContactsAsLocal(t *testing.T) {
	db, user, neighbour := openContactStore(t)
	ctx := context.Background()
	addContact(t, db, user.ID, "Bravo Google", "bravo@example.test", 7, "people/c1")
	addContact(t, db, neighbour.ID, "Neighbour Google", "bravo@example.test", 7, "people/c1")
	demoted, err := db.DemoteGoogleContactsForConnection(ctx, user.ID, 7)
	if err != nil {
		t.Fatal(err)
	}
	if demoted != 1 {
		t.Fatalf("demoted %d contacts, want only this tenant's one", demoted)
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
	// Disconnecting one tenant's account says nothing about anybody else's.
	neighbourGoogle, err := db.ListContactsForUser(ctx, neighbour.ID, ContactListFilter{Source: ContactSourceGoogle})
	if err != nil {
		t.Fatal(err)
	}
	if len(neighbourGoogle) != 1 {
		t.Fatalf("neighbour google contacts = %d, want theirs untouched", len(neighbourGoogle))
	}
}

// An ordinary edit goes through UpdateContact, which must not be able to move a
// contact between accounts or quietly detach it from the one that owns it.
func TestUpdateContactLeavesProvenanceAlone(t *testing.T) {
	db, user, neighbour := openContactStore(t)
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

	// The same id from another session must not reach this row at all.
	if _, err := db.UpdateContact(ctx, neighbour.ID, contact.ID, edited); !IsNotFound(err) {
		t.Fatalf("cross-tenant update err = %v, want not found", err)
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
