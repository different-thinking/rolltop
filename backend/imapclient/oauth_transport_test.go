package imapclient

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"math/big"
	"net"
	"testing"
	"time"

	"rolltop/backend/googletoken"
	"rolltop/backend/xoauth2"

	"github.com/emersion/go-imap/client"
)

// The connection this package hands the IMAP library is always dialed here, so
// the library's own IsTLS is false however the socket was opened. Reading the
// transport state off the client used to refuse every Gmail account as
// cleartext, including a connection that had just completed a TLS handshake.
func TestXOAUTH2AuthenticationAcceptsAnEncryptedTransport(t *testing.T) {
	server := startFakeIMAPServer(t, "good-token")
	account := server.account(t)
	// A remote host, so the loopback exemption does not answer for the gate.
	account.Host = "imap.example.test"
	c := dialFakeServer(t, server)
	defer terminateClient(c)
	if c.IsTLS() {
		t.Fatal("client reported TLS for a connection the caller dialed; the test no longer covers the case it was written for")
	}
	if err := authenticateXOAUTH2(account, "good-token")(c, true); err != nil {
		t.Fatalf("authenticate over an encrypted transport: %v", err)
	}
	if attempts := len(server.payloads()); attempts != 1 {
		t.Fatalf("authentication attempts = %d, want 1", attempts)
	}
}

// The gate still has to hold: an access token is as good as the password it
// replaced, so an unencrypted link must not carry one.
func TestXOAUTH2AuthenticationRefusesAnUnencryptedTransport(t *testing.T) {
	server := startFakeIMAPServer(t, "good-token")
	account := server.account(t)
	account.Host = "imap.example.test"
	c := dialFakeServer(t, server)
	defer terminateClient(c)
	err := authenticateXOAUTH2(account, "good-token")(c, false)
	if !errors.Is(err, xoauth2.ErrCleartext) {
		t.Fatalf("authenticate over a cleartext transport = %v, want ErrCleartext", err)
	}
	if attempts := len(server.payloads()); attempts != 0 {
		t.Fatalf("authentication attempts = %d, want none", attempts)
	}
}

// A transport this side refuses is this side's answer, not the server's. Sent
// back as a credential rejection it would spend a real token refresh at Google
// and open a second connection that fails identically, once per mailbox
// operation, for an answer no token can change.
func TestConnectAndAuthenticateDoesNotTreatACleartextRefusalAsAStaleToken(t *testing.T) {
	server := startFakeIMAPServer(t, "good-token")
	refusal := fmt.Errorf("%w: %s", xoauth2.ErrCleartext, "imap.example.test")
	_, err := (&Fetcher{}).connectAndAuthenticate(server.account(t), func(*client.Client, bool) error {
		return refusal
	})
	if !errors.Is(err, xoauth2.ErrCleartext) {
		t.Fatalf("error = %v, want it to carry ErrCleartext", err)
	}
	var rejected googletoken.AuthError
	if errors.As(err, &rejected) {
		t.Fatalf("cleartext refusal reported as a rejected credential: %v", err)
	}
}

func TestTransportEncryptedReadsThroughTheActivityWrapper(t *testing.T) {
	plain, encrypted := connectionPair(t)
	unhandshaken := tls.Client(plainPipe(t), &tls.Config{InsecureSkipVerify: true})
	cases := []struct {
		name string
		conn net.Conn
		want bool
	}{
		{"plain socket", plain, false},
		{"wrapped plain socket", &activityConn{Conn: plain, activity: &connActivity{}}, false},
		{"TLS socket", encrypted, true},
		{"wrapped TLS socket", &activityConn{Conn: encrypted, activity: &connActivity{}}, true},
		{"TLS socket before the handshake", unhandshaken, false},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := transportEncrypted(testCase.conn); got != testCase.want {
				t.Fatalf("transportEncrypted = %v, want %v", got, testCase.want)
			}
		})
	}
}

func dialFakeServer(t *testing.T, server *fakeIMAPServer) *client.Client {
	t.Helper()
	c, err := client.Dial(server.listener.Addr().String())
	if err != nil {
		t.Fatalf("dial fake IMAP server: %v", err)
	}
	return c
}

// connectionPair returns a plain and a TLS client connection to a throwaway
// loopback listener, the TLS one with its handshake already complete.
func connectionPair(t *testing.T) (net.Conn, net.Conn) {
	t.Helper()
	certificate := selfSignedCertificate(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				server := tls.Server(conn, &tls.Config{Certificates: []tls.Certificate{certificate}})
				// A plain client never gets this far; the handshake simply
				// fails and the connection stays as it is on both sides.
				_ = server.Handshake()
			}()
		}
	}()
	plain, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = plain.Close() })
	encrypted, err := tls.Dial("tcp", listener.Addr().String(), &tls.Config{InsecureSkipVerify: true})
	if err != nil {
		t.Fatalf("TLS dial: %v", err)
	}
	t.Cleanup(func() { _ = encrypted.Close() })
	return plain, encrypted
}

// plainPipe is one end of an in-memory connection, for a TLS client that must
// never complete a handshake.
func plainPipe(t *testing.T) net.Conn {
	t.Helper()
	ours, theirs := net.Pipe()
	t.Cleanup(func() {
		_ = ours.Close()
		_ = theirs.Close()
	})
	return ours
}

func selfSignedCertificate(t *testing.T) tls.Certificate {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "rolltop test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, public, private)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: private}
}
