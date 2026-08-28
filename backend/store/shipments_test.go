package store

import (
	"context"
	"fmt"
	"testing"
	"time"

	"rolltop/backend/mailparse"
)

// shipmentTestMessage stores one message so a shipment has mail to hang off.
func shipmentTestMessage(t *testing.T, ctx context.Context, db *Store, user User, account MailAccount,
	mailbox Mailbox, uid uint32, subject string, date time.Time,
) MessageRecord {
	t.Helper()
	raw := []byte(fmt.Sprintf("Message-ID: <parcel-%d@example.test>\r\nSubject: %s\r\n\r\nBody\r\n", uid, subject))
	messageID := fmt.Sprintf("<parcel-%d@example.test>", uid)
	fingerprint := MessageArrivalFingerprint(raw, messageID, date, int64(len(raw)))
	path := fmt.Sprintf("users/%d/shipment-tests/uid-%d.eml", user.ID, uid)
	blob, err := db.CreateBlob(ctx, BlobRecord{UserID: user.ID, Kind: "message", Path: path,
		SHA256: fingerprint.RawSHA256, Size: int64(len(raw))})
	if err != nil {
		t.Fatal(err)
	}
	message, err := db.CreateMessage(ctx, CreateMessage{
		UserID: user.ID, AccountID: account.ID, MailboxID: mailbox.ID, BlobID: blob.ID,
		MessageIDHeader: messageID, CanonicalSHA256: fingerprint.CanonicalSHA256,
		MessageIDHash: fingerprint.MessageIDHash, ThreadKey: messageID,
		Subject: subject, FromAddr: "noreply@dhl.de", Date: date,
		InternalDate: date, UID: uid, UIDValidity: mailbox.UIDValidity,
		Size: fingerprint.Size, BlobPath: path,
	})
	if err != nil {
		t.Fatal(err)
	}
	return message
}

func shipmentTestSetup(t *testing.T, email string) (*Store, context.Context, User, MailAccount, Mailbox) {
	t.Helper()
	ctx := context.Background()
	db := openArrivalTestStore(t)
	t.Cleanup(func() { db.Close() })
	user := createPendingMoveTestUser(t, ctx, db, email)
	account := createPendingMoveTestAccount(t, ctx, db, user, "primary")
	mailbox := arrivalTestMailbox(t, ctx, db, user, account, "INBOX", 42)
	return db, ctx, user, account, mailbox
}

// The shop's dispatch note and the carrier's own mail name the same number, and
// that has to be one parcel with both mails behind it.
func TestReplaceMessageShipmentsMergesMessagesOntoOneParcel(t *testing.T) {
	db, ctx, user, account, mailbox := shipmentTestSetup(t, "parcel-merge@example.test")
	shipped := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	arriving := time.Date(2026, 9, 3, 7, 0, 0, 0, time.UTC)

	first := shipmentTestMessage(t, ctx, db, user, account, mailbox, 1, "Bestellung versendet", shipped)
	if err := db.ReplaceMessageShipments(ctx, user.ID, first.ID, shipped.Unix(), []mailparse.DeliveryNotice{{
		Carrier: "dhl", TrackingNumber: "00340434212345678901", Status: mailparse.DeliveryAnnounced,
	}}); err != nil {
		t.Fatal(err)
	}
	second := shipmentTestMessage(t, ctx, db, user, account, mailbox, 2, "Ihr Paket kommt heute", arriving)
	if err := db.ReplaceMessageShipments(ctx, user.ID, second.ID, arriving.Unix(), []mailparse.DeliveryNotice{{
		Carrier: "dhl", TrackingNumber: "00340434212345678901", ExpectedDate: "2026-09-03",
		WindowStart: "10:12", WindowEnd: "13:12", Status: mailparse.DeliveryOutForDelivery,
	}}); err != nil {
		t.Fatal(err)
	}

	shipments, err := db.ListShipments(ctx, user.ID, "2026-09-03")
	if err != nil {
		t.Fatal(err)
	}
	if len(shipments) != 1 {
		t.Fatalf("want one parcel, got %d", len(shipments))
	}
	parcel := shipments[0]
	if parcel.ExpectedDate != "2026-09-03" || parcel.Status != mailparse.DeliveryOutForDelivery {
		t.Errorf("parcel = %+v, want the later message's day and status", parcel)
	}
	if parcel.WindowStart != "10:12" || parcel.WindowEnd != "13:12" {
		t.Errorf("window = %q..%q", parcel.WindowStart, parcel.WindowEnd)
	}
	if len(parcel.Messages) != 2 {
		t.Fatalf("want both messages behind the parcel, got %d", len(parcel.Messages))
	}
	if parcel.Messages[0].MessageID != second.ID {
		t.Errorf("messages are not newest first: %+v", parcel.Messages)
	}

	count, err := db.CountShipmentsExpectedOn(ctx, user.ID, "2026-09-03")
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Errorf("count for the day = %d, want 1", count)
	}
}

