package security

import (
	"strings"
	"testing"

	"rolltop/backend/plugins"
)

func TestDetectsAndStripsInlinePGPSignedBody(t *testing.T) {
	raw := strings.Join([]string{
		"From: sender@example.test",
		"To: archive@example.test",
		"Subject: signed text",
		"Content-Type: text/plain; charset=utf-8",
		"",
		"-----BEGIN PGP SIGNED MESSAGE-----",
		"Hash: SHA512",
		"",
		"This is a signed message",
		"-----BEGIN PGP SIGNATURE-----",
		"",
		"wrfakebase64",
		"-----END PGP SIGNATURE-----",
	}, "\r\n")
	bodyText := strings.Join([]string{
		"-----BEGIN PGP SIGNED MESSAGE-----",
		"Hash: SHA512",
		"",
		"This is a signed message",
		"-----BEGIN PGP SIGNATURE-----",
		"",
		"wrfakebase64",
		"-----END PGP SIGNATURE-----",
	}, "\r\n")

	state := Detect([]byte(raw), plugins.MessageBody{})
	if state.Encrypted || !state.Signed {
		t.Fatalf("state = %+v", state)
	}
	transform := Transform([]byte(raw), state, plugins.MessageBody{Purpose: "storage", Text: bodyText})
	if !transform.Applied {
		t.Fatal("signed body transform was not applied")
	}
	if transform.Body.Text != "This is a signed message" {
		t.Fatalf("text = %q", transform.Body.Text)
	}
	if strings.Contains(transform.Body.Text, "BEGIN PGP") || strings.Contains(transform.Body.Text, "SIGNATURE") {
		t.Fatalf("signature armor was retained: %q", transform.Body.Text)
	}
}

func TestInlinePGPEncryptedTransformsStorageAndDisplay(t *testing.T) {
	raw := strings.Join([]string{
		"From: sender@example.test",
		"To: archive@example.test",
		"Subject: encrypted text",
		"Content-Type: text/plain; charset=utf-8",
		"",
		"-----BEGIN PGP MESSAGE-----",
		"",
		"wcDMA123",
		"-----END PGP MESSAGE-----",
	}, "\r\n")

	state := Detect([]byte(raw), plugins.MessageBody{})
	if !state.Encrypted || state.Signed {
		t.Fatalf("state = %+v", state)
	}
	storage := Transform([]byte(raw), state, plugins.MessageBody{Purpose: "storage", Text: raw})
	if !storage.Applied || !storage.DropAttachments || storage.Body.Text != "" || storage.Body.HTML != "" {
		t.Fatalf("storage transform = %+v", storage)
	}
	display := Transform([]byte(raw), state, plugins.MessageBody{Purpose: "display"})
	if !display.Applied || display.Body.HTML != "" {
		t.Fatalf("display transform = %+v", display)
	}
	if !strings.Contains(display.Body.Text, "-----BEGIN PGP MESSAGE-----") || !strings.Contains(display.Body.Text, "wcDMA123") {
		t.Fatalf("display text did not keep ciphertext: %q", display.Body.Text)
	}
}

