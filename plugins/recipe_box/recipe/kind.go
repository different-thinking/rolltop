// File overview: What kind of thing a shared link is, for the notes overview.
//
// A note-to-self that is not a recipe is still something the reader kept for a
// reason, and the overview groups them by what they point at: a video, an
// article, a product, a place. The answer follows the same rule as recipes do:
// the page's own statement first -- its schema.org type and its OpenGraph type
// -- because that is what it publishes about itself, and the platform it lives
// on where the page says nothing useful (a YouTube page calls itself a video
// either way, a shop's product page sometimes forgets to). Nothing reads the
// words of the page to guess: a wrong shelf is worse than "other links".

package recipe

import (
	"encoding/json"
	"net/url"
	"regexp"
	"strings"
)

// The kinds a note can be filed under. The names are stored, so they are part
// of the wire format; the labels are the frontend's.
const (
	KindRecipe  = "recipe"
	KindVideo   = "video"
	KindArticle = "article"
	KindProduct = "product"
	KindPlace   = "place"
	KindEvent   = "event"
	KindMusic   = "music"
	KindMedia   = "media"
	KindTech    = "tech"
	KindSocial  = "social"
	KindDocs    = "docs"
	KindLink    = "link"
	// KindText is a note without a link, decided by the backend rather than by
	// a page.
	KindText = "text"
)

// PageFacts is what a page says about its own kind.
type PageFacts struct {
	OGType      string
	SchemaTypes []string
	// Price is the offer of a product page, as the page wrote it with its
	// currency ("29.99 EUR").
	Price string
	// StartDate is an event's start as the page wrote it, usually ISO 8601.
	StartDate string
}

var itemTypeRE = regexp.MustCompile(`(?is)\bitemtype\s*=\s*["']?https?://schema\.org/([A-Za-z]+)`)

// pageFacts reads the type statements of a page: every schema.org type its
// JSON-LD and microdata name, its og:type, and the price or date that goes
// with a product or an event.
func pageFacts(markup string, meta map[string]string) PageFacts {
	facts := PageFacts{OGType: strings.ToLower(strings.TrimSpace(meta["og:type"]))}
	facts.Price = joinPrice(firstNonEmpty(meta["product:price:amount"], meta["og:price:amount"]), firstNonEmpty(meta["product:price:currency"], meta["og:price:currency"]))
	seen := map[string]bool{}
	addType := func(name string) {
		name = strings.TrimSpace(name)
		if index := strings.LastIndexAny(name, "/:#"); index >= 0 {
			name = name[index+1:]
		}
		key := strings.ToLower(name)
		if key == "" || seen[key] || len(facts.SchemaTypes) >= 40 {
			return
		}
		seen[key] = true
		facts.SchemaTypes = append(facts.SchemaTypes, key)
	}
	for _, match := range scriptLDRE.FindAllStringSubmatch(markup, 20) {
		attrs := attributes("<script " + match[1] + ">")
		if !strings.Contains(strings.ToLower(attrs["type"]), "ld+json") {
			continue
		}
		var doc any
		body := strings.TrimSpace(match[2])
		if err := json.Unmarshal([]byte(body), &doc); err != nil {
			if err := json.Unmarshal([]byte(controlCharReplacer.Replace(body)), &doc); err != nil {
				continue
			}
		}
		walkTypes(doc, 0, addType, &facts)
	}
	for _, match := range itemTypeRE.FindAllStringSubmatch(markup, 40) {
		addType(match[1])
	}
	return facts
}

// walkTypes collects the types of a JSON-LD document's nodes, and the price
// and date of the first product and event in it.
func walkTypes(value any, depth int, add func(string), facts *PageFacts) {
	if depth > 6 {
		return
	}
	switch v := value.(type) {
	case []any:
		for _, item := range v {
			walkTypes(item, depth+1, add, facts)
		}
	case map[string]any:
		types := stringsOf(v["@type"])
		for _, name := range types {
			add(name)
			lower := strings.ToLower(name)
			if facts.Price == "" && (strings.HasSuffix(lower, "product") || strings.HasSuffix(lower, "offer")) {
				facts.Price = offerPrice(v)
			}
			if facts.StartDate == "" && strings.HasSuffix(lower, "event") {
				facts.StartDate = cleanText(textOf(v["startDate"]), 40)
			}
		}
		for _, key := range []string{"@graph", "mainEntity", "mainEntityOfPage", "itemListElement", "item"} {
			walkTypes(v[key], depth+1, add, facts)
		}
	}
}

