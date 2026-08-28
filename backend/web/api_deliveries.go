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
	ID             int64                `json:"id"`
	Carrier        string               `json:"carrier"`
	CarrierLabel   string               `json:"carrier_label"`
	TrackingNumber string               `json:"tracking_number"`
	TrackingURL    string               `json:"tracking_url"`
	ExpectedDate   string               `json:"expected_date"`
	WindowStart    string               `json:"window_start"`
	WindowEnd      string               `json:"window_end"`
	Status         string               `json:"status"`
	Messages       []apiShipmentMessage `json:"messages"`
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
			Status:         item.Status,
			Messages:       messages,
		})
	}
	return out
}
