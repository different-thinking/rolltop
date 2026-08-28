// File overview: Reading a calendar day out of prose, in every spelling mail
// actually uses -- German and English, numeric and written, absolute and
// relative to the day the message was sent.
//
// This sits on its own rather than inside the feature that first needed it
// because a parcel and an invoice state their day the same way. A carrier
// writes "voraussichtliche Zustellung: Do., 04.09." and a biller writes
// "zahlbar bis 04.09."; the anchor words differ and belong to the feature, the
// calendar does not. What is shared is everything from the anchor outwards:
// which spellings count as a date, how a year nobody wrote is chosen, and which
// of the many dates in a message is close enough to the message to be its own.
//
// Nothing here decides *whether* a date is the one being looked for. That is
// the caller's job, and it does it by only ever handing over a window of text
// it has already cut around a word that says what the date is for. Mail is full
// of dates -- the order date, a footer's copyright year, a price that reads
// like one -- and without that gate every message would yield one.

package mailparse

import (
	"regexp"
	"strconv"
	"strings"
	"time"
)

// dateBounds is how far from the message a date may stand and still be the one
// the caller is looking for, counted in days. Both are needed and neither has a
// sensible default: a delivery is announced for the days ahead, while an
// invoice's due date is routinely months behind by the time a dunning letter
// repeats it.
type dateBounds struct {
	// past is negative for a date before the message.
	past int
	// future is positive for a date after it.
	future int
}

// maxDateCandidates bounds how many matches of one spelling are checked inside
// a single window. A window is a sentence or two; past a handful of candidates
// it is a table of numbers rather than a sentence about a day.
const maxDateCandidates = 4

var (
	// germanDateRE is "04.09.2026", "4.9.26" and "04.09." -- the spelling every
	// German sender uses. The year is optional because the day and month alone
	// are what a message about next week says.
	germanDateRE = regexp.MustCompile(`\b(\d{1,2})\.\s*(\d{1,2})\.(?:\s*(\d{4}|\d{2})\b)?`)
	// isoDateRE is the machine spelling, which turns up in mail generated from
	// an order or billing system rather than written for a reader.
	isoDateRE = regexp.MustCompile(`\b(\d{4})-(\d{1,2})-(\d{1,2})\b`)
	// dayMonthNameRE is "4. September" and "4 September 2026".
	dayMonthNameRE = regexp.MustCompile(`(?i)\b(\d{1,2})\.?\s*(` + monthNamePattern + `)\.?(?:\s*(\d{4})\b)?`)
	// monthNameDayRE is the English order, "September 4" and "Sep 4, 2026".
	monthNameDayRE = regexp.MustCompile(`(?i)\b(` + monthNamePattern + `)\.?\s+(\d{1,2})(?:st|nd|rd|th)?(?:,?\s*(\d{4})\b)?`)
	// relativeDayRE is what a message written on or beside the day itself says.
	relativeDayRE = regexp.MustCompile(`(?i)\b(heute|today|übermorgen|uebermorgen|morgen|tomorrow)\b`)
	// weekdayRE is "am Donnerstag", which a sender writes for anything inside
	// the coming week.
	weekdayRE = regexp.MustCompile(`(?i)\b(montag|dienstag|mittwoch|donnerstag|freitag|samstag|sonnabend|sonntag|monday|tuesday|wednesday|thursday|friday|saturday|sunday)\b`)
)

// monthNames maps every spelling of a month this package accepts, German and
// English, long and abbreviated, to its number.
var monthNames = map[string]time.Month{
	"januar": time.January, "january": time.January, "jan": time.January,
	"februar": time.February, "february": time.February, "feb": time.February,
	"märz": time.March, "maerz": time.March, "march": time.March, "mar": time.March, "mrz": time.March,
	"april": time.April, "apr": time.April,
	"mai": time.May, "may": time.May,
	"juni": time.June, "june": time.June, "jun": time.June,
	"juli": time.July, "july": time.July, "jul": time.July,
	"august": time.August, "aug": time.August,
	"september": time.September, "sept": time.September, "sep": time.September,
	"oktober": time.October, "october": time.October, "okt": time.October, "oct": time.October,
	"november": time.November, "nov": time.November,
	"dezember": time.December, "december": time.December, "dez": time.December, "dec": time.December,
}

