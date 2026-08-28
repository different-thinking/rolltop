// File overview: The parcel a message is about, attached to the message the
// browser already fetched.
//
// A message list is the place a reader is when they wonder about a parcel, so
// the row says there is one and when it is due, and the thread says which
// number it was. Neither is a second source of truth: both read the same
// shipment rows the parcel list does, so a row and the list can never disagree.

package web

import (
	"context"
	"log"

	"rolltop/backend/mailparse"
	"rolltop/backend/store"
)

// apiMessageShipment is the parcel summary one message carries.
type apiMessageShipment struct {
	ID             int64  `json:"id"`
	Carrier        string `json:"carrier"`
	CarrierLabel   string `json:"carrier_label"`
	TrackingNumber string `json:"tracking_number"`
	TrackingURL    string `json:"tracking_url"`
	ExpectedDate   string `json:"expected_date"`
	Status         string `json:"status"`
	// Count is how many parcels the message named; the fields above describe
	// the first of them.
	Count int `json:"count"`
}

// messageShipments looks up the parcels a batch of messages named. A failure is
// logged and dropped rather than failing the list: a mailbox that cannot be read
// for parcels is still a mailbox, and the parcel page reports the fault itself.
func (s *Server) messageShipments(ctx context.Context, userID int64, messageIDs []int64) map[int64]*apiMessageShipment {
	if s == nil || userID <= 0 || len(messageIDs) == 0 {
		return nil
	}
	found, err := s.store.ShipmentsForMessages(ctx, userID, messageIDs)
	if err != nil {
		log.Printf("message shipments lookup failed user_id=%d messages=%d: %v", userID, len(messageIDs), err)
		return nil
	}
	if len(found) == 0 {
		return nil
	}
	out := make(map[int64]*apiMessageShipment, len(found))
	for messageID, item := range found {
		out[messageID] = apiMessageShipmentFromStore(item)
	}
	return out
}

func apiMessageShipmentFromStore(item store.MessageShipment) *apiMessageShipment {
	return &apiMessageShipment{
		ID:             item.ShipmentID,
		Carrier:        item.Carrier,
		CarrierLabel:   mailparse.DeliveryCarrierLabel(item.Carrier),
		TrackingNumber: item.TrackingNumber,
		TrackingURL:    mailparse.DeliveryTrackingURL(item.Carrier, item.TrackingNumber),
		ExpectedDate:   item.ExpectedDate,
		Status:         item.Status,
		Count:          item.Count,
	}
}
