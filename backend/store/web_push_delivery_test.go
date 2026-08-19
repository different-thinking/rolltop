package store

import (
	"bytes"
	"context"
	"crypto/elliptic"
	"encoding/base64"
	"testing"
)

func TestWebPushDeliveryCursorBaselinesPersistsAndIsUserScoped(t *testing.T) {
	ctx := context.Background()
	db, err := openTestStore(t)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	owner, err := db.CreateUser(ctx, "push-cursor-owner@example.test", "Owner", "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	other, err := db.CreateUser(ctx, "push-cursor-other@example.test", "Other", "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	ownerFirst := createNewMailEventMessage(t, ctx, db, owner, 501, "First", "Owner first")
	otherFirst := createNewMailEventMessage(t, ctx, db, other, 601, "Other", "Other first")
	ownerEvent, _, err := db.RecordNewMailEvent(ctx, owner.ID, ownerFirst)
	if err != nil {
		t.Fatal(err)
	}
	otherEvent, _, err := db.RecordNewMailEvent(ctx, other.ID, otherFirst)
	if err != nil {
		t.Fatal(err)
	}
	ownerSub, err := db.SaveWebPushSubscription(ctx, owner.ID, WebPushSubscription{
		Endpoint: "https://push.example.test/owner", P256DH: testWebPushValues(3), Auth: testWebPushAuth(3),
	})
	if err != nil {
		t.Fatal(err)
	}
	otherSub, err := db.SaveWebPushSubscription(ctx, other.ID, WebPushSubscription{
		Endpoint: "https://push.example.test/other", P256DH: testWebPushValues(4), Auth: testWebPushAuth(4),
	})
	if err != nil {
		t.Fatal(err)
	}
	if ownerSub.LastNewMailEventID != ownerEvent.ID || otherSub.LastNewMailEventID != otherEvent.ID {
		t.Fatalf("baseline cursors owner=%d/%d other=%d/%d", ownerSub.LastNewMailEventID, ownerEvent.ID, otherSub.LastNewMailEventID, otherEvent.ID)
	}

	ownerSecond := createNewMailEventMessage(t, ctx, db, owner, 502, "Second", "Owner second")
	ownerSecondEvent, _, err := db.RecordNewMailEvent(ctx, owner.ID, ownerSecond)
	if err != nil {
		t.Fatal(err)
	}
	advanced, err := db.AdvanceWebPushSubscriptionNewMailCursor(ctx, other.ID, ownerSub, ownerSecondEvent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if advanced {
		t.Fatal("cross-tenant cursor update reported success")
	}
	otherSubs, err := db.ListWebPushSubscriptions(ctx, other.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(otherSubs) != 1 || otherSubs[0].LastNewMailEventID != otherEvent.ID {
		t.Fatalf("cross-tenant cursor update changed other subscription: %+v", otherSubs)
	}
	advanced, err = db.AdvanceWebPushSubscriptionNewMailCursor(ctx, owner.ID, ownerSub, ownerSecondEvent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !advanced {
		t.Fatal("owner cursor update did not match its subscription")
	}
	ownerThird := createNewMailEventMessage(t, ctx, db, owner, 503, "Third", "Owner third")
	ownerThirdEvent, _, err := db.RecordNewMailEvent(ctx, owner.ID, ownerThird)
	if err != nil {
		t.Fatal(err)
	}
	refreshed, err := db.SaveWebPushSubscription(ctx, owner.ID, WebPushSubscription{
		Endpoint: ownerSub.Endpoint, P256DH: testWebPushValues(5), Auth: testWebPushAuth(5),
	})
	if err != nil {
		t.Fatal(err)
	}
	if refreshed.LastNewMailEventID != ownerSecondEvent.ID {
		t.Fatalf("endpoint refresh cursor = %d, want preserved %d", refreshed.LastNewMailEventID, ownerSecondEvent.ID)
	}
	advanced, err = db.AdvanceWebPushSubscriptionNewMailCursor(ctx, owner.ID, ownerSub, ownerThirdEvent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if advanced {
		t.Fatal("delivery encrypted with rotated keys advanced the replacement subscription")
	}
	deleted, err := db.DeleteWebPushSubscriptionIfCurrent(ctx, owner.ID, ownerSub)
	if err != nil {
		t.Fatal(err)
	}
	if deleted {
		t.Fatal("stale response for rotated keys deleted the replacement subscription")
	}
	advanced, err = db.AdvanceWebPushSubscriptionNewMailCursor(ctx, owner.ID, refreshed, ownerThirdEvent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !advanced {
		t.Fatal("delivery using current keys did not advance its subscription")
	}
}

func TestWebPushSubscriptionRejectsNonPublicEndpoints(t *testing.T) {
	ctx := context.Background()
	db, err := openTestStore(t)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	user, err := db.CreateUser(ctx, "push-validation@example.test", "Push", "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	p256dh, auth := testWebPushKeyMaterial(6)
	for _, endpoint := range []string{
		"http://push.example.com/send",
		"https://localhost/send",
		"https://push.local/send",
		"https://127.0.0.1/send",
		"https://10.1.2.3/send",
		"https://0.0.0.1/send",
		"https://100.64.0.1/send",
		"https://[::1]/send",
		"https://[64:ff9b::a00:1]/send",
		"https://user:password@push.example.com/send",
		"https://push.example.com/send#fragment",
		"https://single-label/send",
	} {
		t.Run(endpoint, func(t *testing.T) {
			_, err := db.SaveWebPushSubscription(ctx, user.ID, WebPushSubscription{
				Endpoint: endpoint, P256DH: p256dh, Auth: auth,
			})
			if err == nil {
				t.Fatalf("accepted non-public endpoint %q", endpoint)
			}
		})
	}
	if _, err := db.SaveWebPushSubscription(ctx, user.ID, WebPushSubscription{
		Endpoint: "https://push.example.com:8443/send/token", P256DH: p256dh, Auth: auth,
	}); err != nil {
		t.Fatalf("rejected public HTTPS endpoint: %v", err)
	}
	for _, invalid := range []WebPushSubscription{
		{Endpoint: "https://push.example.com/send", P256DH: "not-base64", Auth: auth},
		{Endpoint: "https://push.example.com/send", P256DH: p256dh, Auth: "short"},
		{Endpoint: "https://push.example.com/send", P256DH: p256dh + string(bytes.Repeat([]byte("a"), maxWebPushP256DHLength)), Auth: auth},
	} {
		if _, err := db.SaveWebPushSubscription(ctx, user.ID, invalid); err == nil {
			t.Fatalf("accepted invalid key material: %+v", invalid)
		}
	}
}

func testWebPushKeyMaterial(seed byte) (string, string) {
	return testWebPushValues(seed), testWebPushAuth(seed)
}

func testWebPushValues(seed byte) string {
	x, y := elliptic.P256().ScalarBaseMult([]byte{seed})
	return base64.RawURLEncoding.EncodeToString(elliptic.Marshal(elliptic.P256(), x, y))
}

func testWebPushAuth(seed byte) string {
	return base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{seed}, 16))
}
