// File overview: Tests for contact, identity, and icon persistence.

package store

import (
	"context"
	"path/filepath"
	"testing"
)

func TestContactsAreScopedByUser(t *testing.T) {
	ctx := context.Background()
	db, err := Open(filepath.Join(t.TempDir(), "rolltop.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	user, err := db.CreateUser(ctx, "contacts@example.test", "Contacts", "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	other, err := db.CreateUser(ctx, "other-contacts@example.test", "Other", "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	contact, err := db.CreateContact(ctx, user.ID, Contact{
		DisplayName: "Shared Name",
		Emails:      []ContactEmail{{Email: "shared@example.test", IsPrimary: true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateContact(ctx, other.ID, Contact{
		DisplayName: "Other Shared",
		Emails:      []ContactEmail{{Email: "shared@example.test", IsPrimary: true}},
	}); err != nil {
		t.Fatal(err)
	}
	found, err := db.GetContactByEmailForUser(ctx, user.ID, "shared@example.test")
	if err != nil {
		t.Fatal(err)
	}
	if found.ID != contact.ID || found.DisplayName != "Shared Name" {
		t.Fatalf("found = %+v", found)
	}
	items, err := db.AutocompleteContactsForUser(ctx, user.ID, "shared", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Name != "Shared Name" {
		t.Fatalf("autocomplete = %+v", items)
	}
}

func TestContactAutocompleteIncludesOnlyCurrentUsersRecentCorrespondents(t *testing.T) {
	ctx := context.Background()
	db, err := Open(filepath.Join(t.TempDir(), "rolltop.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	user, err := db.CreateUser(ctx, "recent@example.test", "Recent", "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	other, err := db.CreateUser(ctx, "other-recent@example.test", "Other", "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	createNewMailEventMessage(t, ctx, db, user, 11, "Alice Sender <alice@example.test>", "Hello")
	createNewMailEventMessage(t, ctx, db, user, 12, "Alice Sender <alice@example.test>", "Again")
	createNewMailEventMessage(t, ctx, db, other, 21, "Alice Secret <alice-secret@example.test>", "Other tenant")

	items, err := db.AutocompleteContactsForUser(ctx, user.ID, "alice", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Email != "alice@example.test" || items[0].Label != "Recent" {
		t.Fatalf("recent autocomplete = %+v", items)
	}
}

func TestContactIconsAreScopedByUser(t *testing.T) {
	ctx := context.Background()
	db, err := Open(filepath.Join(t.TempDir(), "rolltop.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	user, err := db.CreateUser(ctx, "icon@example.test", "Icon", "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	other, err := db.CreateUser(ctx, "other-icon@example.test", "Other", "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	contact, err := db.CreateContact(ctx, user.ID, Contact{
		DisplayName: "Icon Contact",
		Emails:      []ContactEmail{{Email: "icon@example.test", IsPrimary: true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	blob, err := db.CreateBlob(ctx, BlobRecord{
		UserID: user.ID,
		Kind:   "contact_icon",
		Path:   "users/1/blobs/contacts/1/icons/icon.png",
		SHA256: "hash",
		Size:   4,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.SetContactIcon(ctx, user.ID, contact.ID, blob.ID, "image/png", "icon.png", 4); err != nil {
		t.Fatal(err)
	}
	if _, err := db.GetContactIconForUser(ctx, other.ID, contact.ID); !IsNotFound(err) {
		t.Fatalf("other user icon err = %v", err)
	}
	if _, err := db.GetContactIconByEmailForUser(ctx, other.ID, "icon@example.test"); !IsNotFound(err) {
		t.Fatalf("other user email icon err = %v", err)
	}
	icons, err := db.ListContactIconsByEmailsForUser(ctx, user.ID, []string{"Icon <icon@example.test>", "missing@example.test"})
	if err != nil {
		t.Fatal(err)
	}
	if len(icons) != 1 || icons["icon@example.test"].ContactID != contact.ID {
		t.Fatalf("owner batch icons = %+v", icons)
	}
	otherIcons, err := db.ListContactIconsByEmailsForUser(ctx, other.ID, []string{"icon@example.test"})
	if err != nil {
		t.Fatal(err)
	}
	if len(otherIcons) != 0 {
		t.Fatalf("other user batch icons = %+v", otherIcons)
	}
}

// Two people in one address book may share an address. Google allows it, and a
// unique index over the normalized address used to make the second of them
// impossible to store, which failed the whole contact sync rather than one row.
func TestContactsMayShareAnEmailAddressInsideOneAddressBook(t *testing.T) {
	ctx := context.Background()
	db, err := Open(filepath.Join(t.TempDir(), "rolltop.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	user, err := db.CreateUser(ctx, "household@example.test", "Household", "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	first, err := db.CreateContact(ctx, user.ID, Contact{
		DisplayName: "Ada Lovelace",
		Emails: []ContactEmail{
			{Label: "Work", Email: "ada@example.test", IsPrimary: true},
			{Label: "Home", Email: "haushalt@example.test"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := db.CreateContact(ctx, user.ID, Contact{
		DisplayName: "Charles Babbage",
		Emails:      []ContactEmail{{Label: "Home", Email: "haushalt@example.test", IsPrimary: true}},
	})
	if err != nil {
		t.Fatalf("second holder of a shared address: %v", err)
	}

	// The single-answer lookup still has to name one of them, and it names the
	// contact whose primary address this is. Anything else would let a Me
	// contact, a merge target or a reply identity move with SQLite's row order.
	found, err := db.GetContactByEmailForUser(ctx, user.ID, "haushalt@example.test")
	if err != nil {
		t.Fatal(err)
	}
	if found.ID != second.ID {
		t.Fatalf("lookup returned contact %d, want %d, whose primary address it is", found.ID, second.ID)
	}
	holders, err := db.ListContactEmailHoldersForUser(ctx, user.ID, "haushalt@example.test")
	if err != nil {
		t.Fatal(err)
	}
	if len(holders) != 2 || holders[0].ContactID != second.ID || holders[1].ContactID != first.ID {
		t.Fatalf("holders = %+v, want both contacts with the primary holder first", holders)
	}
	if list, err := db.ListContactEmailHoldersForUser(ctx, user.ID, "nobody@example.test"); err != nil || len(list) != 0 {
		t.Fatalf("unknown address = %+v, %v, want no holders and no error", list, err)
	}
}

// The reader's own contact answers for an address it holds. Everything that
// resolves an address to one contact -- the card beside a sender, its picture,
// a merge target -- would otherwise be able to name a mirrored stranger who
// happens to list the same address as their primary one.
func TestOwnContactOutranksOtherHoldersOfItsAddress(t *testing.T) {
	ctx := context.Background()
	db, err := Open(filepath.Join(t.TempDir(), "rolltop.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	user, err := db.CreateUser(ctx, "owner@example.test", "Owner", "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	// The stranger is created first and carries the address as their primary
	// one, so they win every tie-break the ordering has apart from this one.
	stranger, err := db.CreateContact(ctx, user.ID, Contact{
		DisplayName:        "Team Mailbox",
		Source:             ContactSourceGoogle,
		GoogleConnectionID: 4,
		ExternalID:         "people/c1",
		Emails:             []ContactEmail{{Label: "Work", Email: "team@example.test", IsPrimary: true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	me, err := db.CreateContact(ctx, user.ID, Contact{
		DisplayName: "The Reader",
		IsMe:        true,
		IsPrimary:   true,
		Emails: []ContactEmail{
			{Label: "Home", Email: "reader@example.test", IsPrimary: true},
			{Label: "Work", Email: "team@example.test"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	found, err := db.GetContactByEmailForUser(ctx, user.ID, "team@example.test")
	if err != nil {
		t.Fatal(err)
	}
	if found.ID != me.ID {
		t.Fatalf("address resolved to contact %d (%q), want the reader's own contact %d", found.ID, found.DisplayName, me.ID)
	}
	holders, err := db.ListContactEmailHoldersForUser(ctx, user.ID, "team@example.test")
	if err != nil {
		t.Fatal(err)
	}
	if len(holders) != 2 || holders[0].ContactID != me.ID || holders[1].ContactID != stranger.ID {
		t.Fatalf("holders = %+v, want the reader's contact ahead of the mirror", holders)
	}
	if !holders[1].IsGoogleContact() || holders[0].IsGoogleContact() {
		t.Fatalf("holders = %+v, want only the mirror reported as Google-owned", holders)
	}
}

// Marking a Google mirror as the reader's own contact is a local write to a row
// Google owns: nothing pushes it back, the next sync reverts what it can, and
// every address on that stranger's card becomes an outgoing identity. A Me
// contact of the reader's own is created beside the mirror instead.
func TestEnsureMeContactNeverAdoptsAGoogleMirror(t *testing.T) {
	ctx := context.Background()
	db, err := Open(filepath.Join(t.TempDir(), "rolltop.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	user, err := db.CreateUser(ctx, "owner2@example.test", "Owner", "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	mirror, err := db.CreateContact(ctx, user.ID, Contact{
		DisplayName:        "Shared Team",
		Source:             ContactSourceGoogle,
		GoogleConnectionID: 2,
		ExternalID:         "people/c9",
		Emails:             []ContactEmail{{Label: "Work", Email: "shared@example.test", IsPrimary: true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	me, err := db.EnsureMeContactForEmail(ctx, user.ID, "shared@example.test", "The Reader")
	if err != nil {
		t.Fatal(err)
	}
	if me.ID == mirror.ID {
		t.Fatal("the Google mirror was adopted as the Me contact")
	}
	if !me.IsMe || me.IsGoogleContact() {
		t.Fatalf("me contact = %+v, want a local contact of the reader's own", me)
	}
	stored, err := db.GetContactForUser(ctx, user.ID, mirror.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.IsMe {
		t.Fatal("the mirror was flagged as one of the reader's identities")
	}
	if stored.DisplayName != "Shared Team" {
		t.Fatalf("mirror display name = %q, want Google's version untouched", stored.DisplayName)
	}
	// A second call has a local holder to find and must reuse it rather than
	// keep creating Me contacts for the same address.
	again, err := db.EnsureMeContactForEmail(ctx, user.ID, "shared@example.test", "The Reader")
	if err != nil {
		t.Fatal(err)
	}
	if again.ID != me.ID {
		t.Fatalf("second call created contact %d, want the existing Me contact %d", again.ID, me.ID)
	}
}

// One contact carries an address once. The unique index user-037 dropped was
// also what enforced that, and a Me contact holding the address twice grows two
// identical From identities.
func TestContactStoresOneRowPerAddressEvenWhenGivenSeveral(t *testing.T) {
	ctx := context.Background()
	db, err := Open(filepath.Join(t.TempDir(), "rolltop.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	user, err := db.CreateUser(ctx, "owner3@example.test", "Owner", "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	contact, err := db.CreateContact(ctx, user.ID, Contact{
		DisplayName: "Bob",
		Emails: []ContactEmail{
			{Label: "Home", Email: "Bob@Example.test"},
			{Label: "Work", Email: "bob@example.test", IsPrimary: true},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(contact.Emails) != 1 {
		t.Fatalf("emails = %+v, want the two spellings stored once", contact.Emails)
	}
	if !contact.Emails[0].IsPrimary {
		t.Fatalf("email = %+v, want the primary flag carried over from the copy that had it", contact.Emails[0])
	}
	if contact.Emails[0].Email != "Bob@Example.test" {
		t.Fatalf("email = %q, want the first spelling kept", contact.Emails[0].Email)
	}
}
