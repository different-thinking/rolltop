// File overview: What a message says about money the reader still owes -- which
// invoice, how much, by when, and above all whether anything is left to do.
//
// Like a delivery this is deliberately not a category. A category is one answer
// per message and it is what the reader sees in a list; an invoice is a fact
// about a *document*, several messages talk about the same one -- the invoice,
// a reminder, a dunning letter, the payment confirmation -- and the useful view
// is one row per invoice with the mail hanging off it. Invoices & Contracts
// stays untouched and is the cheap prefilter in front of this: the category
// answers "is this paperwork", this answers "is there anything to pay".
//
// The whole file is built around one asymmetry. A reminder for an invoice that
// was already settled -- by direct debit, by card, by PayPal -- is a false alarm
// on every page the reader opens, and they cannot make it go away except by
// hand. A missed invoice is a row that quietly sits in the list without a chip,
// where the reader still finds it. So every signal that says "already paid"
// outranks every signal that says "please transfer", and a message that says
// neither produces a row with no due date, which is visible but never alarms.
//
// The one exception is a dunning letter, and it is the reason this file knows
// about dunning levels at all: somebody writing to say the money never arrived
// has settled the question, whatever the original invoice said about how it
// would be paid.

package mailparse

import (
	"io"
	"net/mail"
	"regexp"
	"strings"
	"time"
)

// Invoice statuses. They are stored values and travel to the browser, so they
// are named here once and not spelled out anywhere else.
const (
	// InvoiceOpen is money the reader still has to send.
	InvoiceOpen = "open"
	// InvoicePaid is an invoice with nothing left to do: paid, collected by
	// direct debit, charged to a card, or a credit note.
	InvoicePaid = "paid"
)

// Dunning levels. The scale is what a reader needs to tell apart, not what
// German dunning practice distinguishes: a polite nudge, a formal demand, and
// the one that threatens consequences.
const (
	// InvoiceDunningNone is an ordinary invoice.
	InvoiceDunningNone = 0
	// InvoiceDunningReminder is a Zahlungserinnerung: a friendly nudge.
	InvoiceDunningReminder = 1
	// InvoiceDunningNotice is a Mahnung.
	InvoiceDunningNotice = 2
	// InvoiceDunningFinal is a last warning, a collection agency, or a court
	// order in the making.
	InvoiceDunningFinal = 3
)

// How an invoice is settled. This is not the same question as the status: it
// says who moves the money, which is the whole difference between an invoice
// the reader must act on and one that settles itself.
const (
	// SettlementUnknown is a message that never said.
	SettlementUnknown = ""
	// SettlementTransfer is the reader sending money themselves.
	SettlementTransfer = "transfer"
	// SettlementDirectDebit is the biller collecting it.
	SettlementDirectDebit = "direct_debit"
	// SettlementCard is a card being charged.
	SettlementCard = "card"
	// SettlementWallet is PayPal and the other payment services.
	SettlementWallet = "wallet"
)

// ValidInvoiceSettlement reports whether a key is one this build knows. Values
// arrive from stored rows as well as from extraction, and both are checked.
func ValidInvoiceSettlement(value string) bool {
	switch value {
	case SettlementUnknown, SettlementTransfer, SettlementDirectDebit, SettlementCard, SettlementWallet:
		return true
	default:
		return false
	}
}

// InvoiceNotice is one invoice as one message describes it.
type InvoiceNotice struct {
	// Issuer is the registrable domain the message came from. It is half the
	// identity: unlike a tracking number, which exactly one carrier in the
	// world issued, "2026-001" is a number half the senders in a mailbox hand
	// out every January.
	Issuer string
	// Reference is the other half, and is never empty. It is the invoice
	// number where the message stated one and a fallback where it did not --
	// see invoiceReference for what those are and why a row is worth having
	// without a number at all.
	Reference string
	// Number is the invoice number as it was printed, empty when the message
	// never stated one. It is what a reader is shown; Reference is what rows
	// are merged on.
	Number string
	// DueDate is a plain calendar day, "2006-01-02", or empty for an invoice
	// that named no deadline. Empty is the honest answer and not a reason to
	// guess: an undated row is shown without ever raising a chip.
	DueDate string
	// Amount is normalized to "1234.56" -- no thousands separator, a period for
	// the decimal -- so two spellings of one sum compare equal. Currency is the
	// ISO code where the message named one.
	Amount   string
	Currency string
	// Status is InvoiceOpen or InvoicePaid.
	Status string
	// Settlement is how the message said it gets paid.
	Settlement string
	// DunningLevel is one of the four constants above.
	DunningLevel int
}

// InvoiceDocument is one attachment as the invoice reader sees it: the text
// that could be pulled out of it, and the structured e-invoice inside it where
// there was one.
//
// Unlike CategoryFile this carries content, and that is the one real departure
// from how the rest of classification works. It has to be: the case this whole
// feature exists for -- a bill that was already settled through PayPal and
// arrives as a receipt anyway -- is routinely stated only in the PDF, and a
// filename cannot say it.
type InvoiceDocument struct {
	Filename    string
	ContentType string
	// Text is what the document says, already extracted and bounded by the
	// caller. For a PDF that is pdftotext's output, which the search index has
	// paid for anyway.
	Text string
	// Structured is the machine-readable e-invoice found in or as this
	// attachment: an XRechnung XML, or the XML ZUGFeRD and Factur-X embed in
	// their PDF. Empty for an ordinary document, and empty when the caller has
	// not looked -- Raw below is where it is looked for.
	Structured []byte
	// Raw is the attachment's own bytes, for the one thing text extraction
	// cannot do: a ZUGFeRD PDF carries its XML as an embedded file, which is
	// not part of the page content and so never appears in pdftotext's output.
	// It aliases the decoded attachment rather than copying it, and is empty on
	// paths that only had text to give.
	Raw []byte
}

