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
	"strconv"
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
	icsRouteRE = regexp.MustCompile(`^\s*(.+?)\s*(?:->|→|➞|➔|=>|–|—| - | nach )\s*(.+?)\s*$`)
	// icsSeatRE reads a reservation written out in words, the way a calendar
	// description spells what the PDF abbreviates: "Wagen 25, Platz 45".
	icsSeatRE = regexp.MustCompile(`(?i)\bWagen\s*(\d{1,3})\s*,?\s*(?:Platz|Plätze|Sitzplatz)\s*([0-9][0-9 ,]*)`)
)

// travelFromICS reads an attached calendar object. trainOperator says the
// message is a rail operator's own, ticketWords that it calls itself a ticket.
func travelFromICS(data []byte, trainOperator, ticketWords bool) []TravelBooking {
	cal, err := icalendar.Parse(data)
	if err != nil || len(cal.Events) > maxICSTravelEvents {
		return nil
	}
	if trainOperator {
		if booking, ok := travelFromTrainICS(cal); ok {
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

// travelFromTrainICS reads a rail operator's calendar object as one booking.
// DB writes one event per journey -- the way there, the way back -- and spells
// out every train of it in the description:
//
//	Dresden-Neustadt ➞ Leipzig Hbf
//	[Reservierung: 1 Sitzplatz, Wg. 12, Pl. 51]
//	IC 1914
//	● ab 09:29 Dresden-Neustadt ▷ Gleis 5
//	○ an 10:31 Leipzig Hbf ▷ Gleis 15
//
// so each train becomes a leg (dbICSLegs), and the change in Leipzig stays
// inside one journey. An event whose description cannot be read that way is
// still one leg, from its summary and its bounds: its start is what joins it to
// the PDF's leg for the same departure.
func travelFromTrainICS(cal icalendar.Calendar) (TravelBooking, bool) {
	booking := TravelBooking{Kind: TravelTrain, Provider: "Deutsche Bahn"}
	for _, event := range cal.Events {
		if event.Recurring || event.Occurrence || event.Start.IsZero() || event.AllDay {
			continue
		}
		if booking.Reference == "" {
			if match := dbReferenceRE.FindStringSubmatch(event.Description); match != nil {
				booking.Reference = match[1]
			}
		}
		legs := dbICSLegs(event)
		if len(legs) == 0 {
			legs = []TravelSegment{dbICSWholeJourney(event)}
		}
		// Seat reservation or ticket is the subject's to say (applyDBSubject).
		for i := range legs {
			legs[i].Cancelled = event.Status == "CANCELLED"
			// One UID covers the whole journey, so every train of it carries
			// it: a journey reads as in the calendar only when all its legs
			// do, and the provider's imported copy is one event for all of them.
			legs[i].ICalUID = event.UID
		}
		booking.Segments = append(booking.Segments, legs...)
	}
	return booking, len(booking.Segments) > 0
}

var (
	// dbICSProductRE is a train line as DB's calendar export writes it:
	// "ICE 1655", "IC 1914", "RE15 (74824)" -- a regional line with its train
	// number in brackets.
	dbICSProductRE = regexp.MustCompile(`^\s*(ICE|IC|EC|ECE|EN|NJ|RJX|RJ|TGV|THA|EST|FLX|IRE|RE|RB|MEX|ALX|ERB|BRB|VIA|HLB|NWB|S|U|STR|Bus)\s?(\d{1,5})(?:\s*\((\d{1,6})\))?\s*$`)
	// dbICSStopRE is a departure or arrival: "● ab 09:29 Dresden-Neustadt ▷ Gleis 5".
	dbICSStopRE  = regexp.MustCompile(`^\s*[●○•◦]?\s*(ab|an)\s+(\d{1,2}):(\d{2})\s+(.+?)\s*(?:▷\s*Gleis\s*(\S+))?\s*$`)
	dbICSDateRE  = regexp.MustCompile(`(?i)\bDatum:\s*(\d{1,2})\.(\d{1,2})\.(\d{4})`)
	dbICSClassRE = regexp.MustCompile(`(?i)\bKlasse:\s*([12])\.\s*Klasse`)
)

// dbICSLegs reads the trains out of one DB journey event's description.
func dbICSLegs(event icalendar.Event) []TravelSegment {
	if !strings.Contains(event.Description, " ab ") && !strings.Contains(event.Description, "\nab ") {
		return nil
	}
	zone := berlin
	if event.TimeZone != "" {
		if loc, err := time.LoadLocation(event.TimeZone); err == nil {
			zone = loc
		}
	}
	if zone == nil {
		zone = time.UTC
	}
	// The day the journey starts on: the description's own "Datum:", else the
	// event's start read in the timetable's zone.
	start := event.Start
	if !event.Floating {
		start = start.In(zone)
	}
	day := time.Date(start.Year(), start.Month(), start.Day(), 0, 0, 0, 0, zone)
	if match := dbICSDateRE.FindStringSubmatch(event.Description); match != nil {
		d, _ := strconv.Atoi(match[1])
		m, _ := strconv.Atoi(match[2])
		y, _ := strconv.Atoi(match[3])
		day = time.Date(y, time.Month(m), d, 0, 0, 0, 0, zone)
	}
	class := ""
	if match := dbICSClassRE.FindStringSubmatch(event.Description); match != nil {
		class = map[string]string{"1": "1st class", "2": "2nd class"}[match[1]]
	}

	var legs []TravelSegment
	var leg *TravelSegment
	seat := ""
	last := time.Time{}
	// at turns a clock time into a moment on the journey, rolling over
	// midnight when a time comes out earlier than the one before it.
	at := func(hour, minute int) time.Time {
		t := time.Date(day.Year(), day.Month(), day.Day(), hour, minute, 0, 0, zone)
		for !last.IsZero() && t.Before(last) {
			t = t.AddDate(0, 0, 1)
		}
		last = t
		return t
	}
	for _, line := range strings.Split(event.Description, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			leg, seat = nil, ""
			continue
		}
		if strings.HasPrefix(line, "[") && strings.Contains(line, "Wg.") {
			seat = dbSeat(line)
			continue
		}
		if match := dbICSProductRE.FindStringSubmatch(line); match != nil {
			number := match[1] + " " + match[2]
			if match[3] != "" {
				number += " (" + match[3] + ")"
			}
			legs = append(legs, TravelSegment{
				Kind: TravelTrain, Number: number, Provider: "Deutsche Bahn",
				Seat: joinSeatClass(seat, class), Source: TravelSourceICS,
			})
			leg = &legs[len(legs)-1]
			continue
		}
		if leg == nil {
			continue
		}
		match := dbICSStopRE.FindStringSubmatch(line)
		if match == nil {
			continue
		}
		hour, _ := strconv.Atoi(match[2])
		minute, _ := strconv.Atoi(match[3])
		moment := at(hour, minute)
		station := strings.TrimSpace(match[4])
		if match[1] == "ab" && leg.Start == "" {
			leg.From, leg.Start, leg.StartAt = station, moment.Format(travelLocalLayout), moment
		} else if match[1] == "an" && leg.Start != "" {
			leg.To, leg.End, leg.EndAt = station, moment.Format(travelLocalLayout), moment
		}
	}
	kept := legs[:0]
	for _, l := range legs {
		if l.Start != "" {
			kept = append(kept, l)
		}
	}
	return kept
}

// dbICSWholeJourney is the journey as one leg, for an event whose description
// does not list its trains.
func dbICSWholeJourney(event icalendar.Event) TravelSegment {
	text := event.Summary + "\n" + event.Description
	seg := TravelSegment{Kind: TravelTrain, Provider: "Deutsche Bahn", Source: TravelSourceICS}
	if product := dbProductRE.FindStringSubmatch(text); product != nil {
		seg.Number = product[1] + " " + product[2]
	}
	route := strings.Trim(strings.TrimSpace(dbProductRE.ReplaceAllString(event.Summary, "")), " ,:;()")
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
	if seg.Number == "" && seg.From == "" && seg.To == "" {
		seg.Title = event.Summary
	}
	icsBounds(event, &seg)
	return seg
}

func joinSeatClass(seat, class string) string {
	switch {
	case seat == "":
		return class
	case class == "":
		return seat
	default:
		return seat + " (" + class + ")"
	}
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
		if seg.Kind == TravelTrain && berlin != nil {
			// DB exports some journeys without a zone; the wall clock is still
			// its timetable's, which is Berlin's.
			wall := event.Start.UTC()
			seg.StartAt = time.Date(wall.Year(), wall.Month(), wall.Day(), wall.Hour(), wall.Minute(), 0, 0, berlin)
			if event.End.After(event.Start) {
				end := event.End.UTC()
				seg.EndAt = time.Date(end.Year(), end.Month(), end.Day(), end.Hour(), end.Minute(), 0, 0, berlin)
			}
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
