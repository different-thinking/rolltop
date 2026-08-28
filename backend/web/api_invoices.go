// File overview: The invoice list -- what the mail says is still owed, when it
// is due, how hard somebody is chasing it, and which messages said so.

package web

import (
	"net/http"
	"strings"
	"time"

	"rolltop/backend/store"
)

// apiInvoice is one bill as the browser reads it.
type apiInvoice struct {
	ID     int64  `json:"id"`
	Issuer string `json:"issuer"`
	Number string `json:"number"`
	// DueDate is the day the invoice counts as due, the reader's own entry
	// included. ManualDueDate is that entry alone, so the browser can say who
	// decided and offer to take it back.
	DueDate       string `json:"due_date"`
	ManualDueDate string `json:"manual_due_date"`
	Amount        string `json:"amount"`
	Currency      string `json:"currency"`
	// Status is what the invoice counts as, the reader's own answer included.
	// ManualStatus is that answer on its own.
	Status       string              `json:"status"`
	ManualStatus string              `json:"manual_status"`
	Settlement   string              `json:"settlement"`
	DunningLevel int                 `json:"dunning_level"`
	Messages     []apiInvoiceMessage `json:"messages"`
}

// apiInvoiceMessage is one mail that named an invoice, reduced to what opening
// it from the list needs.
type apiInvoiceMessage struct {
	ID        int64  `json:"id"`
	MailboxID int64  `json:"mailbox_id"`
	Subject   string `json:"subject"`
	From      string `json:"from"`
	Date      string `json:"date"`
}

func (s *Server) apiInvoices(w http.ResponseWriter, r *http.Request) {
	cu, ok := s.requireAPIAuth(w, r)
	if !ok {
		return
	}
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	// The day comes from the browser for the same reason the parcel list's
	// does: an invoice list is answered in days, and the day is exactly what
	// the server cannot know.
	day := r.URL.Query().Get("day")
	if day == "" {
		day = time.Now().Format("2006-01-02")
	}
	invoices, err := s.store.ListInvoices(r.Context(), cu.User.ID, day)
	if err != nil {
		if _, parseErr := time.Parse("2006-01-02", day); parseErr != nil {
			writeAPIError(w, http.StatusBadRequest, "A day has to be written YYYY-MM-DD.")
			return
		}
		s.serverError(w, r, err)
		return
	}
	writeJSON(w, map[string]any{"invoices": apiInvoicesFromStore(invoices)})
}

// apiInvoicesDue is the header chip's own read. It is a separate route from the
// list because it is asked on every page and again whenever a sync stores mail,
// while the list is asked only by the page that draws it.
func (s *Server) apiInvoicesDue(w http.ResponseWriter, r *http.Request) {
	cu, ok := s.requireAPIAuth(w, r)
	if !ok {
		return
	}
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	day := r.URL.Query().Get("day")
	if day == "" {
		day = time.Now().Format("2006-01-02")
	}
	if _, err := time.Parse("2006-01-02", day); err != nil {
		writeAPIError(w, http.StatusBadRequest, "A day has to be written YYYY-MM-DD.")
		return
	}
	due, err := s.store.InvoicesDueOn(r.Context(), cu.User.ID, day)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	writeJSON(w, map[string]any{"count": due.Count, "chased": due.Chased, "issuer": due.Issuer})
}

// apiInvoiceByID handles the two things a reader can say about a bill that the
// mail never did: that they have paid it (or that it was never a bill), and
// when it is actually due.
func (s *Server) apiInvoiceByID(w http.ResponseWriter, r *http.Request, rest string) {
	cu, ok := s.requireAPIAuth(w, r)
	if !ok {
		return
	}
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	if !s.verifyCSRF(w, r) {
		return
	}
	invoiceID, ok := parsePositiveID(w, rest)
	if !ok {
		return
	}
	// Both corrections travel on one route because they are one gesture from
	// the reader's side -- fixing what the extractor could not read -- and a
	// request that carries neither is a mistake worth naming rather than a
	// no-op worth answering.
	var in struct {
		ManualStatus  *string `json:"manual_status"`
		ManualDueDate *string `json:"manual_due_date"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	if in.ManualStatus == nil && in.ManualDueDate == nil {
		writeAPIError(w, http.StatusBadRequest, "Say either what the invoice's status is or when it is due.")
		return
	}
	updated := store.Invoice{}
	var err error
	if in.ManualStatus != nil {
		if !store.ValidInvoiceManualStatus(*in.ManualStatus) {
			writeAPIError(w, http.StatusBadRequest, "An invoice is either paid, dismissed, or left to the mail to say.")
			return
		}
		updated, err = s.store.SetInvoiceManualStatus(r.Context(), cu.User.ID, invoiceID, *in.ManualStatus)
		if !s.writeInvoiceError(w, r, err) {
			return
		}
	}
	if in.ManualDueDate != nil {
		day := strings.TrimSpace(*in.ManualDueDate)
		if day != "" {
			if _, parseErr := time.Parse("2006-01-02", day); parseErr != nil {
				writeAPIError(w, http.StatusBadRequest, "A day has to be written YYYY-MM-DD.")
				return
			}
		}
		updated, err = s.store.SetInvoiceDueDate(r.Context(), cu.User.ID, invoiceID, day)
		if !s.writeInvoiceError(w, r, err) {
			return
		}
	}
	// The row is returned without its messages: the browser already has them
	// and only what it just set has changed.
	writeJSON(w, map[string]any{"invoice": apiInvoicesFromStore([]store.Invoice{updated})[0]})
}

// writeInvoiceError reports whether the caller may carry on. A row the reader
// does not own reads as missing rather than as forbidden, which is what every
// other per-row route here does.
func (s *Server) writeInvoiceError(w http.ResponseWriter, r *http.Request, err error) bool {
	switch {
	case err == nil:
		return true
	case store.IsNotFound(err):
		http.NotFound(w, r)
	default:
		s.serverError(w, r, err)
	}
	return false
}

func apiInvoicesFromStore(invoices []store.Invoice) []apiInvoice {
	out := make([]apiInvoice, 0, len(invoices))
	for _, item := range invoices {
		messages := make([]apiInvoiceMessage, 0, len(item.Messages))
		for _, message := range item.Messages {
			messages = append(messages, apiInvoiceMessage{
				ID:        message.MessageID,
				MailboxID: message.MailboxID,
				Subject:   message.Subject,
				From:      message.FromAddr,
				Date:      timeString(time.Unix(message.Date, 0).UTC()),
			})
		}
		out = append(out, apiInvoice{
			ID:            item.ID,
			Issuer:        item.Issuer,
			Number:        item.Number,
			DueDate:       item.EffectiveDueDate(),
			ManualDueDate: item.ManualDueDate,
			Amount:        item.Amount,
			Currency:      item.Currency,
			Status:        item.EffectiveStatus(),
			ManualStatus:  item.ManualStatus,
			Settlement:    item.Settlement,
			DunningLevel:  item.DunningLevel,
			Messages:      messages,
		})
	}
	return out
}
