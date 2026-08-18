package store

import (
	"context"
	"path/filepath"
	"testing"
)

// TestExtraMeContactAddressDoesNotBecomeAnIdentity pins the rule the From menu
// depends on: identities are added deliberately, never derived. A second
// address landing on the reader's own contact card -- a Google sync, a vCard
// import, a manual edit -- must not turn into a sending identity.
func TestExtraMeContactAddressDoesNotBecomeAnIdentity(t *testing.T) {
	ctx := context.Background()
	db, err := Open(filepath.Join(t.TempDir(), "rolltop.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	user, err := db.CreateUser(ctx, "manual@example.test", "Manual", "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	contact, err := db.EnsureMeContactForEmail(ctx, user.ID, user.Email, user.Name)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.EnsureMailIdentityForEmail(ctx, user.ID, user.Email); err != nil {
		t.Fatal(err)
	}
	contact, err = db.GetContactForUser(ctx, user.ID, contact.ID)
	if err != nil {
		t.Fatal(err)
	}
	contact.Emails = append(contact.Emails, ContactEmail{Label: "work", Email: "manual-alias@example.test"})
	if _, err := db.UpdateContact(ctx, user.ID, contact.ID, contact); err != nil {
		t.Fatal(err)
	}
	identities, err := db.ListMailIdentitiesForUser(ctx, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(identities) != 1 || identities[0].Email != user.Email {
		t.Fatalf("identities after a second Me address = %+v, want only %s", identities, user.Email)
	}
}

// TestEditingTheMeContactKeepsItsIdentity guards the other half: saving a
// contact rewrites its address rows, and an identity that hangs off one of them
// must survive with the signature and server choices the user configured.
func TestEditingTheMeContactKeepsItsIdentity(t *testing.T) {
	ctx := context.Background()
	db, err := Open(filepath.Join(t.TempDir(), "rolltop.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	user, err := db.CreateUser(ctx, "stable@example.test", "Stable", "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	smtp, err := db.CreateSMTPAccount(ctx, SMTPAccount{UserID: user.ID, Label: "Stable SMTP", Host: "smtp.stable.test", Port: 587, Username: user.Email, EncryptedPassword: "secret", UseTLS: true})
	if err != nil {
		t.Fatal(err)
	}
	contact, err := db.EnsureMeContactForEmail(ctx, user.ID, user.Email, user.Name)
	if err != nil {
		t.Fatal(err)
	}
	created, err := db.CreateMailIdentityForUser(ctx, user.ID, MailIdentity{
		Email:         user.Email,
		DisplayName:   "Stable",
		Signature:     "-- \nStable",
		SMTPAccountID: smtp.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	contact, err = db.GetContactForUser(ctx, user.ID, contact.ID)
	if err != nil {
		t.Fatal(err)
	}
	contact.JobTitle = "Editor"
	contact.Phones = append(contact.Phones, ContactPhone{Label: "mobile", Number: "+49 30 000000"})
	if _, err := db.UpdateContact(ctx, user.ID, contact.ID, contact); err != nil {
		t.Fatal(err)
	}
	identities, err := db.ListMailIdentitiesForUser(ctx, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(identities) != 1 {
		t.Fatalf("identities after a contact edit = %+v, want one", identities)
	}
	if identities[0].ID != created.ID || identities[0].Signature != "-- \nStable" || identities[0].SMTPAccountID != smtp.ID {
		t.Fatalf("identity after a contact edit = %+v, want the configured one back", identities[0])
	}
}

// TestDeleteMailIdentityLeavesTheMeAddress covers the removal a user needs for
// an identity older versions derived for them: the From entry goes, the address
// stays on their contact card so incoming mail is still recognised as theirs.
func TestDeleteMailIdentityLeavesTheMeAddress(t *testing.T) {
	ctx := context.Background()
	db, err := Open(filepath.Join(t.TempDir(), "rolltop.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	user, err := db.CreateUser(ctx, "removable@example.test", "Removable", "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.EnsureMeContactForEmail(ctx, user.ID, user.Email, user.Name); err != nil {
		t.Fatal(err)
	}
	if err := db.EnsureMailIdentityForEmail(ctx, user.ID, user.Email); err != nil {
		t.Fatal(err)
	}
	identities, err := db.ListMailIdentitiesForUser(ctx, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(identities) != 1 {
		t.Fatalf("identities = %+v, want one", identities)
	}
	removed := identities[0].ID
	if err := db.DeleteMailIdentityForUser(ctx, user.ID, removed); err != nil {
		t.Fatal(err)
	}
	identities, err = db.ListMailIdentitiesForUser(ctx, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(identities) != 0 {
		t.Fatalf("identities after delete = %+v, want none", identities)
	}
	addresses, err := db.ListMeContactEmailsForUser(ctx, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(addresses) != 1 || NormalizeContactEmail(addresses[0]) != NormalizeContactEmail(user.Email) {
		t.Fatalf("Me addresses after delete = %+v, want the address kept", addresses)
	}
	if err := db.DeleteMailIdentityForUser(ctx, user.ID, removed); !IsNotFound(err) {
		t.Fatalf("delete missing identity err = %v, want not found", err)
	}
}

// TestSharedMeAddressKeepsItsOwnIdentity pins which identity a Me address
// belongs to. The by-address re-binding exists for an orphan whose
// contact_emails row was rewritten; a second Me card that merely carries the
// same address must not walk the first card's identity over to itself, because
// deleting that card would then cascade the identity away with it.
func TestSharedMeAddressKeepsItsOwnIdentity(t *testing.T) {
	ctx := context.Background()
	db, err := Open(filepath.Join(t.TempDir(), "rolltop.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	user, err := db.CreateUser(ctx, "shared-me@example.test", "Shared Me", "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := db.EnsureMeContactForEmail(ctx, user.ID, user.Email, user.Name)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.EnsureMailIdentityForEmail(ctx, user.ID, user.Email); err != nil {
		t.Fatal(err)
	}
	identities, err := db.ListMailIdentitiesForUser(ctx, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(identities) != 1 {
		t.Fatalf("identities = %+v, want one", identities)
	}
	before := identities[0]
	second, err := db.CreateContact(ctx, user.ID, Contact{
		DisplayName: "Second Card",
		IsMe:        true,
		Emails:      []ContactEmail{{Email: user.Email, IsPrimary: true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	identities, err = db.ListMailIdentitiesForUser(ctx, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(identities) != 1 || identities[0].ID != before.ID || identities[0].ContactID != owner.ID || identities[0].ContactEmailID != before.ContactEmailID {
		t.Fatalf("identity after a second Me card = %+v, want it still on contact %d", identities, owner.ID)
	}
	if err := db.DeleteContactForUser(ctx, user.ID, second.ID); err != nil {
		t.Fatal(err)
	}
	identities, err = db.ListMailIdentitiesForUser(ctx, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(identities) != 1 || identities[0].ID != before.ID {
		t.Fatalf("identity after removing the second Me card = %+v, want %d kept", identities, before.ID)
	}
}

// TestContactEmailOrderSurvivesASave covers the ordering the stable rows would
// otherwise lose: the ids no longer follow the submitted order, so sort_order
// carries it instead.
func TestContactEmailOrderSurvivesASave(t *testing.T) {
	ctx := context.Background()
	db, err := Open(filepath.Join(t.TempDir(), "rolltop.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	user, err := db.CreateUser(ctx, "ordered@example.test", "Ordered", "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	contact, err := db.CreateContact(ctx, user.ID, Contact{
		DisplayName: "Ordered",
		Emails: []ContactEmail{
			{Label: "one", Email: "one@example.test", IsPrimary: true},
			{Label: "two", Email: "two@example.test"},
			{Label: "three", Email: "three@example.test"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	contact.Emails = []ContactEmail{
		{Label: "three", Email: "three@example.test", IsPrimary: true},
		{Label: "two", Email: "two@example.test"},
		{Label: "one", Email: "one@example.test"},
	}
	if _, err := db.UpdateContact(ctx, user.ID, contact.ID, contact); err != nil {
		t.Fatal(err)
	}
	stored, err := db.GetContactForUser(ctx, user.ID, contact.ID)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"three@example.test", "two@example.test", "one@example.test"}
	if len(stored.Emails) != len(want) {
		t.Fatalf("stored emails = %+v, want %v", stored.Emails, want)
	}
	for i, address := range want {
		if stored.Emails[i].Email != address {
			t.Fatalf("stored emails = %+v, want %v", stored.Emails, want)
		}
	}
}
