// File overview: The Calendar API wire types. They mirror Google's JSON one
// for one; turning them into Rolltop rows is event.go's job. Nothing here
// decides policy, and nothing here may end up in a log line.

package googlecalendar

// CalendarListEntry is one calendar as it appears in the account's list. It is
// not the calendar itself: the same underlying calendar appears in the list of
// everyone subscribed to it, each with their own colour and selection.
type CalendarListEntry struct {
	ID          string `json:"id"`
	Summary     string `json:"summary"`
	Description string `json:"description"`
	TimeZone    string `json:"timeZone"`
	// SummaryOverride is the name this account gave a calendar somebody else
	// owns. When it is set it is the name the user recognizes.
	SummaryOverride string `json:"summaryOverride"`
	BackgroundColor string `json:"backgroundColor"`
	ForegroundColor string `json:"foregroundColor"`
	// AccessRole is owner, writer, reader or freeBusyReader and decides whether
	// Rolltop may offer to edit anything in this calendar.
	AccessRole string `json:"accessRole"`
	Primary    bool   `json:"primary"`
	Selected   bool   `json:"selected"`
	// Deleted marks an entry the account unsubscribed from. A delta reports it
	// this way rather than by omission.
	Deleted bool `json:"deleted"`
	Hidden  bool `json:"hidden"`
}

// Name is the label to show for a calendar: the account's own override when it
// set one, otherwise the owner's title.
func (c CalendarListEntry) Name() string {
	if c.SummaryOverride != "" {
		return c.SummaryOverride
	}
	return c.Summary
}

// CalendarListPage is one response from calendarList.list.
type CalendarListPage struct {
	Items         []CalendarListEntry `json:"items"`
	NextPageToken string              `json:"nextPageToken"`
	// NextSyncToken arrives on the last page. Unlike the People API, Calendar
	// returns it without being asked.
	NextSyncToken string `json:"nextSyncToken"`
}

// EventDateTime is Google's start/end shape. Exactly one of Date and DateTime
// is set: a plain Date means an all-day event, which has no instant at all.
type EventDateTime struct {
	Date     string `json:"date,omitempty"`
	DateTime string `json:"dateTime,omitempty"`
	TimeZone string `json:"timeZone,omitempty"`
}

// EventPerson is an organizer or creator.
type EventPerson struct {
	Email       string `json:"email,omitempty"`
	DisplayName string `json:"displayName,omitempty"`
	Self        bool   `json:"self,omitempty"`
}

// EventAttendee is one invitee.
type EventAttendee struct {
	Email       string `json:"email,omitempty"`
	DisplayName string `json:"displayName,omitempty"`
	Optional    bool   `json:"optional,omitempty"`
	Resource    bool   `json:"resource,omitempty"`
	Organizer   bool   `json:"organizer,omitempty"`
	// Self marks the connected account among the invitees, which is the only
	// attendee whose answer Rolltop is allowed to change.
	Self           bool   `json:"self,omitempty"`
	ResponseStatus string `json:"responseStatus,omitempty"`
}

// Event is one occurrence as Google returns it. Every read asks for
// singleEvents, so a recurring series arrives already expanded and nothing in
// Rolltop has to evaluate a recurrence rule.
type Event struct {
	ID       string `json:"id"`
	ETag     string `json:"etag"`
	Status   string `json:"status"`
	HTMLLink string `json:"htmlLink"`
	Summary  string `json:"summary"`
	// Description carries the organizer's notes and may contain HTML. It is
	// stored as-is and escaped where it is rendered.
	Description string        `json:"description"`
	Location    string        `json:"location"`
	ICalUID     string        `json:"iCalUID"`
	Start       EventDateTime `json:"start"`
	End         EventDateTime `json:"end"`
	// RecurringEventID ties one occurrence back to its series.
	RecurringEventID string          `json:"recurringEventId"`
	Organizer        EventPerson     `json:"organizer"`
	Creator          EventPerson     `json:"creator"`
	Attendees        []EventAttendee `json:"attendees"`
	Updated          string          `json:"updated"`
}

// IsCancelled reports Google's tombstone. On a delta a removed occurrence
// arrives as an entry in this state rather than being left out.
func (e Event) IsCancelled() bool {
	return e.Status == statusCancelled
}

// EventsPage is one response from events.list.
type EventsPage struct {
	Items         []Event `json:"items"`
	NextPageToken string  `json:"nextPageToken"`
	NextSyncToken string  `json:"nextSyncToken"`
	TimeZone      string  `json:"timeZone"`
}

// EventWrite is the body of an insert or a patch. The scalar fields are never
// omitted: a PATCH that leaves one out keeps whatever Google has, so omitting
// the ones the dialog owns would make clearing a location impossible.
//
// Attendees is the exception and is a pointer for a reason. Google has no merge
// semantics for a guest list -- a payload carrying one replaces it wholesale,
// resetting the response status of everybody it does not restate -- so a write
// that is not changing the guests must leave the field out entirely. Otherwise
// renaming an event would silently un-answer everyone's invitation and, with
// sendUpdates, mail them all about it.
type EventWrite struct {
	Summary     string           `json:"summary"`
	Description string           `json:"description"`
	Location    string           `json:"location"`
	Start       EventDateTime    `json:"start"`
	End         EventDateTime    `json:"end"`
	Attendees   *[]EventAttendee `json:"attendees,omitempty"`
}

const statusCancelled = "cancelled"
