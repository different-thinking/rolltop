// File overview: Translation between Graph resources and Rolltop's own rows.
// The awkward parts are time -- Graph states every instant as a wall time plus
// a zone, and an all-day event as a midnight in some zone -- and the invitee
// answers, which Graph spells differently from the store's Google-shaped
// values.

package m365calendar

import (
	"strings"
	"time"

	"rolltop/backend/store"
)

// linkPropertyID is the single-value extended property naming the pair an
// event belongs to and which side of it the event is ("<key>:primary",
// "<key>:copy", or "<key>:busy" for a placeholder in the busy calendar). The GUID is Rolltop's own property set; it is fixed forever,
// because a second Rolltop, a reconnect and a rebuilt mirror all have to find
// the pairs the first one made. Key and role share one property so a single
// $expand filter reads both.
const linkPropertyID = "String {6c1f7a52-3f0e-4d2b-9a77-51c0d4b8e913} Name rolltopLink"

// linkExpand is the $expand that reads the link with an event.
const linkExpand = "singleValueExtendedProperties($filter=id eq '" + linkPropertyID + "')"

const (
	linkRolePrimary = "primary"
	linkRoleCopy    = "copy"
	linkRoleBusy    = "busy"
)

// graphDateLayout is Graph's dateTime: a wall time with up to seven fractional
// digits and no offset -- the zone travels beside it.
const graphDateLayout = "2006-01-02T15:04:05.9999999"

// graphWriteLayout is the wall time Rolltop writes.
const graphWriteLayout = "2006-01-02T15:04:05"

// Online meeting providers Rolltop knows how to ask for, in order of
// preference. A work account offers Teams for business; a personal account
// offers the consumer Teams.
const (
	providerTeamsForBusiness = "teamsForBusiness"
	providerTeamsForConsumer = "teamsForConsumer"
	providerUnknown          = "unknown"
)

// namedColors are the hues Outlook draws its named calendar colours in, for
// a calendar Graph reports without a hex colour.
var namedColors = map[string]string{
	"lightblue":   "#a6d1f5",
	"lightgreen":  "#87d28e",
	"lightorange": "#fcab73",
	"lightgray":   "#c0c0c0",
	"lightyellow": "#f4d07a",
	"lightteal":   "#8ad6d6",
	"lightpink":   "#f08cc0",
	"lightbrown":  "#ca9b77",
	"lightred":    "#f88c9b",
}

// ToCalendarUpsert turns one calendar into the row the store keeps. selfEmail
// is the connected account's address, which is what tells the reader's own
// calendars from the ones shared with them.
func ToCalendarUpsert(calendar Calendar, connectionID int64, selfEmail string) store.MicrosoftCalendarUpsert {
	own := calendar.IsDefaultCalendar || calendar.Owner == nil ||
		sameAddress(calendar.Owner.Address, selfEmail)
	role := "reader"
	switch {
	case calendar.CanEdit && own:
		role = store.CalendarAccessRoleOwner
	case calendar.CanEdit:
		role = store.CalendarAccessRoleWriter
	}
	color := strings.TrimSpace(calendar.HexColor)
	if color == "" {
		color = namedColors[strings.ToLower(strings.TrimSpace(calendar.Color))]
	}
	var providers []string
	for _, provider := range calendar.AllowedOnlineMeetingProviders {
		if provider = strings.TrimSpace(provider); provider != "" && provider != providerUnknown {
			providers = append(providers, provider)
		}
	}
	return store.MicrosoftCalendarUpsert{
		ConnectionID:           connectionID,
		RemoteCalendarID:       strings.TrimSpace(calendar.ID),
		Summary:                strings.TrimSpace(calendar.Name),
		Color:                  color,
		AccessRole:             role,
		IsPrimary:              calendar.IsDefaultCalendar,
		OnlineMeetingProviders: providers,
		// Only the first time a calendar is seen. The reader's own calendars
		// start switched on; a colleague's calendar shared with them, the
		// birthday and holiday feeds start off rather than filling the first
		// week the reader opens.
		Selected: calendar.IsDefaultCalendar || (own && calendar.CanEdit),
	}
}

