package imapclient

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"

	mmcrypto "rolltop/backend/crypto"
	"rolltop/backend/store"
)

// fakeIMAPServer speaks just enough IMAP to exercise authentication: a greeting
// that advertises XOAUTH2, the AUTHENTICATE exchange including Google's
// error-challenge round trip, and LOGIN for the password path.
type fakeIMAPServer struct {
	listener net.Listener

	mu             sync.Mutex
	acceptedTokens map[string]bool
	authPayloads   []string
	logins         []string
	searches       []string
	advertiseOAuth bool
}

func startFakeIMAPServer(t *testing.T, acceptedTokens ...string) *fakeIMAPServer {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &fakeIMAPServer{
		listener:       listener,
		acceptedTokens: map[string]bool{},
		advertiseOAuth: true,
	}
	for _, token := range acceptedTokens {
		server.acceptedTokens[token] = true
	}
	go server.serve()
	t.Cleanup(func() { _ = listener.Close() })
	return server
}

func (s *fakeIMAPServer) serve() {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return
		}
		go s.handle(conn)
	}
}

func (s *fakeIMAPServer) handle(conn net.Conn) {
	defer conn.Close()
	capabilities := "IMAP4rev1"
	s.mu.Lock()
	if s.advertiseOAuth {
		capabilities += " AUTH=XOAUTH2"
	}
	s.mu.Unlock()
	if _, err := fmt.Fprintf(conn, "* OK [CAPABILITY %s] fake server ready\r\n", capabilities); err != nil {
		return
	}
	reader := bufio.NewReader(conn)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return
		}
		tag, command := fields[0], strings.ToUpper(fields[1])
		switch command {
		case "CAPABILITY":
			fmt.Fprintf(conn, "* CAPABILITY %s\r\n%s OK capability\r\n", capabilities, tag)
		case "LOGIN":
			s.mu.Lock()
			s.logins = append(s.logins, strings.TrimSpace(strings.Join(fields[2:], " ")))
			s.mu.Unlock()
			fmt.Fprintf(conn, "%s OK logged in\r\n", tag)
		case "AUTHENTICATE":
			s.authenticate(conn, reader, tag, fields)
		case "SELECT", "EXAMINE":
			fmt.Fprintf(conn, "* 5 EXISTS\r\n* OK [UIDVALIDITY 1]\r\n* OK [UIDNEXT 99]\r\n%s OK [READ-ONLY] selected\r\n", tag)
		case "UID":
			s.mu.Lock()
			s.searches = append(s.searches, strings.TrimSpace(line))
			s.mu.Unlock()
			fmt.Fprintf(conn, "* SEARCH\r\n%s OK search complete\r\n", tag)
		case "LOGOUT":
			fmt.Fprintf(conn, "* BYE\r\n%s OK logout\r\n", tag)
			return
		default:
			fmt.Fprintf(conn, "%s BAD unsupported\r\n", tag)
		}
	}
}

func (s *fakeIMAPServer) authenticate(conn net.Conn, reader *bufio.Reader, tag string, fields []string) {
	if len(fields) < 3 || !strings.EqualFold(fields[2], "XOAUTH2") {
		fmt.Fprintf(conn, "%s NO unsupported mechanism\r\n", tag)
		return
	}
	fmt.Fprint(conn, "+ \r\n")
	encoded, err := reader.ReadString('\n')
	if err != nil {
		return
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded))
	if err != nil {
		fmt.Fprintf(conn, "%s NO malformed response\r\n", tag)
		return
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
		fmt.Fprintf(conn, "%s OK authenticated\r\n", tag)
		return
	}
	// Google answers a bad token with a base64 blob and only fails the command
	// after the client acknowledges it with an empty line.
	blob := base64.StdEncoding.EncodeToString([]byte(`{"status":"400","schemes":"Bearer"}`))
	fmt.Fprintf(conn, "+ %s\r\n", blob)
	if _, err := reader.ReadString('\n'); err != nil {
		return
	}
	fmt.Fprintf(conn, "%s NO [AUTHENTICATIONFAILED] Invalid credentials\r\n", tag)
}

func (s *fakeIMAPServer) account(t *testing.T) store.MailAccount {
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
		ID:                 7,
		UserID:             3,
		Email:              "user@gmail.example.test",
		Username:           "user@gmail.example.test",
		Host:               host,
		Port:               number,
		AuthType:           store.AuthTypeGoogleOAuth,
		GoogleConnectionID: 11,
	}
}

func (s *fakeIMAPServer) payloads() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.authPayloads...)
}

func testMasterKey() []byte { return bytes.Repeat([]byte("k"), 32) }

func encryptForTest(t *testing.T, secret string) string {
	t.Helper()
	encrypted, err := mmcrypto.EncryptString(testMasterKey(), secret)
	if err != nil {
		t.Fatal(err)
	}
	return encrypted
}

// stubTokens records how often each entry point was used, which is what
// separates "retried with a fresh token" from "retried with the same one".
type stubTokens struct {
	mu       sync.Mutex
	tokens   []string
	index    int
	forced   int
	issued   int
	tokenErr error
	forceErr error
}

