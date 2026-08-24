// File overview: Tests for what the SMTP transcript records. Two properties are
// the reason this exists: everything after a STARTTLS upgrade is still
// recorded -- the half of the conversation the previous implementation could
// not see -- and nothing recorded is a credential or a message body.

package smtpclient

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"fmt"
	"math/big"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	mmcrypto "rolltop/backend/crypto"
	"rolltop/backend/smtplog"
	"rolltop/backend/store"
)

const tracePassword = "hunter2-app-password"

func TestSendRawRecordsTheConversationWithoutSecrets(t *testing.T) {
	server := startPasswordSMTPServer(t, tracePassword)
	recorder := smtplog.NewRecorder()
	sender := &Sender{MasterKey: traceMasterKey(), Log: recorder}
	account := server.account(t)

	if err := sender.SendRaw(context.Background(), account, []string{"recipient@example.test"},
		[]byte("From: user@example.test\r\nTo: recipient@example.test\r\n\r\nsecret body text\r\n")); err != nil {
		t.Fatalf("send: %v", err)
	}

	session := onlySession(t, recorder, account.UserID)
	if session.AccountID != account.SMTPAccountID {
		t.Fatalf("session account_id = %d, want %d", session.AccountID, account.SMTPAccountID)
	}
	if session.Kind != smtplog.KindSend {
		t.Fatalf("session kind = %q, want %q", session.Kind, smtplog.KindSend)
	}
	if session.Err != "" {
		t.Fatalf("successful send recorded an error: %q", session.Err)
	}
	transcript := transcriptOf(session)
	for _, want := range []string{"EHLO", "AUTH PLAIN", "MAIL FROM:<user@example.test>", "RCPT TO:<recipient@example.test>", "DATA", "354", "250 delivered", "bytes, not recorded"} {
		if !strings.Contains(transcript, want) {
			t.Fatalf("transcript missing %q:\n%s", want, transcript)
		}
	}
	for _, secret := range []string{tracePassword, base64.StdEncoding.EncodeToString([]byte("\x00user@example.test\x00" + tracePassword)), "secret body text"} {
		if strings.Contains(transcript, secret) {
			t.Fatalf("transcript leaked %q:\n%s", secret, transcript)
		}
	}
}

func TestSendRawRecordsARefusedLogin(t *testing.T) {
	server := startPasswordSMTPServer(t, "the-right-password")
	recorder := smtplog.NewRecorder()
	sender := &Sender{MasterKey: traceMasterKey(), Log: recorder}
	account := server.account(t)

	err := sender.SendRaw(context.Background(), account, []string{"recipient@example.test"},
		[]byte("From: user@example.test\r\nTo: recipient@example.test\r\n\r\nbody\r\n"))
	if err == nil {
		t.Fatal("send with a wrong password succeeded")
	}
	session := onlySession(t, recorder, account.UserID)
	if session.Err == "" {
		t.Fatal("failed send recorded no error")
	}
	if transcript := transcriptOf(session); !strings.Contains(transcript, "535") {
		t.Fatalf("transcript does not carry the server's refusal:\n%s", transcript)
	}
}

// The connection test is the button in settings: it must prove the login and
// stop, because a test that delivered a message would write to somebody.
func TestVerifyAuthenticatesWithoutOfferingAMessage(t *testing.T) {
	server := startPasswordSMTPServer(t, tracePassword)
	recorder := smtplog.NewRecorder()
	sender := &Sender{MasterKey: traceMasterKey(), Log: recorder}
	account := server.account(t)

	sessionID, err := sender.Verify(context.Background(), account)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	session, ok := recorder.Session(account.UserID, sessionID)
	if !ok {
		t.Fatalf("verify reported session %d, which the recorder does not have", sessionID)
	}
	if session.Kind != smtplog.KindTest {
		t.Fatalf("session kind = %q, want %q", session.Kind, smtplog.KindTest)
	}
	transcript := transcriptOf(session)
	if !strings.Contains(transcript, "QUIT") {
		t.Fatalf("connection test did not hang up:\n%s", transcript)
	}
	if strings.Contains(transcript, "DATA") {
		t.Fatalf("connection test offered a message:\n%s", transcript)
	}
	if delivered := server.deliveredCount(); delivered != 0 {
		t.Fatalf("connection test delivered %d messages, want 0", delivered)
	}
}

