package store

import (
	"context"
	"fmt"
	"testing"
	"time"

	"rolltop/backend/mailparse"
)

// invoiceFixture is one tenant with an Inbox and a helper that files billing
// mail. It is the category fixture's shape without the archive wiring, which no
// rule here reads.
type invoiceFixture struct {
	db      *Store
	user    User
	account MailAccount
	inbox   Mailbox
	blob    BlobRecord
	base    time.Time
}

func newInvoiceFixture(t *testing.T) invoiceFixture {
	t.Helper()
	db, err := openTestStore(t)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	user, account, inbox, blob := testMailbox(t, context.Background(), db)
	return invoiceFixture{db: db, user: user, account: account, inbox: inbox, blob: blob,
		base: time.Unix(1700000000, 0)}
}

// message files one mail, uid minutes after the fixture's base time. The offset
// is what makes one message newer than another, which is the thing every rule
// below turns on.
func (f invoiceFixture) message(t *testing.T, uid uint32) MessageRecord {
	t.Helper()
	message, err := f.db.CreateMessage(context.Background(), CreateMessage{
		UserID: f.user.ID, AccountID: f.account.ID, MailboxID: f.inbox.ID, BlobID: f.blob.ID,
		MessageIDHeader: fmt.Sprintf("<invoice-%d@example.test>", uid),
		Subject:         fmt.Sprintf("Rechnung %d", uid),
		FromAddr:        "billing@firma.example.de",
		Category:        mailparse.CategoryInvoices,
		Date:            f.base.Add(time.Duration(uid) * time.Minute),
		UID:             uid, BlobPath: f.blob.Path,
	})
	if err != nil {
		t.Fatal(err)
	}
	return message
}

func (f invoiceFixture) record(t *testing.T, message MessageRecord, notice mailparse.InvoiceNotice) {
	t.Helper()
	if err := f.db.ReplaceMessageInvoice(context.Background(), f.user.ID, message.ID, message.Date.Unix(), &notice); err != nil {
		t.Fatal(err)
	}
}

func (f invoiceFixture) list(t *testing.T, today string) []Invoice {
	t.Helper()
	invoices, err := f.db.ListInvoices(context.Background(), f.user.ID, today)
	if err != nil {
		t.Fatal(err)
	}
	return invoices
}

func openInvoice(reference string) mailparse.InvoiceNotice {
	return mailparse.InvoiceNotice{
		Issuer: "firma.example.de", Reference: reference, Number: reference,
		DueDate: "2023-11-20", Amount: "149.90", Currency: "EUR",
		Status: mailparse.InvoiceOpen, Settlement: mailparse.SettlementTransfer,
	}
}

// The invoice, then the payment confirmation: the newer message closes the row.
func TestInvoiceUpsertLetsTheNewerMessageSettleTheBill(t *testing.T) {
	f := newInvoiceFixture(t)
	invoice := f.message(t, 1)
	confirmation := f.message(t, 2)

	f.record(t, invoice, openInvoice("4711"))
	paid := openInvoice("4711")
	paid.Status = mailparse.InvoicePaid
	f.record(t, confirmation, paid)

	rows := f.list(t, "2023-11-15")
	if len(rows) != 1 {
		t.Fatalf("want the two messages on one row, got %d", len(rows))
	}
	if rows[0].Status != mailparse.InvoicePaid {
		t.Errorf("status = %q, want paid", rows[0].Status)
	}
	if len(rows[0].Messages) != 2 {
		t.Errorf("want both messages hanging off the row, got %d", len(rows[0].Messages))
	}
}