func (s *stubTokens) AccessToken(context.Context, int64, int64) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tokenErr != nil {
		return "", s.tokenErr
	}
	s.issued++
	return s.next(), nil
}

func (s *stubTokens) ForceRefresh(context.Context, int64, int64) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.forceErr != nil {
		return "", s.forceErr
	}
	s.forced++
	return s.next(), nil
}

func (s *stubTokens) next() string {
	if s.index >= len(s.tokens) {
		return s.tokens[len(s.tokens)-1]
	}
	token := s.tokens[s.index]
	s.index++
	return token
}

func TestLoginAuthenticatesGoogleAccountsWithXOAUTH2(t *testing.T) {
	server := startFakeIMAPServer(t, "good-token")
	fetcher := &Fetcher{Tokens: &stubTokens{tokens: []string{"good-token"}}}
	client, err := fetcher.login(server.account(t))
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	defer terminateClient(client)
	payloads := server.payloads()
	if len(payloads) != 1 {
		t.Fatalf("authentication attempts = %d, want 1", len(payloads))
	}
	want := "user=user@gmail.example.test\x01auth=Bearer good-token\x01\x01"
	if payloads[0] != want {
		t.Fatalf("payload = %q, want %q", payloads[0], want)
	}
}

// A token can be rejected while this side still believes in it, so the retry is
// the difference between a working account and a sync run that fails until the
// cached token happens to expire.
func TestLoginRetriesOnceWithARefreshedToken(t *testing.T) {
	server := startFakeIMAPServer(t, "fresh-token")
	tokens := &stubTokens{tokens: []string{"stale-token", "fresh-token"}}
	fetcher := &Fetcher{Tokens: tokens}
	client, err := fetcher.login(server.account(t))
	if err != nil {
		t.Fatalf("login after refresh: %v", err)
	}
	defer terminateClient(client)
	payloads := server.payloads()
	if len(payloads) != 2 {
		t.Fatalf("authentication attempts = %d, want 2", len(payloads))
	}
	if !strings.Contains(payloads[0], "stale-token") || !strings.Contains(payloads[1], "fresh-token") {
		t.Fatalf("attempts did not go stale then fresh: %q", payloads)
	}
	if tokens.forced != 1 {
		t.Fatalf("forced refreshes = %d, want 1", tokens.forced)
	}
}

// Retrying with the token that was just rejected would double every failing
// login for nothing.
func TestLoginDoesNotRetryWhenTheRefreshReturnsTheSameToken(t *testing.T) {
	server := startFakeIMAPServer(t, "other-token")
	tokens := &stubTokens{tokens: []string{"stale-token"}}
	fetcher := &Fetcher{Tokens: tokens}
	if _, err := fetcher.login(server.account(t)); err == nil {
		t.Fatal("login with a rejected token succeeded")
	}
	if attempts := len(server.payloads()); attempts != 1 {
		t.Fatalf("authentication attempts = %d, want 1", attempts)
	}
}

func TestLoginReportsAFetcherThatCannotMintTokens(t *testing.T) {
	server := startFakeIMAPServer(t, "good-token")
	if _, err := (&Fetcher{}).login(server.account(t)); !errors.Is(err, ErrNoTokenSource) {
		t.Fatalf("login without a token source = %v, want ErrNoTokenSource", err)
	}
}

func TestLoginReportsAServerWithoutXOAUTH2(t *testing.T) {
	server := startFakeIMAPServer(t, "good-token")
	server.mu.Lock()
	server.advertiseOAuth = false
	server.mu.Unlock()
	fetcher := &Fetcher{Tokens: &stubTokens{tokens: []string{"good-token"}}}
	_, err := fetcher.login(server.account(t))
	if err == nil {
		t.Fatal("login against a server without XOAUTH2 succeeded")
	}
	if !strings.Contains(err.Error(), "XOAUTH2") {
		t.Fatalf("error = %v, want it to name the missing mechanism", err)
	}
}

// The password path is the one every existing account still uses, so the branch
// must not have moved it onto tokens.
func TestLoginStillUsesPasswordAuthenticationForOrdinaryAccounts(t *testing.T) {
	server := startFakeIMAPServer(t)
	account := server.account(t)
	account.AuthType = store.AuthTypePassword
	account.GoogleConnectionID = 0
	account.EncryptedPassword = encryptForTest(t, "hunter2")
	fetcher := &Fetcher{MasterKey: testMasterKey(), Tokens: &stubTokens{tokens: []string{"unused"}}}
	client, err := fetcher.login(account)
	if err != nil {
		t.Fatalf("password login: %v", err)
	}
	defer terminateClient(client)
	if attempts := len(server.payloads()); attempts != 0 {
		t.Fatalf("password account performed %d XOAUTH2 attempts", attempts)
	}
	server.mu.Lock()
	logins := len(server.logins)
	server.mu.Unlock()
	if logins != 1 {
		t.Fatalf("LOGIN commands = %d, want 1", logins)
	}
}