const (
	// maxInvoiceDocumentText bounds how much of one attachment's text is read.
	// An invoice states its number, its total and its terms on the first page;
	// past this it is line items and terms of business.
	maxInvoiceDocumentText = 64 * 1024
	// maxInvoiceDocuments bounds how many attachments are read at all. A
	// message carrying more documents than this is not describing one invoice.
	maxInvoiceDocuments = 4
	// invoiceDateProximity bounds how far from a word like "zahlbar bis" a date
	// may stand and still be the deadline it announces. Wider than the parcel
	// window because a payment term is routinely written as a sentence -- "Bitte
	// überweisen Sie den Betrag bis zum 15.09.2026 auf das unten genannte
	// Konto" -- rather than as a label and a value.
	invoiceDateProximity = 120
	// invoiceSettlementProximity bounds how far from "Zahlungsart" the method
	// itself may stand. Short on purpose: a shop's footer lists every method it
	// accepts, and the only thing that makes one of them *this* invoice's is a
	// word right next to it saying so.
	invoiceSettlementProximity = 60
	// invoiceConditionWindow is how far back a "falls" may stand and still
	// govern a phrase. Long enough for "Sollten Sie den Betrag bereits
	// überwiesen haben", short enough that the sentence before cannot reach.
	invoiceConditionWindow = 60
)

const (
	// invoiceDatePast and invoiceDateFuture bound how far from the message a
	// due date may be. The past bound is a year because that is exactly what a
	// dunning letter does: it repeats a deadline that has long gone. The future
	// bound is shorter -- a payment term is measured in weeks, and a date
	// further out than this is a contract period or a warranty, not a deadline.
	invoiceDatePast   = -365
	invoiceDateFuture = 180
)

// invoiceDueBounds is the one date window this file reads deadlines in.
var invoiceDueBounds = dateBounds{past: invoiceDatePast, future: invoiceDateFuture}

// ExtractInvoiceNotice reads one message for the invoice it is about, or
// returns nil for a message that is not about money owed.
//
// docs are the attachments' contents where the caller had them; passing none is
// valid and costs only the invoices whose numbers live solely in the PDF.
//
// sent is the message's own date in the zone it was sent in, for the same
// reason deliveries need it: "innerhalb von 14 Tagen" is relative to when the
// letter was written, which is what lets the backfill reach the same answer out
// of a week-old message that the fetch path reached at the time.
func ExtractInvoiceNotice(content CategoryContent, docs []InvoiceDocument, from string, sent time.Time) *InvoiceNotice {
	issuer := registrableDomain(BareAddress(from))
	if issuer == "" {
		// Without a sender domain there is no namespace to hang a number in,
		// and a number on its own merges invoices from unrelated senders.
		return nil
	}
	subject := strings.ToLower(strings.TrimSpace(content.Subject))
	body := strings.ToLower(content.Text)
	documents := invoiceDocumentText(docs)
	// The three are kept apart rather than concatenated because two rules below
	// deliberately read only some of them: a dunning declares itself in the
	// subject, and boilerplate on an ordinary invoice talks about dunning in
	// the body.
	written := subject + ". " + body + " " + documents

	notice := InvoiceNotice{Issuer: issuer, Status: InvoiceOpen}
	notice.DunningLevel = dunningLevel(subject, invoiceFilenames(docs), body+" "+documents)
	notice.Number = invoiceNumberIn(subject, body, documents)
	notice.Amount, notice.Currency = invoiceAmountIn(written)
	notice.DueDate = invoiceDueDate(written, sent)
	notice.Settlement = invoiceSettlement(written)

	// A structured e-invoice states all of this rather than implying it, so it
	// is read last and overwrites what the prose suggested. It is the only
	// source here that cannot be misread: the fields are the standard's, not a
	// sentence somebody wrote.
	applyStructuredInvoice(&notice, docs, sent)

	// The status is settled before the gate rather than after it, because it is
	// one of the things the gate asks about: a payment confirmation states no
	// deadline and no way to pay, and it still has to produce a notice -- that
	// is how the invoice it names gets marked as settled.
	notice.Status = invoiceStatus(written, notice)
	if !payableDocument(subject, invoiceFilenames(docs), notice) {
		return nil
	}
	notice.Reference = invoiceReference(notice, sent)
	if notice.Reference == "" {
		return nil
	}
	return &notice
}

