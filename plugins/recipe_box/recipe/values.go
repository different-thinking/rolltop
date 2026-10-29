// File overview: Durations and URLs as recipe pages write them.

package recipe

import (
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

var (
	isoDurationRE = regexp.MustCompile(`(?i)^P(?:(\d+(?:[.,]\d+)?)Y)?(?:(\d+(?:[.,]\d+)?)M)?(?:(\d+(?:[.,]\d+)?)W)?(?:(\d+(?:[.,]\d+)?)D)?(?:T(?:(\d+(?:[.,]\d+)?)H)?(?:(\d+(?:[.,]\d+)?)M)?(?:(\d+(?:[.,]\d+)?)S)?)?$`)
	// Some plugins write what a person would: "1 Std. 20 Min.", "45 min".
	looseHoursRE   = regexp.MustCompile(`(?i)(\d+(?:[.,]\d+)?)\s*(h|hr|hrs|hour|hours|std|stunde|stunden)\b`)
	looseMinutesRE = regexp.MustCompile(`(?i)(\d+)\s*(m|min|mins|minute|minutes|minuten)\b`)
)

// durationMinutes reads an ISO 8601 duration ("PT1H30M", "P0DT0H45M") into
// whole minutes, falling back to the spelled-out forms some sites publish
// instead. Anything it cannot read is zero, which the card prints as no time
// rather than as a wrong one. A duration of more than four days is not a
// cooking time -- it is a site that wrote seconds where it meant minutes --
// and is dropped for the same reason.
func durationMinutes(value string) int {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	minutes := 0.0
	if match := isoDurationRE.FindStringSubmatch(value); match != nil {
		parts := []float64{0, 0, 0, 0, 0, 0, 0}
		for i := range parts {
			parts[i] = parseNumber(match[i+1])
		}
		// Years and months are never a cooking time; a page that wrote them
		// wrote something else.
		if parts[0] > 0 || parts[1] > 0 {
			return 0
		}
		minutes = parts[2]*7*24*60 + parts[3]*24*60 + parts[4]*60 + parts[5] + parts[6]/60
	} else {
		for _, match := range looseHoursRE.FindAllStringSubmatch(value, -1) {
			minutes += parseNumber(match[1]) * 60
		}
		for _, match := range looseMinutesRE.FindAllStringSubmatch(value, -1) {
			minutes += parseNumber(match[1])
		}
		if minutes == 0 {
			if n, err := strconv.Atoi(value); err == nil {
				minutes = float64(n)
			}
		}
	}
	if minutes <= 0 || minutes > 4*24*60 {
		return 0
	}
	return int(minutes + 0.5)
}

func parseNumber(value string) float64 {
	if value == "" {
		return 0
	}
	f, err := strconv.ParseFloat(strings.ReplaceAll(value, ",", "."), 64)
	if err != nil {
		return 0
	}
	return f
}

// resolveURL makes a page's reference absolute against the page and keeps it
// only when it is http(s). A protocol-relative image ("//cdn...") is the
// ordinary case on recipe sites.
func resolveURL(base, ref string) string {
	ref = strings.TrimSpace(ref)
	if ref == "" || strings.HasPrefix(strings.ToLower(ref), "data:") {
		return ""
	}
	refURL, err := url.Parse(ref)
	if err != nil {
		return ""
	}
	if base != "" {
		if baseURL, err := url.Parse(base); err == nil {
			refURL = baseURL.ResolveReference(refURL)
		}
	}
	scheme := strings.ToLower(refURL.Scheme)
	if (scheme != "http" && scheme != "https") || refURL.Host == "" {
		return ""
	}
	refURL.Fragment = ""
	return refURL.String()
}
