// File overview: Apple Wallet passes (.pkpass) attached to a message.
//
// A pass is a zip holding pass.json, and pass.json says what it is about: a
// boarding pass names its transit type, an event ticket is an eventTicket, and
// every pass carries the moment it is relevant. Passes issued since iOS 15 also
// carry `semantics`, which names each fact -- flight number, airports, the
// event's start, the seats -- as a field rather than as a label a human reads.
// Those are read first; the visible fields are only the fallback.

package mailparse

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io"
	"strconv"
	"strings"
)

const (
	// maxPassJSONBytes bounds pass.json. A pass is a handful of fields.
	maxPassJSONBytes = 256 * 1024
	// maxPassEntries bounds how many zip entries are looked at to find it.
	maxPassEntries = 64
)

func isPassFile(filename, contentType string) bool {
	return strings.Contains(strings.ToLower(contentType), "pkpass") ||
		strings.HasSuffix(strings.ToLower(strings.TrimSpace(filename)), ".pkpass")
}

type passField struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	Value any    `json:"value"`
}

type passStructure struct {
	TransitType     string      `json:"transitType"`
	HeaderFields    []passField `json:"headerFields"`
	PrimaryFields   []passField `json:"primaryFields"`
	SecondaryFields []passField `json:"secondaryFields"`
	AuxiliaryFields []passField `json:"auxiliaryFields"`
}

type passSeat struct {
	SeatSection    string `json:"seatSection"`
	SeatRow        string `json:"seatRow"`
	SeatNumber     string `json:"seatNumber"`
	SeatIdentifier string `json:"seatIdentifier"`
}

type passSemantics struct {
	EventName                 string     `json:"eventName"`
	EventStartDate            string     `json:"eventStartDate"`
	EventEndDate              string     `json:"eventEndDate"`
	VenueName                 string     `json:"venueName"`
	AirlineCode               string     `json:"airlineCode"`
	FlightCode                string     `json:"flightCode"`
	FlightNumber              any        `json:"flightNumber"`
	DepartureAirportCode      string     `json:"departureAirportCode"`
	DepartureAirportName      string     `json:"departureAirportName"`
	DestinationAirportCode    string     `json:"destinationAirportCode"`
	DestinationAirportName    string     `json:"destinationAirportName"`
	DepartureStationName      string     `json:"departureStationName"`
	DestinationStationName    string     `json:"destinationStationName"`
	DepartureLocationDescript string     `json:"departureLocationDescription"`
	DestinationLocationDesc   string     `json:"destinationLocationDescription"`
	VehicleNumber             string     `json:"vehicleNumber"`
	VehicleName               string     `json:"vehicleName"`
	TransitProvider           string     `json:"transitProvider"`
	OriginalDepartureDate     string     `json:"originalDepartureDate"`
	CurrentDepartureDate      string     `json:"currentDepartureDate"`
	OriginalArrivalDate       string     `json:"originalArrivalDate"`
	CurrentArrivalDate        string     `json:"currentArrivalDate"`
	ConfirmationNumber        string     `json:"confirmationNumber"`
	Seats                     []passSeat `json:"seats"`
	TransitStatus             string     `json:"transitStatus"`
}

type passFile struct {
	Description      string         `json:"description"`
	OrganizationName string         `json:"organizationName"`
	RelevantDate     string         `json:"relevantDate"`
	ExpirationDate   string         `json:"expirationDate"`
	Voided           bool           `json:"voided"`
	BoardingPass     *passStructure `json:"boardingPass"`
	EventTicket      *passStructure `json:"eventTicket"`
	Semantics        *passSemantics `json:"semantics"`
}