// payableDocument is the gate that keeps Invoices & Contracts from emptying
// itself into the reminder list. The category holds contracts, terms of
// service and cancellation notices as well as bills, and none of those is money
// owed.
//
// Two things have to be true. The message has to call itself something that is
// about paying, and there has to be something to record: a deadline, a way to
// pay, or a statement that the bill is settled. An order confirmation clears
// the first and fails the second, which is exactly the intent -- it names a
// number and a total and there is nothing to do about it.
//
// A settled bill qualifies for the same reason an unpaid one does, even though
// it raises nothing: it is how a payment confirmation reaches the row its
// invoice already has and closes it.
func payableDocument(subject string, filenames string, notice InvoiceNotice) bool {
	if notice.DunningLevel > InvoiceDunningNone {
		// Somebody writing to say the money never arrived has said all of it.
		return true
	}
	if !payableWordRE.MatchString(subject) && !payableWordRE.MatchString(filenames) {
		return false
	}
	return notice.DueDate != "" || notice.Settlement != SettlementUnknown || notice.Status == InvoicePaid
}

// payableWordRE is what a document about money calls itself. Contract words are
// deliberately absent: "Vertrag", "Kündigung" and "Nutzungsbedingungen" belong
// in the category and never in this list.
//
// Umlauts are spelled out as alternations rather than folded away, because the
// shared date reader downstream needs real text: folding "März" to "marz"
// would leave a month name no calendar knows.
var payableWordRE = regexp.MustCompile(`(?i)rechnung|faktura|invoice|zahlung|zahlbar|beleg|quittung|receipt|gutschrift|mahnung|lastschrift|abbuchung|bill(?:ing)?\b|payment`)

// invoiceStatus decides the only thing the reader actually asked for: is there
// anything left to do. The order of the cases is the whole rule.
func invoiceStatus(written string, notice InvoiceNotice) string {
	// A dunning outranks everything. Whatever the original invoice said about
	// how it would be settled, somebody is writing to say it was not.
	if notice.DunningLevel > InvoiceDunningNone {
		return InvoiceOpen
	}
	// A structured e-invoice that states nothing is left to pay has said so in
	// a field rather than a sentence.
	if notice.Amount == "0.00" {
		return InvoicePaid
	}
	if creditNoteRE.MatchString(written) {
		// Money coming back is not money owed.
		return InvoicePaid
	}
	if statedAsPaid(written) {
		return InvoicePaid
	}
	switch notice.Settlement {
	case SettlementDirectDebit, SettlementCard, SettlementWallet:
		// The reader is not the one who moves the money. This is the case the
		// whole feature was asked for: the PayPal receipt that arrives as an
		// invoice anyway.
		return InvoicePaid
	}
	return InvoiceOpen
}

// creditNoteRE is a document that owes money the other way.
var creditNoteRE = regexp.MustCompile(`(?i)gutschrift|credit\s+note|storno(?:rechnung)?|erstattung|refund`)

// paidPhraseRE is a message saying the money has arrived. Every alternative is
// in the indicative and about a payment that happened -- an announcement that
// one *will* be made is settlement, not payment, and is graded separately.
var paidPhraseRE = regexp.MustCompile(`(?i)bereits\s+(?:be)?(?:zahlt|glichen)|schon\s+(?:be)?(?:zahlt|glichen)|zahlung(?:seingang)?\s+(?:ist\s+)?(?:bei\s+uns\s+)?eingegangen|zahlungseingang\s+(?:ist\s+)?(?:bereits\s+)?(?:erfolgt|verbucht)|betrag\s+(?:wurde|ist)\s+(?:bereits\s+)?(?:beglichen|bezahlt|gutgeschrieben)|dankend\s+erhalten|(?:vielen\s+)?dank\s+f(?:ü|ue|u)r\s+(?:ihre|deine)\s+zahlung|keine\s+(?:weitere\s+)?zahlung\s+(?:ist\s+)?(?:mehr\s+)?(?:erforderlich|n(?:ö|oe|o)tig)|nichts\s+(?:weiter\s+)?(?:zu\s+tun|veranlassen)|erfolgreich\s+(?:bezahlt|beglichen)|payment\s+(?:received|complete)|(?:we\s+)?(?:have\s+)?received\s+your\s+payment|thank\s+you\s+for\s+your\s+payment|paid\s+in\s+full|successfully\s+charged`)

// conditionalRE are the words that turn a statement into a supposition. They
// matter because the single most common place "bereits bezahlt" appears is the
// escape clause at the bottom of a dunning letter -- "Sollten Sie den Betrag
// bereits überwiesen haben, betrachten Sie dieses Schreiben als
// gegenstandslos" -- which is the opposite of a receipt.
var conditionalRE = regexp.MustCompile(`(?i)(?:falls|sollten|sofern|wenn|should\s+you|in\s+case|unless)\b`)

// statedAsPaid reports whether the message says the money arrived, ignoring the
// ones that only suppose it might have. RE2 has no lookbehind, so the window
// before each match is cut out and tested on its own.
func statedAsPaid(written string) bool {
	for _, at := range paidPhraseRE.FindAllStringIndex(written, maxInvoicePhraseMatches) {
		if !conditionalRE.MatchString(windowBefore(written, at[0], invoiceConditionWindow)) {
			return true
		}
	}
	return false
}

// maxInvoicePhraseMatches bounds how many occurrences of one phrase are
// examined. Past a handful the message is a template listing every case rather
// than a letter stating one.
const maxInvoicePhraseMatches = 8

