package mailparse

import (
	"encoding/base64"
	"net/mail"
	"strings"
	"testing"
)

func TestInvoiceEvidenceOnlyEverTakesMachineMail(t *testing.T) {
	cases := []struct {
		name    string
		header  mail.Header
		from    string
		content CategoryContent
		want    string
	}{
		{
			// The word in the subject is enough for mail that is already only
			// a robot talking: nothing else in Notifications is hurt by being
			// filed one list further along.
			name:    "automated mail naming an invoice",
			header:  mail.Header{"Auto-Submitted": []string{"auto-generated"}},
			from:    "billing@shop.test",
			content: CategoryContent{Subject: "Ihre Rechnung für Juli"},
			want:    CategoryInvoices,
		},
		{
			name:    "automated mail about anything else",
			header:  mail.Header{"Auto-Submitted": []string{"auto-generated"}},
			from:    "shipping@shop.test",
			content: CategoryContent{Subject: "Dein Paket ist unterwegs"},
			want:    CategoryNotifications,
		},
		{
			// The classifier reads what the file calls itself, so a robot that
			// says nothing in its subject is still filed by what it attached.
			name:    "no-reply sender attaching a structured e-invoice",
			header:  mail.Header{},
			from:    "no-reply@energie.test",
			content: CategoryContent{Subject: "Ihre Unterlagen", Files: []CategoryFile{{Filename: "factur-x.xml", ContentType: "application/xml"}}},
			want:    CategoryInvoices,
		},
		{
			// An image somebody named after an invoice is a photo, not the
			// invoice, so the extension has to agree with the name.
			name:    "attachment named like an invoice but not shaped like one",
			header:  mail.Header{"Precedence": []string{"bulk"}},
			from:    "hello@shop.test",
			content: CategoryContent{Subject: "Unsere Neuheiten", Files: []CategoryFile{{Filename: "rechnung.jpg", ContentType: "image/jpeg"}}},
			want:    CategoryNotifications,
		},
		{
			// Broadcast mail is perfectly capable of putting the word in a
			// subject line, so the word alone must not empty a newsletter list.
			name:    "newsletter merely using the word",
			header:  mail.Header{"List-Unsubscribe": []string{"<https://shop.test/unsub>"}},
			from:    "news@shop.test",
			content: CategoryContent{Subject: "Rechnung gefällig? 20% auf alles"},
			want:    CategoryNewsletters,
		},
		{
			// A real billing mail routinely carries an unsubscribe route beside
			// its invoice, which is why that list is not exempt from the
			// stronger evidence.
			name:   "billing mail that also carries an unsubscribe route",
			header: mail.Header{"List-Unsubscribe": []string{"<https://telco.test/unsub>"}},
			from:   "rechnung@telco.test",
			content: CategoryContent{
				Subject: "Ihre Mobilfunkrechnung",
				Text:    "Rechnungsnummer 2024-000123, Gesamtbetrag 49,90 EUR, fällig am 15.08.",
			},
			want: CategoryInvoices,
		},
		{
			// The rule the whole category is bounded by: a person writing about
			// a contract is a person writing.
			name:    "a person sending a contract",
			header:  mail.Header{},
			from:    "ada@example.test",
			content: CategoryContent{Subject: "Vertrag zur Unterschrift", Files: []CategoryFile{{Filename: "Vertrag.pdf", ContentType: "application/pdf"}}},
			want:    CategoryRelevant,
		},
		{
			name:    "a discussion list talking about invoices",
			header:  mail.Header{"List-Post": []string{"<mailto:list@example.test>"}},
			from:    "list@example.test",
			content: CategoryContent{Subject: "Re: Rechnung Nr. 12/2024 falsch gestellt"},
			want:    CategoryForums,
		},
		{
			// "Berechnung" contains "Rechnung" and is not one.
			name:    "a word that merely contains one of the keywords",
			header:  mail.Header{"Auto-Submitted": []string{"auto-generated"}},
			from:    "no-reply@klima.test",
			content: CategoryContent{Subject: "Neuberechnung Ihres CO2-Fußabdrucks"},
			want:    CategoryNotifications,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := CategorizeWithContent(tc.header, tc.from, tc.content); got != tc.want {
				t.Fatalf("CategorizeWithContent() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestInvoiceWordsAreReadInEverySpellingTheyArriveIn(t *testing.T) {
	// The same document is written three ways in German mail, and all three
	// have to reach the same list.
	for _, subject := range []string{"Kündigungsbestätigung", "Kuendigungsbestaetigung", "Kundigungsbestatigung", "KÜNDIGUNGSBESTÄTIGUNG"} {
		content := CategoryContent{Subject: subject}
		if got := CategorizeWithContent(mail.Header{"Auto-Submitted": []string{"auto-generated"}}, "no-reply@versicherung.test", content); got != CategoryInvoices {
			t.Fatalf("subject %q classified as %q, want %q", subject, got, CategoryInvoices)
		}
	}
	// The word lists are matched against folded text, so a word that does not
	// survive folding could never match anything.
	for _, word := range invoiceWords {
		if folded := foldCategoryText(word); folded != word {
			t.Fatalf("keyword %q folds to %q and can never match", word, folded)
		}
	}
	for _, marker := range structuredInvoiceFiles {
		if folded := foldCategoryText(marker); folded != marker {
			t.Fatalf("filename marker %q folds to %q and can never match", marker, folded)
		}
	}
}

func TestInvoiceNumberAndAmountAreBothRequired(t *testing.T) {
	robot := mail.Header{"Auto-Submitted": []string{"auto-generated"}}
	// A newsletter is the list that needs document-grade evidence, so it is
	// what these cases are graded through.
	newsletter := mail.Header{"List-Unsubscribe": []string{"<https://shop.test/unsub>"}}
	cases := []struct {
		name string
		text string
		want string
	}{
		{name: "number and amount", text: "Rechnungsnummer 2024-7, Betrag 12,00 EUR", want: CategoryInvoices},
		{name: "amount alone", text: "Nur heute: 12,00 EUR sparen", want: CategoryNewsletters},
		{name: "number alone", text: "Rechnungsnummer 2024-7 wurde storniert", want: CategoryNewsletters},
		{name: "a sentence that only looks like a reference", text: "Rechnung: bitte beachten Sie unsere Hinweise", want: CategoryNewsletters},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := CategorizeWithContent(newsletter, "news@shop.test", CategoryContent{Subject: "Info", Text: tc.text}); got != tc.want {
				t.Fatalf("CategorizeWithContent(%q) = %q, want %q", tc.text, got, tc.want)
			}
		})
	}
	// The same text out of a robot is filed on the weaker grade anyway; this is
	// only here so the two grades cannot silently become one.
	if got := CategorizeWithContent(robot, "no-reply@shop.test", CategoryContent{Subject: "Ihre Rechnung"}); got != CategoryInvoices {
		t.Fatalf("automated mail with a named subject = %q, want %q", got, CategoryInvoices)
	}
}

func TestParseFilesAnInvoiceFromWhatTheMessageCarries(t *testing.T) {
	attachment := base64.StdEncoding.EncodeToString([]byte(strings.Repeat("invoice bytes ", 64)))
	raw := []byte("From: Shop <no-reply@shop.test>\r\n" +
		"Subject: Ihre Bestellung 4711\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: multipart/mixed; boundary=b1\r\n\r\n" +
		"--b1\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nVielen Dank.\r\n" +
		"--b1\r\nContent-Type: application/pdf; name=\"Rechnung_4711.pdf\"\r\n" +
		"Content-Disposition: attachment; filename=\"Rechnung_4711.pdf\"\r\n" +
		"Content-Transfer-Encoding: base64\r\n\r\n" + attachment + "\r\n" +
		"--b1--\r\n")
	parsed, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Category != CategoryInvoices {
		t.Fatalf("parsed category = %q, want %q", parsed.Category, CategoryInvoices)
	}
	// The backfill reaches the same answer from the stored message, which is
	// what keeps mail synced before this rule existed from being filed
	// differently than mail synced after it.
	if got := CategorizeReader(strings.NewReader(string(raw)), "no-reply@shop.test"); got != CategoryInvoices {
		t.Fatalf("CategorizeReader() = %q, want %q", got, CategoryInvoices)
	}
}

func TestCategorizeReaderReadsTheBodyAndTheAttachmentNames(t *testing.T) {
	// A message whose evidence is only in the body: the scan has to walk past
	// the header block to find it.
	raw := "From: no-reply@telco.test\r\n" +
		"Subject: Ihre Unterlagen\r\n" +
		"Auto-Submitted: auto-generated\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n\r\n" +
		"Guten Tag,\r\nRechnungsnummer 2024-000123 über 49,90 EUR liegt bereit.\r\n"
	if got := CategorizeReader(strings.NewReader(raw), "no-reply@telco.test"); got != CategoryInvoices {
		t.Fatalf("CategorizeReader(body evidence) = %q, want %q", got, CategoryInvoices)
	}
	// And one that says nothing: it stays where its headers put it rather than
	// being pulled into the new list by the scan.
	plain := "From: no-reply@telco.test\r\n" +
		"Subject: Wartungsarbeiten\r\n" +
		"Auto-Submitted: auto-generated\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n\r\nWir sind am Sonntag nicht erreichbar.\r\n"
	if got := CategorizeReader(strings.NewReader(plain), "no-reply@telco.test"); got != CategoryNotifications {
		t.Fatalf("CategorizeReader(plain notice) = %q, want %q", got, CategoryNotifications)
	}
}

func TestCategoryScanKeepsAttachmentNamesAndDropsTheirBytes(t *testing.T) {
	payload := base64.StdEncoding.EncodeToString([]byte(strings.Repeat("x", 4096)))
	raw := "From: no-reply@shop.test\r\n" +
		"Subject: Unterlagen\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: multipart/mixed; boundary=b1\r\n\r\n" +
		"--b1\r\nContent-Type: text/html; charset=utf-8\r\n\r\n<p>Hallo <b>Welt</b></p>\r\n" +
		"--b1\r\nContent-Type: application/xml; name=\"zugferd-invoice.xml\"\r\n" +
		"Content-Disposition: attachment; filename=\"zugferd-invoice.xml\"\r\n" +
		"Content-Transfer-Encoding: base64\r\n\r\n" + payload + "\r\n--b1--\r\n"
	_, content, _, err := scanCategoryContent(strings.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if len(content.Files) != 1 || content.Files[0].Filename != "zugferd-invoice.xml" {
		t.Fatalf("scanned files = %+v, want the one attachment name", content.Files)
	}
	// HTML-only mail still says what it is once the markup is off.
	if !strings.Contains(content.Text, "Hallo Welt") {
		t.Fatalf("scanned text = %q, want the stripped body", content.Text)
	}
	if strings.Contains(content.Text, "xxxx") {
		t.Fatal("scanned text carries attachment bytes")
	}
}

func TestATruncatedScanSaysSoInsteadOfAnsweringAsIfItReadEverything(t *testing.T) {
	// The invoice is attached behind more body text than the scan budget, which
	// is exactly the case where the scan knows less than the parse that filed
	// the message when it arrived.
	filler := strings.Repeat("Guten Tag, hier ist Ihre Sendungsverfolgung. ", (maxCategoryScanBytes/44)+1024)
	raw := "From: Shop <no-reply@shop.test>\r\n" +
		"Subject: Ihre Bestellung\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: multipart/mixed; boundary=b1\r\n\r\n" +
		"--b1\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n" + filler + "\r\n" +
		"--b1\r\nContent-Type: application/pdf; name=\"Rechnung.pdf\"\r\n" +
		"Content-Disposition: attachment; filename=\"Rechnung.pdf\"\r\n\r\nbytes\r\n--b1--\r\n"
	category, complete := CategorizeReaderScan(strings.NewReader(raw), "no-reply@shop.test")
	if complete {
		t.Fatal("a scan that stopped at its budget reported itself complete")
	}
	if category != CategoryNotifications {
		t.Fatalf("truncated scan category = %q, want %q", category, CategoryNotifications)
	}
	// The parse of the same message, which reads all of it, does find the
	// invoice -- which is why the truncated answer must not be allowed to
	// replace one this produced.
	parsed, err := Parse([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Category != CategoryInvoices {
		t.Fatalf("parsed category = %q, want %q", parsed.Category, CategoryInvoices)
	}
	// A message that fits is reported complete, or nothing could ever be
	// re-classified.
	small := "From: no-reply@shop.test\r\nSubject: Ihre Rechnung\r\n\r\nDanke.\r\n"
	if category, complete := CategorizeReaderScan(strings.NewReader(small), "no-reply@shop.test"); !complete || category != CategoryInvoices {
		t.Fatalf("small message = %q complete=%v, want %q and true", category, complete, CategoryInvoices)
	}
}
