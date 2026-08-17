// File overview: Write-back against the fake People API. The behaviour worth
// pinning down here is what happens when the two copies disagree, because that
// is the only case where "Google is the leading system" has a visible cost.

package googlepeople

import (
	"context"
	"errors"
	"testing"

	"rolltop/backend/store"
)

// A create has to go to Google first: the resource name and etag it answers
// with are what every later write depends on, and inventing them locally would
// produce a contact no write could ever address.
func TestCreateRemoteContactStoresGooglesIdentifiers(t *testing.T) {
	fake := &fakePeople{}
	syncer, db, user := newSyncFixture(t, fake)
	ctx := context.Background()

	created, err := syncer.CreateRemoteContact(ctx, user.ID, 7, store.Contact{
		DisplayName: "Grace Hopper",
		GivenName:   "Grace",
		FamilyName:  "Hopper",
		Categories:  "Navy",
		Emails:      []store.ContactEmail{{Label: "Work", Email: "grace@example.test", IsPrimary: true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !created.IsGoogleContact() || created.ExternalID != "people/created1" || created.ETag != "etag-created" {
		t.Fatalf("created contact = %+v, want Google's resource name and etag", created)
	}
	// Google does not model categories. Losing them on the way through would
	// make saving to Google quietly destructive.
	if created.Categories != "Navy" {
		t.Fatalf("categories = %q, want the submitted value preserved", created.Categories)
	}
	stored, err := db.GetContactForUser(ctx, user.ID, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.ExternalID != "people/created1" {
		t.Fatalf("stored contact = %+v, want the link persisted", stored)
	}
}

// An edit that Google accepts must leave the local row matching what Google
// stored, including the new etag -- otherwise the very next edit fails as a
// conflict against a token that is already stale.
func TestUpdateRemoteContactAdoptsTheAcceptedVersion(t *testing.T) {
	fake := &fakePeople{responses: []ConnectionsPage{{
		People:        []Person{personWithEmail("people/c1", "etag-1", "Ada", "ada@example.test")},
		NextSyncToken: "token-1",
	}}}
	syncer, db, user := newSyncFixture(t, fake)
	ctx := context.Background()
	if _, err := syncer.SyncConnection(ctx, user.ID, 7); err != nil {
		t.Fatal(err)
	}
	existing, ok := contactByEmail(t, db, user.ID, "ada@example.test")
	if !ok {
		t.Fatal("the synced contact is missing")
	}

	edited := existing
	edited.JobTitle = "Countess"
	updated, err := syncer.UpdateRemoteContact(ctx, user.ID, existing, edited)
	if err != nil {
		t.Fatal(err)
	}
	if updated.JobTitle != "Countess" {
		t.Fatalf("job title = %q, want the edit applied", updated.JobTitle)
	}
	if updated.ETag != "etag-updated" {
		t.Fatalf("etag = %q, want the one Google answered with", updated.ETag)
	}
	if fake.updates != 1 {
		t.Fatalf("update calls = %d, want exactly one", fake.updates)
	}
}

// When somebody edited the contact at Google first, the submitted edit loses.
// The caller has to be told, and the local row has to end up as Google's
// version -- keeping the rejected edit would leave a contact that disagrees
// with the account it claims to come from until the next sync overwrote it.
func TestUpdateRemoteContactYieldsToGoogleOnConflict(t *testing.T) {
	fake := &fakePeople{responses: []ConnectionsPage{{
		People:        []Person{personWithEmail("people/c1", "etag-1", "Ada", "ada@example.test")},
		NextSyncToken: "token-1",
	}}}
	syncer, db, user := newSyncFixture(t, fake)
	ctx := context.Background()
	if _, err := syncer.SyncConnection(ctx, user.ID, 7); err != nil {
		t.Fatal(err)
	}
	existing, ok := contactByEmail(t, db, user.ID, "ada@example.test")
	if !ok {
		t.Fatal("the synced contact is missing")
	}

	// Google's copy moved on, and the next write is rejected as stale.
	remote := personWithEmail("people/c1", "etag-remote", "Ada Lovelace", "ada@example.test")
	remote.Orgs = []Organization{{Title: "Analyst"}}
	fake.mu.Lock()
	fake.people["people/c1"] = remote
	fake.updateConflict = true
	fake.mu.Unlock()

	edited := existing
	edited.JobTitle = "Countess"
	adopted, err := syncer.UpdateRemoteContact(ctx, user.ID, existing, edited)
	if !errors.Is(err, ErrRemoteChanged) {
		t.Fatalf("error = %v, want ErrRemoteChanged", err)
	}
	if adopted.JobTitle != "Analyst" {
		t.Fatalf("job title = %q, want Google's value to win", adopted.JobTitle)
	}
	if adopted.ETag != "etag-remote" {
		t.Fatalf("etag = %q, want Google's current etag so the next edit can succeed", adopted.ETag)
	}
	stored, err := db.GetContactForUser(ctx, user.ID, existing.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.JobTitle != "Analyst" {
		t.Fatalf("stored contact = %+v, want the adopted remote version", stored)
	}
}

// Deleting has to reach Google, or the next sync brings the contact back and
// the confirmation the user gave meant nothing.
func TestDeleteRemoteContactRemovesThePersonAtGoogle(t *testing.T) {
	fake := &fakePeople{responses: []ConnectionsPage{{
		People:        []Person{personWithEmail("people/c1", "etag-1", "Ada", "ada@example.test")},
		NextSyncToken: "token-1",
	}}}
	syncer, db, user := newSyncFixture(t, fake)
	ctx := context.Background()
	if _, err := syncer.SyncConnection(ctx, user.ID, 7); err != nil {
		t.Fatal(err)
	}
	existing, ok := contactByEmail(t, db, user.ID, "ada@example.test")
	if !ok {
		t.Fatal("the synced contact is missing")
	}
	if err := syncer.DeleteRemoteContact(ctx, user.ID, existing); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.deleted) != 1 || fake.deleted[0] != "people/c1" {
		t.Fatalf("deleted at Google = %v, want people/c1", fake.deleted)
	}
}

// Disconnecting an account is not a request to lose the address book. The
// contacts stay, they just stop being mirrors -- for many users Rolltop's copy
// is the one they can still reach.
func TestDisconnectingAConnectionKeepsItsContactsAsLocalOnes(t *testing.T) {
	fake := &fakePeople{responses: []ConnectionsPage{{
		People:        []Person{personWithEmail("people/c1", "etag-1", "Ada", "ada@example.test")},
		NextSyncToken: "token-1",
	}}}
	syncer, db, user := newSyncFixture(t, fake)
	ctx := context.Background()
	if _, err := syncer.SyncConnection(ctx, user.ID, 7); err != nil {
		t.Fatal(err)
	}

	if _, err := db.DemoteGoogleContactsForConnection(ctx, user.ID, 7); err != nil {
		t.Fatal(err)
	}
	if err := db.DeleteGooglePeopleSync(ctx, user.ID, 7); err != nil {
		t.Fatal(err)
	}
	contact, ok := contactByEmail(t, db, user.ID, "ada@example.test")
	if !ok {
		t.Fatal("disconnecting deleted the contact")
	}
	if contact.IsGoogleContact() || contact.ExternalID != "" || contact.ETag != "" {
		t.Fatalf("contact = %+v, want a plain local contact", contact)
	}
	state, err := db.GetGooglePeopleSync(ctx, user.ID, 7)
	if err != nil {
		t.Fatal(err)
	}
	if state.SyncToken != "" {
		t.Fatalf("sync token = %q, want it dropped with the connection", state.SyncToken)
	}
}