// The three dunning grades, matched hardest first: "letzte Mahnung" is also a
// "Mahnung", and a document that is both is the former.
var (
	dunningFinalRE    = regexp.MustCompile(`(?i)letzte\s+mahnung|letztmalig|(?:2|3|zweite|dritte)\.?\s*mahnung|mahnbescheid|inkasso|gerichtlich(?:es)?\s+mahnverfahren|final\s+(?:notice|reminder|demand)|last\s+reminder`)
	dunningNoticeRE   = regexp.MustCompile(`(?i)\bmahnung\b|\bmahnschreiben\b|mahngeb(?:ü|ue|u)hr|in\s+verzug|verzugszins|zahlungsverzug|overdue\s+notice|\bdunning\b|demand\s+for\s+payment`)
	dunningReminderRE = regexp.MustCompile(`(?i)zahlungserinnerung|zahlungs-erinnerung|erinnerung\s+an\s+(?:ihre\s+)?(?:offene\s+)?(?:rechnung|zahlung)|payment\s+reminder|friendly\s+reminder|reminder:\s*(?:invoice|payment)`)
)

// dunningBodyRE are the phrases that identify a dunning letter from its body.
// They are deliberately a much shorter list than the three above, and every one
// of them is a sentence only somebody chasing money writes.
//
// The reason for the separate list is that an ordinary invoice talks about
// dunning in its own fine print -- "bei Zahlungsverzug werden Mahngebühren
// fällig" is on the back of half the invoices ever printed -- so reading the
// grades above out of a body would mark every one of them as chased.
var dunningBodyRE = regexp.MustCompile(`(?i)konnten\s+wir\s+(?:bisher|bis\s+heute)\s+kein(?:en)?\s+zahlungseingang|bis\s+heute\s+(?:noch\s+)?kein(?:en)?\s+zahlungseingang|(?:leider\s+)?keinen\s+zahlungseingang\s+(?:feststellen|verzeichnen)|trotz\s+(?:unserer\s+)?(?:mehrfacher\s+)?(?:mahnung|erinnerung)|befinden\s+sie\s+sich\s+(?:nunmehr\s+)?in\s+verzug|we\s+have\s+not\s+(?:yet\s+)?received\s+(?:your\s+)?payment|your\s+account\s+is\s+past\s+due`)

// dunningLevel grades how hard the message is chasing the money.
//
// The subject and the attachment names decide, because that is where a dunning
// letter says what it is: the word is the whole point of sending it, and it is
// in the subject line every time. The body is read only through dunningBodyRE.
func dunningLevel(subject, filenames, body string) int {
	declared := subject + " " + filenames
	switch {
	case dunningFinalRE.MatchString(declared):
		return InvoiceDunningFinal
	case dunningNoticeRE.MatchString(declared):
		return InvoiceDunningNotice
	case dunningReminderRE.MatchString(declared):
		return InvoiceDunningReminder
	}
	if dunningBodyRE.MatchString(body) {
		// A letter whose subject is only "Ihre Rechnung 4711" but whose text
		// says the money never came is a dunning; without a grade in the
		// subject there is nothing to say how hard it is chasing, so it counts
		// as the ordinary one.
		return InvoiceDunningNotice
	}
	return InvoiceDunningNone
}

// invoiceNumberCaptureRE matches a document naming its own number and captures it.
// It is the same shape as the one categorization grades a message with, with a
// capture group added and the separators kept loose for the same reason: a
// document writes its reference every way there is -- "Rechnung-Nr. 4711",
// "Rechnungsnummer: 2024/0815", "Invoice no 7".
//
// The number itself is matched case-sensitively in the character class while
// the label is not, which is what stops the match running on into the words
// after it: "Rechnungsnummer 4711 vom 3. September" ends at the lower-case
// word, which no invoice number contains.
var invoiceNumberCaptureRE = regexp.MustCompile(
	`(?i:(?:rechnung|invoice|beleg|quittung|receipt|faktura|bill)[a-zä-ü]*[\s.-]*(?:nr\.?|nummer|no\.?|number|#|:))[\s:]*([0-9A-Za-z][0-9A-Za-z._/-]{1,38})`)

// invoiceLooseNumberRE is the same reference without the word that introduces
// it. Half the invoices in a mailbox are headed "Ihre Rechnung 12345" and never
// say "Nummer" anywhere, so requiring the label loses them -- and losing the
// number costs more than a number that is occasionally something else, because
// it is what a reminder and its invoice are merged on.
//
// What it may pick up instead is a sum: "Rechnung über 42,00 EUR" puts a figure
// exactly where a reference would stand. numberIsAnAmount below rejects those
// by looking at what follows, which RE2 cannot express as a lookahead.
var invoiceLooseNumberRE = regexp.MustCompile(
	`(?i:(?:rechnung|invoice|faktura)[a-zä-ü]*)[\s:#-]+([0-9A-Za-z][0-9A-Za-z._/-]{2,38})`)