// monthNamePattern is the alternation the two month-name expressions share.
// Longest first, so "september" is not matched as "sep" with a stray "tember".
var monthNamePattern = buildMonthNamePattern()

func buildMonthNamePattern() string {
	names := make([]string, 0, len(monthNames))
	for name := range monthNames {
		names = append(names, name)
	}
	sortByLengthDesc(names)
	for i, name := range names {
		names[i] = regexp.QuoteMeta(name)
	}
	return strings.Join(names, "|")
}

func sortByLengthDesc(values []string) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0; j-- {
			if len(values[j]) > len(values[j-1]) || (len(values[j]) == len(values[j-1]) && values[j] < values[j-1]) {
				values[j], values[j-1] = values[j-1], values[j]
				continue
			}
			break
		}
	}
}

// weekdayNames maps both languages' weekday spellings to the day they name.
var weekdayNames = map[string]time.Weekday{
	"montag": time.Monday, "monday": time.Monday,
	"dienstag": time.Tuesday, "tuesday": time.Tuesday,
	"mittwoch": time.Wednesday, "wednesday": time.Wednesday,
	"donnerstag": time.Thursday, "thursday": time.Thursday,
	"freitag": time.Friday, "friday": time.Friday,
	"samstag": time.Saturday, "sonnabend": time.Saturday, "saturday": time.Saturday,
	"sonntag": time.Sunday, "sunday": time.Sunday,
}

// startOfDay is the message's own calendar day, in the zone it was written in.
// Every relative spelling below resolves against it, and a message written just
// after midnight in another zone resolves to the wrong day once that offset has
// been folded away -- which is why callers hold on to the sent time with its
// zone rather than the UTC instant they store.
func startOfDay(sent time.Time) time.Time {
	year, month, day := sent.Date()
	return time.Date(year, month, day, 0, 0, 0, 0, sent.Location())
}

// findDateNear tries every spelling on one window of text, in the order that
// puts the least ambiguous first.
//
// Every spelling is tried, and every match of each is tried, until one yields a
// date that survives plausibleDate. A match is not an answer: a reference
// number reads as "12.34.56" and a price as "1.234,56", both of which the
// German expression matches and the calendar then rejects. Stopping at the
// first *match* rather than the first *answer* loses the real date standing
// beside it.
func findDateNear(window string, sentDay time.Time, bounds dateBounds) (string, bool) {
	if window == "" {
		return "", false
	}
	for _, match := range isoDateRE.FindAllStringSubmatch(window, maxDateCandidates) {
		if date, ok := plausibleDate(atoi(match[1]), time.Month(atoi(match[2])), atoi(match[3]), sentDay, bounds); ok {
			return date, true
		}
	}
	for _, match := range germanDateRE.FindAllStringSubmatch(window, maxDateCandidates) {
		if date, ok := dateWithOptionalYear(atoi(match[1]), time.Month(atoi(match[2])), match[3], sentDay, bounds); ok {
			return date, true
		}
	}
	for _, match := range dayMonthNameRE.FindAllStringSubmatch(window, maxDateCandidates) {
		month, ok := monthNames[strings.ToLower(match[2])]
		if !ok {
			continue
		}
		if date, ok := dateWithOptionalYear(atoi(match[1]), month, match[3], sentDay, bounds); ok {
			return date, true
		}
	}
	for _, match := range monthNameDayRE.FindAllStringSubmatch(window, maxDateCandidates) {
		month, ok := monthNames[strings.ToLower(match[1])]
		if !ok {
			continue
		}
		if date, ok := dateWithOptionalYear(atoi(match[2]), month, match[3], sentDay, bounds); ok {
			return date, true
		}
	}
	if match := relativeDayRE.FindStringSubmatch(window); match != nil {
		switch strings.ToLower(match[1]) {
		case "heute", "today":
			return plainDate(sentDay), true
		case "morgen", "tomorrow":
			return plainDate(sentDay.AddDate(0, 0, 1)), true
		case "übermorgen", "uebermorgen":
			return plainDate(sentDay.AddDate(0, 0, 2)), true
		}
	}
	if match := weekdayRE.FindStringSubmatch(window); match != nil {
		if weekday, ok := weekdayNames[strings.ToLower(match[1])]; ok {
			return plainDate(nextWeekday(sentDay, weekday)), true
		}
	}
	return "", false
}