// ToEvent turns one Graph event into the row the store keeps. selfEmail marks
// the connected account among the attendees.
func ToEvent(event Event, calendarID int64, selfEmail string) store.CalendarEvent {
	start, end, allDay := eventBounds(event)
	out := store.CalendarEvent{
		CalendarID:       calendarID,
		ExternalID:       strings.TrimSpace(event.ID),
		ETag:             event.version(),
		ICalUID:          strings.TrimSpace(event.ICalUID),
		Summary:          strings.TrimSpace(event.Subject),
		Status:           store.CalendarEventStatusConfirmed,
		StartAt:          start,
		EndAt:            end,
		AllDay:           allDay,
		TimeZone:         portableZone(event.OriginalStartTimeZone),
		RecurringEventID: strings.TrimSpace(event.SeriesMasterID),
		HTMLLink:         strings.TrimSpace(event.WebLink),
		RemoteUpdatedAt:  parseTimestamp(event.LastModifiedDateTime),
	}
	if event.IsCancelled {
		// A meeting its organizer cancelled stays in an invitee's calendar
		// until they remove it; it is shown as cancelled rather than hidden.
		out.Status = store.CalendarEventStatusCancelled
	}
	if event.Body != nil {
		out.Description = strings.TrimSpace(event.Body.Content)
	}
	if event.Location != nil {
		out.Location = strings.TrimSpace(event.Location.DisplayName)
	}
	if event.Organizer != nil {
		out.OrganizerEmail = strings.TrimSpace(event.Organizer.EmailAddress.Address)
		out.OrganizerName = strings.TrimSpace(event.Organizer.EmailAddress.Name)
	}
	if event.IsOnlineMeeting {
		out.OnlineMeeting = true
		out.OnlineMeetingProvider = strings.TrimSpace(event.OnlineMeetingProvider)
		if event.OnlineMeeting != nil {
			out.OnlineMeetingURL = strings.TrimSpace(event.OnlineMeeting.JoinURL)
		}
	}
	out.Attendees = attendees(event, selfEmail)
	out.MyResponse = myResponse(event)
	out.LinkKey, out.LinkPrimary, out.LinkMasked = eventLink(event)
	return out
}

// eventBounds resolves Graph's start/end pair into a half-open interval.
//
// An all-day event is a date, not an instant, and is stored at midnight UTC of
// that date like Google's. Graph states it as a midnight in some zone -- the
// zone it was entered in, or the one asked for -- so the read in UTC lands up
// to half a day either side of midnight. The nearest UTC midnight is the date
// the event names for every zone within twelve hours of UTC, which is every
// zone that holds a calendar.
func eventBounds(event Event) (time.Time, time.Time, bool) {
	start := parseGraphTime(event.Start)
	end := parseGraphTime(event.End)
	if event.IsAllDay {
		start = nearestMidnight(start)
		end = nearestMidnight(end)
		if end.IsZero() || !end.After(start) {
			end = start.AddDate(0, 0, 1)
		}
		return start, end, true
	}
	if end.IsZero() || end.Before(start) {
		end = start
	}
	return start, end, false
}

// portableZone keeps an event's zone only when it is an IANA name. Graph
// reports the zone an event was entered in the way Outlook spells it -- a
// Windows name such as "W. Europe Standard Time", or "tzone://Microsoft/Custom"
// -- and the stored zone travels on: into a copy in a Google calendar, which
// refuses anything but an IANA name, and back into Graph on an edit. An empty
// zone is the safe answer there, because the stored bounds are absolute.
func portableZone(zone string) string {
	zone = strings.TrimSpace(zone)
	if zone == "" {
		return ""
	}
	if _, err := time.LoadLocation(zone); err != nil {
		return ""
	}
	return zone
}

func nearestMidnight(at time.Time) time.Time {
	if at.IsZero() {
		return at
	}
	return at.UTC().Add(12 * time.Hour).Truncate(24 * time.Hour)
}

// parseGraphTime reads a wall time in the zone Graph named. A zone this
// process cannot load is read as UTC, which is what every read asked for; a
// value it cannot parse at all costs the event its place rather than the page.
func parseGraphTime(value *DateTimeTimeZone) time.Time {
	if value == nil || strings.TrimSpace(value.DateTime) == "" {
		return time.Time{}
	}
	location := time.UTC
	if zone := strings.TrimSpace(value.TimeZone); zone != "" && !strings.EqualFold(zone, "UTC") {
		if loaded, err := time.LoadLocation(zone); err == nil {
			location = loaded
		}
	}
	parsed, err := time.ParseInLocation(graphDateLayout, strings.TrimSpace(value.DateTime), location)
	if err != nil {
		return time.Time{}
	}
	return parsed.UTC()
}

