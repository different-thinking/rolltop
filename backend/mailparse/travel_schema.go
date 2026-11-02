// File overview: schema.org reservations embedded in a message's HTML.
//
// This is the markup Gmail draws its trip cards from, so the senders that matter
// -- airlines, rail operators, ticket shops -- publish it in a predictable form:
// a JSON-LD script block holding a FlightReservation, TrainReservation,
// BusReservation or EventReservation, alone, in a list, or in an @graph. Every
// property may be a string, an object or a list, which is why every read below
// accepts all three.
//
// Only reservations are read. A bare Event in a newsletter is an advertisement
// for something the reader has not booked.

package mailparse

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var (
	travelScriptRE = regexp.MustCompile(`(?is)<script\b([^>]*)>(.*?)</script>`)
	travelTypeRE   = regexp.MustCompile(`(?i)type\s*=\s*["']?application/ld\+json`)
)

const (
	// maxTravelScripts bounds how many JSON-LD blocks one message is read for.
	maxTravelScripts = 8
	// maxTravelScriptBytes bounds one of them.
	maxTravelScriptBytes = 256 * 1024
	// maxTravelSchemaNodes bounds the walk over one document, so a hostile one
	// cannot turn the walk into a memory cost.
	maxTravelSchemaNodes = 2000
)

// travelFromSchema returns the bookings the message's markup declares.
func travelFromSchema(markup string) []TravelBooking {
	lower := strings.ToLower(markup)
	if !strings.Contains(lower, "ld+json") || !strings.Contains(lower, "reservation") {
		return nil
	}
	var out []TravelBooking
	scripts := 0
	for _, match := range travelScriptRE.FindAllStringSubmatch(markup, -1) {
		if !travelTypeRE.MatchString(match[1]) {
			continue
		}
		scripts++
		if scripts > maxTravelScripts {
			break
		}
		body := strings.TrimSpace(match[2])
		if len(body) > maxTravelScriptBytes {
			continue
		}
		body = strings.TrimPrefix(body, "<!--")
		body = strings.TrimSuffix(body, "-->")
		body = strings.TrimSpace(strings.TrimPrefix(strings.TrimSuffix(body, "]]>"), "//<![CDATA["))
		body = unescapeScriptEntities(body)
		var doc any
		if err := json.Unmarshal([]byte(body), &doc); err != nil {
			continue
		}
		budget := maxTravelSchemaNodes
		for _, node := range schemaReservations(doc, &budget) {
			if booking, ok := bookingFromReservation(node); ok {
				out = append(out, booking)
			}
		}
	}
	return out
}

// unescapeScriptEntities undoes the one escaping some senders' templates apply
// to everything, script bodies included.
func unescapeScriptEntities(body string) string {
	if !strings.Contains(body, "&quot;") {
		return body
	}
	return strings.NewReplacer("&quot;", `"`, "&amp;", "&", "&lt;", "<", "&gt;", ">", "&#39;", "'").Replace(body)
}

// schemaReservations walks a JSON-LD document for the reservation nodes in it.
func schemaReservations(doc any, budget *int) []map[string]any {
	var out []map[string]any
	var walk func(any)
	walk = func(node any) {
		if *budget <= 0 {
			return
		}
		*budget--
		switch value := node.(type) {
		case []any:
			for _, item := range value {
				walk(item)
			}
		case map[string]any:
			if travelKindForSchema(schemaType(value)) != "" {
				out = append(out, value)
				return
			}
			for _, key := range []string{"@graph", "mainEntity", "itemListElement", "item"} {
				if child, ok := value[key]; ok {
					walk(child)
				}
			}
		}
	}
	walk(doc)
	return out
}

// schemaType is a node's @type with any vocabulary prefix taken off. A node may
// declare several; the first reservation type among them is the one returned.
func schemaType(node map[string]any) string {
	var types []string
	switch value := node["@type"].(type) {
	case string:
		types = []string{value}
	case []any:
		for _, item := range value {
			if s, ok := item.(string); ok {
				types = append(types, s)
			}
		}
	}
	for _, t := range types {
		t = t[strings.LastIndexAny(t, "/:#")+1:]
		if travelKindForSchema(t) != "" {
			return t
		}
	}
	if len(types) > 0 {
		t := types[0]
		return t[strings.LastIndexAny(t, "/:#")+1:]
	}
	return ""
}

