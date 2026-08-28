// File overview: What a message says about a parcel on its way -- which
// carrier, which tracking number, and which day it is announced for.
//
// This is deliberately not a category. A category is one answer per message and
// it is what the reader sees in a list; a delivery is a fact about a *shipment*,
// several messages talk about the same one, and the useful view of it is the
// other way round -- one row per parcel, with the mail that mentioned it hanging
// off it. So extraction produces records, and the category a carrier mail lands
// in is untouched.
//
// Two things follow from where the numbers come from. A tracking number turns up
// in mail the carrier never sent: a shop's dispatch confirmation carries it, and
// that mail is the earliest warning a reader gets. So the sender is a hint here,
// never a gate. And a bare number is not evidence of anything -- an order number,
// an invoice number and a phone number are all digits -- so a number is only
// taken when something around it says what it is: a carrier's own tracking link,
// a label naming it, or a shape only one carrier issues.
package mailparse

import (
	"io"
	"net/mail"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Delivery statuses. They are stored values and travel to the browser, so they
// are named here once and not spelled out anywhere else.
const (
	// DeliveryAnnounced is a parcel that has been handed over or is on its way.
	DeliveryAnnounced = "announced"
	// DeliveryOutForDelivery is a parcel on the van today.
	DeliveryOutForDelivery = "out_for_delivery"
	// DeliveryDelivered is a parcel that has arrived.
	DeliveryDelivered = "delivered"
)

// maxDeliveryNoticesPerMessage bounds what one message can produce. A dispatch
// confirmation for a large order names one number per parcel, which is a handful;
// a message that appears to name more than this is a listing of something else.
const maxDeliveryNoticesPerMessage = 8

// deliveryDateProximity bounds how far from the words "expected delivery" a date
// may stand and still be that delivery's date. Mail is full of dates -- the order
// date, the invoice date, a footer's copyright year -- so a date only counts when
// it is next to something that says it is the delivery's.
const deliveryDateProximity = 80

// DeliveryNotice is one shipment as one message describes it. The zero value of
// ExpectedDate is a shipment that is known but not yet dated, which is the normal
// state of a parcel between "we have your order" and "it arrives tomorrow".
type DeliveryNotice struct {
	// Carrier is a key from deliveryCarriers, or empty when a message labelled a
	// tracking number without saying whose it is.
	Carrier string
	// TrackingNumber is upper-cased with separators removed, which is what makes
	// the same parcel in a shop's mail and in the carrier's mail one row.
	TrackingNumber string
	// ExpectedDate is a plain calendar day, "2006-01-02", or empty. It is a day
	// and not an instant on purpose: what a carrier announces is a day, and
	// storing it as a timestamp would need a timezone nobody stated.
	ExpectedDate string
	// WindowStart and WindowEnd are "15:04" or empty, from the delivery windows
	// German carriers give on the morning of the day.
	WindowStart string
	WindowEnd   string
	// Status is one of the three constants above.
	Status string
}

// DeliveryCarrier is one carrier as the extractor and the browser both know it.
type DeliveryCarrier struct {
	// Key is the stored value. It is in the database and in URLs, so it is not
	// renamed without a migration.
	Key string
	// Label is what a reader is shown.
	Label string
	// senderDomains are the domains the carrier's own mail comes from. They only
	// ever answer "whose number is this", never "is this a parcel mail".
	senderDomains []string
	// linkHosts are the hosts of the carrier's tracking pages. A link to one is
	// the strongest signal there is: no other business puts it in a message.
	linkHosts []string
	// linkParams are the query keys those pages carry the number in.
	linkParams []string
	// numberShape matches a number only this carrier issues, so a message that
	// names one has said which carrier it means. Nil where the carrier's numbers
	// are plain digits that anything else could be.
	numberShape *regexp.Regexp
	// bareShape matches the digit lengths this carrier issues. Unlike
	// numberShape it says nothing on its own -- plenty of numbers are twenty
	// digits long -- so it is only read out of mail the carrier itself sent.
	bareShape *regexp.Regexp
	// trackURL builds the page a reader can follow the parcel on. Empty for a
	// carrier whose tracking page needs more than the number.
	trackURL func(number string) string
}

// deliveryCarriers is the one place a carrier is defined. Order matters only for
// link matching, where the first host that matches wins.
var deliveryCarriers = []DeliveryCarrier{
	{
		Key:           "dhl",
		Label:         "DHL",
		senderDomains: []string{"dhl.de", "dhl.com", "deutschepost.de", "dpdhl.com"},
		linkHosts:     []string{"nolp.dhl.de", "dhl.de", "dhl.com"},
		linkParams:    []string{"piececode", "idc", "tracking-id", "trackingnumber"},
		// 12, 16 or 20 digits, all of which other things are too, so a bare one
		// counts only in DHL's own mail.
		bareShape: regexp.MustCompile(`\b(?:\d{20}|\d{16}|\d{12})\b`),
		trackURL: func(number string) string {
			return "https://www.dhl.de/de/privatkunden/pakete-empfangen/verfolgen.html?piececode=" + number
		},
	},
	{
		Key:           "dpd",
		Label:         "DPD",
		senderDomains: []string{"dpd.de", "dpd.com", "dpdgroup.com"},
		linkHosts:     []string{"tracking.dpd.de", "my.dpd.de", "dpd.com", "dpd.de"},
		linkParams:    []string{"parcelno", "pknr", "query"},
		bareShape:     regexp.MustCompile(`\b\d{14}\b`),
		trackURL: func(number string) string {
			return "https://tracking.dpd.de/status/de_DE/parcel/" + number
		},
	},
	{
		Key:           "gls",
		Label:         "GLS",
		senderDomains: []string{"gls-group.eu", "gls-group.com", "gls-pakete.de", "gls-germany.de"},
		linkHosts:     []string{"gls-group.eu", "gls-group.com", "gls-pakete.de"},
		linkParams:    []string{"match", "txtaction", "trackingnumber"},
		bareShape:     regexp.MustCompile(`\b\d{11,12}\b`),
		trackURL: func(number string) string {
			return "https://gls-group.eu/DE/de/paketverfolgung?match=" + number
		},
	},
	{
		Key:           "ups",
		Label:         "UPS",
		senderDomains: []string{"ups.com"},
		linkHosts:     []string{"ups.com", "wwwapps.ups.com"},
		linkParams:    []string{"tracknum", "tracknums", "trackingnumber"},
		// The one number shape in this list that is unmistakable.
		numberShape: regexp.MustCompile(`\b1Z[0-9A-Z]{16}\b`),
		trackURL: func(number string) string {
			return "https://www.ups.com/track?tracknum=" + number
		},
	},
	{
		Key:           "fedex",
		Label:         "FedEx",
		senderDomains: []string{"fedex.com"},
		linkHosts:     []string{"fedex.com"},
		linkParams:    []string{"trknbr", "tracknumbers"},
		bareShape:     regexp.MustCompile(`\b(?:\d{20}|\d{15}|\d{12})\b`),
		trackURL: func(number string) string {
			return "https://www.fedex.com/fedextrack/?trknbr=" + number
		},
	},
	{
		Key:           "hermes",
		Label:         "Hermes",
		senderDomains: []string{"myhermes.de", "hermesworld.com", "hermes-europe.co.uk", "evri.com"},
		linkHosts:     []string{"myhermes.de", "hermesworld.com", "tracking.hermesworld.com"},
		linkParams:    []string{"trackid", "trackingid"},
		bareShape:     regexp.MustCompile(`\b\d{14}\b`),
		trackURL: func(number string) string {
			return "https://www.myhermes.de/empfangen/sendungsverfolgung/sendungsinformation/#" + number
		},
	},
	{
		Key:           "amazon",
		Label:         "Amazon",
		senderDomains: []string{"amazon.de", "amazon.com", "amazon.co.uk", "amazon.fr", "amazon.it", "amazon.es"},
		linkHosts:     []string{"amazon.de", "amazon.com", "amazon.co.uk"},
		linkParams:    []string{"trackingid", "trackingnumber"},
		// Amazon's own last-mile numbers, which no one else issues.
		numberShape: regexp.MustCompile(`\bTBA\d{9,13}\b`),
		// Amazon's tracking page is reached through the order, not the number,
		// so there is no link to build from what a message states.
	},
}

// DeliveryCarrierLabel is what a reader is shown for a stored carrier key. An
// unknown key -- a row written by a newer build, or the empty carrier of a
// labelled number nobody claimed -- reads as a parcel, which is what it is.
func DeliveryCarrierLabel(key string) string {
	for _, carrier := range deliveryCarriers {
		if carrier.Key == key {
			return carrier.Label
		}
	}
	return "Paket"
}

// DeliveryTrackingURL is the carrier's own page for one number, or empty when
// the carrier does not have one that a number alone reaches.
func DeliveryTrackingURL(carrier, number string) string {
	number = strings.TrimSpace(number)
	if number == "" {
		return ""
	}
	for _, definition := range deliveryCarriers {
		if definition.Key != carrier || definition.trackURL == nil {
			continue
		}
		return definition.trackURL(number)
	}
	return ""
}

// ValidDeliveryCarrier reports whether a key is one this build knows. Keys
// arrive from stored rows and from requests alike, and both are checked.
func ValidDeliveryCarrier(key string) bool {
	if key == "" {
		return true
	}
	for _, carrier := range deliveryCarriers {
		if carrier.Key == key {
			return true
		}
	}
	return false
}

// DeliveryLinkCandidate reports whether a URL is worth carrying out of a message
// for delivery extraction. The scan that reads stored mail keeps only these, so
// a marketing mail's hundred links cost nothing.
func DeliveryLinkCandidate(rawURL string) bool {
	return carrierForLink(rawURL) != nil
}

// ExtractDeliveryNotices reads one message for the parcels it talks about.
//
// sent is the message's own date, in the zone it was sent in: "arrives tomorrow"
// is relative to when it was written, not to when the extractor runs, which is
// what lets the backfill get the same answer out of a week-old mail that the
// fetch path got at the time.
func ExtractDeliveryNotices(content CategoryContent, from string, sent time.Time) []DeliveryNotice {
	text := deliveryText(content)
	if text == "" && len(content.DeliveryLinks) == 0 {
		return nil
	}
	// The numbers are looked for before anything else, and the day and the
	// status are only worked out once one has been found.
	//
	// That order is the whole performance story of this function. Nearly no mail
	// names a parcel, and reading a day out of a message is the expensive half:
	// it walks every delivery word in the body and tries six spellings of a date
	// in a window around each. A newsletter that says "Lieferung" four hundred
	// times pays all of that and answers "no parcel here" anyway -- 37ms per
	// message measured, enough to push a sync turn past its budget. Finding no
	// number first costs three regex passes and skips the rest.
	found := make([]DeliveryNotice, 0, 4)
	// The number alone is the identity here, not the number and the carrier.
	// One parcel is routinely found twice in one message -- once through the
	// carrier's link, which names the carrier, and once through the label in the
	// text, which does not -- and keying on both would make those two parcels.
	// They would then be two rows under the store's unique index, listed twice
	// and counted twice.
	//
	// The list is at most maxDeliveryNoticesPerMessage long, so it is scanned
	// rather than indexed: a map keyed on the number would need a second pass to
	// answer the one question that is not equality, which is what to do when two
	// different carriers are named for one number.
	add := func(carrier, number string) {
		number = normalizeTrackingNumber(number)
		if number == "" {
			return
		}
		for i := range found {
			if found[i].TrackingNumber != number {
				continue
			}
			// The half that knows the carrier wins. Two *different* named
			// carriers for one number are not the same parcel -- the numbering
			// schemes overlap -- so those stay apart and the loop keeps looking.
			if found[i].Carrier == "" {
				found[i].Carrier = carrier
				return
			}
			if carrier == "" || found[i].Carrier == carrier {
				return
			}
		}
		if len(found) >= maxDeliveryNoticesPerMessage {
			return
		}
		found = append(found, DeliveryNotice{Carrier: carrier, TrackingNumber: number})
	}

	// Links first: a carrier's own tracking URL says both things at once, and
	// says them without depending on how the sender worded anything.
	linkCarrier := ""
	for _, link := range content.DeliveryLinks {
		carrier := carrierForLink(link)
		if carrier == nil {
			continue
		}
		if linkCarrier == "" {
			linkCarrier = carrier.Key
		}
		if number := trackingNumberFromLink(link, *carrier); number != "" {
			add(carrier.Key, number)
		}
	}
	// Shapes only one carrier issues need no label around them.
	for _, carrier := range deliveryCarriers {
		if carrier.numberShape == nil {
			continue
		}
		for _, match := range carrier.numberShape.FindAllString(text, maxDeliveryNoticesPerMessage) {
			add(carrier.Key, match)
		}
	}
	// A bare number in the shape the sender's own carrier issues. This is the
	// one place an unlabelled number is taken, and the sender is what makes it
	// safe: DHL writing twenty digits is writing a parcel number.
	//
	// It exists because the carriers put their number behind a click-tracking
	// redirect and print it in the body under a heading -- "Sendungsstatus
	// einsehen" -- that is not a label. Without this rule their own "arrives
	// today" mail, which is the one the reader most wants on the list, is the
	// one message that yields nothing.
	senderCarrier := carrierForSender(from)
	if senderCarrier != "" {
		for _, carrier := range deliveryCarriers {
			if carrier.Key != senderCarrier || carrier.bareShape == nil {
				continue
			}
			for _, match := range carrier.bareShape.FindAllString(text, maxDeliveryNoticesPerMessage) {
				add(carrier.Key, match)
			}
		}
	}
	// Then labelled numbers. Whose they are is not stated beside the number, so
	// the message as a whole answers it: the carrier its links point at, the
	// carrier that sent it, or a carrier it names in words.
	labelled := labelledTrackingNumbers(text)
	if len(labelled) > 0 {
		labelCarrier := firstNonEmpty(linkCarrier, senderCarrier, carrierNamedIn(text))
		for _, number := range labelled {
			add(labelCarrier, number)
		}
	}
	if len(found) == 0 {
		return nil
	}

	// The status and the date describe the message, not each number in it: a
	// dispatch mail listing three parcels announces all three for the same day.
	status := deliveryStatus(text)
	date, windowStart, windowEnd := deliveryDate(text, sent, status)
	// A parcel announced for the day its mail was written is on the van, not
	// merely on its way. The carriers say so in words often enough, but a shop
	// forwarding the carrier's date does not, and the reader's answer to "is it
	// coming today" should not depend on which of the two wrote to them.
	if status == DeliveryAnnounced && date != "" && date == plainDate(sent) {
		status = DeliveryOutForDelivery
	}
	// The other direction: "kommt heute" and "wurde zugestellt" are both a date
	// as well as a status, and neither is written next to one. The message's own
	// day is what they mean, and taking it here is what puts a parcel on the
	// reader's list on the one day they care about it.
	if date == "" && !sent.IsZero() && (status == DeliveryOutForDelivery || status == DeliveryDelivered) {
		date = plainDate(sent)
	}
	for i := range found {
		found[i].ExpectedDate = date
		found[i].WindowStart = windowStart
		found[i].WindowEnd = windowEnd
		found[i].Status = status
	}
	return found
}

// deliveryText is the message as the extractor reads it: the subject first,
// because a subject line is where "arrives today" is stated, then the body.
func deliveryText(content CategoryContent) string {
	subject := strings.TrimSpace(content.Subject)
	body := strings.TrimSpace(content.Text)
	switch {
	case subject == "":
		return body
	case body == "":
		return subject
	default:
		return subject + ". " + body
	}
}

// normalizeTrackingNumber is what makes two mails about one parcel one row. A
// number is written with spaces in one mail and without in the next, and the
// letters are upper case in every carrier's own spelling.
func normalizeTrackingNumber(number string) string {
	var b strings.Builder
	for _, r := range number {
		switch {
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r)
		case r >= 'a' && r <= 'z':
			b.WriteRune(r - 32)
		}
	}
	normalized := b.String()
	// Below eight characters nothing is a tracking number, and above thirty-five
	// nothing is either; both bounds are well outside every shape in use.
	if len(normalized) < 8 || len(normalized) > 35 {
		return ""
	}
	if countDigits(normalized) < 6 {
		return ""
	}
	return normalized
}