func parseTimestamp(value string) time.Time {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}
	}
	return parsed.UTC()
}

// attendees converts the guest list. The organizer is listed first, as Graph
// does not list them among the attendees but every invitee sees them as one.
func attendees(event Event, selfEmail string) []store.CalendarAttendee {
	var out []store.CalendarAttendee
	organizer := ""
	if event.Organizer != nil && len(event.Attendees) > 0 {
		organizer = strings.TrimSpace(event.Organizer.EmailAddress.Address)
		if organizer != "" {
			out = append(out, store.CalendarAttendee{
				Email:     organizer,
				Name:      strings.TrimSpace(event.Organizer.EmailAddress.Name),
				Response:  store.CalendarResponseAccepted,
				Organizer: true,
				Self:      sameAddress(organizer, selfEmail),
			})
		}
	}
	for _, attendee := range event.Attendees {
		email := strings.TrimSpace(attendee.EmailAddress.Address)
		name := strings.TrimSpace(attendee.EmailAddress.Name)
		if email == "" && name == "" {
			continue
		}
		if organizer != "" && sameAddress(email, organizer) {
			continue
		}
		response := ""
		if attendee.Status != nil {
			response = responseFromGraph(attendee.Status.Response)
		}
		out = append(out, store.CalendarAttendee{
			Email:    email,
			Name:     name,
			Response: response,
			Optional: attendee.Type == attendeeOptional,
			Resource: attendee.Type == attendeeResource,
			Self:     sameAddress(email, selfEmail),
		})
	}
	return out
}

// myResponse is the reader's own answer to an invitation. The organizer of a
// meeting has none to give, and neither does the owner of an event nobody else
// was invited to, so both read as empty -- which is what keeps the invitation
// card off an event the reader made.
func myResponse(event Event) string {
	if event.IsOrganizer || len(event.Attendees) == 0 || event.ResponseStatus == nil {
		return ""
	}
	return responseFromGraph(event.ResponseStatus.Response)
}

func responseFromGraph(response string) string {
	switch strings.TrimSpace(response) {
	case graphResponseAccepted, graphResponseOrganizer:
		return store.CalendarResponseAccepted
	case graphResponseTentative:
		return store.CalendarResponseTentative
	case graphResponseDeclined:
		return store.CalendarResponseDeclined
	case graphResponseNone, graphResponseNotAnswered:
		return store.CalendarResponseNeedsAction
	}
	return ""
}

// responseAction is the Graph action that gives an answer.
func responseAction(response string) (string, bool) {
	switch response {
	case store.CalendarResponseAccepted:
		return "accept", true
	case store.CalendarResponseTentative:
		return "tentativelyAccept", true
	case store.CalendarResponseDeclined:
		return "decline", true
	}
	return "", false
}

// eventLink reads the pair an event belongs to off its extended property:
// the key, whether this is the copy that was entered, and whether it is a
// placeholder.
func eventLink(event Event) (key string, primary, masked bool) {
	for _, property := range event.ExtendedProperties {
		if !strings.EqualFold(strings.TrimSpace(property.ID), linkPropertyID) {
			continue
		}
		key, role, _ := strings.Cut(strings.TrimSpace(property.Value), ":")
		key = strings.TrimSpace(key)
		if key == "" {
			return "", false, false
		}
		return key, role == linkRolePrimary, role == linkRoleBusy
	}
	return "", false, false
}

// linkValue renders an event's link for the extended property, or nothing for
// an event that is not linked.
func linkValue(event store.CalendarEvent) string {
	key := strings.TrimSpace(event.LinkKey)
	if key == "" {
		return ""
	}
	role := linkRoleCopy
	switch {
	case event.LinkPrimary:
		role = linkRolePrimary
	case event.LinkMasked:
		role = linkRoleBusy
	}
	return key + ":" + role
}