func offerPrice(node map[string]any) string {
	price := textOf(node["price"])
	currency := textOf(node["priceCurrency"])
	if price == "" {
		switch offers := node["offers"].(type) {
		case map[string]any:
			price = firstNonEmpty(textOf(offers["price"]), textOf(offers["lowPrice"]))
			currency = textOf(offers["priceCurrency"])
		case []any:
			for _, item := range offers {
				if offer, ok := item.(map[string]any); ok {
					if price = firstNonEmpty(textOf(offer["price"]), textOf(offer["lowPrice"])); price != "" {
						currency = textOf(offer["priceCurrency"])
						break
					}
				}
			}
		}
	}
	return joinPrice(price, currency)
}

func joinPrice(amount, currency string) string {
	amount = cleanText(amount, 20)
	if amount == "" || amount == "0" {
		return ""
	}
	return strings.TrimSpace(amount + " " + cleanText(strings.ToUpper(currency), 8))
}

// schemaKinds maps schema.org types to a kind, most specific first: a product
// page usually also calls itself a WebPage, and an article about a place is an
// article. Generic types (WebPage, WebSite, Organization, BreadcrumbList) are
// in no list and say nothing.
var schemaKinds = []struct {
	kind  string
	types []string
}{
	{KindProduct, []string{"product", "productgroup", "offer", "aggregateoffer", "individualproduct", "vehicle", "car"}},
	{KindEvent, []string{"event", "musicevent", "festival", "sportsevent", "theaterevent", "comedyevent", "socialevent", "exhibitionevent", "businessevent", "educationevent", "screeningevent"}},
	{KindVideo, []string{"videoobject", "clip", "broadcastevent"}},
	{KindMusic, []string{"musicrecording", "musicalbum", "musicplaylist", "musicgroup", "podcastepisode", "podcastseries", "radioepisode", "audioobject"}},
	{KindMedia, []string{"book", "movie", "tvseries", "tvepisode", "tvseason", "videogame", "creativeworkseries"}},
	{KindTech, []string{"softwareapplication", "mobileapplication", "webapplication", "softwaresourcecode"}},
	{KindPlace, []string{"place", "localbusiness", "restaurant", "foodestablishment", "cafeorcoffeeshop", "bakery", "barorpub", "hotel", "lodgingbusiness", "touristattraction", "touristdestination", "landmarksorhistoricalbuildings", "museum", "park", "campground", "skiresort", "store", "vacationrental", "trip"}},
	{KindArticle, []string{"article", "newsarticle", "blogposting", "report", "techarticle", "scholarlyarticle", "socialmediaposting", "analysisnewsarticle", "opinionnewsarticle", "reviewnewsarticle", "howto", "liveblogposting"}},
}

// hostKinds are the platforms whose kind is the platform's, whatever a single
// page of it says. Order matters where hosts nest: YouTube Music is music
// before YouTube is video.
var hostKinds = []struct {
	kind  string
	hosts []string
}{
	{KindMusic, []string{"music.youtube.com", "spotify.com", "spotify.link", "soundcloud.com", "bandcamp.com", "music.apple.com", "podcasts.apple.com", "deezer.com", "tidal.com", "music.amazon.de", "music.amazon.com", "audible.de", "audible.com"}},
	{KindVideo, []string{"youtube.com", "youtu.be", "vimeo.com", "dailymotion.com", "twitch.tv", "ardmediathek.de", "zdf.de", "arte.tv", "3sat.de", "ted.com"}},
	{KindSocial, []string{"instagram.com", "tiktok.com", "x.com", "twitter.com", "facebook.com", "fb.watch", "fb.com", "reddit.com", "redd.it", "linkedin.com", "threads.net", "bsky.app", "mastodon.social", "pinterest.com", "pinterest.de", "pin.it", "snapchat.com", "tumblr.com"}},
	{KindTech, []string{"github.com", "gitlab.com", "gist.github.com", "stackoverflow.com", "stackexchange.com", "npmjs.com", "pypi.org", "apps.apple.com", "play.google.com", "producthunt.com", "huggingface.co", "chromewebstore.google.com", "addons.mozilla.org"}},
	{KindDocs, []string{"docs.google.com", "drive.google.com", "dropbox.com", "onedrive.live.com", "1drv.ms", "sharepoint.com", "notion.so", "notion.site", "box.com", "icloud.com", "wetransfer.com", "we.tl", "scribd.com", "slideshare.net"}},
	{KindPlace, []string{"maps.google.com", "maps.app.goo.gl", "maps.apple.com", "tripadvisor.com", "tripadvisor.de", "booking.com", "airbnb.com", "airbnb.de", "komoot.com", "komoot.de", "outdooractive.com", "yelp.com", "yelp.de", "hotels.com", "expedia.de", "expedia.com", "holidaycheck.de"}},
	{KindEvent, []string{"eventbrite.com", "eventbrite.de", "meetup.com", "eventim.de", "ticketmaster.de", "ticketmaster.com", "reservix.de", "dice.fm", "lu.ma"}},
	{KindMedia, []string{"imdb.com", "goodreads.com", "letterboxd.com", "netflix.com", "primevideo.com", "disneyplus.com", "joyn.de", "rtlplus.de", "thalia.de", "hugendubel.de", "moviepilot.de", "steampowered.com", "store.steampowered.com"}},
	{KindProduct, []string{"amazon.de", "amazon.com", "amazon.co.uk", "amzn.to", "amzn.eu", "a.co", "ebay.de", "ebay.com", "kleinanzeigen.de", "etsy.com", "zalando.de", "otto.de", "ikea.com", "aliexpress.com", "temu.com", "idealo.de", "galaxus.de", "digitec.ch", "mediamarkt.de", "saturn.de", "alternate.de", "notebooksbilliger.de", "cyberport.de", "decathlon.de", "about-you.de", "aboutyou.de", "bol.com", "geizhals.de", "manomano.de", "hornbach.de", "obi.de"}},
	{KindArticle, []string{"medium.com", "substack.com", "wikipedia.org", "spiegel.de", "zeit.de", "sueddeutsche.de", "faz.net", "tagesschau.de", "heise.de", "golem.de", "t3n.de", "nytimes.com", "theguardian.com", "bbc.co.uk", "bbc.com"}},
}

