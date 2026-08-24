package imapclient

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"

	mmcrypto "rolltop/backend/crypto"
	"rolltop/backend/googletoken"
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
	// allUIDs answers an unfiltered search and sinceUIDs a search carrying
	// SINCE, so a test can tell the two snapshot searches apart by result.
	allUIDs   []uint32
	sinceUIDs []uint32
	// missingMailboxes are the folders STATUS refuses as not existing.
	missingMailboxes map[string]bool
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
		case "STATUS":
			s.status(conn, tag, line)
		case "SELECT", "EXAMINE":
			fmt.Fprintf(conn, "* 5 EXISTS\r\n* OK [UIDVALIDITY 1]\r\n* OK [UIDNEXT 99]\r\n%s OK [READ-ONLY] selected\r\n", tag)
		case "UID":
			s.mu.Lock()
			command := strings.TrimSpace(line)
			s.searches = append(s.searches, command)
			matches := s.allUIDs
			if strings.Contains(strings.ToUpper(command), "SINCE ") {
				matches = s.sinceUIDs
			}
			s.mu.Unlock()
			fmt.Fprintf(conn, "* SEARCH%s\r\n%s OK search complete\r\n", formatUIDs(matches), tag)
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

// status answers STATUS, which is where a folder the account does not have is
// refused. The wording is Dovecot's, timings included: the response code that
// would say this in one word never reaches the caller, so the text is what
// there is to recognize it by.
func (s *fakeIMAPServer) status(conn net.Conn, tag, line string) {
	raw := statusMailboxArgument(line)
	name := strings.Trim(raw, `"`)
	s.mu.Lock()
	missing := s.missingMailboxes[name]
	s.mu.Unlock()
	if missing {
		fmt.Fprintf(conn, "%s NO Mailbox doesn't exist: %s (0.002 + 0.000 secs).\r\n", tag, name)
		return
	}
	fmt.Fprintf(conn, "* STATUS %s (MESSAGES 3 UNSEEN 1 UIDNEXT 12 UIDVALIDITY 44)\r\n%s OK status completed\r\n", raw, tag)
}

// statusMailboxArgument returns the mailbox token of a STATUS command as the
// client wrote it, quotes included, so the untagged reply can echo it back.
func statusMailboxArgument(line string) string {
	rest := strings.TrimSpace(line)
	if index := strings.Index(strings.ToUpper(rest), "STATUS "); index >= 0 {
		rest = strings.TrimSpace(rest[index+len("STATUS "):])
	}
	if strings.HasPrefix(rest, `"`) {
		if end := strings.Index(rest[1:], `"`); end >= 0 {
			return rest[:end+2]
		}
		return rest
	}
	return strings.Fields(rest)[0]
}

// setMissingMailboxes names the folders this server has no folder for.
func (s *fakeIMAPServer) setMissingMailboxes(names ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.missingMailboxes = map[string]bool{}
	for _, name := range names {
		s.missingMailboxes[name] = true
	}
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

// setSearchResults decides what an unfiltered and a SINCE-limited search return.
func (s *fakeIMAPServer) setSearchResults(all, since []uint32) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.allUIDs = append([]uint32(nil), all...)
	s.sinceUIDs = append([]uint32(nil), since...)
}

func (s *fakeIMAPServer) searchCommands() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.searches...)
}

func formatUIDs(uids []uint32) string {
	out := ""
	for _, uid := range uids {
		out += " " + strconv.FormatUint(uint64(uid), 10)
	}
	return out
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

func TestLoginAuthenticatesGoogleAccountsWithXOAUTH2(t *testing.T) {
	server := startFakeIMAPServer(t, "good-token")
	fetcher := &Fetcher{Tokens: &googletoken.StubTokenSource{Tokens: []string{"good-token"}}}
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
	tokens := &googletoken.StubTokenSource{Tokens: []string{"stale-token", "fresh-token"}}
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
	if tokens.Forced != 1 {
		t.Fatalf("forced refreshes = %d, want 1", tokens.Forced)
	}
}

// Retrying with the token that was just rejected would double every failing
// login for nothing.
func TestLoginDoesNotRetryWhenTheRefreshReturnsTheSameToken(t *testing.T) {
	server := startFakeIMAPServer(t, "other-token")
	tokens := &googletoken.StubTokenSource{Tokens: []string{"stale-token"}}
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
	if _, err := (&Fetcher{}).login(server.account(t)); !errors.Is(err, googletoken.ErrNoTokenSource) {
		t.Fatalf("login without a token source = %v, want ErrNoTokenSource", err)
	}
}

// A server that does not offer the mechanism has not rejected the credential.
// Treating it as one would spend a real token refresh at Google and a second
// connection on every mailbox, forever, against a server that can never accept
// the result. The stub hands out a different token on refresh, so a retry would
// be visible here rather than hidden behind an unchanged value.
func TestLoginDoesNotRefreshTheTokenWhenTheServerLacksXOAUTH2(t *testing.T) {
	server := startFakeIMAPServer(t, "good-token")
	server.mu.Lock()
	server.advertiseOAuth = false
	server.mu.Unlock()
	tokens := &googletoken.StubTokenSource{Tokens: []string{"first-token", "second-token"}}
	_, err := (&Fetcher{Tokens: tokens}).login(server.account(t))
	if err == nil {
		t.Fatal("login against a server without XOAUTH2 succeeded")
	}
	if !strings.Contains(err.Error(), "XOAUTH2") {
		t.Fatalf("error = %v, want it to name the missing mechanism", err)
	}
	if tokens.Forced != 0 {
		t.Fatalf("forced refreshes = %d, want none for a server that cannot do XOAUTH2", tokens.Forced)
	}
	if attempts := len(server.payloads()); attempts != 0 {
		t.Fatalf("authentication attempts = %d, want none", attempts)
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
	fetcher := &Fetcher{MasterKey: testMasterKey(), Tokens: &googletoken.StubTokenSource{Tokens: []string{"unused"}}}
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