func TestPGPMIMEEncryptedDisplayShowsCiphertextPart(t *testing.T) {
	raw := strings.Join([]string{
		"From: sender@example.test",
		"To: archive@example.test",
		"Subject: encrypted mime",
		"MIME-Version: 1.0",
		`Content-Type: multipart/encrypted; protocol="application/pgp-encrypted"; boundary="pgp-boundary"`,
		"",
		"--pgp-boundary",
		"Content-Type: application/pgp-encrypted",
		"",
		"Version: 1",
		"--pgp-boundary",
		`Content-Type: application/octet-stream; name="encrypted.asc"`,
		`Content-Disposition: inline; filename="encrypted.asc"`,
		"Content-Transfer-Encoding: 7bit",
		"",
		"-----BEGIN PGP MESSAGE-----",
		"",
		"wcDMA456",
		"-----END PGP MESSAGE-----",
		"--pgp-boundary--",
	}, "\r\n")

	state := Detect([]byte(raw), plugins.MessageBody{})
	if !state.Encrypted || state.Signed {
		t.Fatalf("state = %+v", state)
	}
	display := Transform([]byte(raw), state, plugins.MessageBody{Purpose: "display"})
	if !display.Applied || display.Body.HTML != "" {
		t.Fatalf("display transform = %+v", display)
	}
	if !strings.Contains(display.Body.Text, "-----BEGIN PGP MESSAGE-----") || !strings.Contains(display.Body.Text, "wcDMA456") {
		t.Fatalf("display text did not keep PGP/MIME ciphertext: %q", display.Body.Text)
	}
	if strings.Contains(display.Body.Text, "Version: 1") {
		t.Fatalf("display text included PGP/MIME version part: %q", display.Body.Text)
	}
}

func TestDetectIgnoresPGPTypeNamesQuotedInBody(t *testing.T) {
	// A normal message that merely quotes the PGP/MIME type names in its body --
	// a reply explaining how PGP works -- used to be declared encrypted by the
	// substring heuristic, which then dropped its attachments. The structural
	// detector reads the real Content-Type (text/plain) and leaves it alone.
	raw := strings.Join([]string{
		"From: sender@example.test",
		"To: archive@example.test",
		"Subject: Re: how PGP/MIME works",
		"Content-Type: text/plain; charset=utf-8",
		"",
		"You asked how it looks on the wire. The outer part is",
		`Content-Type: multipart/encrypted; protocol="application/pgp-encrypted"`,
		"and it wraps an application/pgp-encrypted control part plus an",
		"application/pgp-signature is what a signed message carries. Hope it helps!",
	}, "\r\n")
	state := Detect([]byte(raw), plugins.MessageBody{Purpose: "storage", Text: "quoted the types above"})
	if state.Encrypted || state.Signed {
		t.Fatalf("a message merely quoting the PGP MIME types was flagged: %+v", state)
	}
}

func TestDetectRecognizesPGPMIMEEncrypted(t *testing.T) {
	raw := strings.Join([]string{
		"From: sender@example.test",
		"To: rcpt@example.test",
		"Subject: secret",
		"MIME-Version: 1.0",
		`Content-Type: multipart/encrypted; protocol="application/pgp-encrypted"; boundary="b"`,
		"",
		"--b",
		"Content-Type: application/pgp-encrypted",
		"",
		"Version: 1",
		"--b",
		"Content-Type: application/octet-stream",
		"",
		"-----BEGIN PGP MESSAGE-----",
		"ciphertext",
		"-----END PGP MESSAGE-----",
		"--b--",
	}, "\r\n")
	state := Detect([]byte(raw), plugins.MessageBody{})
	if !state.Encrypted {
		t.Fatalf("PGP/MIME encrypted message not detected: %+v", state)
	}
}

func TestDetectRecognizesPGPMIMESigned(t *testing.T) {
	raw := strings.Join([]string{
		"From: sender@example.test",
		"To: rcpt@example.test",
		"Subject: signed",
		"MIME-Version: 1.0",
		`Content-Type: multipart/signed; protocol="application/pgp-signature"; micalg=pgp-sha256; boundary="b"`,
		"",
		"--b",
		"Content-Type: text/plain; charset=utf-8",
		"",
		"hello",
		"--b",
		"Content-Type: application/pgp-signature",
		"",
		"-----BEGIN PGP SIGNATURE-----",
		"sig",
		"-----END PGP SIGNATURE-----",
		"--b--",
	}, "\r\n")
	state := Detect([]byte(raw), plugins.MessageBody{})
	if state.Encrypted || !state.Signed {
		t.Fatalf("PGP/MIME signed message not detected as signed only: %+v", state)
	}
}