// dateWithOptionalYear settles a day and month that may or may not have said
// which year they are in. A sender writing "04.01." on the 29th of December
// means the January nine days away, not the one eleven months back, so the year
// is chosen as the one that puts the date nearest the message.
func dateWithOptionalYear(day int, month time.Month, yearText string, sentDay time.Time, bounds dateBounds) (string, bool) {
	if yearText != "" {
		year := atoi(yearText)
		if year < 100 {
			year += 2000
		}
		return plausibleDate(year, month, day, sentDay, bounds)
	}
	for _, year := range []int{sentDay.Year(), sentDay.Year() + 1, sentDay.Year() - 1} {
		if date, ok := plausibleDate(year, month, day, sentDay, bounds); ok {
			return date, true
		}
	}
	return "", false
}

// plausibleDate rejects what the calendar does not hold and what is too far
// from the message to be the day the caller is after. The round-trip through
// time.Date is what catches "31.02.": normalization would silently make it the
// third of March.
func plausibleDate(year int, month time.Month, day int, sentDay time.Time, bounds dateBounds) (string, bool) {
	if year < 1970 || month < time.January || month > time.December || day < 1 || day > 31 {
		return "", false
	}
	date := time.Date(year, month, day, 0, 0, 0, 0, sentDay.Location())
	if date.Year() != year || date.Month() != month || date.Day() != day {
		return "", false
	}
	distance := int(date.Sub(sentDay).Hours() / 24)
	if distance < bounds.past || distance > bounds.future {
		return "", false
	}
	return plainDate(date), true
}

// nextWeekday is the named day on or after the message's own day. "Donnerstag"
// in a mail written on a Thursday is that Thursday, which is how a reader reads
// it too.
func nextWeekday(sentDay time.Time, weekday time.Weekday) time.Time {
	shift := (int(weekday) - int(sentDay.Weekday()) + 7) % 7
	return sentDay.AddDate(0, 0, shift)
}

// windowAfter and windowBefore cut the text around an anchor a caller found.
// They are byte windows rather than sentence ones on purpose: a sentence
// boundary is not a thing mail reliably has, and a fixed number of bytes is
// what keeps the amount of text every rule reads bounded.
func windowAfter(text string, from, size int) string {
	if from >= len(text) {
		return ""
	}
	to := from + size
	if to > len(text) {
		to = len(text)
	}
	return text[from:to]
}

func windowBefore(text string, to, size int) string {
	if to <= 0 {
		return ""
	}
	from := to - size
	if from < 0 {
		from = 0
	}
	return text[from:to]
}

// plainDate is the one spelling a day is stored and compared in. It is a day
// and not an instant on purpose: what a sender announces is a calendar day, and
// storing it as a timestamp would need a timezone nobody stated.
func plainDate(date time.Time) string {
	return date.Format("2006-01-02")
}

func pad2(value int) string {
	if value < 10 {
		return "0" + strconv.Itoa(value)
	}
	return strconv.Itoa(value)
}

func atoi(value string) int {
	n, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil {
		return -1
	}
	return n
}
