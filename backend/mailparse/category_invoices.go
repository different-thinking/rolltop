// File overview: The evidence that moves machine mail into Invoices &
// Contracts, and the text folding the rules read it through.
//
// The category is deliberately carved out of machine mail only. A message the
// headers already called a person writing stays where it is however much it
// talks about a contract: the cost of wrongly filing a person's mail into a
// paperwork list is a conversation the reader never sees, while the cost of
// missing one invoice is a message that stays in Notifications, where it would
// have been anyway. The per-sender correction is what covers the rest.
//
// Evidence comes in two grades because the two sources it can be pulled out of
// are not equally trustworthy. Mail that is only automated (Notifications) is
// filed on the word it uses about itself; broadcast mail (Newsletters) needs
// something only a real document carries, because a marketing mail is perfectly
// capable of putting "Rechnung" in a subject line.

package mailparse

import (
	"regexp"
	"strings"
)

// invoiceEvidence grades what a message says about itself.
type invoiceEvidence int

const (
	// invoiceEvidenceNone is a message that never mentions paperwork.
	invoiceEvidenceNone invoiceEvidence = iota
	// invoiceEvidenceNamed is a subject line using one of the words, and
	// nothing else to back it up.
	invoiceEvidenceNamed
	// invoiceEvidenceDocument is something only a real document carries: a
	// structured e-invoice, a file named after what it is, or a document number
	// beside an amount of money.
	invoiceEvidenceDocument
)

// applyInvoiceEvidence upgrades an already-decided category when the message is
// a document rather than the announcement its headers made it look like. It is
// the one place the new category is reached from, so what it may take -- and
// what it may never take -- is written once.
func applyInvoiceEvidence(category string, content CategoryContent) string {
	switch category {
	case CategoryNotifications:
		if invoiceEvidenceOf(content) >= invoiceEvidenceNamed {
			return CategoryInvoices
		}
	case CategoryNewsletters:
		// A billing mail from a large provider routinely carries an
		// unsubscribe route beside its invoice, so this list cannot be exempt.
		// It does have to clear the higher bar.
		if invoiceEvidenceOf(content) >= invoiceEvidenceDocument {
			return CategoryInvoices
		}
	}
	// Relevant and Forums are untouched on purpose: mail from a person, and a
	// discussion list a person can answer, are not paperwork however they read.
	return category
}

// invoiceEvidenceOf grades one message. The attachments are read first because
// a file that names itself is the one signal a sender cannot produce by
// accident.
func invoiceEvidenceOf(content CategoryContent) invoiceEvidence {
	for _, file := range content.Files {
		if fileInvoiceEvidence(file) == invoiceEvidenceDocument {
			return invoiceEvidenceDocument
		}
	}
	subject := foldCategoryText(content.Subject)
	body := foldCategoryText(content.Text)
	// A number and an amount together are a document being sent, not a word
	// being used: either alone is ordinary in mail that is not paperwork.
	if invoiceNumberRE.MatchString(subject+" "+body) && invoiceAmountRE.MatchString(subject+" "+body) {
		return invoiceEvidenceDocument
	}
	// Only the subject is read for the weaker grade. A body footer carries
	// "Rechnungsadresse" and an unsubscribe blurb in mail that has nothing to
	// do with an invoice, and matching those would empty Notifications into
	// this list.
	if containsAny(subject, invoiceWords) {
		return invoiceEvidenceNamed
	}
	return invoiceEvidenceNone
}

// fileInvoiceEvidence grades one attachment by its name. The extension has to
// agree with the name: "Rechnung.jpg" is a photo somebody sent, while
// "Rechnung.pdf" is the invoice itself.
func fileInvoiceEvidence(file CategoryFile) invoiceEvidence {
	name := foldCategoryText(file.Filename)
	if name == "" {
		return invoiceEvidenceNone
	}
	// The e-invoicing standards mandate the filename of the structured part, so
	// this is the one attachment name that means what it says without help.
	if containsAny(name, structuredInvoiceFiles) {
		return invoiceEvidenceDocument
	}
	if !hasAnySuffix(name, invoiceDocumentExtensions) {
		return invoiceEvidenceNone
	}
	if containsAny(name, invoiceWords) {
		return invoiceEvidenceDocument
	}
	return invoiceEvidenceNone
}