// invoiceNumberIn reads the number, preferring the places and the spellings
// least likely to be talking about something else. The subject is a sender's
// own one-line summary of what the message is; the attachment's text is the
// document itself but also holds customer numbers, order numbers and tax IDs.
//
// Every labelled candidate anywhere is tried before the first unlabelled one,
// because a document that says "Rechnungsnummer" has named its reference and a
// document that does not has only been read as if it had.
func invoiceNumberIn(subject, body, documents string) string {
	sources := []string{subject, body, documents}
	for _, source := range sources {
		if number := firstInvoiceNumberIn(source, invoiceNumberCaptureRE, false); number != "" {
			return number
		}
	}
	for _, source := range sources {
		if number := firstInvoiceNumberIn(source, invoiceLooseNumberRE, true); number != "" {
			return number
		}
	}
	return ""
}

// firstInvoiceNumberIn walks every match rather than stopping at the first,
// because the first is routinely a word: "Rechnung vom 03.09." matches, and
// what it captures is "vom", which normalization rejects for having no digit in
// it. Stopping there would miss the real reference two sentences later.
func firstInvoiceNumberIn(source string, pattern *regexp.Regexp, loose bool) string {
	for _, at := range pattern.FindAllStringSubmatchIndex(source, maxInvoicePhraseMatches) {
		if at[2] < 0 {
			continue
		}
		candidate := source[at[2]:at[3]]
		if loose && numberIsAnAmount(source, at[3]) {
			continue
		}
		if number := normalizeInvoiceNumber(candidate); number != "" {
			return number
		}
	}
	return ""
}

// amountTailRE recognises a captured token that was the whole part of a sum.
// It is matched against the text immediately after the capture: "42" out of
// "Rechnung über 42,00 EUR" is followed by ",00 eur", and no invoice number is.
var amountTailRE = regexp.MustCompile(`^\s*(?:[.,]\d{2})?\s*(?:€|eur|chf|usd|gbp|\$|£)`)

func numberIsAnAmount(source string, after int) bool {
	return amountTailRE.MatchString(windowAfter(source, after, 12))
}

// normalizeInvoiceNumber trims what a number is written with but keeps what it
// is written *as*. Unlike a tracking number the separators here are not noise:
// "2024/0815" and "2024-0815" are two different references from a sender that
// uses both shapes, and flattening them would merge two invoices into one row.
// Only the surrounding punctuation and case are normalized.
func normalizeInvoiceNumber(raw string) string {
	value := strings.ToUpper(strings.TrimSpace(raw))
	value = strings.Trim(value, "._/-")
	if len(value) < 2 || len(value) > 40 {
		return ""
	}
	if countDigits(value) == 0 {
		// "Rechnungsnummer: siehe Anhang" is a sentence, not a reference.
		return ""
	}
	return value
}

// invoiceAmountAnchorRE marks where a total stands. An invoice is full of
// amounts -- every line item, the net, the tax -- and the only one worth
// storing is the one the reader has to send.
var invoiceAmountAnchorRE = regexp.MustCompile(`(?i)(?:gesamt|end|rechnungs|zahl|brutto|f(?:ä|ae|a)llig)(?:betrag|summe)|zu\s+zahlen(?:der\s+betrag)?|zahlbetrag|offener?\s+betrag|(?:total|amount)\s+(?:due|payable)|amount\s+to\s+pay|grand\s+total|\btotal\b|\bsumme\b`)

// invoiceAmountValueRE matches an amount of money on either side of its currency,
// and captures both halves. It is stricter than the one categorization grades
// with: here the number is read rather than merely counted, so a shape that
// cannot be parsed is worse than no answer.
var invoiceAmountValueRE = regexp.MustCompile(`(?i)(€|eur|chf|usd|gbp|\$|£)\s?(\d{1,3}(?:[.,\s]\d{3})*(?:[.,]\d{2})?|\d+(?:[.,]\d{2})?)|(\d{1,3}(?:[.,\s]\d{3})*[.,]\d{2}|\d+[.,]\d{2})\s?(€|eur|chf|usd|gbp|\$|£)`)

// invoiceAmountIn reads the total from beside a word that says it is one. There
// is deliberately no fallback to "the largest amount in the message": a
// marketing line offering a discount is routinely the largest number on an
// invoice, and a wrong total is worse than none -- it is half the fallback
// identity a numberless invoice is filed under.
func invoiceAmountIn(written string) (string, string) {
	for _, anchor := range invoiceAmountAnchorRE.FindAllStringIndex(written, maxInvoicePhraseMatches) {
		window := windowAfter(written, anchor[1], invoiceSettlementProximity)
		if amount, currency, ok := firstAmountIn(window); ok {
			return amount, currency
		}
		// Some layouts put the label to the right of the figure, and a table
		// flattened to text puts them on the same line either way round.
		window = windowBefore(written, anchor[0], invoiceSettlementProximity)
		if amount, currency, ok := firstAmountIn(window); ok {
			return amount, currency
		}
	}
	return "", ""
}

func firstAmountIn(window string) (string, string, bool) {
	for _, match := range invoiceAmountValueRE.FindAllStringSubmatch(window, maxDateCandidates) {
		currency, digits := match[1], match[2]
		if currency == "" {
			currency, digits = match[4], match[3]
		}
		amount, ok := normalizeAmount(digits)
		if !ok {
			continue
		}
		return amount, normalizeCurrency(currency), true
	}
	return "", "", false
}