// The backfill reads newest first, so the invoice routinely arrives after the
// dunning letter that chases it. The older message must not undo the newer one.
func TestInvoiceUpsertRefusesToBeUndoneByAnOlderMessage(t *testing.T) {
	f := newInvoiceFixture(t)
	invoice := f.message(t, 1)
	dunning := f.message(t, 2)

	chased := openInvoice("4711")
	chased.DunningLevel = mailparse.InvoiceDunningNotice
	f.record(t, dunning, chased)
	// Now the older invoice, exactly as the backfill would reach it.
	f.record(t, invoice, openInvoice("4711"))

	rows := f.list(t, "2023-11-15")
	if len(rows) != 1 {
		t.Fatalf("want one row, got %d", len(rows))
	}
	if rows[0].DunningLevel != mailparse.InvoiceDunningNotice {
		t.Errorf("dunning level = %d, want the chase to survive the older message", rows[0].DunningLevel)
	}
	if rows[0].Status != mailparse.InvoiceOpen {
		t.Errorf("status = %q, want open", rows[0].Status)
	}
}

// Being chased is a thing that happened. A later message that says nothing
// about it must not lower the grade.
func TestInvoiceUpsertKeepsTheDunningLevelMonotone(t *testing.T) {
	f := newInvoiceFixture(t)
	reminder := f.message(t, 1)
	dunning := f.message(t, 2)
	later := f.message(t, 3)

	nudged := openInvoice("4711")
	nudged.DunningLevel = mailparse.InvoiceDunningReminder
	f.record(t, reminder, nudged)

	chased := openInvoice("4711")
	chased.DunningLevel = mailparse.InvoiceDunningFinal
	f.record(t, dunning, chased)

	f.record(t, later, openInvoice("4711"))

	rows := f.list(t, "2023-11-15")
	if len(rows) != 1 || rows[0].DunningLevel != mailparse.InvoiceDunningFinal {
		t.Fatalf("want the highest grade kept, got %+v", rows)
	}
}

// Two senders using the same invoice number is the ordinary case, not the
// exotic one, which is why the issuer is half the key.
func TestInvoiceUpsertKeepsTwoIssuersApart(t *testing.T) {
	f := newInvoiceFixture(t)
	first := f.message(t, 1)
	second := f.message(t, 2)

	f.record(t, first, openInvoice("2026-001"))
	other := openInvoice("2026-001")
	other.Issuer = "andere.example.de"
	f.record(t, second, other)

	if rows := f.list(t, "2023-11-15"); len(rows) != 2 {
		t.Fatalf("want two rows for two issuers, got %d", len(rows))
	}
}

func TestInvoicesDueOnCountsOverdueAndChased(t *testing.T) {
	f := newInvoiceFixture(t)
	ctx := context.Background()

	overdue := openInvoice("A")
	overdue.DueDate = "2023-11-01"
	f.record(t, f.message(t, 1), overdue)

	future := openInvoice("B")
	future.DueDate = "2023-12-24"
	f.record(t, f.message(t, 2), future)

	// No deadline anybody could read, but somebody is chasing it.
	chased := openInvoice("C")
	chased.DueDate = ""
	chased.DunningLevel = mailparse.InvoiceDunningNotice
	f.record(t, f.message(t, 3), chased)

	settled := openInvoice("D")
	settled.DueDate = "2023-11-01"
	settled.Status = mailparse.InvoicePaid
	f.record(t, f.message(t, 4), settled)

	due, err := f.db.InvoicesDueOn(ctx, f.user.ID, "2023-11-15")
	if err != nil {
		t.Fatal(err)
	}
	if due.Count != 2 {
		t.Errorf("count = %d, want the overdue one and the chased one", due.Count)
	}
	if due.Chased != 1 {
		t.Errorf("chased = %d, want 1", due.Chased)
	}
}