// The backfill reads mail newest-first, so an older message is read after a
// newer one routinely. It must not undo what the newer one established.
func TestReplaceMessageShipmentsIgnoresOlderClaims(t *testing.T) {
	db, ctx, user, account, mailbox := shipmentTestSetup(t, "parcel-order@example.test")
	early := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	late := time.Date(2026, 9, 3, 16, 0, 0, 0, time.UTC)

	delivered := shipmentTestMessage(t, ctx, db, user, account, mailbox, 1, "Zugestellt", late)
	if err := db.ReplaceMessageShipments(ctx, user.ID, delivered.ID, late.Unix(), []mailparse.DeliveryNotice{{
		Carrier: "gls", TrackingNumber: "12345678901", ExpectedDate: "2026-09-03", Status: mailparse.DeliveryDelivered,
	}}); err != nil {
		t.Fatal(err)
	}
	announced := shipmentTestMessage(t, ctx, db, user, account, mailbox, 2, "Versendet", early)
	if err := db.ReplaceMessageShipments(ctx, user.ID, announced.ID, early.Unix(), []mailparse.DeliveryNotice{{
		Carrier: "gls", TrackingNumber: "12345678901", ExpectedDate: "2026-09-05", Status: mailparse.DeliveryAnnounced,
	}}); err != nil {
		t.Fatal(err)
	}

	shipments, err := db.ListShipments(ctx, user.ID, "2026-09-03")
	if err != nil {
		t.Fatal(err)
	}
	if len(shipments) != 1 {
		t.Fatalf("want one parcel, got %d", len(shipments))
	}
	if shipments[0].Status != mailparse.DeliveryDelivered || shipments[0].ExpectedDate != "2026-09-03" {
		t.Errorf("an older message overwrote a newer one: %+v", shipments[0])
	}
	// It is still evidence for the parcel, though.
	if len(shipments[0].Messages) != 2 {
		t.Errorf("want both messages linked, got %d", len(shipments[0].Messages))
	}
	// A delivered parcel is not what "coming today" means.
	count, err := db.CountShipmentsExpectedOn(ctx, user.ID, "2026-09-03")
	if err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Errorf("count = %d, want 0 for a parcel that has arrived", count)
	}
}

// Re-reading a message under a new generation replaces what it said rather than
// adding to it, and stamps it either way.
func TestReplaceMessageShipmentsReplacesAndStamps(t *testing.T) {
	db, ctx, user, account, mailbox := shipmentTestSetup(t, "parcel-restamp@example.test")
	date := time.Date(2026, 9, 2, 9, 0, 0, 0, time.UTC)
	message := shipmentTestMessage(t, ctx, db, user, account, mailbox, 1, "Versand", date)

	pending, err := db.ListMessagesNeedingDeliveryScan(ctx, user.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].ID != message.ID {
		t.Fatalf("want the new message pending a scan, got %+v", pending)
	}
	if err := db.ReplaceMessageShipments(ctx, user.ID, message.ID, date.Unix(), []mailparse.DeliveryNotice{{
		Carrier: "ups", TrackingNumber: "1Z999AA10123456784", Status: mailparse.DeliveryAnnounced,
	}}); err != nil {
		t.Fatal(err)
	}
	if pending, err = db.ListMessagesNeedingDeliveryScan(ctx, user.ID, 10); err != nil {
		t.Fatal(err)
	} else if len(pending) != 0 {
		t.Errorf("a scanned message is still pending: %+v", pending)
	}
	// A second reading that finds nothing detaches the message, which is what
	// keeps a corrected rule from leaving its old answer behind.
	if err := db.ReplaceMessageShipments(ctx, user.ID, message.ID, date.Unix(), nil); err != nil {
		t.Fatal(err)
	}
	shipments, err := db.ListShipments(ctx, user.ID, "2026-09-02")
	if err != nil {
		t.Fatal(err)
	}
	if len(shipments) != 0 {
		t.Errorf("a parcel with no message left is still listed: %+v", shipments)
	}
}

// A message that names a number without saying whose it is still describes a
// parcel, and an empty carrier is its own identity rather than a wildcard.
func TestReplaceMessageShipmentsKeepsUnclaimedNumbersApart(t *testing.T) {
	db, ctx, user, account, mailbox := shipmentTestSetup(t, "parcel-unclaimed@example.test")
	date := time.Date(2026, 9, 2, 9, 0, 0, 0, time.UTC)
	message := shipmentTestMessage(t, ctx, db, user, account, mailbox, 1, "Versand", date)
	if err := db.ReplaceMessageShipments(ctx, user.ID, message.ID, date.Unix(), []mailparse.DeliveryNotice{
		{Carrier: "", TrackingNumber: "12345678901", ExpectedDate: "2026-09-02", Status: mailparse.DeliveryAnnounced},
		{Carrier: "dpd", TrackingNumber: "12345678901", ExpectedDate: "2026-09-02", Status: mailparse.DeliveryAnnounced},
	}); err != nil {
		t.Fatal(err)
	}
	shipments, err := db.ListShipments(ctx, user.ID, "2026-09-02")
	if err != nil {
		t.Fatal(err)
	}
	if len(shipments) != 2 {
		t.Fatalf("want two parcels, got %d: %+v", len(shipments), shipments)
	}
}

func TestListShipmentsRejectsAMalformedDay(t *testing.T) {
	db, ctx, user, _, _ := shipmentTestSetup(t, "parcel-day@example.test")
	if _, err := db.ListShipments(ctx, user.ID, "3. September"); err == nil {
		t.Fatal("want an error for a day that is not YYYY-MM-DD")
	}
}