// normalizeAmount folds the two spellings of a sum into one. German writes
// "1.234,56" and English "1,234.56", and the same invoice is quoted both ways
// in a mail and its own PDF, so a row keyed or compared on the raw text would
// see two amounts where there is one.
//
// The rule is the last separator wins, which is what actually distinguishes
// them: whichever of "." and "," comes last is the decimal point, and a lone
// separator is a decimal point only when exactly two digits follow it.
func normalizeAmount(raw string) (string, bool) {
	value := strings.NewReplacer(" ", "", " ", "").Replace(strings.TrimSpace(raw))
	if value == "" {
		return "", false
	}
	lastDot, lastComma := strings.LastIndex(value, "."), strings.LastIndex(value, ",")
	decimal := -1
	switch {
	case lastDot >= 0 && lastComma >= 0:
		decimal = max(lastDot, lastComma)
	case lastDot >= 0 || lastComma >= 0:
		at := max(lastDot, lastComma)
		if len(value)-at-1 == 2 {
			decimal = at
		}
	}
	whole, cents := value, "00"
	if decimal >= 0 {
		whole, cents = value[:decimal], value[decimal+1:]
	}
	if len(cents) != 2 || countDigits(cents) != 2 {
		return "", false
	}
	// Whatever separators are left in the whole part have to be thousands
	// grouping, and grouping is groups of exactly three. Checking that is what
	// tells a sum from the other things written with dots in them -- a version,
	// a reference, an IP address -- all of which would otherwise fold into a
	// plausible-looking figure once the separators were simply removed.
	groups := strings.Split(strings.ReplaceAll(whole, ",", "."), ".")
	for i, group := range groups {
		if group == "" || countDigits(group) != len(group) {
			return "", false
		}
		if i > 0 && len(group) != 3 {
			return "", false
		}
		if i == 0 && len(groups) > 1 && len(group) > 3 {
			return "", false
		}
	}
	whole = strings.Join(groups, "")
	// A sum longer than this is a reference number that happened to be written
	// with a separator, not a bill anybody is sending.
	if len(whole) > 12 {
		return "", false
	}
	whole = strings.TrimLeft(whole, "0")
	if whole == "" {
		whole = "0"
	}
	return whole + "." + cents, true
}

// normalizeCurrency turns whichever way the message wrote it into the ISO code,
// which is what a row stores and what a reader is shown.
func normalizeCurrency(raw string) string {
	switch strings.ToUpper(strings.TrimSpace(raw)) {
	case "€", "EUR":
		return "EUR"
	case "$", "USD":
		return "USD"
	case "£", "GBP":
		return "GBP"
	case "CHF":
		return "CHF"
	default:
		return strings.ToUpper(strings.TrimSpace(raw))
	}
}

// invoiceDueAnchorRE marks the places where a date beside it is the deadline.
// Everything here says *by when*, and nothing says merely *when*: an invoice
// date, a delivery period and a service month are all dates on the same page
// and none of them is what the reader has to act on.
var invoiceDueAnchorRE = regexp.MustCompile(`(?i)f(?:ä|ae|a)llig(?:keit|keitsdatum)?|zahlbar\s+(?:bis|am)?|zahlungsziel|zahlungstermin|zu\s+zahlen\s+bis|(?:bitte\s+)?(?:ü|ue|u)berweisen\s+sie[^.]{0,40}?bis|bis\s+sp(?:ä|ae|a)testens|sp(?:ä|ae|a)testens\s+(?:bis|am)|valuta|due\s+(?:date|by|on)|payable\s+(?:by|until)|payment\s+due|pay\s+by`)

// relativeDeadlineRE is the other way a payment term is written: not a date at
// all but a count of days from the letter. Dunning letters use it almost to the
// exclusion of dates.
var relativeDeadlineRE = regexp.MustCompile(`(?i)(?:innerhalb\s+von|binnen|within)\s+(\d{1,3})\s+(?:kalender)?(?:tagen|tage|days)`)

// immediateDueRE is a term stated as a word rather than a deadline. It is not a
// guess at a date the way "recently" would be -- the document is saying the
// money is owed now -- so it resolves to the day the message was written.
var immediateDueRE = regexp.MustCompile(`(?i)sofort\s+(?:f(?:ä|ae|a)llig|zahlbar)|zahlbar\s+sofort|f(?:ä|ae|a)llig\s+sofort|zahlbar\s+(?:bei|nach)\s+erhalt|due\s+(?:up)?on\s+receipt|payable\s+immediately`)

// invoiceDueDate reads the deadline the message states, in the order that puts
// the most explicit first.
func invoiceDueDate(written string, sent time.Time) string {
	if sent.IsZero() {
		// Every spelling below resolves against the day the letter was written.
		// Without a Date header there is nothing to resolve against, and the
		// clock would date a backfilled message to today.
		return ""
	}
	sentDay := startOfDay(sent)
	for _, anchor := range invoiceDueAnchorRE.FindAllStringIndex(written, maxInvoicePhraseMatches) {
		if date, ok := findDateNear(windowAfter(written, anchor[1], invoiceDateProximity), sentDay, invoiceDueBounds); ok {
			return date
		}
		if date, ok := findDateNear(windowBefore(written, anchor[0], invoiceDateProximity), sentDay, invoiceDueBounds); ok {
			return date
		}
	}
	if match := relativeDeadlineRE.FindStringSubmatch(written); match != nil {
		if days := atoi(match[1]); days >= 0 && days <= 180 {
			return plainDate(sentDay.AddDate(0, 0, days))
		}
	}
	if immediateDueRE.MatchString(written) {
		return plainDate(sentDay)
	}
	return ""
}