func travelKindForSchema(t string) string {
	switch t {
	case "FlightReservation":
		return TravelFlight
	case "TrainReservation":
		return TravelTrain
	case "BusReservation":
		return TravelBus
	case "EventReservation":
		return TravelEvent
	default:
		return ""
	}
}

func bookingFromReservation(node map[string]any) (TravelBooking, bool) {
	kind := travelKindForSchema(schemaType(node))
	trip := schemaObject(node["reservationFor"])
	if kind == "" || trip == nil {
		return TravelBooking{}, false
	}
	booking := TravelBooking{
		Kind:      kind,
		Reference: schemaText(node["reservationNumber"]),
	}
	if booking.Reference == "" {
		booking.Reference = schemaText(node["confirmationNumber"])
	}
	status := strings.ToLower(schemaText(node["reservationStatus"]))
	cancelled := strings.Contains(status, "cancel")
	seg := TravelSegment{Kind: kind, Cancelled: cancelled, Source: TravelSourceSchema, Seat: schemaSeat(node, kind)}
	switch kind {
	case TravelFlight:
		airline := schemaObject(trip["airline"])
		code := schemaText(firstOf(airline, "iataCode"))
		number := strings.TrimSpace(schemaText(trip["flightNumber"]))
		if code != "" && number != "" && !strings.HasPrefix(strings.ToUpper(number), strings.ToUpper(code)) {
			number = code + " " + number
		}
		seg.Number = number
		seg.Provider = schemaText(firstOf(airline, "name"))
		dep := schemaObject(trip["departureAirport"])
		arr := schemaObject(trip["arrivalAirport"])
		seg.FromCode = schemaText(firstOf(dep, "iataCode"))
		seg.ToCode = schemaText(firstOf(arr, "iataCode"))
		seg.From = schemaPlaceName(trip["departureAirport"])
		seg.To = schemaPlaceName(trip["arrivalAirport"])
		seg.Start, seg.StartAt = schemaTime(trip["departureTime"], airportZone(seg.FromCode))
		seg.End, seg.EndAt = schemaTime(trip["arrivalTime"], airportZone(seg.ToCode))
	case TravelTrain, TravelBus:
		numberKey, nameKey, companyKey := "trainNumber", "trainName", "trainCompany"
		depKey, arrKey := "departureStation", "arrivalStation"
		if kind == TravelBus {
			numberKey, nameKey, companyKey = "busNumber", "busName", "busCompany"
			depKey, arrKey = "departureBusStop", "arrivalBusStop"
		}
		name := schemaText(trip[nameKey])
		number := schemaText(trip[numberKey])
		switch {
		case number != "" && name != "" && !strings.Contains(number, name):
			seg.Number = name + " " + number
		case number != "":
			seg.Number = number
		default:
			seg.Number = name
		}
		seg.Provider = schemaText(firstOf(schemaObject(trip[companyKey]), "name"))
		if seg.Provider == "" {
			seg.Provider = schemaText(firstOf(schemaObject(trip["provider"]), "name"))
		}
		seg.From = schemaPlaceName(trip[depKey])
		seg.To = schemaPlaceName(trip[arrKey])
		seg.Start, seg.StartAt = schemaTime(trip["departureTime"], nil)
		seg.End, seg.EndAt = schemaTime(trip["arrivalTime"], nil)
	case TravelEvent:
		seg.Title = schemaText(trip["name"])
		place := schemaObject(trip["location"])
		seg.Venue = schemaText(firstOf(place, "name"))
		seg.Address = schemaAddress(firstOf(place, "address"))
		if place == nil {
			seg.Venue = schemaText(trip["location"])
		}
		seg.Provider = schemaText(firstOf(schemaObject(node["provider"]), "name"))
		if seg.Provider == "" {
			seg.Provider = schemaText(firstOf(schemaObject(trip["organizer"]), "name"))
		}
		seg.Start, seg.StartAt = schemaTime(trip["startDate"], nil)
		seg.End, seg.EndAt = schemaTime(trip["endDate"], nil)
		if seg.End != "" && len(seg.End) != len(seg.Start) {
			seg.End, seg.EndAt = "", time.Time{}
		}
	}
	booking.Provider = seg.Provider
	if booking.Provider == "" {
		booking.Provider = schemaText(firstOf(schemaObject(node["provider"]), "name"))
		if booking.Provider == "" {
			booking.Provider = schemaText(firstOf(schemaObject(node["broker"]), "name"))
		}
	}
	if seg.Start == "" {
		return TravelBooking{}, false
	}
	booking.Segments = []TravelSegment{seg}
	return booking, true
}

