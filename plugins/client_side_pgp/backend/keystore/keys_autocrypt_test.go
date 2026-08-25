package keystore

import (
	"context"
	"testing"

	"rolltop/backend/store"
	"rolltop/backend/store/storetest"
)

func TestSaveAutocryptContactKeyNeverDemotesManualKey(t *testing.T) {
	ctx := context.Background()
	db, err := storetest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	user, err := db.CreateUser(ctx, "owner@example.test", "Owner", "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	contact, err := db.CreateContact(ctx, user.ID, store.Contact{
		DisplayName: "Alice",
		Emails:      []store.ContactEmail{{Email: "alice@example.test", IsPrimary: true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	// The user pins Alice's real key by hand.
	manual, err := UpsertContactPublicKey(ctx, db, store.ContactPGPPublicKey{
		UserID:           user.ID,
		ContactID:        contact.ID,
		Email:            "alice@example.test",
		PublicKeyArmored: "alice real key",
		SourceKind:       "manual",
		IsPreferred:      true,
	})
	if err != nil {
		t.Fatal(err)
	}

	// A spoofed Autocrypt header arrives with an attacker key for the same
	// address. It must neither be stored nor displace the pinned key.
	if err := SaveAutocryptContactKey(ctx, db, user.ID, ContactPublicKeyInput{
		Email:            "alice@example.test",
		Label:            "alice@example.test",
		PublicKeyArmored: "attacker key",
		SourceKind:       "autocrypt",
	}); err != nil {
		t.Fatal(err)
	}

	keys, err := ListAllPublicKeysForEmails(ctx, db, user.ID, []string{"alice@example.test"})
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 {
		t.Fatalf("key count = %d, want 1 (autocrypt key must not be stored over a manual one)", len(keys))
	}
	if keys[0].ID != manual.ID || !keys[0].IsPreferred || keys[0].PublicKeyArmored != "alice real key" {
		t.Fatalf("manual key was disturbed: %+v", keys[0])
	}
}

func TestSaveAutocryptContactKeyPrefersDiscoveredWhenNoManualKey(t *testing.T) {
	ctx := context.Background()
	db, err := storetest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	user, err := db.CreateUser(ctx, "owner2@example.test", "Owner", "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := SaveAutocryptContactKey(ctx, db, user.ID, ContactPublicKeyInput{
		Email:            "bob@example.test",
		Label:            "bob@example.test",
		PublicKeyArmored: "bob autocrypt key",
		SourceKind:       "autocrypt",
	}); err != nil {
		t.Fatal(err)
	}
	keys, err := ListAllPublicKeysForEmails(ctx, db, user.ID, []string{"bob@example.test"})
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 || !keys[0].IsPreferred || keys[0].SourceKind != "autocrypt" {
		t.Fatalf("discovered key = %+v, want one preferred autocrypt key", keys)
	}
}
