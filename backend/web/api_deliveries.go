// File overview: The parcel list -- what the mail says is on its way, when it
// is expected, and which messages said so.

package web

import (
	"net/http"
	"time"

	"rolltop/backend/mailparse"
	"rolltop/backend/store"
)

// apiShipment is one parcel as the browser reads it. The carrier's label and
// tracking URL are resolved here rather than in the browser so the one list of
// carriers stays in Go, where extraction reads it too.
type apiShipment struct {
	ID             int64  `json:"id"`
	Carrier        string `json:"carrier"`
	CarrierLabel   string `json:"carrier_label"`
	TrackingNumber string `json:"tracking_number"`
	TrackingURL    string `json:"tracking_url"`
	ExpectedDate   string `json:"expected_date"`
	WindowStart    string `json:"window_start"`
	WindowEnd      string `json:"window_end"`
	// Status is what the parcel counts as, the reader's own answer included.
	// ManualStatus is that answer on its own, so the browser can say who
	// decided and offer to take it back.
	Status       string               `json:"status"`
	ManualStatus string               `json:"manual_status"`
	Messages     []apiShipmentMessage `json:"messages"`
}

// apiShipmentMessage is one mail that named a parcel, reduced to what opening it
// from the list needs.
type apiShipmentMessage struct {
	ID        int64  `json:"id"`
	MailboxID int64  `json:"mailbox_id"`
	Subject   string `json:"subject"`
	From      string `json:"from"`
	Date      string `json:"date"`
}

func (s *Server) apiDeliveries(w http.ResponseWriter, r *http.Request) {
	cu, ok := s.requireAPIAuth(w, r)
	if !ok {
		return
	}
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	// The day comes from the browser. A parcel list is answered in days, and
	// the day is exactly what the server cannot know: it has no timezone for a
	// reader, and getting it wrong moves "arrives today" to the wrong row. The
	// server's own day is the fallback for a caller that did not say.
	day := r.URL.Query().Get("day")
	if day == "" {
		day = time.Now().Format("2006-01-02")
	}
	shipments, err := s.store.ListShipments(r.Context(), cu.User.ID, day)
	if err != nil {
		// A malformed day is the caller's mistake, not the server's.
		if _, parseErr := time.Parse("2006-01-02", day); parseErr != nil {
			writeAPIError(w, http.StatusBadRequest, "A day has to be written YYYY-MM-DD.")
			return
		}
		s.serverError(w, r, err)
		return
	}
	writeJSON(w, map[string]any{"shipments": apiShipmentsFromStore(shipments)})
}

// apiDeliveriesExpected is the header chip's own read. It is a separate route
// from the list because it is asked on every page and again whenever a sync
// stores mail, while the list is asked only by the page that draws it.
func (s *Server) apiDeliveriesExpected(w http.ResponseWriter, r *http.Request) {
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
	expected, err := s.store.ShipmentsExpectedOn(r.Context(), cu.User.ID, day)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	carrierLabel := ""
	if expected.Count == 1 {
		carrierLabel = mailparse.DeliveryCarrierLabel(expected.Carrier)
	}
	writeJSON(w, map[string]any{"count": expected.Count, "carrier_label": carrierLabel})
}

// apiDeliveryByID handles the one thing a reader can do to a parcel: say what
// they know about it that the mail never said.
func (s *Server) apiDeliveryByID(w http.ResponseWriter, r *http.Request, rest string) {
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
	shipmentID, ok := parsePositiveID(w, rest)
	if !ok {
		return
	}
	var in struct {
		ManualStatus string `json:"manual_status"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	if !store.ValidShipmentManualStatus(in.ManualStatus) {
		writeAPIError(w, http.StatusBadRequest, "A parcel is either delivered, dismissed, or left to the mail to say.")
		return
	}
	updated, err := s.store.SetShipmentManualStatus(r.Context(), cu.User.ID, shipmentID, in.ManualStatus)
	if err != nil {
		if store.IsNotFound(err) {
			http.NotFound(w, r)
			return
		}
		s.serverError(w, r, err)
		return
	}
	// The row is returned without its messages: the browser already has them
	// and only the status it just set has changed.
	writeJSON(w, map[string]any{"shipment": apiShipmentsFromStore([]store.Shipment{updated})[0]})
}

func apiShipmentsFromStore(shipments []store.Shipment) []apiShipment {
	out := make([]apiShipment, 0, len(shipments))
	for _, item := range shipments {
		messages := make([]apiShipmentMessage, 0, len(item.Messages))
		for _, message := range item.Messages {
			messages = append(messages, apiShipmentMessage{
				ID:        message.MessageID,
				MailboxID: message.MailboxID,
				Subject:   message.Subject,
				From:      message.FromAddr,
				Date:      timeString(time.Unix(message.Date, 0).UTC()),
			})
		}
		out = append(out, apiShipment{
			ID:             item.ID,
			Carrier:        item.Carrier,
			CarrierLabel:   mailparse.DeliveryCarrierLabel(item.Carrier),
			TrackingNumber: item.TrackingNumber,
			TrackingURL:    mailparse.DeliveryTrackingURL(item.Carrier, item.TrackingNumber),
			ExpectedDate:   item.ExpectedDate,
			WindowStart:    item.WindowStart,
			WindowEnd:      item.WindowEnd,
			Status:         item.EffectiveStatus(),
			ManualStatus:   item.ManualStatus,
			Messages:       messages,
		})
	}
	return out
}