// HostKind answers the kind a link has by its platform alone, or "" when the
// platform says nothing. It is also what a link that could not be fetched is
// filed by.
func HostKind(link string) string {
	u, err := url.Parse(link)
	if err != nil {
		return ""
	}
	host := strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.")
	path := strings.ToLower(u.Path)
	// Google keeps maps under its main host and its short links on goo.gl.
	if (hostMatches(host, []string{"google.com", "google.de"}) && strings.HasPrefix(path, "/maps")) || (host == "goo.gl" && strings.HasPrefix(path, "/maps")) {
		return KindPlace
	}
	for _, entry := range hostKinds {
		if hostMatches(host, entry.hosts) {
			return entry.kind
		}
	}
	return ""
}

// Classify decides the kind of a fetched page, and the one detail its card
// shows for that kind: the price of a product, the date of an event.
func Classify(page Page, link string) (string, string) {
	if page.Recipe != nil {
		return KindRecipe, ""
	}
	facts := page.Facts
	byHost := HostKind(link)
	// A platform with one kind of content wins over what its page says: a
	// YouTube page that also carries a Person or an Organization is a video.
	if byHost != "" && byHost != KindArticle && byHost != KindProduct {
		return byHost, ""
	}
	for _, entry := range schemaKinds {
		for _, name := range facts.SchemaTypes {
			for _, want := range entry.types {
				if name == want {
					return entry.kind, detailFor(entry.kind, facts)
				}
			}
		}
	}
	switch {
	case strings.HasPrefix(facts.OGType, "video"):
		return KindVideo, ""
	case strings.HasPrefix(facts.OGType, "music"):
		return KindMusic, ""
	case facts.OGType == "book" || strings.HasPrefix(facts.OGType, "books"):
		return KindMedia, ""
	case facts.OGType == "product" || strings.HasPrefix(facts.OGType, "product") || facts.OGType == "og:product":
		return KindProduct, facts.Price
	case facts.OGType == "article":
		return KindArticle, ""
	case facts.OGType == "place" || facts.OGType == "restaurant.restaurant" || strings.HasPrefix(facts.OGType, "business"):
		return KindPlace, ""
	}
	if byHost != "" {
		return byHost, detailFor(byHost, facts)
	}
	return KindLink, ""
}

func detailFor(kind string, facts PageFacts) string {
	switch kind {
	case KindProduct:
		return facts.Price
	case KindEvent:
		return facts.StartDate
	}
	return ""
}

// NoFetch reports whether a link's page is not worth fetching: a document,
// a repository or a map behind a login or a script, whose page would answer
// with a sign-in form rather than with what the reader saved. Such a link is
// filed by its platform alone.
func NoFetch(link string) bool {
	switch HostKind(link) {
	case KindDocs, KindPlace:
		u, err := url.Parse(link)
		if err != nil {
			return true
		}
		host := strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.")
		// Booking and travel sites answer like any page; only the map and
		// document hosts are behind a login or a script.
		return hostMatches(host, []string{"docs.google.com", "drive.google.com", "dropbox.com", "onedrive.live.com", "1drv.ms", "sharepoint.com", "icloud.com", "box.com", "wetransfer.com", "we.tl", "maps.google.com", "maps.app.goo.gl", "maps.apple.com", "google.com", "google.de", "goo.gl"})
	}
	return false
}