// The reader's own answer outranks the mail's, and taking it back returns the
// mail's rather than a stale copy of it.
func TestSetInvoiceManualStatusOutranksAndReverts(t *testing.T) {
	f := newInvoiceFixture(t)
	ctx := context.Background()
	message := f.message(t, 1)
	f.record(t, message, openInvoice("4711"))

	rows := f.list(t, "2023-11-15")
	if len(rows) != 1 {
		t.Fatalf("want one row, got %d", len(rows))
	}
	updated, err := f.db.SetInvoiceManualStatus(ctx, f.user.ID, rows[0].ID, InvoiceManualPaid)
	if err != nil {
		t.Fatal(err)
	}
	if updated.EffectiveStatus() != mailparse.InvoicePaid {
		t.Errorf("effective status = %q, want paid", updated.EffectiveStatus())
	}
	if updated.Status != mailparse.InvoiceOpen {
		t.Errorf("the mail's own answer should be left alone underneath, got %q", updated.Status)
	}
	reverted, err := f.db.SetInvoiceManualStatus(ctx, f.user.ID, rows[0].ID, InvoiceManualNone)
	if err != nil {
		t.Fatal(err)
	}
	if reverted.EffectiveStatus() != mailparse.InvoiceOpen {
		t.Errorf("effective status = %q, want open again", reverted.EffectiveStatus())
	}
}

// A dismissed row is gone from every read: the reader said it was never a bill,
// and a list that kept showing it would be asking them again.
func TestDismissedInvoiceLeavesTheList(t *testing.T) {
	f := newInvoiceFixture(t)
	message := f.message(t, 1)
	f.record(t, message, openInvoice("4711"))
	rows := f.list(t, "2023-11-15")
	if _, err := f.db.SetInvoiceManualStatus(context.Background(), f.user.ID, rows[0].ID, InvoiceManualDismissed); err != nil {
		t.Fatal(err)
	}
	if after := f.list(t, "2023-11-15"); len(after) != 0 {
		t.Errorf("want a dismissed bill gone from the list, got %d rows", len(after))
	}
}

// The commonest bill with no deadline is one whose terms were only in a scan,
// and the reader is the only one who can read those.
func TestSetInvoiceDueDateOutranksTheExtractedOne(t *testing.T) {
	f := newInvoiceFixture(t)
	ctx := context.Background()
	undated := openInvoice("4711")
	undated.DueDate = ""
	f.record(t, f.message(t, 1), undated)

	rows := f.list(t, "2023-11-15")
	if len(rows) != 1 {
		t.Fatalf("want the undated bill listed, got %d", len(rows))
	}
	updated, err := f.db.SetInvoiceDueDate(ctx, f.user.ID, rows[0].ID, "2023-11-20")
	if err != nil {
		t.Fatal(err)
	}
	if updated.EffectiveDueDate() != "2023-11-20" {
		t.Errorf("effective due date = %q, want the reader's entry", updated.EffectiveDueDate())
	}
	// And it counts for the chip, which is the whole reason to enter one.
	due, err := f.db.InvoicesDueOn(ctx, f.user.ID, "2023-11-25")
	if err != nil {
		t.Fatal(err)
	}
	if due.Count != 1 {
		t.Errorf("count = %d, want the hand-entered deadline to raise the chip", due.Count)
	}
}

