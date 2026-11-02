// File overview: Deutsche Bahn tickets and seat reservations, read from the PDF.
//
// DB sends its tickets as PDFs, and the part worth reading is a fixed table:
//
//	Halt              Datum   Zeit      Gleis  Produkte   Reservierung
//	Berlin Hbf        14.11.  ab 08:29  7      ICE 1001   Wg. 25, Pl. 45, 1 Fenster
//	München Hbf       14.11.  an 12:32  21
//
// One table per direction, one departure line and one arrival line per train,
// so the way there, the way back and every change between are all on the page.
// The table is read from the layout text (pdftotext -layout), because the
// whitespace-collapsed text the search index keeps cannot tell where a station
// name ends and the date begins.
//
// Two things must be true before a line counts: the document has the table's
// header, and it is DB's -- named in the text or sent from DB's own domains.
// Times in the table are the stations' local times, which for the trains DB
// sells is Europe/Berlin's offset; that is the operator's clock, not a guess
// about the reader.

package mailparse

import (
	"regexp"
	"strconv"
	"strings"
	"time"
)

var (
	dbStopLineRE  = regexp.MustCompile(`^\s*(\S.*?)\s+(\d{1,2})\.(\d{1,2})\.(\d{2,4})?\s+(ab|an)\s+(\d{1,2}):(\d{2})\b(.*)$`)
	dbProductRE   = regexp.MustCompile(`\b(ICE|IC|EC|ECE|EN|NJ|RJX|RJ|TGV|THA|EST|FLX|IRE|RE|RB|MEX|ALX|ERB|BRB|VIA|HLB|NWB|S|U|STR|Bus)\s?(\d{1,5})\b`)
	dbSeatRE      = regexp.MustCompile(`Wg\.?\s*(\d{1,3})\s*,?\s*Pl\.?\s*([0-9][0-9 ,]*)`)
	dbYearRE      = regexp.MustCompile(`\b(\d{1,2})\.(\d{1,2})\.(20\d{2})\b`)
	dbReferenceRE = regexp.MustCompile(`(?i)\b(?:Auftragsnummer|Auftrag|Buchungsnummer|Ticketnummer|Ticket-Nr\.?)\s*:?\s*([A-Z0-9]{6,14})\b`)
)

// berlin is the zone DB's timetable is printed in. It is loaded once; tzdata is
// embedded, so it is there whatever the server image carries.
var berlin = func() *time.Location {
	loc, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		return nil
	}
	return loc
}()

func travelFromDBPDF(data []byte, from string, sent time.Time) (TravelBooking, bool) {
	text, err := pdfTextExtractor(data)
	if err != nil || text == "" {
		return TravelBooking{}, false
	}
	return travelFromDBText(text, from, sent)
}

// travelFromDBText reads the itinerary table out of a DB ticket's layout text.
func travelFromDBText(text, from string, sent time.Time) (TravelBooking, bool) {
	lower := strings.ToLower(text)
	if !dbSender(from) && !strings.Contains(lower, "deutsche bahn") && !strings.Contains(lower, "db fernverkehr") && !strings.Contains(lower, "bahn.de") {
		return TravelBooking{}, false
	}
	lines := strings.Split(strings.ReplaceAll(text, "\r", ""), "\n")
	header := -1
	for i, line := range lines {
		l := strings.ToLower(line)
		if strings.Contains(l, "halt") && strings.Contains(l, "datum") && strings.Contains(l, "zeit") {
			header = i
			break
		}
	}
	if header < 0 {
		return TravelBooking{}, false
	}
	stated := dbStatedYears(text)
	year := 0
	seatOnly := strings.Contains(lower, "reservierung") && !containsAnyWord(lower,
		"fahrkarte", "online-ticket", "flexpreis", "sparpreis", "supersparpreis", "bahncard 100", "deutschland-ticket")

	var segments []TravelSegment
	var open *TravelSegment
	lastMonth := 0
	for _, line := range lines[header+1:] {
		match := dbStopLineRE.FindStringSubmatch(line)
		if match == nil {
			continue
		}
		station := strings.TrimSpace(match[1])
		day, _ := strconv.Atoi(match[2])
		month, _ := strconv.Atoi(match[3])
		hour, _ := strconv.Atoi(match[6])
		minute, _ := strconv.Atoi(match[7])
		lineYear := year
		if y, ok := stated[[2]int{day, month}]; ok && match[4] == "" {
			// The ticket names this very day with its year elsewhere
			// ("Hinfahrt am 14.11.2026"); that is the year, whatever other
			// dates -- the booking's, the invoice's -- the page also carries.
			lineYear = y
		} else if match[4] != "" {
			if y, err := strconv.Atoi(match[4]); err == nil {
				if y < 100 {
					y += 2000
				}
				lineYear = y
			}
		} else if year != 0 && lastMonth == 12 && month == 1 {
			// A journey over New Year prints no year either side of midnight.
			lineYear = year + 1
		} else if year == 0 {
			lineYear = dbFallbackYear(day, month, sent)
		}
		year = lineYear
		lastMonth = month
		if month < 1 || month > 12 || day < 1 || day > 31 || hour > 23 || minute > 59 {
			continue
		}
		at := time.Date(lineYear, time.Month(month), day, hour, minute, 0, 0, time.UTC)
		local := at.Format(travelLocalLayout)
		var instant time.Time
		if berlin != nil {
			instant = time.Date(lineYear, time.Month(month), day, hour, minute, 0, 0, berlin)
		}
		rest := match[8]
		if match[5] == "ab" {
			product := dbProductRE.FindStringSubmatch(rest)
			if product == nil {
				open = nil
				continue
			}
			seg := TravelSegment{
				Kind:     TravelTrain,
				Number:   product[1] + " " + product[2],
				Provider: "Deutsche Bahn",
				From:     station,
				Start:    local,
				StartAt:  instant,
				Seat:     dbSeat(rest),
				SeatOnly: seatOnly,
				Source:   TravelSourcePDF,
			}
			segments = append(segments, seg)
			open = &segments[len(segments)-1]
			continue
		}
		if open != nil && open.To == "" {
			open.To = station
			open.End = local
			open.EndAt = instant
		}
		open = nil
	}
	kept := segments[:0]
	for _, seg := range segments {
		if seg.To != "" {
			kept = append(kept, seg)
		}
	}
	if len(kept) == 0 {
		return TravelBooking{}, false
	}
	reference := ""
	if match := dbReferenceRE.FindStringSubmatch(text); match != nil {
		reference = match[1]
	}
	return TravelBooking{Kind: TravelTrain, Reference: reference, Provider: "Deutsche Bahn", Segments: kept}, true
}

