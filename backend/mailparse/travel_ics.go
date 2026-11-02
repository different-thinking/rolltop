// File overview: A calendar object attached to a ticket mail.
//
// Two kinds of sender attach one, and they mean different things by it.
//
// A rail operator attaches the journey: Deutsche Bahn puts an .ics on the
// booking confirmation holding the way there and the way back, and another on a
// seat reservation. Those are train legs, read as such (travelFromTrainICS) so
// they join the legs the ticket PDF and the markup name -- the same train at the
// same minute is one leg -- instead of standing beside them as an "event". A
// DB calendar object is read whatever its iTIP method says: the operator is
// stating a timetable, not inviting anybody.
//
// A ticket shop attaches the event as a published .ics beside the ticket. The
// object is structured -- a start, an end, a place -- but a calendar object on
// its own says nothing about whether it is a ticket, so it is only read when the
// message calls itself one (ticketEvidence), and never when it is an invitation:
// a meeting request is answered on the calendar card, not filed as an admission.

package mailparse

import (
	"regexp"
	"strings"
	"time"

	"rolltop/backend/icalendar"
)

// maxICSTravelEvents bounds what one attached calendar contributes. A ticket
// names its event and a booking its few journeys; a file with more events in it
// is a published schedule.
const maxICSTravelEvents = 6

var (
	// icsRouteRE splits a summary like "Berlin Hbf -> München Hbf" or
	// "Berlin Hbf – München Hbf" into its two stations.
	icsRouteRE = regexp.MustCompile(`^\s*(.+?)\s*(?:->|→|=>|–|—| - | nach )\s*(.+?)\s*$`)
	// icsSeatRE reads a reservation written out in words, the way a calendar
	// description spells what the PDF abbreviates: "Wagen 25, Platz 45".
	icsSeatRE = regexp.MustCompile(`(?i)\bWagen\s*(\d{1,3})\s*,?\s*(?:Platz|Plätze|Sitzplatz)\s*([0-9][0-9 ,]*)`)
)

// travelFromICS reads an attached calendar object. trainOperator says the
// message is a rail operator's own, ticketWords that it calls itself a ticket.
func travelFromICS(data []byte, subject string, trainOperator, ticketWords bool) []TravelBooking {
	cal, err := icalendar.Parse(data)
	if err != nil || len(cal.Events) > maxICSTravelEvents {
		return nil
	}
	if trainOperator {
		if booking, ok := travelFromTrainICS(cal, subject); ok {
			return []TravelBooking{booking}
		}
	}
	if !ticketWords {
		return nil
	}
	switch cal.Method {
	case "", "PUBLISH":
	default:
		return nil
	}
	var out []TravelBooking
	for _, event := range cal.Events {
		if event.Recurring || event.Occurrence || event.Start.IsZero() || len(event.Attendees) > 0 {
			continue
		}
		seg := TravelSegment{
			Kind:      TravelEvent,
			Title:     event.Summary,
			Venue:     event.Location,
			Cancelled: event.Status == "CANCELLED",
			Source:    TravelSourceICS,
			ICalUID:   event.UID,
		}
		icsBounds(event, &seg)
		out = append(out, TravelBooking{
			Kind:     TravelEvent,
			Provider: event.Organizer.Name,
			Segments: []TravelSegment{seg},
			keyHint:  event.UID,
		})
	}
	return out
}

// travelFromTrainICS reads a rail operator's calendar object as one booking
// with a leg per event: the way there and the way back of a booking, or the one
// train of a seat reservation. An event that names no train is still a leg --
// its start is what joins it to the PDF's leg for the same departure.
func travelFromTrainICS(cal icalendar.Calendar, subject string) (TravelBooking, bool) {
	booking := TravelBooking{Kind: TravelTrain, Provider: "Deutsche Bahn"}
	seatOnly := dbSeatOnlySubject(subject)
	for _, event := range cal.Events {
		if event.Recurring || event.Occurrence || event.Start.IsZero() || event.AllDay {
			continue
		}
		text := event.Summary + "\n" + event.Description
		seg := TravelSegment{
			Kind:      TravelTrain,
			Provider:  "Deutsche Bahn",
			Cancelled: event.Status == "CANCELLED",
			Source:    TravelSourceICS,
			ICalUID:   event.UID,
			SeatOnly:  seatOnly,
		}
		if product := dbProductRE.FindStringSubmatch(text); product != nil {
			seg.Number = product[1] + " " + product[2]
		}
		route := strings.TrimSpace(dbProductRE.ReplaceAllString(event.Summary, ""))
		route = strings.Trim(route, " ,:;()")
		if match := icsRouteRE.FindStringSubmatch(route); match != nil {
			seg.From, seg.To = strings.Trim(match[1], " ,:;()"), strings.Trim(match[2], " ,:;()")
		}
		if seg.From == "" {
			seg.From = event.Location
		}
		seg.Seat = dbSeat(event.Description)
		if seg.Seat == "" {
			if match := icsSeatRE.FindStringSubmatch(event.Description); match != nil {
				seg.Seat = dbSeat("Wg. " + match[1] + ", Pl. " + match[2])
			}
		}
		if booking.Reference == "" {
			if match := dbReferenceRE.FindStringSubmatch(event.Description); match != nil {
				booking.Reference = match[1]
			}
		}
		if seg.Number == "" && seg.From == "" && seg.To == "" {
			seg.Title = event.Summary
		}
		icsBounds(event, &seg)
		booking.Segments = append(booking.Segments, seg)
	}
	return booking, len(booking.Segments) > 0
}

// dbSeatOnlySubject reads whether a DB mail is a seat reservation without a
// ticket from its subject, which says so ("Ihre Reservierung") where the
// booking says "Buchungsbestätigung".
func dbSeatOnlySubject(subject string) bool {
	lower := strings.ToLower(subject)
	return strings.Contains(lower, "reservierung") && !containsAnyWord(lower, "buchung", "ticket", "fahrkarte")
}

// icsBounds copies an event's bounds onto a leg in the shape legs carry them:
// a plain day for an all-day event (its last day inclusive), a wall clock for a
// floating time, and the instant with its wall clock in the zone it was written
// in otherwise.
func icsBounds(event icalendar.Event, seg *TravelSegment) {
	switch {
	case event.AllDay:
		seg.Start = event.Start.UTC().Format(travelDateLayout)
		last := event.End.UTC().Add(-24 * time.Hour)
		if !event.End.IsZero() && last.After(event.Start.UTC()) {
			seg.End = last.Format(travelDateLayout)
		}
	case event.Floating:
		seg.Start = event.Start.UTC().Format(travelLocalLayout)
		if event.End.After(event.Start) {
			seg.End = event.End.UTC().Format(travelLocalLayout)
		}
	default:
		loc := time.UTC
		if event.TimeZone != "" {
			if zone, err := time.LoadLocation(event.TimeZone); err == nil {
				loc = zone
			}
		} else if seg.Kind == TravelTrain && berlin != nil {
			// A DB object written in UTC is still a timetable in Berlin's clock.
			loc = berlin
		}
		seg.StartAt = event.Start
		seg.Start = event.Start.In(loc).Format(travelLocalLayout)
		if event.End.After(event.Start) {
			seg.EndAt = event.End
			seg.End = event.End.In(loc).Format(travelLocalLayout)
		}
	}
}