// Only paperwork is selected, and only until it has been read.
func TestListMessagesNeedingInvoiceScanSelectsOnlyPaperwork(t *testing.T) {
	f := newInvoiceFixture(t)
	ctx := context.Background()
	paperwork := f.message(t, 1)
	other, err := f.db.CreateMessage(ctx, CreateMessage{
		UserID: f.user.ID, AccountID: f.account.ID, MailboxID: f.inbox.ID, BlobID: f.blob.ID,
		MessageIDHeader: "<other@example.test>", Subject: "Newsletter",
		FromAddr: "news@example.de", Category: mailparse.CategoryNewsletters,
		Date: f.base, UID: 99, BlobPath: f.blob.Path,
	})
	if err != nil {
		t.Fatal(err)
	}

	pending, err := f.db.ListMessagesNeedingInvoiceScan(ctx, f.user.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].ID != paperwork.ID {
		t.Fatalf("want only the paperwork selected, got %+v (other=%d)", pending, other.ID)
	}

	// Stamping it as read takes it out of the queue rather than leaving it to be
	// selected on every pass.
	if err := f.db.ClearMessageInvoices(ctx, f.user.ID, []int64{paperwork.ID}); err != nil {
		t.Fatal(err)
	}
	after, err := f.db.ListMessagesNeedingInvoiceScan(ctx, f.user.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 0 {
		t.Errorf("want nothing pending once read, got %d", len(after))
	}
}

// A message read on the fetch path arrives already stamped, so the backfill
// never opens it again. That is what keeps a pass that decodes attachments off
// the whole mailbox.
func TestInvoiceScannedMessagesSkipTheBackfill(t *testing.T) {
	f := newInvoiceFixture(t)
	ctx := context.Background()
	if _, err := f.db.CreateMessage(ctx, CreateMessage{
		UserID: f.user.ID, AccountID: f.account.ID, MailboxID: f.inbox.ID, BlobID: f.blob.ID,
		MessageIDHeader: "<read@example.test>", Subject: "Rechnung",
		FromAddr: "billing@firma.example.de", Category: mailparse.CategoryInvoices,
		Date: f.base, UID: 42, BlobPath: f.blob.Path, InvoiceScanned: true,
	}); err != nil {
		t.Fatal(err)
	}
	pending, err := f.db.ListMessagesNeedingInvoiceScan(ctx, f.user.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Errorf("want an already-read message skipped, got %d", len(pending))
	}
}

// An undated open bill is aged by the open horizon, not by the much shorter one
// that retires settled bills. Giving it the settled cutoff quietly took undated
// bills off the list after a month.
func TestUndatedOpenInvoiceSurvivesPastThePaidHistory(t *testing.T) {
	f := newInvoiceFixture(t)
	message := f.message(t, 1)
	undated := openInvoice("4711")
	undated.DueDate = ""
	f.record(t, message, undated)

	// The message is 60 days before the day being asked about: well past the
	// 30-day settled history, well inside the 180-day open horizon.
	day := f.base.AddDate(0, 0, 60).Format("2006-01-02")
	rows := f.list(t, day)
	if len(rows) != 1 {
		t.Fatalf("want the undated bill still listed after two months, got %d rows", len(rows))
	}
}

// The chip and the list have to agree. A badge that opens an empty page is
// worse than either answer on its own.
func TestInvoicesDueOnAgreesWithTheListAboutTheHorizon(t *testing.T) {
	f := newInvoiceFixture(t)
	ctx := context.Background()
	ancient := openInvoice("OLD")
	ancient.DueDate = "2023-01-05"
	f.record(t, f.message(t, 1), ancient)

	// A year and a half after that deadline: past the open horizon.
	day := "2024-07-01"
	rows := f.list(t, day)
	due, err := f.db.InvoicesDueOn(ctx, f.user.ID, day)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Errorf("want the ancient bill off the list, got %d rows", len(rows))
	}
	if due.Count != 0 {
		t.Errorf("count = %d, want the chip to hide what the list hides", due.Count)
	}
}

// A chased bill ignores the horizon in both places, because somebody is still
// writing about it.
func TestChasedInvoiceIgnoresTheHorizonEverywhere(t *testing.T) {
	f := newInvoiceFixture(t)
	ctx := context.Background()
	ancient := openInvoice("OLD")
	ancient.DueDate = "2023-01-05"
	ancient.DunningLevel = mailparse.InvoiceDunningNotice
	f.record(t, f.message(t, 1), ancient)

	day := "2024-07-01"
	if rows := f.list(t, day); len(rows) != 1 {
		t.Errorf("want a chased bill listed however old, got %d rows", len(rows))
	}
	due, err := f.db.InvoicesDueOn(ctx, f.user.ID, day)
	if err != nil {
		t.Fatal(err)
	}
	if due.Count != 1 || due.Chased != 1 {
		t.Errorf("count/chased = %d/%d, want 1/1", due.Count, due.Chased)
	}
}