// dbStatedYears collects the days the ticket writes out with their year, keyed
// by day and month. The table prints day and month only; the ticket states the
// journey's dates in full elsewhere ("Hinfahrt am 14.11.2026", "Gültigkeit: ab
// 14.11.2026"). Matching by day is what keeps the booking date or an invoice
// date -- which may fall in the previous year -- from deciding the journey's.
func dbStatedYears(text string) map[[2]int]int {
	out := map[[2]int]int{}
	for _, match := range dbYearRE.FindAllStringSubmatch(text, 64) {
		day, _ := strconv.Atoi(match[1])
		month, _ := strconv.Atoi(match[2])
		year, err := strconv.Atoi(match[3])
		if err != nil {
			continue
		}
		key := [2]int{day, month}
		if _, ok := out[key]; !ok {
			out[key] = year
		}
	}
	return out
}

// dbFallbackYear is the year for a day the ticket never writes out in full:
// the one that puts the journey nearest after the mail was sent. A ticket is
// sent before the journey, so a day well before the mail's own belongs to the
// following year.
func dbFallbackYear(day, month int, sent time.Time) int {
	if sent.IsZero() {
		sent = time.Now()
	}
	year := sent.Year()
	if time.Date(year, time.Month(month), day, 0, 0, 0, 0, time.UTC).Before(sent.AddDate(0, 0, -60)) {
		year++
	}
	return year
}

// dbSeat spells a DB reservation the way a reader says it. "Wg. 25, Pl. 45, 1
// Fenster" is coach 25, seat 45 -- the "1" is the count of seats by the window,
// not a second seat, which is why the list of seat numbers stops at the first
// number followed by a word.
func dbSeat(rest string) string {
	match := dbSeatRE.FindStringSubmatchIndex(rest)
	if match == nil {
		return ""
	}
	coach := rest[match[2]:match[3]]
	tail := rest[match[4]:]
	var seats []string
	for _, piece := range strings.Split(tail, ",") {
		token := strings.TrimSpace(piece)
		if token == "" {
			break
		}
		fields := strings.Fields(token)
		if len(fields) == 0 || !allDigits(fields[0]) {
			break
		}
		if len(fields) > 1 && len(seats) > 0 {
			// "1 Fenster": a count and a word end the list.
			break
		}
		seats = append(seats, fields[0])
		if len(fields) > 1 {
			break
		}
	}
	if len(seats) == 0 {
		return "Coach " + coach
	}
	label := "seat "
	if len(seats) > 1 {
		label = "seats "
	}
	return "Coach " + coach + ", " + label + strings.Join(seats, ", ")
}

func allDigits(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func containsAnyWord(haystack string, words ...string) bool {
	for _, word := range words {
		if strings.Contains(haystack, word) {
			return true
		}
	}
	return false
}