// invoiceWords are what an invoice, a receipt, or a contract calls itself, in
// the folded spelling foldCategoryText produces. German compounds are matched
// as substrings on purpose -- "Jahresrechnung" and "Rechnungskorrektur" are the
// same document as "Rechnung" -- which is why the words that merely contain one
// of these are removed from the text first (invoiceWordNoise).
var invoiceWords = []string{
	"rechnung",
	"faktura",
	"gutschrift",
	"zahlungsbestatigung",
	"zahlungserinnerung",
	"zahlungsavis",
	"lastschrift",
	"mahnung",
	"quittung",
	"beleg",
	"kontoauszug",
	"vertrag",
	"kundigung",
	"versicherungsschein",
	"versicherungspolice",
	"geschaftsbedingungen",
	"nutzungsbedingungen",
	"invoice",
	"receipt",
	"contract",
	"billing statement",
	"account statement",
	"payment confirmation",
	"terms of service",
	"terms and conditions",
}

// invoiceWordNoise are the words that contain one of the above without being
// it. They are cut out of the text before the search rather than guarded
// against with a word boundary, because a boundary would also lose the German
// compounds the substring match exists for.
var invoiceWordNoise = strings.NewReplacer(
	// Longest first: at one position the replacer takes the first listed match.
	"belegschaft", " ",
	"belegung", " ",
	"belegt", " ",
	"berechnung", " ",
	"contractor", " ",
)

// structuredInvoiceFiles are the filename markers the e-invoicing standards
// prescribe for the machine-readable part of an invoice.
var structuredInvoiceFiles = []string{"factur-x", "facturx", "zugferd", "xrechnung"}

// invoiceDocumentExtensions are what a document arrives as. An image or an
// archive named after an invoice is not the invoice.
var invoiceDocumentExtensions = []string{".pdf", ".xml", ".doc", ".docx", ".odt", ".rtf"}

// invoiceNumberRE matches a document naming its own number: the word, whatever
// compound it sits in, then the number itself. A digit is required, because
// "Rechnung: bitte beachten" is a sentence and not a document reference.
var invoiceNumberRE = regexp.MustCompile(`(?:rechnung|invoice|beleg|quittung|receipt|faktura|vertrag|contract)[a-z]*\s*(?:nr\.?|nummer|no\.?|number|#|:)\s*[a-z0-9._/-]*\d[a-z0-9._/-]*`)

// invoiceAmountRE matches an amount of money on either side of its currency.
// Cents are required where the currency trails, because a bare number in front
// of "EUR" is common in prose ("save 20 EUR"); a leading currency symbol is
// already specific enough without them.
var invoiceAmountRE = regexp.MustCompile(`(?:€|eur|chf|usd|\$)\s?\d+(?:[.,\s]\d{3})*(?:[.,]\d{2})?|\d+(?:[.,\s]\d{3})*[.,]\d{2}\s?(?:€|eur|chf|usd|\$)`)

// categoryTextFolder normalizes the spellings the same word arrives in. A
// German umlaut is written three ways in mail -- "Kündigung", "Kuendigung",
// "Kundigung" -- and all three name the same document, so all three have to
// fold to one string before anything is compared. Both the text and the word
// lists go through it, which is what keeps them in step.
var categoryTextFolder = strings.NewReplacer(
	"ä", "a", "ö", "o", "ü", "u", "ß", "ss",
	"ae", "a", "oe", "o", "ue", "u",
)

// foldCategoryText lowercases, folds, and drops the near-misses, leaving the
// one spelling the rules are written in.
func foldCategoryText(value string) string {
	if value == "" {
		return ""
	}
	folded := categoryTextFolder.Replace(strings.ToLower(value))
	return invoiceWordNoise.Replace(folded)
}

func containsAny(haystack string, needles []string) bool {
	for _, needle := range needles {
		if strings.Contains(haystack, needle) {
			return true
		}
	}
	return false
}

func hasAnySuffix(value string, suffixes []string) bool {
	for _, suffix := range suffixes {
		if strings.HasSuffix(value, suffix) {
			return true
		}
	}
	return false
}
