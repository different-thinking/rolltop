// File overview: The travel roles in Microsoft's link property.

package m365calendar

import (
	"testing"

	"rolltop/backend/store"
)

// A travel block's role survives the round trip through the extended
// property, and never reads as a copy or a placeholder.
func TestTravelRoleRoundTrips(t *testing.T) {
	for _, side := range []string{store.CalendarTravelBefore, store.CalendarTravelAfter} {
		value := linkValue(store.CalendarEvent{LinkKey: "k", LinkTravel: side})
		read := ToEvent(Event{ID: "x", ExtendedProperties: []ExtendedProperty{{ID: linkPropertyID, Value: value}}}, 1, "")
		if read.LinkKey != "k" || read.LinkTravel != side || read.LinkPrimary || read.LinkMasked {
			t.Fatalf("%s (%q) read back as %+v", side, value, read)
		}
	}
}

// Outlook's one reminder is read as the event's own; one switched off is
// stored as none rather than left to a default Graph does not have.
func TestToEventReadsTheReminder(t *testing.T) {
	on := ToEvent(Event{ID: "x", IsReminderOn: true, ReminderMinutesBeforeStart: 15}, 1, "")
	if on.Reminders != "15" {
		t.Fatalf("reminder on = %q", on.Reminders)
	}
	off := ToEvent(Event{ID: "y", IsReminderOn: false, ReminderMinutesBeforeStart: 15}, 1, "")
	if off.Reminders != store.CalendarRemindersNone {
		t.Fatalf("reminder off = %q", off.Reminders)
	}
}
