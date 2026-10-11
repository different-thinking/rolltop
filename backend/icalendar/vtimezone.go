// File overview: The object's own zone definitions (VTIMEZONE), the fallback
// for a TZID this process cannot load by name -- Outlook's custom zones, or a
// Windows name the table does not know.

package icalendar

import (
	"strconv"
	"strings"
	"time"
)

// vtimezone is one VTIMEZONE: the offsets it switches between and when.
type vtimezone struct {
	id    string
	rules []tzRule
}

// tzRule is one STANDARD or DAYLIGHT block. Only the yearly rules every
// producer actually writes are read -- a month and an nth weekday -- and a
// block without a rule applies from its DTSTART on.
type tzRule struct {
	start      wallClock
	offsetFrom time.Duration
	offsetTo   time.Duration
	month      int
	weekday    time.Weekday
	// nth is the occurrence of weekday in the month, negative from the end;
	// zero when the rule names no weekday.
	nth    int
	day    int
	until  time.Time
	yearly bool
}

func readTimezone(c *component) *vtimezone {
	tzid := strings.TrimSpace(firstValue(c, "TZID"))
	if tzid == "" {
		return nil
	}
	zone := &vtimezone{id: tzid}
	for _, child := range c.children {
		if child.name != "STANDARD" && child.name != "DAYLIGHT" {
			continue
		}
		rule, ok := readTZRule(child)
		if ok {
			zone.rules = append(zone.rules, rule)
		}
	}
	if len(zone.rules) == 0 {
		return nil
	}
	return zone
}

func readTZRule(c *component) (tzRule, bool) {
	to, ok := parseOffset(firstValue(c, "TZOFFSETTO"))
	if !ok {
		return tzRule{}, false
	}
	from, ok := parseOffset(firstValue(c, "TZOFFSETFROM"))
	if !ok {
		from = to
	}
	rule := tzRule{offsetFrom: from, offsetTo: to}
	if start, _, _, ok := parseWallClock(firstValue(c, "DTSTART")); ok {
		rule.start = start
	} else {
		rule.start = wallClock{year: 1601, month: 1, day: 1}
	}
	rrule, hasRule := c.first("RRULE")
	if !hasRule {
		return rule, true
	}
	parts := map[string]string{}
	for _, part := range strings.Split(rrule.value, ";") {
		key, value, ok := strings.Cut(part, "=")
		if ok {
			parts[strings.ToUpper(strings.TrimSpace(key))] = strings.ToUpper(strings.TrimSpace(value))
		}
	}
	if parts["FREQ"] != "YEARLY" {
		return rule, true
	}
	rule.yearly = true
	rule.month = rule.start.month
	if month, err := strconv.Atoi(parts["BYMONTH"]); err == nil && month >= 1 && month <= 12 {
		rule.month = month
	}
	rule.day = rule.start.day
	if byday := parts["BYDAY"]; byday != "" {
		weekday, nth, ok := parseByDay(byday)
		if !ok {
			return tzRule{}, false
		}
		rule.weekday, rule.nth = weekday, nth
		if rule.nth == 0 {
			// "BYDAY=SU;BYMONTHDAY=8,9,...,14" is the old way of saying 2SU.
			if monthday, err := strconv.Atoi(strings.Split(parts["BYMONTHDAY"], ",")[0]); err == nil && monthday > 0 {
				rule.nth = (monthday-1)/7 + 1
			} else {
				rule.nth = 1
			}
		}
	}
	if until := parts["UNTIL"]; until != "" {
		if wall, _, _, ok := parseWallClock(until); ok {
			rule.until = wall.utc()
		}
	}
	return rule, true
}

var weekdayCodes = map[string]time.Weekday{
	"SU": time.Sunday, "MO": time.Monday, "TU": time.Tuesday, "WE": time.Wednesday,
	"TH": time.Thursday, "FR": time.Friday, "SA": time.Saturday,
}

