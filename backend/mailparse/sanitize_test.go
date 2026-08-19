// File overview: Tests for repairing mail text into PostgreSQL-storable UTF-8.

package mailparse

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestSanitizeTextRepairsUndeclaredLatin1(t *testing.T) {
	// "änd" written as ISO-8859-1 is the byte sequence PostgreSQL refused:
	// invalid byte sequence for encoding "UTF8": 0xe4 0x6e 0x64.
	got := SanitizeText("Wichtige \xc4nderung am \xe4nderungsauftrag")
	if want := "Wichtige Änderung am änderungsauftrag"; got != want {
		t.Fatalf("repaired header = %q, want %q", got, want)
	}
	if !utf8.ValidString(got) {
		t.Fatalf("repaired header is not valid UTF-8: %q", got)
	}
}

func TestSanitizeTextRepairsWindows1252Punctuation(t *testing.T) {
	got := SanitizeText("say \x93hello\x94 \x96 now")
	if want := "say “hello” – now"; got != want {
		t.Fatalf("repaired punctuation = %q, want %q", got, want)
	}
}

func TestSanitizeTextKeepsValidUTF8AroundBadBytes(t *testing.T) {
	got := SanitizeText("Grüße \xe4 日本語 �")
	if want := "Grüße ä 日本語 �"; got != want {
		t.Fatalf("repaired mixed text = %q, want %q", got, want)
	}
}

func TestSanitizeTextDropsNULBytes(t *testing.T) {
	got := SanitizeText("before\x00after")
	if want := "beforeafter"; got != want {
		t.Fatalf("sanitized text = %q, want %q", got, want)
	}
}

func TestSanitizeTextLeavesValidTextUntouched(t *testing.T) {
	value := "Grüße aus München — 日本語 — �"
	if got := SanitizeText(value); got != value {
		t.Fatalf("sanitized valid text = %q, want %q", got, value)
	}
}

func TestSanitizeTextRepairsUndefinedWindows1252Byte(t *testing.T) {
	got := SanitizeText("bad \x81 byte")
	if want := "bad � byte"; got != want {
		t.Fatalf("sanitized text = %q, want %q", got, want)
	}
}

func TestParseRepairsRawLatin1HeadersAndBody(t *testing.T) {
	raw := strings.Join([]string{
		"From: \"M\xfcller, Bj\xf6rn\" <bjoern@example.test>",
		"To: \x84Empf\xe4nger\x93 <inbox@example.test>",
		"Subject: Wichtige \xc4nderung",
		"Content-Type: text/plain",
		"",
		"Sch\xf6nen Gru\xdf, die \xc4nderung ist erledigt.",
	}, "\r\n")
	parsed, err := Parse([]byte(raw))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	for name, value := range map[string]string{
		"subject": parsed.Subject,
		"from":    parsed.From,
		"to":      parsed.To,
		"text":    parsed.Text,
	} {
		if !utf8.ValidString(value) {
			t.Fatalf("%s is not valid UTF-8: %q", name, value)
		}
	}
	if parsed.Subject != "Wichtige Änderung" {
		t.Fatalf("subject = %q, want %q", parsed.Subject, "Wichtige Änderung")
	}
	if !strings.Contains(parsed.From, "Müller, Björn") {
		t.Fatalf("from = %q, want the repaired display name", parsed.From)
	}
	if !strings.Contains(parsed.Text, "Schönen Gruß, die Änderung ist erledigt.") {
		t.Fatalf("text = %q, want the repaired body", parsed.Text)
	}
}

// A display name is quoted before it is joined, and strconv.Quote escapes bytes
// that are not valid UTF-8 into literal \xNN text - valid UTF-8 that no later
// repair would recognise as broken. The repair therefore has to happen first.
func TestParseRepairsDisplayNameInEncodedWordWithWrongCharset(t *testing.T) {
	raw := strings.Join([]string{
		"From: =?UTF-8?Q?M=FCller=2C_Bj=F6rn?= <bjoern@example.test>",
		"To: =?UTF-8?Q?Empf=E4nger?= <inbox@example.test>",
		"Subject: =?UTF-8?Q?Wichtige_=C4nderung?=",
		"Content-Type: text/plain",
		"",
		"body",
	}, "\r\n")
	parsed, err := Parse([]byte(raw))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if want := `"Müller, Björn" <bjoern@example.test>`; parsed.From != want {
		t.Fatalf("from = %q, want %q", parsed.From, want)
	}
	if want := `"Empfänger" <inbox@example.test>`; parsed.To != want {
		t.Fatalf("to = %q, want %q", parsed.To, want)
	}
	if strings.Contains(parsed.From, `\x`) {
		t.Fatalf("from kept an escaped raw byte: %q", parsed.From)
	}
}

func TestParseRepairsBodyThatLiesAboutItsCharset(t *testing.T) {
	raw := strings.Join([]string{
		"From: sender@example.test",
		"To: inbox@example.test",
		"Subject: charset mismatch",
		"Content-Type: text/plain; charset=utf-8",
		"",
		"Gru\xdf aus M\xfcnchen\x00.",
	}, "\r\n")
	parsed, err := Parse([]byte(raw))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !utf8.ValidString(parsed.Text) {
		t.Fatalf("body is not valid UTF-8: %q", parsed.Text)
	}
	if strings.ContainsRune(parsed.Text, 0) {
		t.Fatalf("body still carries a NUL byte: %q", parsed.Text)
	}
	if !strings.Contains(parsed.Text, "Gruß aus München") {
		t.Fatalf("text = %q, want the repaired body", parsed.Text)
	}
}

func TestParseRepairsAttachmentFilename(t *testing.T) {
	raw := strings.Join([]string{
		"From: sender@example.test",
		"To: inbox@example.test",
		"Subject: attachment",
		"Content-Type: multipart/mixed; boundary=sep",
		"",
		"--sep",
		"Content-Type: text/plain",
		"",
		"body",
		"--sep",
		"Content-Type: application/pdf; name=\"\xc4nderung.pdf\"",
		"Content-Disposition: attachment; filename=\"\xc4nderung.pdf\"",
		"",
		"payload",
		"--sep--",
	}, "\r\n")
	parsed, err := Parse([]byte(raw))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(parsed.Files) != 1 {
		t.Fatalf("attachments = %d, want 1", len(parsed.Files))
	}
	if got := parsed.Files[0].Filename; got != "Änderung.pdf" {
		t.Fatalf("filename = %q, want %q", got, "Änderung.pdf")
	}
}

func TestParseDisplayBodyRepairsRawLatin1(t *testing.T) {
	raw := strings.Join([]string{
		"From: sender@example.test",
		"Subject: display",
		"Content-Type: text/plain",
		"",
		"Sch\xf6ne Gr\xfc\xdfe",
	}, "\r\n")
	text, _, err := ParseDisplayBody(strings.NewReader(raw))
	if err != nil {
		t.Fatalf("parse display body: %v", err)
	}
	if !utf8.ValidString(text) {
		t.Fatalf("display text is not valid UTF-8: %q", text)
	}
	if !strings.Contains(text, "Schöne Grüße") {
		t.Fatalf("display text = %q, want the repaired body", text)
	}
}
