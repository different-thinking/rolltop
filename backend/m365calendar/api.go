// File overview: The Microsoft Graph calendar wire types. They mirror Graph's
// JSON; turning them into Rolltop rows is event.go's job. Nothing here decides
// policy, and nothing here may end up in a log line.

package m365calendar

import "strings"

// Calendar is one calendar as Graph lists it under /me/calendars.
type Calendar struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Color is one of Graph's named colours ("lightBlue", "auto", ...);
	// HexColor is the colour Outlook actually draws, when Graph knows it.
	Color             string        `json:"color"`
	HexColor          string        `json:"hexColor"`
	IsDefaultCalendar bool          `json:"isDefaultCalendar"`
	CanEdit           bool          `json:"canEdit"`
	Owner             *EmailAddress `json:"owner"`
	// AllowedOnlineMeetingProviders is what an event in this calendar may be
	// made an online meeting with: teamsForBusiness for a work account,
	// teamsForConsumer or skypeForConsumer for a personal one, unknown for
	// none.
	AllowedOnlineMeetingProviders []string `json:"allowedOnlineMeetingProviders"`
	DefaultOnlineMeetingProvider  string   `json:"defaultOnlineMeetingProvider"`
}

// CalendarsPage is one page of /me/calendars.
type CalendarsPage struct {
	Value    []Calendar `json:"value"`
	NextLink string     `json:"@odata.nextLink"`
}

// EmailAddress is Graph's name-and-address pair.
type EmailAddress struct {
	Name    string `json:"name,omitempty"`
	Address string `json:"address,omitempty"`
}

// Recipient wraps an address the way Graph's organizer field does.
type Recipient struct {
	EmailAddress EmailAddress `json:"emailAddress"`
}

// DateTimeTimeZone is Graph's wall time plus the zone it is read in. Every read
// asks for UTC (preferHeader), so on the way in the zone is UTC; on the way out
// it is the zone the reader entered the event in.
type DateTimeTimeZone struct {
	DateTime string `json:"dateTime"`
	TimeZone string `json:"timeZone"`
}

// ResponseStatus is an invitee's answer.
type ResponseStatus struct {
	Response string `json:"response,omitempty"`
	Time     string `json:"time,omitempty"`
}

// Attendee is one invitee. Type is required, optional or resource.
type Attendee struct {
	Type         string          `json:"type,omitempty"`
	EmailAddress EmailAddress    `json:"emailAddress"`
	Status       *ResponseStatus `json:"status,omitempty"`
}

// ItemBody is an event's notes. Every read asks for the text rendering, which
// is what the dialog edits and what a copy in another calendar carries.
type ItemBody struct {
	ContentType string `json:"contentType"`
	Content     string `json:"content"`
}

// Location is where an event happens. Only its display name is used.
type Location struct {
	DisplayName string `json:"displayName"`
}

// OnlineMeetingInfo carries the join link of an online meeting.
type OnlineMeetingInfo struct {
	JoinURL string `json:"joinUrl"`
}

// ExtendedProperty is one single-value MAPI property, which is where Rolltop
// keeps the link between the two copies of one event.
type ExtendedProperty struct {
	ID    string `json:"id"`
	Value string `json:"value"`
}

// Event is one event or occurrence. calendarView expands recurring series, so
// an occurrence arrives with its own id and its series in SeriesMasterID.
type Event struct {
	ID                    string             `json:"id"`
	ETag                  string             `json:"@odata.etag"`
	ChangeKey             string             `json:"changeKey"`
	ICalUID               string             `json:"iCalUId"`
	Subject               string             `json:"subject"`
	Body                  *ItemBody          `json:"body"`
	Location              *Location          `json:"location"`
	Start                 *DateTimeTimeZone  `json:"start"`
	End                   *DateTimeTimeZone  `json:"end"`
	IsAllDay              bool               `json:"isAllDay"`
	IsCancelled           bool               `json:"isCancelled"`
	IsOrganizer           bool               `json:"isOrganizer"`
	Organizer             *Recipient         `json:"organizer"`
	Attendees             []Attendee         `json:"attendees"`
	ResponseStatus        *ResponseStatus    `json:"responseStatus"`
	SeriesMasterID        string             `json:"seriesMasterId"`
	Type                  string             `json:"type"`
	WebLink               string             `json:"webLink"`
	LastModifiedDateTime  string             `json:"lastModifiedDateTime"`
	OriginalStartTimeZone string             `json:"originalStartTimeZone"`
	IsOnlineMeeting       bool               `json:"isOnlineMeeting"`
	OnlineMeetingProvider string             `json:"onlineMeetingProvider"`
	OnlineMeeting         *OnlineMeetingInfo `json:"onlineMeeting"`
	ExtendedProperties    []ExtendedProperty `json:"singleValueExtendedProperties"`
}

// EventsPage is one page of a calendarView read.
type EventsPage struct {
	Value    []Event `json:"value"`
	NextLink string  `json:"@odata.nextLink"`
}

// version is what tells two reads of one event apart: the OData etag when
// Graph sends one, its change key otherwise.
func (e Event) version() string {
	if etag := strings.TrimSpace(e.ETag); etag != "" {
		return etag
	}
	return strings.TrimSpace(e.ChangeKey)
}

// Graph's response values for an invitee.
const (
	graphResponseNone        = "none"
	graphResponseOrganizer   = "organizer"
	graphResponseTentative   = "tentativelyAccepted"
	graphResponseAccepted    = "accepted"
	graphResponseDeclined    = "declined"
	graphResponseNotAnswered = "notResponded"
)

// Graph's attendee types.
const (
	attendeeRequired = "required"
	attendeeOptional = "optional"
	attendeeResource = "resource"
)
