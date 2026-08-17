package xoauth2

import (
	"errors"
	"net/smtp"
	"strings"
	"testing"
)

func TestInitialResponseUsesGooglesLayout(t *testing.T) {
	got := string(initialResponse("user@gmail.example.test", "ya29.token"))
	want := "user=user@gmail.example.test\x01auth=Bearer ya29.token\x01\x01"
	if got != want {
		t.Fatalf("initial response = %q, want %q", got, want)
	}
}

func TestSASLClientStartsWithTheCredentials(t *testing.T) {
	mechanism, response, err := NewSASLClient("user@gmail.example.test", "ya29.token").Start()
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if mechanism != "XOAUTH2" {
		t.Fatalf("mechanism = %q, want XOAUTH2", mechanism)
	}
	if !strings.Contains(string(response), "auth=Bearer ya29.token") {
		t.Fatalf("response %q does not carry the token", response)
	}
}

// The empty answer is what lets the caller read Google's real error instead of
// aborting the command, so it is asserted rather than assumed.
func TestSASLClientAnswersTheErrorChallengeWithAnEmptyLine(t *testing.T) {
	client := NewSASLClient("user@gmail.example.test", "ya29.token")
	if _, _, err := client.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	response, err := client.Next([]byte(`{"status":"400","schemes":"Bearer"}`))
	if err != nil {
		t.Fatalf("next: %v", err)
	}
	if response == nil {
		t.Fatal("challenge answered with nil, which sends no line at all")
	}
	if len(response) != 0 {
		t.Fatalf("challenge answered with %q, want an empty response", response)
	}
	if _, err := client.Next([]byte("{}")); err == nil {
		t.Fatal("a second challenge should be refused rather than answered again")
	}
}

func TestSASLClientRejectsMissingCredentials(t *testing.T) {
	for _, testCase := range []struct{ name, username, token string }{
		{"no username", "  ", "ya29.token"},
		{"no token", "user@gmail.example.test", ""},
		{"separator in token", "user@gmail.example.test", "ya29\x01auth=Bearer other"},
		{"separator in username", "user\x01other@gmail.example.test", "ya29.token"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if _, _, err := NewSASLClient(testCase.username, testCase.token).Start(); err == nil {
				t.Fatal("expected an error before anything reaches the wire")
			}
		})
	}
}

func TestSMTPAuthRequiresAnEncryptedConnection(t *testing.T) {
	_, _, err := NewSMTPAuth("user@gmail.example.test", "ya29.token").
		Start(&smtp.ServerInfo{Name: "smtp.gmail.com", TLS: false})
	if !errors.Is(err, ErrCleartext) {
		t.Fatalf("error = %v, want ErrCleartext", err)
	}
	if err != nil && strings.Contains(err.Error(), "ya29.token") {
		t.Fatal("the refusal message must not quote the access token")
	}
}

func TestSMTPAuthAllowsTLSAndLoopback(t *testing.T) {
	for _, server := range []*smtp.ServerInfo{
		{Name: "smtp.gmail.com", TLS: true, Auth: []string{"PLAIN", "XOAUTH2"}},
		{Name: "127.0.0.1", TLS: false},
		{Name: "localhost:2525", TLS: false},
		{Name: "[::1]:2525", TLS: false},
	} {
		mechanism, response, err := NewSMTPAuth("user@gmail.example.test", "ya29.token").Start(server)
		if err != nil {
			t.Fatalf("start against %q: %v", server.Name, err)
		}
		if mechanism != "XOAUTH2" || len(response) == 0 {
			t.Fatalf("start against %q produced %q/%q", server.Name, mechanism, response)
		}
	}
}

func TestSMTPAuthReportsAServerWithoutTheMechanism(t *testing.T) {
	_, _, err := NewSMTPAuth("user@gmail.example.test", "ya29.token").
		Start(&smtp.ServerInfo{Name: "smtp.example.test", TLS: true, Auth: []string{"PLAIN", "LOGIN"}})
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("error = %v, want ErrUnsupported", err)
	}
}

func TestSMTPAuthAnswersTheErrorChallengeOnce(t *testing.T) {
	auth := NewSMTPAuth("user@gmail.example.test", "ya29.token")
	if _, _, err := auth.Start(&smtp.ServerInfo{Name: "smtp.gmail.com", TLS: true}); err != nil {
		t.Fatalf("start: %v", err)
	}
	response, err := auth.Next([]byte(`{"status":"400"}`), true)
	if err != nil {
		t.Fatalf("next: %v", err)
	}
	if response == nil || len(response) != 0 {
		t.Fatalf("challenge answered with %q, want an empty response", response)
	}
	if _, err := auth.Next([]byte("{}"), true); err == nil {
		t.Fatal("a second challenge should be refused")
	}
	if response, err := auth.Next(nil, false); err != nil || response != nil {
		t.Fatalf("completed exchange returned %q/%v", response, err)
	}
}
