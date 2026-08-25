// File overview: XOAUTH2 authentication for Gmail's IMAP and SMTP endpoints.
// Google never adopted the standard OAUTHBEARER mechanism its own documentation
// points at, so the one mechanism that works against imap.gmail.com and
// smtp.gmail.com has to be written here. The wire format is identical for both
// protocols; only the surrounding interface differs, which is why the IMAP and
// SMTP adapters share a payload builder rather than each spelling it out.

package xoauth2

import (
	"errors"
	"fmt"
	"net"
	"net/smtp"
	"strings"

	"github.com/emersion/go-sasl"
)

// Mechanism is the SASL mechanism name both servers advertise.
const Mechanism = "XOAUTH2"

// ErrCleartext reports that the transport would put an access token on the wire
// unencrypted. A bearer token is as good as the password it replaced, so this is
// refused rather than downgraded.
var ErrCleartext = errors.New("refusing to send an OAuth access token over an unencrypted connection")

// ErrUnsupported reports that the server does not offer XOAUTH2, which is worth
// naming explicitly: the alternative is a bare "unsupported mechanism" failure
// from deep inside the protocol library.
var ErrUnsupported = errors.New("server does not advertise the XOAUTH2 authentication mechanism")

// initialResponse builds the mechanism's only real message. The layout is
// Google's, not an RFC's: two key-value pairs separated by ^A, terminated by a
// second ^A.
func initialResponse(username, accessToken string) []byte {
	return []byte("user=" + username + "\x01auth=Bearer " + accessToken + "\x01\x01")
}

// EnsureEncrypted refuses to proceed when a bearer token would travel over an
// unencrypted link. tls reports whether the transport is already encrypted;
// host is the peer, so a local relay or test server on loopback still works. It
// is the single cleartext gate both the IMAP and SMTP adapters call, so neither
// protocol can put an access token on the wire in the clear.
func EnsureEncrypted(host string, tls bool) error {
	if tls || isLoopback(host) {
		return nil
	}
	return fmt.Errorf("%w: %s", ErrCleartext, host)
}

func validate(username, accessToken string) error {
	if strings.TrimSpace(username) == "" {
		return errors.New("XOAUTH2 requires an account name")
	}
	if strings.TrimSpace(accessToken) == "" {
		return errors.New("XOAUTH2 requires an access token")
	}
	// A token carrying either separator would let the sender inject a second
	// key-value pair. No legitimate token contains one.
	if strings.ContainsAny(username, "\x01") || strings.ContainsAny(accessToken, "\x01") {
		return errors.New("XOAUTH2 credentials must not contain separator bytes")
	}
	return nil
}

// NewSASLClient authenticates a go-imap connection.
func NewSASLClient(username, accessToken string) sasl.Client {
	return &saslClient{username: username, accessToken: accessToken}
}

type saslClient struct {
	username    string
	accessToken string
	challenged  bool
}

func (c *saslClient) Start() (string, []byte, error) {
	if err := validate(c.username, c.accessToken); err != nil {
		return "", nil, err
	}
	return Mechanism, initialResponse(c.username, c.accessToken), nil
}

// Next answers the one challenge this mechanism can receive. On failure Google
// does not fail the command outright; it sends a base64 error blob and waits for
// an empty line before issuing the tagged NO. Returning an error here instead
// would abort mid-command and leave the connection out of step with the server,
// so the empty response is sent and the real error is read from the NO. The blob
// is deliberately not surfaced: it is diagnostics for a request that quoted the
// access token.
func (c *saslClient) Next([]byte) ([]byte, error) {
	if c.challenged {
		return nil, errors.New("unexpected second XOAUTH2 challenge")
	}
	c.challenged = true
	return []byte{}, nil
}

// NewSMTPAuth authenticates a net/smtp connection.
func NewSMTPAuth(username, accessToken string) smtp.Auth {
	return &smtpAuth{username: username, accessToken: accessToken}
}

type smtpAuth struct {
	username    string
	accessToken string
	challenged  bool
}

func (a *smtpAuth) Start(server *smtp.ServerInfo) (string, []byte, error) {
	if err := validate(a.username, a.accessToken); err != nil {
		return "", nil, err
	}
	if server == nil {
		return "", nil, ErrCleartext
	}
	if err := EnsureEncrypted(server.Name, server.TLS); err != nil {
		return "", nil, err
	}
	// An empty advertisement means the caller reached Auth without an EHLO
	// response to inspect; only a populated list that omits the mechanism is
	// evidence of an unsupported server.
	if len(server.Auth) > 0 && !supports(server.Auth) {
		return "", nil, fmt.Errorf("%w: %s", ErrUnsupported, strings.Join(server.Auth, " "))
	}
	return Mechanism, initialResponse(a.username, a.accessToken), nil
}

// Next mirrors the IMAP side: the error blob is acknowledged with an empty line
// so the server can report the actual failure code.
func (a *smtpAuth) Next(_ []byte, more bool) ([]byte, error) {
	if !more {
		return nil, nil
	}
	if a.challenged {
		return nil, errors.New("unexpected second XOAUTH2 challenge")
	}
	a.challenged = true
	return []byte{}, nil
}

func supports(mechanisms []string) bool {
	for _, mechanism := range mechanisms {
		if strings.EqualFold(strings.TrimSpace(mechanism), Mechanism) {
			return true
		}
	}
	return false
}

// isLoopback keeps tests and a local relay working without weakening the rule
// for anything that actually leaves the machine.
func isLoopback(host string) bool {
	host = strings.TrimSpace(host)
	if host == "" {
		return false
	}
	if bare, _, err := net.SplitHostPort(host); err == nil {
		host = bare
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	address := net.ParseIP(host)
	return address != nil && address.IsLoopback()
}