func countDigits(value string) int {
	n := 0
	for _, r := range value {
		if r >= '0' && r <= '9' {
			n++
		}
	}
	return n
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

// carrierForSender answers "whose number is this" from the address the message
// came from. It is a hint and never a gate: most first notice of a parcel comes
// from the shop, not the carrier.
func carrierForSender(from string) string {
	address := strings.ToLower(BareAddress(from))
	at := strings.LastIndex(address, "@")
	if at < 0 {
		return ""
	}
	domain := address[at+1:]
	for _, carrier := range deliveryCarriers {
		for _, candidate := range carrier.senderDomains {
			if domain == candidate || strings.HasSuffix(domain, "."+candidate) {
				return carrier.Key
			}
		}
	}
	return ""
}

// carrierNamedIn is the last way to answer whose number a message stated: the
// carrier it names in words. A shop writes "versendet mit DHL" and then gives the
// number, which is the whole of what the reader has to go on as well.
func carrierNamedIn(text string) string {
	lower := strings.ToLower(text)
	for _, carrier := range deliveryCarriers {
		if carrierNameRE(carrier.Key).MatchString(lower) {
			return carrier.Key
		}
	}
	return ""
}

// carrierNames are the words a message calls a carrier by, beyond its key.
var carrierNames = map[string][]string{
	"dhl":    {"dhl", "deutsche post"},
	"dpd":    {"dpd"},
	"gls":    {"gls"},
	"ups":    {"ups"},
	"fedex":  {"fedex", "fed ex"},
	"hermes": {"hermes", "evri"},
	"amazon": {"amazon"},
}

// carrierNameREs is built once at startup rather than on demand: extraction
// runs on every fetch worker at once, and a lazily filled map would be a data
// race for the sake of seven regexes.
var carrierNameREs = buildCarrierNameREs()

func buildCarrierNameREs() map[string]*regexp.Regexp {
	out := make(map[string]*regexp.Regexp, len(deliveryCarriers))
	for _, carrier := range deliveryCarriers {
		names := carrierNames[carrier.Key]
		if len(names) == 0 {
			names = []string{carrier.Key}
		}
		quoted := make([]string, 0, len(names))
		for _, name := range names {
			quoted = append(quoted, regexp.QuoteMeta(name))
		}
		// Word boundaries matter: "ups" is inside "groups" and "backups".
		out[carrier.Key] = regexp.MustCompile(`\b(?:` + strings.Join(quoted, "|") + `)\b`)
	}
	return out
}

func carrierNameRE(key string) *regexp.Regexp {
	if re, ok := carrierNameREs[key]; ok {
		return re
	}
	return regexp.MustCompile(`\b` + regexp.QuoteMeta(key) + `\b`)
}

// carrierForLink answers which carrier's tracking page a URL points at, or nil
// for the hundred other links a message carries.
func carrierForLink(rawURL string) *DeliveryCarrier {
	host := linkHost(rawURL)
	if host == "" {
		return nil
	}
	for index := range deliveryCarriers {
		for _, candidate := range deliveryCarriers[index].linkHosts {
			if host == candidate || strings.HasSuffix(host, "."+candidate) {
				return &deliveryCarriers[index]
			}
		}
	}
	return nil
}

// linkHost pulls the host out of a URL without net/url's allocation and error
// handling. What is needed is the authority between "//" and the next "/", "?"
// or "#", lower-cased and without credentials, port, or a leading "www.".
func linkHost(rawURL string) string {
	value := strings.TrimSpace(rawURL)
	if index := strings.Index(value, "//"); index >= 0 {
		value = value[index+2:]
	} else {
		return ""
	}
	if cut := strings.IndexAny(value, "/?#"); cut >= 0 {
		value = value[:cut]
	}
	if at := strings.LastIndex(value, "@"); at >= 0 {
		value = value[at+1:]
	}
	if colon := strings.Index(value, ":"); colon >= 0 {
		value = value[:colon]
	}
	value = strings.ToLower(strings.TrimSuffix(value, "."))
	return strings.TrimPrefix(value, "www.")
}

// trackingNumberFromLink reads the number out of a carrier's tracking URL: the
// query key the carrier uses, then the fragment, then the last path segment.
// Each fallback is one the carriers actually use -- Hermes puts the number in a
// fragment and DPD in the path -- and each is checked against the same shape
// rules a number found in text is.
func trackingNumberFromLink(rawURL string, carrier DeliveryCarrier) string {
	value := rawURL
	fragment := ""
	if hash := strings.Index(value, "#"); hash >= 0 {
		fragment = value[hash+1:]
		value = value[:hash]
	}
	query := ""
	if mark := strings.Index(value, "?"); mark >= 0 {
		query = value[mark+1:]
		value = value[:mark]
	}
	for _, pair := range strings.Split(query, "&") {
		key, raw, ok := strings.Cut(pair, "=")
		if !ok {
			continue
		}
		key = strings.ToLower(strings.TrimSpace(key))
		if !containsString(carrier.linkParams, key) {
			continue
		}
		// A carrier that tracks several parcels on one page separates them with
		// a comma; the first is as good an answer as any, and the rest turn up
		// as their own links or in the text.
		first, _, _ := strings.Cut(raw, ",")
		if number := normalizeTrackingNumber(first); number != "" {
			return number
		}
	}
	// A named parameter says what it holds. The two fallbacks below do not, so
	// they are only read from a URL whose path says it is a tracking page: every
	// other link to a carrier is a shop page, a help article or a footer, and
	// their last path segment and their fragment are an article id and an anchor
	// that read as a number just as well.
	if !trackingPagePath(value) {
		return ""
	}
	if number := trackedNumberFromSegment(fragment, carrier); number != "" {
		return number
	}
	value = strings.TrimSuffix(value, "/")
	if slash := strings.LastIndex(value, "/"); slash >= 0 {
		if number := trackedNumberFromSegment(value[slash+1:], carrier); number != "" {
			return number
		}
	}
	return ""
}

// trackingPageMarkers are what a carrier calls the page a parcel is followed on.
// One of them has to be in the path before an unlabelled part of the URL is read
// as a parcel number.
var trackingPageMarkers = []string{
	"track", "verfolg", "sendungsinformation", "sendungsstatus",
	"parcel", "paket", "shipment", "nextt-online-public", "progress-tracker",
}

func trackingPagePath(url string) bool {
	lower := strings.ToLower(url)
	for _, marker := range trackingPageMarkers {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

// trackedNumberFromSegment reads a number out of a part of a URL that does not
// say what it is. Beyond the ordinary shape rules it has to look like a number a
// carrier issued: all digits, or a shape only this carrier uses. An anchor like
// "#faq-1234567890" and an article id both clear the shape rules and neither is
// a parcel.
func trackedNumberFromSegment(segment string, carrier DeliveryCarrier) string {
	number := normalizeTrackingNumber(segment)
	if number == "" {
		return ""
	}
	if carrier.numberShape != nil && carrier.numberShape.MatchString(number) {
		return number
	}
	if countDigits(number) == len(number) {
		return number
	}
	return ""
}

func containsString(values []string, needle string) bool {
	for _, value := range values {
		if value == needle {
			return true
		}
	}
	return false
}

// labelledTrackingRE matches a number a message says the purpose of. The label
// is matched case-insensitively; the number itself is not, because restricting
// it to upper case and digits is what stops the match running on into the words
// after it -- "Sendungsnummer: 1234567890 und weitere" ends at the lower-case
// word, which no tracking number contains.
//
// Spaces inside the number are deliberately not allowed for the same reason. A
// carrier that prints its number in groups is missed here; it is still found
// through its own tracking link, which is the same message's other half.
var labelledTrackingRE = regexp.MustCompile(
	`(?i:\b(?:sendungs(?:verfolgungs)?|paket|tracking|shipment|parcel|versand|track)[\s-]*(?:nummer|nr\.?|no\.?|number|id|code|status)\b)[^0-9A-Z]{0,15}([0-9A-Z][0-9A-Z-]{6,34})`)

// labelledTrackingNumbers returns every number the text says is a tracking
// number, in the order they appear.
func labelledTrackingNumbers(text string) []string {
	matches := labelledTrackingRE.FindAllStringSubmatch(text, maxDeliveryNoticesPerMessage)
	if len(matches) == 0 {
		return nil
	}
	out := make([]string, 0, len(matches))
	for _, match := range matches {
		out = append(out, match[1])
	}
	return out
}

// deliveredRE and outForDeliveryRE are matched in that order: a parcel that has
// arrived is no longer on the van, and a mail announcing today's delivery is
// followed by one reporting it, often within the hour.
//
// The German forms are all past tense on purpose. "Die Zustellung erfolgt" and
// "wird zugestellt" are announcements, and reading them as arrivals would clear
// the day's list before anything had been delivered.
// Every alternative here is past tense, and that is the whole rule rather than
// a stylistic one. "Zustellung erfolgt am 07.09." and "will be delivered on
// September 7" are announcements, and the tense-free forms that used to sit in
// this list -- a bare "zugestellt am", a bare "delivered on" -- matched both
// them and the reports, which cleared a parcel off the day's list before
// anything had been delivered. A real delivery report says who did what:
// "wurde zugestellt", "has been delivered". RE2 has no lookbehind to exclude
// the future forms with, so the past forms are named instead.
var deliveredRE = regexp.MustCompile(`(?i)(?:wurde|wurden|ist|sind)\s+(?:heute\s+|bereits\s+|erfolgreich\s+)?(?:zugestellt|abgeliefert|geliefert)|erfolgreich\s+zugestellt|(?:paket|sendung|lieferung)\s+zugestellt|(?:has|have)\s+been\s+delivered|(?:was|were)\s+delivered|your\s+(?:package|parcel|order)\s+(?:has|was)\s+deliver`)

// Every alternative here says *today*, either in the word or in the state: a
// parcel in Zustellung is on the van now. "Auf dem Weg zu Ihnen" used to be in
// this list and is not, because it is what a shop writes the moment it hands a
// parcel over -- it says the parcel is moving, not that it arrives today, and
// reading it as today put a chip in the header days early through the date
// fallback that gives an out-for-delivery message the day it was written.
var outForDeliveryRE = regexp.MustCompile(`(?i)in\s+zustellung|out\s+for\s+delivery|kommt\s+heute|wird\s+heute\s+(?:zugestellt|geliefert)|heute\s+(?:zugestellt|geliefert)|zustellung\s+heute|noch\s+heute|arriv\w*\s+today|on\s+its\s+way\s+today|im\s+zustellfahrzeug`)

// deliveryStatus grades what the message reports about the parcel.
func deliveryStatus(text string) string {
	switch {
	case deliveredRE.MatchString(text):
		return DeliveryDelivered
	case outForDeliveryRE.MatchString(text):
		return DeliveryOutForDelivery
	default:
		return DeliveryAnnounced
	}
}

// deliveryAnchorRE marks the places in a message where a date next to it is the
// delivery's date. Mail is full of dates -- when the order was placed, when the
// invoice is due, what year the footer was written -- and none of them are the
// day a reader wants on their list, so a date is only read out of a window
// around one of these words.
var deliveryAnchorRE = regexp.MustCompile(`(?i)voraussichtlich\w*|zustell\w*|zugestellt|liefer\w*|geliefert|ankunft|eintreff\w*|erwartet|delivery|deliver\w*|arriv\w*|expected|estimated|scheduled`)

var (
	// germanDateRE is "04.09.2026", "4.9.26" and "04.09." -- the spelling every
	// German carrier uses. The year is optional because the day and month alone
	// are what a mail about next week says.
	germanDateRE = regexp.MustCompile(`\b(\d{1,2})\.\s*(\d{1,2})\.(?:\s*(\d{4}|\d{2})\b)?`)
	// isoDateRE is the machine spelling, which turns up in mail generated from
	// an order system rather than written for a reader.
	isoDateRE = regexp.MustCompile(`\b(\d{4})-(\d{1,2})-(\d{1,2})\b`)
	// dayMonthNameRE is "4. September" and "4 September 2026".
	dayMonthNameRE = regexp.MustCompile(`(?i)\b(\d{1,2})\.?\s*(` + monthNamePattern + `)\.?(?:\s*(\d{4})\b)?`)
	// monthNameDayRE is the English order, "September 4" and "Sep 4, 2026".
	monthNameDayRE = regexp.MustCompile(`(?i)\b(` + monthNamePattern + `)\.?\s+(\d{1,2})(?:st|nd|rd|th)?(?:,?\s*(\d{4})\b)?`)
	// relativeDayRE is what a mail sent on the morning of the delivery says.
	relativeDayRE = regexp.MustCompile(`(?i)\b(heute|today|übermorgen|uebermorgen|morgen|tomorrow)\b`)
	// weekdayRE is "am Donnerstag", which a carrier writes for anything inside
	// the coming week.
	weekdayRE = regexp.MustCompile(`(?i)\b(montag|dienstag|mittwoch|donnerstag|freitag|samstag|sonnabend|sonntag|monday|tuesday|wednesday|thursday|friday|saturday|sunday)\b`)
	// deliveryWindowRE is the two-hour slot the German carriers give on the day
	// itself. Both spellings require a word that makes them a time of day, so an
	// amount of money or a reference number cannot be read as one.
	deliveryWindowRE = regexp.MustCompile(`(?i)zwischen\s+(\d{1,2})[:.](\d{2})\s*(?:und|bis|-|–)\s*(\d{1,2})[:.](\d{2})|\b(\d{1,2})[:.](\d{2})\s*(?:-|–|bis|und)\s*(\d{1,2})[:.](\d{2})\s*uhr`)
)

// monthNames maps every spelling of a month this extractor accepts, German and
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

const (
	// deliveryDatePast and deliveryDateFuture bound how far from the message a
	// date may be and still be its parcel's day. A mail announcing a delivery
	// names a day within the fortnight; something months away is a date the
	// window happened to reach, not a delivery.
	deliveryDatePast   = -60
	deliveryDateFuture = 120
)

// deliveryDate reads the day the message announces, and the window inside it.
// It returns empty strings for a message that names a parcel without saying
// when, which is most of what a shop sends.
func deliveryDate(text string, sent time.Time, status string) (string, string, string) {
	if sent.IsZero() {
		// Every date this reads is relative to when the message was written --
		// "tomorrow" plainly so, but a bare "04.09." needs the year too. Without
		// a Date header there is nothing to resolve against, and guessing from
		// the clock would date a week-old backfilled message to this week.
		return "", "", ""
	}
	date := ""
	for _, anchor := range deliveryAnchorRE.FindAllStringIndex(text, -1) {
		// After the anchor first: "voraussichtliche Zustellung: Do., 04.09."
		// is the order every carrier writes it in.
		if found, ok := findDeliveryDate(windowAfter(text, anchor[1], deliveryDateProximity), sent, status); ok {
			date = found
			break
		}
		// Then before it: "am 04.09. wird Ihr Paket zugestellt" puts the same
		// date on the other side of the same word.
		if found, ok := findDeliveryDate(windowBefore(text, anchor[0], deliveryDateProximity), sent, status); ok {
			date = found
			break
		}
	}
	if date == "" {
		return "", "", ""
	}
	start, end := deliveryWindow(text)
	return date, start, end
}

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

// findDeliveryDate tries every spelling on one window, in the order that puts
// the least ambiguous first.
func findDeliveryDate(window string, sent time.Time, status string) (string, bool) {
	if window == "" {
		return "", false
	}
	year, month, day := sent.Date()
	sentDay := time.Date(year, month, day, 0, 0, 0, 0, sent.Location())

	// Every spelling is tried, and every match of each is tried, until one
	// yields a date that survives plausibleDate. A match is not an answer: a
	// reference number reads as "12.34.56" and a price as "1.234,56", both of
	// which the German date expression matches and the calendar then rejects.
	// Stopping at the first *match* rather than the first *answer* -- which is
	// what this used to do -- lost the real date standing beside it.
	for _, match := range isoDateRE.FindAllStringSubmatch(window, maxDeliveryDateCandidates) {
		if date, ok := plausibleDate(atoi(match[1]), time.Month(atoi(match[2])), atoi(match[3]), sentDay, status); ok {
			return date, true
		}
	}
	for _, match := range germanDateRE.FindAllStringSubmatch(window, maxDeliveryDateCandidates) {
		if date, ok := dateWithOptionalYear(atoi(match[1]), time.Month(atoi(match[2])), match[3], sentDay, status); ok {
			return date, true
		}
	}
	for _, match := range dayMonthNameRE.FindAllStringSubmatch(window, maxDeliveryDateCandidates) {
		month, ok := monthNames[strings.ToLower(match[2])]
		if !ok {
			continue
		}
		if date, ok := dateWithOptionalYear(atoi(match[1]), month, match[3], sentDay, status); ok {
			return date, true
		}
	}
	for _, match := range monthNameDayRE.FindAllStringSubmatch(window, maxDeliveryDateCandidates) {
		month, ok := monthNames[strings.ToLower(match[1])]
		if !ok {
			continue
		}
		if date, ok := dateWithOptionalYear(atoi(match[2]), month, match[3], sentDay, status); ok {
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

// maxDeliveryDateCandidates bounds how many matches of one spelling are checked
// inside a window. The window is deliveryDateProximity bytes; past a handful of
// candidates it is a table of numbers rather than a sentence about a delivery.
const maxDeliveryDateCandidates = 4

// dateWithOptionalYear settles a day and month that may or may not have said
// which year they are in. A carrier writing "04.01." on the 29th of December
// means the January nine days away, not the one eleven months back, so the year
// is chosen as the one that puts the date nearest the message.
func dateWithOptionalYear(day int, month time.Month, yearText string, sentDay time.Time, status string) (string, bool) {
	if yearText != "" {
		year := atoi(yearText)
		if year < 100 {
			year += 2000
		}
		return plausibleDate(year, month, day, sentDay, status)
	}
	for _, year := range []int{sentDay.Year(), sentDay.Year() + 1, sentDay.Year() - 1} {
		if date, ok := plausibleDate(year, month, day, sentDay, status); ok {
			return date, true
		}
	}
	return "", false
}

// plausibleDate rejects what the calendar does not hold and what is too far
// from the message to be its parcel. The round-trip through time.Date is what
// catches "31.02.": normalization would silently make it the third of March.
func plausibleDate(year int, month time.Month, day int, sentDay time.Time, status string) (string, bool) {
	if year < 1970 || month < time.January || month > time.December || day < 1 || day > 31 {
		return "", false
	}
	date := time.Date(year, month, day, 0, 0, 0, 0, sentDay.Location())
	if date.Year() != year || date.Month() != month || date.Day() != day {
		return "", false
	}
	distance := int(date.Sub(sentDay).Hours() / 24)
	past := deliveryDatePast
	if status == DeliveryDelivered {
		// A delivery report names a day that has already happened, and it is
		// the same day often enough that the ordinary past bound would do; the
		// wider one covers a report forwarded or filed late.
		past = -365
	}
	if distance < past || distance > deliveryDateFuture {
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

// deliveryWindow reads the slot inside the day, which only the morning-of mail
// carries. Both halves are validated as clock times: a match that is not one is
// a pair of numbers that happened to be spelled like one.
func deliveryWindow(text string) (string, string) {
	match := deliveryWindowRE.FindStringSubmatch(text)
	if match == nil {
		return "", ""
	}
	groups := match[1:5]
	if match[1] == "" {
		groups = match[5:9]
	}
	start, startOK := clockTime(groups[0], groups[1])
	end, endOK := clockTime(groups[2], groups[3])
	if !startOK || !endOK {
		return "", ""
	}
	return start, end
}

func clockTime(hourText, minuteText string) (string, bool) {
	hour, minute := atoi(hourText), atoi(minuteText)
	if hour < 0 || hour > 23 || minute < 0 || minute > 59 {
		return "", false
	}
	return pad2(hour) + ":" + pad2(minute), true
}

func pad2(value int) string {
	if value < 10 {
		return "0" + strconv.Itoa(value)
	}
	return strconv.Itoa(value)
}

// plainDate is the one spelling a delivery day is stored and compared in.
func plainDate(date time.Time) string {
	return date.Format("2006-01-02")
}

func atoi(value string) int {
	n, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil {
		return -1
	}
	return n
}

// DeliveryNoticesReaderScan reads a stored message for the parcels it names. It
// is the backfill's counterpart to what Parse has in hand for newly fetched
// mail, and it reads the same bounded content classification does.
//
// The second return says whether the scan reached the end of the message. A
// truncated one may have stopped before the link that named the carrier, so its
// answer may add a shipment nothing knew about and must never be taken as proof
// that a message names none.
func DeliveryNoticesReaderScan(r io.Reader) ([]DeliveryNotice, bool, error) {
	if r == nil {
		return nil, false, nil
	}
	header, content, complete, err := scanCategoryContent(r)
	if err != nil {
		return nil, false, err
	}
	sent, dateErr := mail.ParseDate(header.Get("Date"))
	if dateErr != nil {
		sent = time.Time{}
	}
	return ExtractDeliveryNotices(content, addressHeader(header.Get("From")), sent), complete, nil
}
