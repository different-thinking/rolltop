// File overview: The bill a message is about, attached to the message the
// browser already fetched.
//
// A message list is the place a reader is when they wonder whether something
// still has to be paid, so the row says there is a bill and when it is due, and
// the thread says which invoice it was. Neither is a second source of truth:
// both read the same invoice rows the invoice list does, so a row and the list
// can never disagree.

package web

import (
	"context"
	"log"

	"rolltop/backend/store"
)

// apiMessageInvoice is the bill summary one message carries.
type apiMessageInvoice struct {
	ID           int64  `json:"id"`
	Issuer       string `json:"issuer"`
	Number       string `json:"number"`
	DueDate      string `json:"due_date"`
	Amount       string `json:"amount"`
	Currency     string `json:"currency"`
	Status       string `json:"status"`
	Settlement   string `json:"settlement"`
	DunningLevel int    `json:"dunning_level"`
}

// messageInvoices looks up the bills a batch of messages named. A failure is
// logged and dropped rather than failing the list: a mailbox that cannot be
// read for invoices is still a mailbox, and the invoice page reports the fault
// itself.
func (s *Server) messageInvoices(ctx context.Context, userID int64, messageIDs []int64) map[int64]*apiMessageInvoice {
	if s == nil || userID <= 0 || len(messageIDs) == 0 {
		return nil
	}
	found, err := s.store.InvoicesForMessages(ctx, userID, messageIDs)
	if err != nil {
		log.Printf("message invoices lookup failed user_id=%d messages=%d: %v", userID, len(messageIDs), err)
		return nil
	}
	if len(found) == 0 {
		return nil
	}
	out := make(map[int64]*apiMessageInvoice, len(found))
	for messageID, item := range found {
		out[messageID] = apiMessageInvoiceFromStore(item)
	}
	return out
}

func apiMessageInvoiceFromStore(item store.MessageInvoice) *apiMessageInvoice {
	return &apiMessageInvoice{
		ID:           item.InvoiceID,
		Issuer:       item.Issuer,
		Number:       item.Number,
		DueDate:      item.DueDate,
		Amount:       item.Amount,
		Currency:     item.Currency,
		Status:       item.Status,
		Settlement:   item.Settlement,
		DunningLevel: item.DunningLevel,
	}
}