// travelFromPass reads one pass. A pass that is neither a boarding pass nor an
// event ticket -- a coupon, a store card -- is not travel, and a pass that names
// no moment cannot be put in a calendar.
func travelFromPass(data []byte) (TravelBooking, bool) {
	pass, ok := readPassJSON(data)
	if !ok || pass.Voided {
		return TravelBooking{}, false
	}
	sem := passSemantics{}
	if pass.Semantics != nil {
		sem = *pass.Semantics
	}
	var kind string
	var fields *passStructure
	switch {
	case pass.BoardingPass != nil:
		fields = pass.BoardingPass
		switch pass.BoardingPass.TransitType {
		case "PKTransitTypeAir":
			kind = TravelFlight
		case "PKTransitTypeTrain":
			kind = TravelTrain
		case "PKTransitTypeBus":
			kind = TravelBus
		default:
			return TravelBooking{}, false
		}
	case pass.EventTicket != nil:
		fields = pass.EventTicket
		kind = TravelEvent
	default:
		return TravelBooking{}, false
	}
	seg := TravelSegment{Kind: kind, Source: TravelSourcePass, Provider: firstText(sem.TransitProvider, pass.OrganizationName)}
	startRaw := firstText(sem.CurrentDepartureDate, sem.OriginalDepartureDate, sem.EventStartDate, pass.RelevantDate)
	endRaw := firstText(sem.CurrentArrivalDate, sem.OriginalArrivalDate, sem.EventEndDate)
	seg.Start, seg.StartAt = schemaTime(startRaw, nil)
	seg.End, seg.EndAt = schemaTime(endRaw, nil)
	seg.Seat = passSeats(sem.Seats, kind)
	seg.Cancelled = strings.EqualFold(sem.TransitStatus, "cancelled")
	switch kind {
	case TravelFlight:
		seg.Number = sem.FlightCode
		if seg.Number == "" {
			number := passText(sem.FlightNumber)
			if number != "" && sem.AirlineCode != "" {
				seg.Number = sem.AirlineCode + " " + number
			}
		}
		seg.FromCode = sem.DepartureAirportCode
		seg.ToCode = sem.DestinationAirportCode
		seg.From = firstText(sem.DepartureAirportName, sem.DepartureLocationDescript)
		seg.To = firstText(sem.DestinationAirportName, sem.DestinationLocationDesc)
	case TravelTrain, TravelBus:
		seg.Number = strings.TrimSpace(strings.TrimSpace(sem.VehicleName + " " + sem.VehicleNumber))
		seg.From = firstText(sem.DepartureStationName, sem.DepartureLocationDescript)
		seg.To = firstText(sem.DestinationStationName, sem.DestinationLocationDesc)
	case TravelEvent:
		seg.Title = firstText(sem.EventName, pass.Description)
		seg.Venue = sem.VenueName
	}
	// A boarding pass without semantics still prints where it goes: the first
	// two primary fields are origin and destination by Wallet's own layout.
	if kind != TravelEvent && seg.From == "" && seg.FromCode == "" && len(fields.PrimaryFields) >= 2 {
		from, to := passText(fields.PrimaryFields[0].Value), passText(fields.PrimaryFields[1].Value)
		if kind == TravelFlight && len(from) == 3 && len(to) == 3 {
			seg.FromCode, seg.ToCode = from, to
		} else {
			seg.From, seg.To = from, to
		}
		if seg.From == "" {
			seg.From = fields.PrimaryFields[0].Label
		}
	}
	if seg.Number == "" && kind != TravelEvent {
		seg.Number = passFieldValue(fields, "flight", "flightnumber", "train", "trainnumber", "zug", "flug")
	}
	if seg.Seat == "" {
		seg.Seat = passFieldValue(fields, "seat", "sitz", "platz")
	}
	if kind == TravelEvent && seg.Venue == "" {
		seg.Venue = passFieldValue(fields, "venue", "location", "ort")
	}
	if kind == TravelFlight && !seg.StartAt.IsZero() && seg.FromCode != "" {
		if zone := airportZone(seg.FromCode); zone != nil {
			seg.Start = seg.StartAt.In(zone).Format(travelLocalLayout)
		}
	}
	if seg.Title == "" && kind == TravelEvent {
		seg.Title = pass.OrganizationName
	}
	if seg.Start == "" {
		return TravelBooking{}, false
	}
	return TravelBooking{
		Kind:      kind,
		Reference: sem.ConfirmationNumber,
		Provider:  seg.Provider,
		Segments:  []TravelSegment{seg},
	}, true
}

func readPassJSON(data []byte) (passFile, bool) {
	reader, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return passFile{}, false
	}
	for i, entry := range reader.File {
		if i >= maxPassEntries {
			break
		}
		if entry.Name != "pass.json" {
			continue
		}
		if entry.UncompressedSize64 > maxPassJSONBytes {
			return passFile{}, false
		}
		rc, err := entry.Open()
		if err != nil {
			return passFile{}, false
		}
		raw, err := io.ReadAll(io.LimitReader(rc, maxPassJSONBytes+1))
		rc.Close()
		if err != nil || len(raw) > maxPassJSONBytes {
			return passFile{}, false
		}
		// Some issuers write a byte-order mark.
		raw = bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf"))
		var pass passFile
		if err := json.Unmarshal(raw, &pass); err != nil {
			return passFile{}, false
		}
		return pass, true
	}
	return passFile{}, false
}

func passSeats(seats []passSeat, kind string) string {
	var out []string
	for _, seat := range seats {
		node := map[string]any{"reservedTicket": map[string]any{"ticketedSeat": map[string]any{
			"seatSection": seat.SeatSection, "seatRow": seat.SeatRow, "seatNumber": firstText(seat.SeatNumber, seat.SeatIdentifier),
		}}}
		if text := schemaSeat(node, kind); text != "" {
			out = append(out, text)
		}
		if len(out) >= 6 {
			break
		}
	}
	return strings.Join(out, "; ")
}

// passFieldValue finds a visible field by its key or label.
func passFieldValue(fields *passStructure, names ...string) string {
	if fields == nil {
		return ""
	}
	groups := [][]passField{fields.PrimaryFields, fields.SecondaryFields, fields.AuxiliaryFields, fields.HeaderFields}
	for _, group := range groups {
		for _, field := range group {
			key := strings.ToLower(field.Key)
			label := strings.ToLower(field.Label)
			for _, name := range names {
				if key == name || label == name {
					if value := passText(field.Value); value != "" {
						return value
					}
				}
			}
		}
	}
	return ""
}

func passText(value any) string {
	switch v := value.(type) {
	case string:
		return strings.TrimSpace(v)
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	default:
		return ""
	}
}

// firstText is the first value that is not blank, trimmed.
func firstText(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
