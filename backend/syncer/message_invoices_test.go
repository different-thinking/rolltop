package syncer

import (
	"testing"

	"rolltop/backend/mailparse"
)

// The rule these cover is the one that keeps the reminder honest: a scan that
// stopped at its budget may have stopped before the page saying the bill was
// already settled, so it is never allowed to raise one.
func TestInvoiceFromScanRefusesAnOpenBillFromATruncatedScan(t *testing.T) {
	notice := &mailparse.InvoiceNotice{
		Issuer:    "firma.example.de",
		Reference: "4711",
		Status:    mailparse.InvoiceOpen,
		DueDate:   "2026-09-17",
	}
	if got := invoiceFromScan(notice, false); got != nil {
		t.Errorf("want an open bill from a truncated scan to be dropped, got %+v", got)
	}
}

func TestInvoiceFromScanKeepsASettledBillFromATruncatedScan(t *testing.T) {
	notice := &mailparse.InvoiceNotice{
		Issuer:    "firma.example.de",
		Reference: "4711",
		Status:    mailparse.InvoicePaid,
	}
	// A settled answer raises nothing and can only close a row that is already
	// on the list, so a partial reading of one is still worth having.
	if got := invoiceFromScan(notice, false); got == nil {
		t.Error("want a settled bill from a truncated scan to be kept")
	}
}

func TestInvoiceFromScanKeepsEverythingFromACompleteScan(t *testing.T) {
	notice := &mailparse.InvoiceNotice{
		Issuer:    "firma.example.de",
		Reference: "4711",
		Status:    mailparse.InvoiceOpen,
	}
	if got := invoiceFromScan(notice, true); got != notice {
		t.Errorf("want a complete scan's answer kept as it is, got %+v", got)
	}
}

// A dunning letter is an open bill by definition, so the truncation rule has to
// be able to drop one -- and the fetch path, which reads whole messages, is what
// catches it instead.
func TestInvoiceFromScanDropsATruncatedDunning(t *testing.T) {
	notice := &mailparse.InvoiceNotice{
		Issuer:       "mahnwesen.example.de",
		Reference:    "4711",
		Status:       mailparse.InvoiceOpen,
		DunningLevel: mailparse.InvoiceDunningNotice,
	}
	if got := invoiceFromScan(notice, false); got != nil {
		t.Errorf("want a truncated dunning dropped rather than guessed at, got %+v", got)
	}
}

func TestInvoiceFromScanPassesNothingThrough(t *testing.T) {
	if got := invoiceFromScan(nil, true); got != nil {
		t.Errorf("want nil for a message that named no bill, got %+v", got)
	}
}