// basePayload renders the fields Rolltop's dialog owns. Graph merges a PATCH
// property by property, so what is left out here is left alone: the notes, the
// guests and the online meeting are added by the caller only when the edit
// changes them.
func basePayload(event store.CalendarEvent) map[string]any {
	payload := map[string]any{
		"subject":  event.Summary,
		"location": map[string]any{"displayName": event.Location},
		"isAllDay": event.AllDay,
		"start":    graphDateTime(event.StartAt, event.AllDay, event.TimeZone),
		"end":      graphDateTime(event.EndAt, event.AllDay, event.TimeZone),
	}
	if value := linkValue(event); value != "" {
		payload["singleValueExtendedProperties"] = []ExtendedProperty{{ID: linkPropertyID, Value: value}}
	}
	return payload
}

// bodyPayload renders notes as text, which is how they were read.
func bodyPayload(description string) map[string]any {
	return map[string]any{"contentType": "text", "content": description}
}

// graphDateTime renders an instant for a write. A timed event is written as a
// wall time in the zone it was entered in, so Outlook shows it in that zone
// and a recurring edit keeps its wall time; a zone this process cannot load is
// written as UTC, which names the same instant. An all-day event is the date
// it names at midnight, in the reader's zone -- Graph requires a midnight and
// shows the event on that date in the zone given.
func graphDateTime(at time.Time, allDay bool, zone string) DateTimeTimeZone {
	zone = strings.TrimSpace(zone)
	location, err := time.LoadLocation(zone)
	if zone == "" || err != nil {
		location, zone = time.UTC, "UTC"
	}
	if allDay {
		return DateTimeTimeZone{DateTime: at.UTC().Format("2006-01-02") + "T00:00:00", TimeZone: zone}
	}
	return DateTimeTimeZone{DateTime: at.In(location).Format(graphWriteLayout), TimeZone: zone}
}

// graphAttendees renders the guest list for a write. The organizer is not an
// attendee in Graph's model -- the row ToEvent lists first stands for them --
// and is left out by flag and by address, because the dialog sends the list
// back without the flag; an address-less row would fail the whole request.
func graphAttendees(list []store.CalendarAttendee, organizer string) []Attendee {
	out := make([]Attendee, 0, len(list))
	for _, attendee := range list {
		email := strings.TrimSpace(attendee.Email)
		if email == "" || attendee.Organizer || sameAddress(email, organizer) {
			continue
		}
		kind := attendeeRequired
		switch {
		case attendee.Resource:
			kind = attendeeResource
		case attendee.Optional:
			kind = attendeeOptional
		}
		out = append(out, Attendee{
			Type:         kind,
			EmailAddress: EmailAddress{Address: email, Name: strings.TrimSpace(attendee.Name)},
		})
	}
	return out
}

// guestKeys is the set of invitees a list names, without the organizer.
func guestKeys(list []store.CalendarAttendee, organizer string) map[string]bool {
	out := map[string]bool{}
	for _, attendee := range list {
		if attendee.Organizer || sameAddress(attendee.Email, organizer) {
			continue
		}
		if key := strings.ToLower(strings.TrimSpace(attendee.Email)); key != "" {
			out[key] = true
		}
	}
	return out
}

// sameGuests reports whether two lists invite the same people, which decides
// whether a write has to carry the list at all: an edit that leaves the guests
// alone is safer without it.
func sameGuests(a, b []store.CalendarAttendee, organizer string) bool {
	left, right := guestKeys(a, organizer), guestKeys(b, organizer)
	if len(left) != len(right) {
		return false
	}
	for key := range left {
		if !right[key] {
			return false
		}
	}
	return true
}

// chooseMeetingProvider picks the online meeting kind to ask for: Teams, in
// whichever flavour the calendar offers, before anything else it allows.
func chooseMeetingProvider(allowed []string) (string, bool) {
	for _, want := range []string{providerTeamsForBusiness, providerTeamsForConsumer} {
		for _, provider := range allowed {
			if provider == want {
				return provider, true
			}
		}
	}
	for _, provider := range allowed {
		if provider != "" && provider != providerUnknown {
			return provider, true
		}
	}
	return "", false
}

func sameAddress(a, b string) bool {
	a, b = strings.TrimSpace(a), strings.TrimSpace(b)
	return a != "" && strings.EqualFold(a, b)
}