func TestVerifyReportsARefusedLogin(t *testing.T) {
	server := startPasswordSMTPServer(t, "the-right-password")
	recorder := smtplog.NewRecorder()
	sender := &Sender{MasterKey: traceMasterKey(), Log: recorder}
	account := server.account(t)

	sessionID, err := sender.Verify(context.Background(), account)
	if err == nil {
		t.Fatal("verify with a wrong password succeeded")
	}
	if !strings.Contains(err.Error(), "535") {
		t.Fatalf("verify error = %v, want the server's own refusal", err)
	}
	if _, ok := recorder.Session(account.UserID, sessionID); !ok {
		t.Fatal("a failed connection test recorded no session")
	}
}

// The upgrade is what net/smtp performed out of reach of any wrapper this
// package could install, so everything after it went unrecorded. It is
// asserted here directly on the conversation, because the property is that
// commands and replies keep reaching the recorder once the socket is
// encrypted.
func TestSTARTTLSKeepsRecordingAfterTheUpgrade(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	certificate, roots := selfSignedCertificate(t, "smtp.example.test")
	_ = clientConn.SetDeadline(time.Now().Add(10 * time.Second))
	go serveSTARTTLSExchange(serverConn, certificate)

	recorder := smtplog.NewRecorder()
	trace := recorder.Start(smtplog.Session{UserID: 3, Kind: smtplog.KindTest})
	conversation, err := newConversation(clientConn, "smtp.example.test", false, trace)
	if err != nil {
		t.Fatalf("greeting: %v", err)
	}
	if err := conversation.hello("localhost"); err != nil {
		t.Fatalf("hello: %v", err)
	}
	if _, ok := conversation.supports("STARTTLS"); !ok {
		t.Fatal("server advertised STARTTLS and the conversation did not see it")
	}
	config := &tls.Config{ServerName: "smtp.example.test", RootCAs: roots, MinVersion: tls.VersionTLS12}
	if err := conversation.startTLS(config, "localhost"); err != nil {
		t.Fatalf("start TLS: %v", err)
	}
	if err := conversation.mail("user@example.test"); err != nil {
		t.Fatalf("MAIL FROM after upgrade: %v", err)
	}

	transcript := transcriptOf(trace.Snapshot())
	for _, want := range []string{"TLS established", "AUTH PLAIN LOGIN", "MAIL FROM:<user@example.test>", "250 encrypted sender ok"} {
		if !strings.Contains(transcript, want) {
			t.Fatalf("transcript lost the encrypted half of the conversation, missing %q:\n%s", want, transcript)
		}
	}
}

// fakePasswordSMTPServer answers AUTH PLAIN for one password and counts what it
// was asked to deliver.
type fakePasswordSMTPServer struct {
	listener net.Listener
	password string

	delivered chan struct{}
}

func startPasswordSMTPServer(t *testing.T, password string) *fakePasswordSMTPServer {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &fakePasswordSMTPServer{listener: listener, password: password, delivered: make(chan struct{}, 16)}
	go func() {
		for {
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			go server.handle(conn)
		}
	}()
	t.Cleanup(func() { _ = listener.Close() })
	return server
}

