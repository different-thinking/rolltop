// File overview: The invoice route's write path. What it must accept, and above
// all what it must refuse *without leaving anything behind*.

package web

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"rolltop/backend/mailparse"
	"rolltop/backend/store"
	"rolltop/backend/store/storetest"
)

// invoiceTestDay is the day every read in this file is answered in, and the day
// the fixture's message is dated. Fixing it keeps the list's horizons out of
// what these tests are about.
var invoiceTestDay = time.Date(2026, time.September, 3, 9, 0, 0, 0, time.UTC)

type invoiceAPIFixture struct {
	server  *Server
	db      *store.Store
	ctx     context.Context
	owner   store.User
	invoice store.Invoice
}

func newInvoiceAPIFixture(t *testing.T) invoiceAPIFixture {
	t.Helper()
	ctx := context.Background()
	db, err := storetest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	owner, err := db.CreateUser(ctx, "invoice-owner@example.test", "Owner", "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	account, err := db.CreateMailAccount(ctx, store.MailAccount{
		UserID: owner.ID, Email: "owner@example.test", Host: "imap.example.test", Port: 993,
		Username: "owner@example.test", EncryptedPassword: "secret", UseTLS: true, Mailbox: "*",
	})
	if err != nil {
		t.Fatal(err)
	}
	mailbox, err := db.GetOrCreateMailbox(ctx, owner.ID, account.ID, "INBOX")
	if err != nil {
		t.Fatal(err)
	}
	blob, err := db.CreateBlob(ctx, store.BlobRecord{UserID: owner.ID, Path: "blobs/invoice.eml", Size: 10})
	if err != nil {
		t.Fatal(err)
	}
	message, err := db.CreateMessage(ctx, store.CreateMessage{
		UserID: owner.ID, AccountID: account.ID, MailboxID: mailbox.ID, BlobID: blob.ID,
		MessageIDHeader: "<invoice-api@example.test>", Subject: "Ihre Rechnung 4711",
		FromAddr: "billing@firma.example.de", Category: mailparse.CategoryInvoices,
		Date: invoiceTestDay, UID: 1, BlobPath: blob.Path,
	})
	if err != nil {
		t.Fatal(err)
	}
	notice := &mailparse.InvoiceNotice{
		Issuer: "firma.example.de", Reference: "4711", Number: "4711",
		DueDate: invoiceTestDay.Format("2006-01-02"), Amount: "149.90", Currency: "EUR",
		Status: mailparse.InvoiceOpen, Settlement: mailparse.SettlementTransfer,
	}
	if err := db.ReplaceMessageInvoice(ctx, owner.ID, message.ID, message.Date.Unix(), notice); err != nil {
		t.Fatal(err)
	}
	invoices, err := db.ListInvoices(ctx, owner.ID, invoiceTestDay.Format("2006-01-02"))
	if err != nil || len(invoices) != 1 {
		t.Fatalf("want the invoice listed, got %d (%v)", len(invoices), err)
	}
	server := &Server{store: db, masterKey: bytes.Repeat([]byte{7}, 32), events: newEventHub()}
	return invoiceAPIFixture{server: server, db: db, ctx: ctx, owner: owner, invoice: invoices[0]}
}

func (f invoiceAPIFixture) post(t *testing.T, body string) *httptest.ResponseRecorder {
	t.Helper()
	target := "/api/invoices/" + strconv.FormatInt(f.invoice.ID, 10)
	request := httptest.NewRequest(http.MethodPost, target, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request = request.WithContext(context.WithValue(request.Context(), userContextKey, currentUser{User: f.owner}))
	const csrfBase = "invoice-action-csrf"
	request.AddCookie(&http.Cookie{Name: csrfCookie, Value: csrfBase})
	request.Header.Set("X-CSRF-Token", f.server.csrfForBase(csrfBase))
	response := httptest.NewRecorder()
	f.server.handleAPI(response, request)
	return response
}

func (f invoiceAPIFixture) reload(t *testing.T) store.Invoice {
	t.Helper()
	invoices, err := f.db.ListInvoices(f.ctx, f.owner.ID, invoiceTestDay.Format("2006-01-02"))
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range invoices {
		if item.ID == f.invoice.ID {
			return item
		}
	}
	t.Fatalf("invoice %d is gone", f.invoice.ID)
	return store.Invoice{}
}

// A request that is refused must leave the row exactly as it found it. Writing
// the status and then answering 400 tells the browser nothing happened while
// something did, and the row it goes on showing is a lie until the next reload.
func TestInvoiceWriteRefusesWithoutStoringAnything(t *testing.T) {
	f := newInvoiceAPIFixture(t)
	response := f.post(t, `{"manual_status":"paid","manual_due_date":"not-a-day"}`)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", response.Code, response.Body.String())
	}
	after := f.reload(t)
	if after.ManualStatus != store.InvoiceManualNone {
		t.Errorf("manual status = %q, want the refused request to have stored nothing", after.ManualStatus)
	}
	if after.ManualDueDate != "" {
		t.Errorf("manual due date = %q, want empty", after.ManualDueDate)
	}
}

// The same in the other order: a bad status must not let a good day through.
func TestInvoiceWriteRefusesABadStatusBeforeStoringTheDay(t *testing.T) {
	f := newInvoiceAPIFixture(t)
	response := f.post(t, `{"manual_status":"nonsense","manual_due_date":"2026-09-30"}`)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", response.Code, response.Body.String())
	}
	if after := f.reload(t); after.ManualDueDate != "" {
		t.Errorf("manual due date = %q, want empty", after.ManualDueDate)
	}
}

// And the accepted case still applies both.
func TestInvoiceWriteAppliesBothCorrections(t *testing.T) {
	f := newInvoiceAPIFixture(t)
	response := f.post(t, `{"manual_status":"paid","manual_due_date":"2026-09-30"}`)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", response.Code, response.Body.String())
	}
	var payload struct {
		Invoice struct {
			Status        string `json:"status"`
			ManualStatus  string `json:"manual_status"`
			ManualDueDate string `json:"manual_due_date"`
		} `json:"invoice"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Invoice.ManualStatus != "paid" || payload.Invoice.ManualDueDate != "2026-09-30" {
		t.Errorf("payload = %+v, want both corrections applied", payload.Invoice)
	}
	after := f.reload(t)
	if after.ManualStatus != store.InvoiceManualPaid || after.ManualDueDate != "2026-09-30" {
		t.Errorf("stored row = %+v, want both corrections", after)
	}
}

// A request carrying neither field is a mistake worth naming.
func TestInvoiceWriteRefusesAnEmptyCorrection(t *testing.T) {
	f := newInvoiceAPIFixture(t)
	if response := f.post(t, `{}`); response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", response.Code)
	}
}
