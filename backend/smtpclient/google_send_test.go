package smtpclient

import (
	"bufio"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"

	"rolltop/backend/googletoken"
	"rolltop/backend/store"
)

// fakeSMTPServer accepts one message per connection and records the credentials
// it was offered, so a test can tell a retry from a first attempt.
type fakeSMTPServer struct {
	listener net.Listener

	mu             sync.Mutex
	acceptedTokens map[string]bool
	authPayloads   []string
	delivered      int
}

func startFakeSMTPServer(t *testing.T, acceptedTokens ...string) *fakeSMTPServer {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &fakeSMTPServer{listener: listener, acceptedTokens: map[string]bool{}}
	for _, token := range acceptedTokens {
		server.acceptedTokens[token] = true
	}
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go server.handle(conn)
		}
	}()
	t.Cleanup(func() { _ = listener.Close() })
	return server
}

func (s *fakeSMTPServer) handle(conn net.Conn) {
	defer conn.Close()
	write := func(format string, args ...any) { fmt.Fprintf(conn, format, args...) }
	write("220 fake ESMTP ready\r\n")
	reader := bufio.NewReader(conn)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		switch strings.ToUpper(fields[0]) {
		case "EHLO", "HELO":
			write("250-fake greets you\r\n250 AUTH XOAUTH2 PLAIN\r\n")
		case "AUTH":
			if !s.authenticate(conn, reader, fields) {
				continue
			}
		case "MAIL", "RCPT":
			write("250 ok\r\n")
		case "DATA":
			write("354 send it\r\n")
			for {
				body, err := reader.ReadString('\n')
				if err != nil {
					return
				}
				if strings.TrimRight(body, "\r\n") == "." {
					break
				}
			}
			s.mu.Lock()
			s.delivered++
			s.mu.Unlock()
			write("250 delivered\r\n")
		case "QUIT":
			write("221 bye\r\n")
			return
		default:
			write("250 ok\r\n")
		}
	}
}

func (s *fakeSMTPServer) authenticate(conn net.Conn, reader *bufio.Reader, fields []string) bool {
	write := func(format string, args ...any) { fmt.Fprintf(conn, format, args...) }
	if len(fields) < 3 || !strings.EqualFold(fields[1], "XOAUTH2") {
		write("504 unsupported mechanism\r\n")
		return false
	}
	decoded, err := base64.StdEncoding.DecodeString(fields[2])
	if err != nil {
		write("501 malformed\r\n")
		return false
	}
	payload := string(decoded)
	s.mu.Lock()
	s.authPayloads = append(s.authPayloads, payload)
	accepted := false
	for token := range s.acceptedTokens {
		if strings.Contains(payload, "auth=Bearer "+token+"\x01") {
			accepted = true
			break
		}
	}
	s.mu.Unlock()
	if accepted {
		write("235 authenticated\r\n")
		return true
	}
	// The challenge-then-fail exchange Gmail performs on a bad token.
	write("334 %s\r\n", base64.StdEncoding.EncodeToString([]byte(`{"status":"400"}`)))
	if _, err := reader.ReadString('\n'); err != nil {
		return false
	}
	write("535 invalid credentials\r\n")
	return false
}

func (s *fakeSMTPServer) account(t *testing.T) store.MailAccount {
	t.Helper()
	host, port, err := net.SplitHostPort(s.listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	number, err := strconv.Atoi(port)
	if err != nil {
		t.Fatal(err)
	}
	return store.MailAccount{
		ID:                 5,
		UserID:             2,
		Email:              "user@gmail.example.test",
		SMTPHost:           host,
		SMTPPort:           number,
		SMTPUsername:       "user@gmail.example.test",
		AuthType:           store.AuthTypeGoogleOAuth,
		GoogleConnectionID: 9,
	}
}

func (s *fakeSMTPServer) payloads() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.authPayloads...)
}

func sendTestMessage(sender *Sender, account store.MailAccount) error {
	return sender.SendRaw(context.Background(), account, []string{"recipient@example.test"},
		[]byte("From: user@gmail.example.test\r\nTo: recipient@example.test\r\n\r\nbody\r\n"))
}

func TestSendRawAuthenticatesGoogleAccountsWithXOAUTH2(t *testing.T) {
	server := startFakeSMTPServer(t, "good-token")
	sender := &Sender{Tokens: &googletoken.StubTokenSource{Tokens: []string{"good-token"}}}
	if err := sendTestMessage(sender, server.account(t)); err != nil {
		t.Fatalf("send: %v", err)
	}
	payloads := server.payloads()
	if len(payloads) != 1 {
		t.Fatalf("authentication attempts = %d, want 1", len(payloads))
	}
	if want := "user=user@gmail.example.test\x01auth=Bearer good-token\x01\x01"; payloads[0] != want {
		t.Fatalf("payload = %q, want %q", payloads[0], want)
	}
	server.mu.Lock()
	defer server.mu.Unlock()
	if server.delivered != 1 {
		t.Fatalf("delivered messages = %d, want 1", server.delivered)
	}
}

// Retrying is only safe because authentication precedes MAIL FROM; the delivery
// count is asserted so a future change that moves auth later cannot silently
// turn this into a duplicate send.
func TestSendRawRetriesOnceWithARefreshedTokenAndDeliversOnce(t *testing.T) {
	server := startFakeSMTPServer(t, "fresh-token")
	tokens := &googletoken.StubTokenSource{Tokens: []string{"stale-token", "fresh-token"}}
	if err := sendTestMessage(&Sender{Tokens: tokens}, server.account(t)); err != nil {
		t.Fatalf("send after refresh: %v", err)
	}
	if attempts := len(server.payloads()); attempts != 2 {
		t.Fatalf("authentication attempts = %d, want 2", attempts)
	}
	if tokens.Forced != 1 {
		t.Fatalf("forced refreshes = %d, want 1", tokens.Forced)
	}
	server.mu.Lock()
	defer server.mu.Unlock()
	if server.delivered != 1 {
		t.Fatalf("delivered messages = %d, want exactly 1", server.delivered)
	}
}

func TestSendRawReportsASenderThatCannotMintTokens(t *testing.T) {
	server := startFakeSMTPServer(t, "good-token")
	if err := sendTestMessage(&Sender{}, server.account(t)); !errors.Is(err, googletoken.ErrNoTokenSource) {
		t.Fatalf("send without a token source = %v, want googletoken.ErrNoTokenSource", err)
	}
}
