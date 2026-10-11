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