func parseByDay(value string) (time.Weekday, int, bool) {
	value = strings.Split(value, ",")[0]
	if len(value) < 2 {
		return 0, 0, false
	}
	weekday, ok := weekdayCodes[value[len(value)-2:]]
	if !ok {
		return 0, 0, false
	}
	prefix := value[:len(value)-2]
	if prefix == "" {
		return weekday, 0, true
	}
	nth, err := strconv.Atoi(prefix)
	if err != nil || nth == 0 || nth < -5 || nth > 5 {
		return 0, 0, false
	}
	return weekday, nth, true
}

// parseOffset reads a UTC offset such as +0100 or -043000.
func parseOffset(value string) (time.Duration, bool) {
	value = strings.TrimSpace(value)
	if len(value) != 5 && len(value) != 7 {
		return 0, false
	}
	sign := time.Duration(1)
	switch value[0] {
	case '-':
		sign = -1
	case '+':
	default:
		return 0, false
	}
	hours, err1 := strconv.Atoi(value[1:3])
	minutes, err2 := strconv.Atoi(value[3:5])
	seconds := 0
	var err3 error
	if len(value) == 7 {
		seconds, err3 = strconv.Atoi(value[5:7])
	}
	if err1 != nil || err2 != nil || err3 != nil {
		return 0, false
	}
	return sign * (time.Duration(hours)*time.Hour + time.Duration(minutes)*time.Minute + time.Duration(seconds)*time.Second), true
}

// onsetIn is the wall-clock moment the rule takes effect in year, read on a
// UTC clock so moments compare; ok is false when the rule does not apply then.
func (r tzRule) onsetIn(year int) (time.Time, bool) {
	start := r.start.utc()
	if !r.yearly {
		return start, year >= r.start.year
	}
	if year < r.start.year {
		return time.Time{}, false
	}
	var day int
	if r.nth == 0 {
		day = r.day
	} else {
		day = nthWeekday(year, r.month, r.weekday, r.nth)
	}
	onset := time.Date(year, time.Month(r.month), day, r.start.hour, r.start.minute, r.start.second, 0, time.UTC)
	if onset.Before(start) {
		return time.Time{}, false
	}
	if !r.until.IsZero() && onset.Add(-r.offsetFrom).After(r.until) {
		return time.Time{}, false
	}
	return onset, true
}

func nthWeekday(year, month int, weekday time.Weekday, nth int) int {
	if nth > 0 {
		first := time.Date(year, time.Month(month), 1, 0, 0, 0, 0, time.UTC)
		shift := (int(weekday) - int(first.Weekday()) + 7) % 7
		day := 1 + shift + 7*(nth-1)
		last := time.Date(year, time.Month(month)+1, 0, 0, 0, 0, 0, time.UTC).Day()
		for day > last {
			day -= 7
		}
		return day
	}
	last := time.Date(year, time.Month(month)+1, 0, 0, 0, 0, 0, time.UTC)
	shift := (int(last.Weekday()) - int(weekday) + 7) % 7
	day := last.Day() - shift - 7*(-nth-1)
	for day < 1 {
		day += 7
	}
	return day
}

// offsetAt is the zone's UTC offset at a wall-clock time: that of the rule
// whose latest onset at or before it is the latest of all. The hour a
// transition skips or repeats is read with the offset that follows it, which
// is as good an answer as either for a time that does not exist once.
func (z *vtimezone) offsetAt(wall wallClock) time.Duration {
	at := wall.utc()
	var best tzRule
	var bestOnset time.Time
	found := false
	for _, rule := range z.rules {
		for _, year := range []int{wall.year, wall.year - 1} {
			onset, ok := rule.onsetIn(year)
			if !ok || onset.After(at) {
				continue
			}
			if !found || onset.After(bestOnset) {
				best, bestOnset, found = rule, onset, true
			}
			break
		}
	}
	if found {
		return best.offsetTo
	}
	// Before every rule: the offset the earliest one switched away from.
	earliest := z.rules[0]
	for _, rule := range z.rules[1:] {
		if rule.start.utc().Before(earliest.start.utc()) {
			earliest = rule
		}
	}
	return earliest.offsetFrom
}