// schemaSeat reads the seat from wherever a sender put it: the reserved
// ticket's seat, or the flight reservation's own airplaneSeat.
func schemaSeat(node map[string]any, kind string) string {
	if seat := schemaText(node["airplaneSeat"]); seat != "" {
		return seat
	}
	ticket := schemaObject(node["reservedTicket"])
	if ticket == nil {
		return ""
	}
	seat := schemaObject(ticket["ticketedSeat"])
	if seat == nil {
		return ""
	}
	number := schemaText(seat["seatNumber"])
	row := schemaText(seat["seatRow"])
	section := schemaText(seat["seatSection"])
	var parts []string
	switch kind {
	case TravelTrain, TravelBus:
		// A rail seat is a coach and a seat; senders put the coach in either
		// field.
		coach := section
		if coach == "" {
			coach = row
		}
		if coach != "" {
			parts = append(parts, "Coach "+coach)
		}
		if number != "" {
			parts = append(parts, "seat "+number)
		}
	case TravelEvent:
		if section != "" {
			parts = append(parts, "Block "+section)
		}
		if row != "" {
			parts = append(parts, "row "+row)
		}
		if number != "" {
			parts = append(parts, "seat "+number)
		}
	default:
		if row != "" && number != "" && !strings.HasPrefix(number, row) {
			number = row + number
		}
		if number != "" {
			parts = append(parts, number)
		}
	}
	return strings.Join(parts, ", ")
}

func schemaObject(value any) map[string]any {
	switch v := value.(type) {
	case map[string]any:
		return v
	case []any:
		for _, item := range v {
			if m, ok := item.(map[string]any); ok {
				return m
			}
		}
	}
	return nil
}

func firstOf(node map[string]any, key string) any {
	if node == nil {
		return nil
	}
	return node[key]
}

// schemaText reads a property as text whichever shape it came in.
func schemaText(value any) string {
	switch v := value.(type) {
	case string:
		return strings.TrimSpace(v)
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	case []any:
		for _, item := range v {
			if s := schemaText(item); s != "" {
				return s
			}
		}
	case map[string]any:
		for _, key := range []string{"name", "@value", "value", "@id"} {
			if s, ok := v[key].(string); ok && strings.TrimSpace(s) != "" {
				return strings.TrimSpace(s)
			}
		}
	}
	return ""
}

func schemaPlaceName(value any) string {
	place := schemaObject(value)
	if place == nil {
		return schemaText(value)
	}
	if name := schemaText(place["name"]); name != "" {
		return name
	}
	return schemaText(place["iataCode"])
}

func schemaAddress(value any) string {
	address := schemaObject(value)
	if address == nil {
		return schemaText(value)
	}
	var parts []string
	for _, key := range []string{"streetAddress", "postalCode", "addressLocality"} {
		if s := schemaText(address[key]); s != "" {
			parts = append(parts, s)
		}
	}
	if len(parts) == 3 {
		return parts[0] + ", " + parts[1] + " " + parts[2]
	}
	return strings.Join(parts, ", ")
}

func schemaTime(value any, zone *time.Location) (string, time.Time) {
	local, at, ok := parseTravelTime(schemaText(value), zone)
	if !ok {
		return "", time.Time{}
	}
	return local, at
}