func (s *fakePasswordSMTPServer) handle(conn net.Conn) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
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
			write("250-fake greets you\r\n250 AUTH PLAIN\r\n")
		case "AUTH":
			if len(fields) < 3 || !strings.EqualFold(fields[1], "PLAIN") {
				write("504 unsupported mechanism\r\n")
				continue
			}
			decoded, decodeErr := base64.StdEncoding.DecodeString(fields[2])
			if decodeErr != nil || !strings.HasSuffix(string(decoded), "\x00"+s.password) {
				write("535 invalid credentials\r\n")
				continue
			}
			write("235 authenticated\r\n")
		case "MAIL", "RCPT":
			write("250 ok\r\n")
		case "DATA":
			write("354 send it\r\n")
			for {
				body, bodyErr := reader.ReadString('\n')
				if bodyErr != nil {
					return
				}
				if strings.TrimRight(body, "\r\n") == "." {
					break
				}
			}
			select {
			case s.delivered <- struct{}{}:
			default:
			}
			write("250 delivered\r\n")
		case "QUIT":
			write("221 bye\r\n")
			return
		default:
			write("250 ok\r\n")
		}
	}
}

func (s *fakePasswordSMTPServer) deliveredCount() int {
	return len(s.delivered)
}

// account describes the fake server the way the settings page would have saved
// it, with the password encrypted under traceMasterKey.
func (s *fakePasswordSMTPServer) account(t *testing.T) store.MailAccount {
	t.Helper()
	host, port, err := net.SplitHostPort(s.listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	number, err := strconv.Atoi(port)
	if err != nil {
		t.Fatal(err)
	}
	encrypted, err := mmcrypto.EncryptString(traceMasterKey(), tracePassword)
	if err != nil {
		t.Fatal(err)
	}
	return store.MailAccount{
		UserID:                4,
		Email:                 "user@example.test",
		SMTPAccountID:         12,
		SMTPHost:              host,
		SMTPPort:              number,
		SMTPUsername:          "user@example.test",
		EncryptedSMTPPassword: encrypted,
	}
}

func traceMasterKey() []byte {
	return bytes.Repeat([]byte("k"), 32)
}

// serveSTARTTLSExchange plays the half of a submission server this test needs:
// greet, advertise STARTTLS, upgrade, then answer the encrypted commands.
func serveSTARTTLSExchange(conn net.Conn, certificate tls.Certificate) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	reader := bufio.NewReader(conn)
	fmt.Fprint(conn, "220 fake ESMTP ready\r\n")
	if _, err := reader.ReadString('\n'); err != nil {
		return
	}
	fmt.Fprint(conn, "250-fake greets you\r\n250 STARTTLS\r\n")
	if _, err := reader.ReadString('\n'); err != nil {
		return
	}
	fmt.Fprint(conn, "220 ready to start TLS\r\n")
	tlsConn := tls.Server(conn, &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS12})
	if err := tlsConn.Handshake(); err != nil {
		return
	}
	defer tlsConn.Close()
	_ = tlsConn.SetDeadline(time.Now().Add(10 * time.Second))
	encrypted := bufio.NewReader(tlsConn)
	if _, err := encrypted.ReadString('\n'); err != nil {
		return
	}
	fmt.Fprint(tlsConn, "250-fake greets you again\r\n250 AUTH PLAIN LOGIN\r\n")
	if _, err := encrypted.ReadString('\n'); err != nil {
		return
	}
	fmt.Fprint(tlsConn, "250 encrypted sender ok\r\n")
	// Hold the connection open until the client is done reading; the test
	// closes its own end.
	_, _ = encrypted.ReadString('\n')
}

func selfSignedCertificate(t *testing.T, host string) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: host},
		DNSNames:              []string{host},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(parsed)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: parsed}, roots
}

func onlySession(t *testing.T, recorder *smtplog.Recorder, userID int64) smtplog.Session {
	t.Helper()
	sessions := recorder.Sessions(userID, 10)
	if len(sessions) != 1 {
		t.Fatalf("recorded sessions = %d, want 1", len(sessions))
	}
	return sessions[0]
}

func transcriptOf(session smtplog.Session) string {
	lines := make([]string, 0, len(session.Lines))
	for _, line := range session.Lines {
		lines = append(lines, line.Direction+" "+line.Text)
	}
	return strings.Join(lines, "\n")
}
