// Package icalendar reads the events out of an iCalendar object (RFC 5545) --
// the .ics file or text/calendar part a mailed invitation carries -- far enough
// to show the reader what is planned and when, and to seed a new event from it.
//
// It is a reader for display, not a calendar engine. A recurring event is
// reported as its first occurrence with Recurring set, because expanding a rule
// is the calendar provider's job and the event the reader adds is a single one;
// and a value it cannot read costs that event, never the whole object.
package icalendar

import (
	"bytes"
	"errors"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Limits on what one object may make the reader carry. An invitation holds one
// event; a published calendar (a school's holidays) holds a few dozen, and
// nothing a mail view shows needs more than the first of them.
const (
	maxEvents     = 25
	maxAttendees  = 200
	maxTextBytes  = 16 * 1024
	maxShortBytes = 1024
)

// ErrNoCalendar is returned for data that holds no VCALENDAR at all.
var ErrNoCalendar = errors.New("icalendar: no calendar object")

// Person is an organizer or attendee as the object names them.
type Person struct {
	Name  string
	Email string
	// PartStat is the attendee's answer as the object states it --
	// ACCEPTED, TENTATIVE, DECLINED or NEEDS-ACTION -- upper case, or empty.
	PartStat string
}

// Event is one VEVENT, resolved to absolute instants.
type Event struct {
	UID         string
	Summary     string
	Description string
	Location    string
	URL         string
	// Start and End are half-open. For an all-day event they are midnight UTC
	// of the plain dates the object named, the same convention the calendar
	// store uses, and must be read in UTC.
	Start  time.Time
	End    time.Time
	AllDay bool
	// Floating marks wall-clock times bound to no zone: 09:00 wherever the
	// reader is. Start and End then carry the wall clock in UTC fields and are
	// not instants.
	Floating bool
	// TimeZone is the IANA zone the times were written in, when there is one.
	TimeZone string
	// Status is CONFIRMED, TENTATIVE or CANCELLED, upper case, or empty.
	Status    string
	Organizer Person
	Attendees []Person
	// Recurring marks an event that repeats; Start is its first occurrence.
	Recurring bool
	// Occurrence marks a VEVENT that changes one occurrence of a series
	// (RECURRENCE-ID) rather than describing the series.
	Occurrence bool
	Sequence   int
	// recurrenceID is the RECURRENCE-ID, resolved the way DTSTART is, carried
	// into a reply so the answer names the occurrence it was asked about.
	recurrenceID *resolvedTime
}

// Calendar is one VCALENDAR object.
type Calendar struct {
	// Method is the iTIP method -- REQUEST for an invitation, CANCEL for a
	// cancellation, PUBLISH for a plain shared event -- upper case, or empty.
	Method string
	Events []Event
}

type property struct {
	name   string
	params map[string]string
	value  string
}

func (p property) param(name string) string {
	return p.params[name]
}

type component struct {
	name     string
	props    []property
	children []*component
}

func (c *component) first(name string) (property, bool) {
	for _, prop := range c.props {
		if prop.name == name {
			return prop, true
		}
	}
	return property{}, false
}

func (c *component) all(name string) []property {
	var out []property
	for _, prop := range c.props {
		if prop.name == name {
			out = append(out, prop)
		}
	}
	return out
}

// Parse reads every event of the first VCALENDAR in data.
func Parse(data []byte) (Calendar, error) {
	root := parseComponents(data)
	var cal *component
	for _, child := range root.children {
		if child.name == "VCALENDAR" {
			cal = child
			break
		}
	}
	if cal == nil {
		return Calendar{}, ErrNoCalendar
	}
	out := Calendar{}
	if method, ok := cal.first("METHOD"); ok {
		out.Method = strings.ToUpper(strings.TrimSpace(method.value))
	}
	zones := map[string]*vtimezone{}
	for _, child := range cal.children {
		if child.name == "VTIMEZONE" {
			if zone := readTimezone(child); zone != nil {
				zones[zone.id] = zone
			}
		}
	}
	for _, child := range cal.children {
		if child.name != "VEVENT" {
			continue
		}
		event, ok := readEvent(child, zones)
		if !ok {
			continue
		}
		out.Events = append(out.Events, event)
		if len(out.Events) >= maxEvents {
			break
		}
	}
	return out, nil
}

// parseComponents unfolds the content lines and nests them by BEGIN/END. An
// END that closes nothing open is ignored and a component left open at the end
// is kept, so a truncated attachment still shows what it holds.
func parseComponents(data []byte) *component {
	root := &component{}
	stack := []*component{root}
	for _, line := range unfold(data) {
		prop, ok := parseLine(line)
		if !ok {
			continue
		}
		switch prop.name {
		case "BEGIN":
			child := &component{name: strings.ToUpper(strings.TrimSpace(prop.value))}
			parent := stack[len(stack)-1]
			parent.children = append(parent.children, child)
			stack = append(stack, child)
		case "END":
			name := strings.ToUpper(strings.TrimSpace(prop.value))
			for i := len(stack) - 1; i > 0; i-- {
				if stack[i].name == name {
					stack = stack[:i]
					break
				}
			}
		default:
			current := stack[len(stack)-1]
			if current != root {
				current.props = append(current.props, prop)
			}
		}
	}
	return root
}

// unfold joins folded lines (RFC 5545 3.1): a line starting with a space or a
// tab continues the one before it, without that one character.
func unfold(data []byte) []string {
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
	text := string(data)
	if !utf8.ValidString(text) {
		text = strings.ToValidUTF8(text, "�")
	}
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	raw := strings.Split(text, "\n")
	lines := make([]string, 0, len(raw))
	for _, line := range raw {
		if (strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t")) && len(lines) > 0 {
			lines[len(lines)-1] += line[1:]
			continue
		}
		lines = append(lines, line)
	}
	return lines
}

// parseLine splits `NAME;PARAM=value;PARAM="quoted:value":value`.
func parseLine(line string) (property, bool) {
	if strings.TrimSpace(line) == "" {
		return property{}, false
	}
	i := 0
	for i < len(line) && line[i] != ';' && line[i] != ':' {
		i++
	}
	if i == 0 || i == len(line) {
		return property{}, false
	}
	prop := property{name: strings.ToUpper(strings.TrimSpace(line[:i])), params: map[string]string{}}
	for i < len(line) && line[i] == ';' {
		i++
		start := i
		for i < len(line) && line[i] != '=' && line[i] != ';' && line[i] != ':' {
			i++
		}
		key := strings.ToUpper(strings.TrimSpace(line[start:i]))
		value := ""
		if i < len(line) && line[i] == '=' {
			i++
			var b strings.Builder
			for i < len(line) && line[i] != ';' && line[i] != ':' {
				if line[i] == '"' {
					end := strings.IndexByte(line[i+1:], '"')
					if end < 0 {
						b.WriteString(line[i+1:])
						i = len(line)
						break
					}
					b.WriteString(line[i+1 : i+1+end])
					i += end + 2
					continue
				}
				b.WriteByte(line[i])
				i++
			}
			value = b.String()
		}
		if key != "" {
			prop.params[key] = value
		}
	}
	if i >= len(line) || line[i] != ':' {
		return property{}, false
	}
	prop.value = line[i+1:]
	return prop, true
}

// unescapeText reads a TEXT value (RFC 5545 3.3.11).
func unescapeText(value string) string {
	if !strings.Contains(value, `\`) {
		return value
	}
	var b strings.Builder
	for i := 0; i < len(value); i++ {
		if value[i] != '\\' || i+1 == len(value) {
			b.WriteByte(value[i])
			continue
		}
		i++
		switch value[i] {
		case 'n', 'N':
			b.WriteByte('\n')
		default:
			b.WriteByte(value[i])
		}
	}
	return b.String()
}

func textValue(c *component, name string, limit int) string {
	prop, ok := c.first(name)
	if !ok {
		return ""
	}
	return clip(strings.TrimSpace(unescapeText(prop.value)), limit)
}

func clip(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(value[cut]) {
		cut--
	}
	return value[:cut]
}

func readEvent(c *component, zones map[string]*vtimezone) (Event, bool) {
	startProp, ok := c.first("DTSTART")
	if !ok {
		return Event{}, false
	}
	start, ok := readDateTime(startProp, zones)
	if !ok {
		return Event{}, false
	}
	event := Event{
		UID:         clip(strings.TrimSpace(firstValue(c, "UID")), 300),
		Summary:     textValue(c, "SUMMARY", maxShortBytes),
		Description: textValue(c, "DESCRIPTION", maxTextBytes),
		Location:    textValue(c, "LOCATION", maxShortBytes),
		URL:         clip(strings.TrimSpace(firstValue(c, "URL")), maxShortBytes),
		Status:      strings.ToUpper(strings.TrimSpace(firstValue(c, "STATUS"))),
		Start:       start.at,
		AllDay:      start.date,
		Floating:    start.floating,
		TimeZone:    start.zone,
	}
	if seq, err := strconv.Atoi(strings.TrimSpace(firstValue(c, "SEQUENCE"))); err == nil {
		event.Sequence = seq
	}
	_, hasRule := c.first("RRULE")
	_, hasDates := c.first("RDATE")
	event.Recurring = hasRule || hasDates
	if recurrence, ok := c.first("RECURRENCE-ID"); ok {
		event.Occurrence = true
		if at, ok := readDateTime(recurrence, zones); ok {
			event.recurrenceID = &at
		}
	}

	if endProp, ok := c.first("DTEND"); ok {
		if end, ok := readDateTime(endProp, zones); ok && end.date == start.date {
			event.End = end.at
		}
	}
	if event.End.IsZero() {
		if durProp, ok := c.first("DURATION"); ok {
			if dur, ok := parseDuration(durProp.value); ok {
				event.End = addDuration(event.Start, dur, event.AllDay)
			}
		}
	}
	if event.End.IsZero() || event.End.Before(event.Start) {
		// RFC 5545 3.6.1: without an end, a date lasts the day and a
		// date-time is an instant.
		if event.AllDay {
			event.End = event.Start.AddDate(0, 0, 1)
		} else {
			event.End = event.Start
		}
	}
	if event.AllDay && !event.End.After(event.Start) {
		event.End = event.Start.AddDate(0, 0, 1)
	}

	if organizer, ok := c.first("ORGANIZER"); ok {
		event.Organizer = readPerson(organizer)
	}
	for _, attendee := range c.all("ATTENDEE") {
		if len(event.Attendees) >= maxAttendees {
			break
		}
		person := readPerson(attendee)
		if person.Email == "" && person.Name == "" {
			continue
		}
		event.Attendees = append(event.Attendees, person)
	}
	return event, true
}

func firstValue(c *component, name string) string {
	prop, _ := c.first(name)
	return prop.value
}

func readPerson(prop property) Person {
	email := strings.TrimSpace(prop.value)
	if len(email) >= 7 && strings.EqualFold(email[:7], "mailto:") {
		email = email[7:]
	}
	if !strings.Contains(email, "@") {
		email = ""
	}
	name := strings.TrimSpace(prop.param("CN"))
	if strings.EqualFold(name, email) {
		name = ""
	}
	return Person{
		Name:     clip(name, maxShortBytes),
		Email:    clip(strings.TrimSpace(email), 320),
		PartStat: strings.ToUpper(strings.TrimSpace(prop.param("PARTSTAT"))),
	}
}

type resolvedTime struct {
	at       time.Time
	date     bool
	floating bool
	zone     string
}

type wallClock struct {
	year, month, day, hour, minute, second int
}

func (w wallClock) utc() time.Time {
	return time.Date(w.year, time.Month(w.month), w.day, w.hour, w.minute, w.second, 0, time.UTC)
}

// parseWallClock reads a DATE (YYYYMMDD) or DATE-TIME (YYYYMMDDTHHMMSS[Z]).
func parseWallClock(value string) (wallClock, bool, bool, bool) {
	value = strings.TrimSpace(value)
	utc := strings.HasSuffix(value, "Z") || strings.HasSuffix(value, "z")
	value = strings.TrimRight(value, "Zz")
	num := func(s string) (int, bool) {
		n, err := strconv.Atoi(s)
		return n, err == nil
	}
	if len(value) < 8 {
		return wallClock{}, false, false, false
	}
	var w wallClock
	var ok1, ok2, ok3 bool
	w.year, ok1 = num(value[0:4])
	w.month, ok2 = num(value[4:6])
	w.day, ok3 = num(value[6:8])
	if !ok1 || !ok2 || !ok3 || w.month < 1 || w.month > 12 || w.day < 1 || w.day > 31 {
		return wallClock{}, false, false, false
	}
	if len(value) == 8 {
		return w, true, false, true
	}
	if len(value) < 13 || (value[8] != 'T' && value[8] != 't') {
		return wallClock{}, false, false, false
	}
	var ok4, ok5 bool
	w.hour, ok1 = num(value[9:11])
	w.minute, ok4 = num(value[11:13])
	ok5 = true
	if len(value) >= 15 {
		w.second, ok5 = num(value[13:15])
	}
	if !ok1 || !ok4 || !ok5 || w.hour > 24 || w.minute > 59 || w.second > 60 {
		return wallClock{}, false, false, false
	}
	return w, false, utc, true
}

func readDateTime(prop property, zones map[string]*vtimezone) (resolvedTime, bool) {
	value := prop.value
	// A list is allowed for RDATE, never for DTSTART; take its first entry.
	if comma := strings.IndexByte(value, ','); comma >= 0 {
		value = value[:comma]
	}
	wall, isDate, utc, ok := parseWallClock(value)
	if !ok {
		return resolvedTime{}, false
	}
	if isDate || strings.EqualFold(prop.param("VALUE"), "DATE") {
		return resolvedTime{at: time.Date(wall.year, time.Month(wall.month), wall.day, 0, 0, 0, 0, time.UTC), date: true}, true
	}
	if utc {
		return resolvedTime{at: wall.utc()}, true
	}
	tzid := strings.TrimSpace(prop.param("TZID"))
	if tzid == "" {
		return resolvedTime{at: wall.utc(), floating: true}, true
	}
	if loc, name := loadZone(tzid); loc != nil {
		at := time.Date(wall.year, time.Month(wall.month), wall.day, wall.hour, wall.minute, wall.second, 0, loc)
		return resolvedTime{at: at.UTC(), zone: name}, true
	}
	if zone := zones[tzid]; zone != nil {
		return resolvedTime{at: wall.utc().Add(-zone.offsetAt(wall))}, true
	}
	// A zone nobody can resolve is read as wall-clock time: the hour the
	// sender wrote is right for everyone in their zone, which is most readers.
	return resolvedTime{at: wall.utc(), floating: true}, true
}

// loadZone resolves a TZID to a location. Besides IANA names it reads the
// prefixed forms some producers write ("/mozilla.org/20050126_1/Europe/Berlin")
// and the Windows names Outlook and Exchange write ("W. Europe Standard Time").
func loadZone(tzid string) (*time.Location, string) {
	tzid = strings.Trim(strings.TrimSpace(tzid), `"`)
	if tzid == "" {
		return nil, ""
	}
	try := func(name string) (*time.Location, string) {
		if name == "" || strings.EqualFold(name, "local") {
			return nil, ""
		}
		loc, err := time.LoadLocation(name)
		if err != nil {
			return nil, ""
		}
		return loc, name
	}
	if loc, name := try(tzid); loc != nil {
		return loc, name
	}
	if iana, ok := windowsZones[strings.ToLower(tzid)]; ok {
		if loc, name := try(iana); loc != nil {
			return loc, name
		}
	}
	parts := strings.Split(strings.Trim(tzid, "/"), "/")
	for i := 1; i < len(parts); i++ {
		if loc, name := try(strings.Join(parts[i:], "/")); loc != nil {
			return loc, name
		}
	}
	return nil, ""
}

// windowsZones maps the Windows zone names Outlook and Exchange put in TZID to
// IANA names. The object's own VTIMEZONE is the fallback for anything missing.
var windowsZones = map[string]string{
	"utc":                            "UTC",
	"gmt standard time":              "Europe/London",
	"greenwich standard time":        "Atlantic/Reykjavik",
	"w. europe standard time":        "Europe/Berlin",
	"central europe standard time":   "Europe/Budapest",
	"central european standard time": "Europe/Warsaw",
	"romance standard time":          "Europe/Paris",
	"e. europe standard time":        "Europe/Chisinau",
	"fle standard time":              "Europe/Kiev",
	"gtb standard time":              "Europe/Bucharest",
	"turkey standard time":           "Europe/Istanbul",
	"russian standard time":          "Europe/Moscow",
	"israel standard time":           "Asia/Jerusalem",
	"south africa standard time":     "Africa/Johannesburg",
	"arabian standard time":          "Asia/Dubai",
	"india standard time":            "Asia/Kolkata",
	"china standard time":            "Asia/Shanghai",
	"singapore standard time":        "Asia/Singapore",
	"tokyo standard time":            "Asia/Tokyo",
	"korea standard time":            "Asia/Seoul",
	"aus eastern standard time":      "Australia/Sydney",
	"new zealand standard time":      "Pacific/Auckland",
	"eastern standard time":          "America/New_York",
	"central standard time":          "America/Chicago",
	"mountain standard time":         "America/Denver",
	"us mountain standard time":      "America/Phoenix",
	"pacific standard time":          "America/Los_Angeles",
	"alaskan standard time":          "America/Anchorage",
	"hawaiian standard time":         "Pacific/Honolulu",
	"atlantic standard time":         "America/Halifax",
	"e. south america standard time": "America/Sao_Paulo",
}

type duration struct {
	negative bool
	days     int
	seconds  int
}

// parseDuration reads a DURATION value such as P1D, PT1H30M or P2W.
func parseDuration(value string) (duration, bool) {
	value = strings.ToUpper(strings.TrimSpace(value))
	var d duration
	if strings.HasPrefix(value, "-") {
		d.negative = true
		value = value[1:]
	}
	value = strings.TrimPrefix(value, "+")
	if !strings.HasPrefix(value, "P") || len(value) < 3 {
		return duration{}, false
	}
	value = value[1:]
	inTime := false
	num := 0
	digits := false
	for _, r := range value {
		switch {
		case r >= '0' && r <= '9':
			num = num*10 + int(r-'0')
			digits = true
			if num > 1_000_000 {
				return duration{}, false
			}
		case r == 'T':
			inTime = true
		case digits && r == 'W' && !inTime:
			d.days += 7 * num
		case digits && r == 'D' && !inTime:
			d.days += num
		case digits && r == 'H' && inTime:
			d.seconds += 3600 * num
		case digits && r == 'M' && inTime:
			d.seconds += 60 * num
		case digits && r == 'S' && inTime:
			d.seconds += num
		default:
			return duration{}, false
		}
		if r != 'T' && (r < '0' || r > '9') {
			num, digits = 0, false
		}
	}
	return d, !digits
}

func addDuration(start time.Time, d duration, allDay bool) time.Time {
	if d.negative {
		return time.Time{}
	}
	end := start.AddDate(0, 0, d.days).Add(time.Duration(d.seconds) * time.Second)
	if allDay {
		end = end.Truncate(24 * time.Hour)
	}
	return end
}