// invoiceSettlementAnchorRE marks where a message says how *this* invoice is
// paid, as against which methods the sender accepts in general.
//
// The anchor is the whole rule. Every shop footer lists PayPal, and a rule that
// read a method out of a bare mention would mark every invoice in the mailbox
// as already settled -- which is the one failure this feature cannot have.
var invoiceSettlementAnchorRE = regexp.MustCompile(`(?i)zahlungs(?:art|weise|methode|mittel)|bezahl(?:t|ung)\s+(?:ü|ue|u)ber|(?:be)?zahlt\s+(?:mit|per|via)|beglichen\s+(?:mit|per|(?:ü|ue|u)ber)|bezahlung\s+(?:erfolgt|per|mit)|einzug\s+(?:erfolgt|über)|payment\s+method|paid\s+(?:with|via|by)|charged\s+to`)

var (
	directDebitRE = regexp.MustCompile(`(?i)lastschrift|bankeinzug|abbuchung|abgebucht|eingezogen|direct\s+debit|sepa-?mandat`)
	cardRE        = regexp.MustCompile(`(?i)kreditkarte|credit\s?card|debitkarte|\bvisa\b|mastercard|american\s+express|\bamex\b|card\s+ending`)
	walletRE      = regexp.MustCompile(`(?i)paypal|apple\s?pay|google\s?pay|amazon\s?pay|klarna|sofort(?:\s?(?:ü|ue|u)berweisung)?|giropay|stripe`)
	transferRE    = regexp.MustCompile(`(?i)(?:ü|ue|u)berweis|bank(?:verbindung|transfer)|verwendungszweck|\biban\b|wire\s+transfer|credit\s+transfer`)
)

// unanchoredDebitRE are the statements that say the money is being collected
// without needing a "Zahlungsart:" in front of them. Unlike a method word in a
// footer these are whole sentences about this invoice, and they are how a
// direct-debit notice is actually written.
// The filler between the verb and its participle is matched with "." rather
// than with a class excluding the full stop, which is what an earlier spelling
// did and what stopped it matching the commonest form of all: "wird am
// 15.09.2026 von Ihrem Konto abgebucht" has two full stops inside the date. The
// length bound is what keeps the match inside one statement instead.
//
// The "s" flag is there for the same reason and matters just as much. Half of
// what this reads is pdftotext's output, which wraps the page's lines wherever
// the layout did, so the filler routinely contains a newline -- and RE2's "."
// does not cross one without it. Without the flag "Der Betrag wird am
// 15.09.2026\nvon Ihrem Konto abgebucht" reads as an invoice nobody has paid,
// which is exactly the false reminder this whole file is arranged to prevent.
var unanchoredDebitRE = regexp.MustCompile(`(?is)(?:wird|werden).{0,60}?(?:abgebucht|eingezogen)|buchen\s+wir.{0,60}?\bab\b|ziehen\s+wir.{0,60}?\bein\b|per\s+sepa-?lastschrift|will\s+be\s+(?:automatically\s+)?(?:charged|debited)|automatically\s+charged`)

// invoiceSettlement works out who moves the money.
//
// Anchored methods first, because those are the sender stating this invoice's
// own method. Then the two unanchored cases, each of which is a sentence rather
// than a word: a debit notice, and a transfer instruction. A transfer signal is
// the weakest of the three and is checked last, because an invoice settled by
// card still prints a bank account in its footer.
func invoiceSettlement(written string) string {
	for _, anchor := range invoiceSettlementAnchorRE.FindAllStringIndex(written, maxInvoicePhraseMatches) {
		window := windowAfter(written, anchor[1], invoiceSettlementProximity)
		if settlement := settlementIn(window); settlement != SettlementUnknown {
			return settlement
		}
	}
	if unanchoredDebitRE.MatchString(written) {
		return SettlementDirectDebit
	}
	if transferRE.MatchString(written) {
		return SettlementTransfer
	}
	return SettlementUnknown
}

// settlementIn grades one window, and the method standing nearest the anchor
// wins.
//
// Nearest rather than a fixed order of preference, because the window routinely
// holds more than one method and only the first is the answer: "Zahlungsart:
// PayPal. Wir akzeptieren außerdem Kreditkarte, Lastschrift und Überweisung"
// names four, and a rule that preferred direct debit over wallets would read
// that message -- a PayPal receipt -- as a direct debit. Where two do start at
// the same offset the list order below decides, which puts the methods that
// collect themselves ahead of the one that does not.
func settlementIn(window string) string {
	best, nearest := SettlementUnknown, len(window)+1
	for _, candidate := range []struct {
		re   *regexp.Regexp
		kind string
	}{
		{directDebitRE, SettlementDirectDebit},
		{walletRE, SettlementWallet},
		{cardRE, SettlementCard},
		{transferRE, SettlementTransfer},
	} {
		at := candidate.re.FindStringIndex(window)
		if at == nil || at[0] >= nearest {
			continue
		}
		best, nearest = candidate.kind, at[0]
	}
	return best
}

