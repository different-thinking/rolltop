// File overview: A calendar object attached to a ticket mail.
//
// Ticket shops attach the event as a published .ics beside the ticket. The
// object is structured -- a start, an end, a place -- but a calendar object on
// its own says nothing about whether it is a ticket, so it is only read when the
// message calls itself one (ticketEvidence), and never when it is an invitation:
// a meeting request is answered on the calendar card, not filed as an admission.

package mailparse

import (
	"time"

	"rolltop/backend/icalendar"
)

// maxICSTravelEvents bounds what one attached calendar contributes. A ticket
// names its event; a file with more events in it is a published schedule.
const maxICSTravelEvents = 4

func travelFromICS(data []byte) []TravelBooking {
	cal, err := icalendar.Parse(data)
	if err != nil {
		return nil
	}
	switch cal.Method {
	case "", "PUBLISH":
	default:
		return nil
	}
	if len(cal.Events) > maxICSTravelEvents {
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
		}
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
			}
			seg.StartAt = event.Start
			seg.Start = event.Start.In(loc).Format(travelLocalLayout)
			if event.End.After(event.Start) {
				seg.EndAt = event.End
				seg.End = event.End.In(loc).Format(travelLocalLayout)
			}
		}
		out = append(out, TravelBooking{
			Kind:     TravelEvent,
			Provider: event.Organizer.Name,
			Segments: []TravelSegment{seg},
			keyHint:  event.UID,
		})
	}
	return out
}