// invoiceReference is what rows are merged on inside one issuer. It is never
// empty for a notice that is returned at all, because a row nothing can be
// matched to is a row that duplicates itself on every reminder.
//
// The three fallbacks are in order of how well they identify one document:
//
//   - The number, where the document stated one.
//   - The amount, where it did not. Two invoices from one sender for the same
//     sum do collide, and that is the accepted cost: a subscription billed
//     monthly is the case, and merging two months of it is a smaller error than
//     losing the dunning letter that named neither number nor date.
//   - The message's own day, for a dunning that stated neither. It gives each
//     day's letter one row, so a resend merges and a genuinely new demand does
//     not. Nothing but a dunning gets this far -- payableDocument sees to that
//     -- which is the point: a chase with no identifiable invoice behind it is
//     still worth a row.
func invoiceReference(notice InvoiceNotice, sent time.Time) string {
	if notice.Number != "" {
		return notice.Number
	}
	if notice.Amount != "" {
		return "betrag:" + notice.Currency + notice.Amount
	}
	if notice.DunningLevel > InvoiceDunningNone && !sent.IsZero() {
		return "mahnung:" + plainDate(startOfDay(sent))
	}
	return ""
}

// InvoiceNoticeReaderScan reads a stored message for the invoice it is about.
// It is the backfill's counterpart to what Parse has in hand for newly fetched
// mail, and unlike every other scan in this package it opens the attachments.
//
// The second return says whether the scan reached the end of the message, and
// here that matters more than it does anywhere else. A truncated scan may have
// stopped before the very PDF that says the bill was settled, and recording an
// open invoice on that evidence is the one failure this feature cannot have --
// a chip on every page for money that is not owed. So the caller is told, and
// what it does about it is written where it acts on it.
func InvoiceNoticeReaderScan(r io.Reader) (*InvoiceNotice, bool, error) {
	if r == nil {
		return nil, false, nil
	}
	header, content, docs, complete, err := scanInvoiceContent(r)
	if err != nil {
		return nil, false, err
	}
	sent, dateErr := mail.ParseDate(header.Get("Date"))
	if dateErr != nil {
		sent = time.Time{}
	}
	return ExtractInvoiceNotice(content, docs, addressHeader(header.Get("From")), sent), complete, nil
}

// invoiceDocumentText is the attachments' text as one string, bounded twice:
// how many documents are read at all, and how much of each.
func invoiceDocumentText(docs []InvoiceDocument) string {
	if len(docs) == 0 {
		return ""
	}
	var b strings.Builder
	for i, doc := range docs {
		if i >= maxInvoiceDocuments {
			break
		}
		text := doc.Text
		if len(text) > maxInvoiceDocumentText {
			text = text[:maxInvoiceDocumentText]
		}
		if strings.TrimSpace(text) == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(strings.ToLower(text))
	}
	return b.String()
}

// invoiceFilenames is the attachment names as one lower-case string. A document
// is routinely named after what it is -- "Mahnung_4711.pdf" -- and that name is
// as good a declaration as a subject line.
func invoiceFilenames(docs []InvoiceDocument) string {
	names := make([]string, 0, len(docs))
	for _, doc := range docs {
		if doc.Filename != "" {
			names = append(names, strings.ToLower(doc.Filename))
		}
	}
	return strings.Join(names, " ")
}

// registrableDomain reduces a sender address to the domain an invoice number is
// unique within. It is the registrable domain rather than the full host,
// because one biller sends from "billing.example.com" one month and
// "mail.example.com" the next, and those have to be one issuer or the reminder
// and the invoice never meet.
//
// The multi-part suffixes below are the ones a European mailbox actually sees.
// A name that is not on the list and has three labels is cut to two, which is
// right far more often than it is wrong, and being wrong costs a row that does
// not merge rather than two invoices that wrongly do.
func registrableDomain(address string) string {
	value := strings.ToLower(strings.TrimSpace(address))
	if at := strings.LastIndex(value, "@"); at >= 0 {
		value = value[at+1:]
	}
	value = strings.TrimSuffix(strings.TrimPrefix(value, "www."), ".")
	if value == "" || strings.ContainsAny(value, " \t") {
		return ""
	}
	labels := strings.Split(value, ".")
	if len(labels) < 2 {
		return ""
	}
	keep := 2
	if len(labels) >= 3 {
		lastTwo := labels[len(labels)-2] + "." + labels[len(labels)-1]
		if multiPartSuffixes[lastTwo] {
			keep = 3
		}
	}
	return strings.Join(labels[len(labels)-keep:], ".")
}

// multiPartSuffixes are the two-label public suffixes worth knowing about. It is
// deliberately a short list and not a copy of the public suffix list: the cost
// of a miss is one issuer that does not merge, and carrying a few thousand
// entries to avoid that is not a trade this makes.
var multiPartSuffixes = map[string]bool{
	"co.uk": true, "org.uk": true, "ac.uk": true, "gov.uk": true, "me.uk": true,
	"com.au": true, "net.au": true, "org.au": true,
	"co.nz": true, "co.za": true, "co.jp": true, "or.jp": true, "ne.jp": true,
	"com.br": true, "com.mx": true, "com.tr": true, "com.pl": true, "com.es": true,
	"co.at": true, "or.at": true, "ac.at": true,
	"com.cn": true, "net.cn": true, "org.cn": true,
}
