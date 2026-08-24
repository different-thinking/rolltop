// File overview: The SMTP conversation itself, written out rather than driven
// through net/smtp, because the transcript is the point. net/smtp performs the
// STARTTLS upgrade inside itself and hands the plaintext to a connection this
// package cannot reach, so a wrapper around the dialed socket sees the
// greeting and then ciphertext -- everything a failing account is diagnosed
// from (the EHLO the server answers after the upgrade, the AUTH result, the
// reply to MAIL FROM or RCPT TO) happens on the far side of it.
//
// The exchange this file speaks is the same one net/smtp speaks, including the
// SASL loop against a net/smtp Auth, so PlainAuth and the XOAUTH2 mechanism
// keep working unchanged. What it adds is that every command and every reply
// passes through a smtplog.Recording on the way, and that a failure carries the
// server's own words back to the caller.

package smtpclient

import (
	"bytes"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/smtp"
	"net/textproto"
	"strings"

	"rolltop/backend/smtplog"
)

// conversation is one SMTP session on one connection.
type conversation struct {
	host  string
	conn  net.Conn
	text  *textproto.Conn
	trace *smtplog.Recording
	// ext holds what the last EHLO advertised. STARTTLS replaces it, because a
	// server may offer different extensions -- AUTH in particular -- once the
	// connection is encrypted.
	ext map[string]string
	tls bool
}

// newConversation reads the opening greeting. A server that answers anything
// but 220 has refused the connection, and the reply says why.
func newConversation(conn net.Conn, host string, encrypted bool, trace *smtplog.Recording) (*conversation, error) {
	c := &conversation{host: host, conn: conn, text: textproto.NewConn(conn), trace: trace, tls: encrypted}
	code, message, err := c.text.ReadResponse(220)
	c.recordReply(code, message, err)
	if err != nil {
		return nil, fmt.Errorf("SMTP greeting: %w", err)
	}
	return c, nil
}

// cmd writes one command and reads its reply, recording both. expect is the
// reply code the caller requires; 0 accepts any final reply and leaves the
// judgement to the caller, which is how the SASL loop reads a challenge.
func (c *conversation) cmd(expect int, format string, args ...any) (int, string, error) {
	line := fmt.Sprintf(format, args...)
	if err := validateLine(line); err != nil {
		return 0, "", err
	}
	c.trace.Client(line)
	id, err := c.text.Cmd("%s", line)
	if err != nil {
		c.trace.Note("write failed: " + err.Error())
		return 0, "", err
	}
	c.text.StartResponse(id)
	defer c.text.EndResponse(id)
	code, message, err := c.text.ReadResponse(expect)
	c.recordReply(code, message, err)
	return code, message, err
}

// recordReply keeps the server's own words even when textproto turned them
// into an error, which is the case for every refusal: the code and text of a
// 535 or a 550 are the answer the reader came for.
func (c *conversation) recordReply(code int, message string, err error) {
	if err != nil {
		var protoErr *textproto.Error
		if errors.As(err, &protoErr) {
			c.trace.Server(protoErr.Code, protoErr.Msg)
			return
		}
		c.trace.Note("no reply: " + err.Error())
		return
	}
	c.trace.Server(code, message)
}

// hello introduces the client. A server that rejects EHLO is tried with HELO,
// which is the only thing a pre-ESMTP relay understands; extensions are then
// empty, and everything that needs one says so by name.
func (c *conversation) hello(name string) error {
	code, message, err := c.cmd(250, "EHLO %s", name)
	if err != nil {
		if code >= 400 && code < 600 {
			if _, _, helloErr := c.cmd(250, "HELO %s", name); helloErr != nil {
				return fmt.Errorf("SMTP hello: %w", helloErr)
			}
			c.ext = map[string]string{}
			return nil
		}
		return fmt.Errorf("SMTP hello: %w", err)
	}
	c.ext = parseExtensions(message)
	return nil
}

// parseExtensions reads the EHLO reply. textproto joins the continuation lines
// with newlines and strips the repeated code, so the first line is the server's
// greeting text and each line after it is one extension.
func parseExtensions(message string) map[string]string {
	ext := map[string]string{}
	lines := strings.Split(message, "\n")
	for _, line := range lines[min(1, len(lines)):] {
		name, params, _ := strings.Cut(strings.TrimSpace(line), " ")
		if name == "" {
			continue
		}
		ext[strings.ToUpper(name)] = params
	}
	return ext
}

// supports reports whether the last EHLO advertised an extension.
func (c *conversation) supports(name string) (string, bool) {
	params, ok := c.ext[strings.ToUpper(name)]
	return params, ok
}

// startTLS upgrades the connection and introduces the client again, because
// everything the server said before the upgrade was said to an unauthenticated
// peer and is discarded by the protocol.
func (c *conversation) startTLS(config *tls.Config, name string) error {
	if _, _, err := c.cmd(220, "STARTTLS"); err != nil {
		return fmt.Errorf("start SMTP TLS: %w", err)
	}
	tlsConn := tls.Client(c.conn, config)
	if err := tlsConn.Handshake(); err != nil {
		c.trace.Note("TLS handshake failed: " + err.Error())
		return fmt.Errorf("start SMTP TLS: %w", err)
	}
	c.trace.Note("TLS established: " + tlsSummary(tlsConn.ConnectionState()))
	c.conn = tlsConn
	c.text = textproto.NewConn(tlsConn)
	c.tls = true
	return c.hello(name)
}

// auth runs the SASL exchange for a net/smtp mechanism. The base64 blobs the
// client sends are the credential itself, so they are recorded as having been
// sent and never as what they contained.
func (c *conversation) auth(a smtp.Auth) error {
	mechanisms := strings.Fields(c.ext["AUTH"])
	mech, resp, err := a.Start(&smtp.ServerInfo{Name: c.host, TLS: c.tls, Auth: mechanisms})
	if err != nil {
		return fmt.Errorf("authenticate to SMTP server: %w", err)
	}
	command := "AUTH " + mech
	if len(resp) > 0 {
		command += " " + base64.StdEncoding.EncodeToString(resp)
	}
	code, message, err := c.cmd(0, "%s", command)
	for err == nil {
		var challenge []byte
		switch code {
		case 334:
			challenge, err = base64.StdEncoding.DecodeString(message)
		case 235:
			// The final reply is prose, not a challenge, so it is handed to the
			// mechanism unchanged rather than decoded.
			challenge = []byte(message)
		default:
			err = &textproto.Error{Code: code, Msg: message}
		}
		if err != nil {
			break
		}
		resp, err = a.Next(challenge, code == 334)
		if err != nil {
			// The mechanism gave up mid-exchange, so the server is still
			// waiting for a reply: "*" is how a client withdraws an AUTH it
			// cannot finish. A server that already refused needs no such
			// courtesy, which is why this is not in the error path above.
			c.trace.Note("aborting authentication: " + err.Error())
			_, _, _ = c.cmd(0, "*")
			break
		}
		if resp == nil {
			break
		}
		c.trace.Secret()
		code, message, err = c.cmdSecret(base64.StdEncoding.EncodeToString(resp))
	}
	if err != nil {
		return fmt.Errorf("authenticate to SMTP server: %w", err)
	}
	return nil
}

// cmdSecret writes a line the transcript must not carry. The caller has already
// recorded that a credential was sent.
func (c *conversation) cmdSecret(line string) (int, string, error) {
	if err := validateLine(line); err != nil {
		return 0, "", err
	}
	id, err := c.text.Cmd("%s", line)
	if err != nil {
		c.trace.Note("write failed: " + err.Error())
		return 0, "", err
	}
	c.text.StartResponse(id)
	defer c.text.EndResponse(id)
	code, message, err := c.text.ReadResponse(0)
	c.recordReply(code, message, err)
	return code, message, err
}

func (c *conversation) mail(from string) error {
	if _, _, err := c.cmd(250, "MAIL FROM:<%s>", from); err != nil {
		return fmt.Errorf("SMTP MAIL FROM: %w", err)
	}
	return nil
}

func (c *conversation) rcpt(to string) error {
	if _, _, err := c.cmd(25, "RCPT TO:<%s>", to); err != nil {
		return fmt.Errorf("SMTP RCPT TO: %w", err)
	}
	return nil
}

// data offers the message. The payload never enters the transcript; its size
// does, because a server that refuses at the end of DATA is usually refusing
// the size.
func (c *conversation) data(raw []byte) error {
	if _, _, err := c.cmd(354, "DATA"); err != nil {
		return fmt.Errorf("SMTP DATA: %w", err)
	}
	writer := c.text.DotWriter()
	if _, err := io.Copy(writer, bytes.NewReader(raw)); err != nil {
		_ = writer.Close()
		return fmt.Errorf("write SMTP message: %w", err)
	}
	if err := writer.Close(); err != nil {
		return fmt.Errorf("finish SMTP message: %w", err)
	}
	c.trace.Body(len(raw))
	code, message, err := c.text.ReadResponse(250)
	c.recordReply(code, message, err)
	if err != nil {
		return fmt.Errorf("finish SMTP message: %w", err)
	}
	return nil
}

// quit ends the session politely. Only the connection test waits for the
// answer: a send that has been accepted must not spend another round trip
// on a reply that cannot change the outcome.
func (c *conversation) quit() {
	_, _, _ = c.cmd(221, "QUIT")
}

func tlsSummary(state tls.ConnectionState) string {
	return fmt.Sprintf("%s, %s", tls.VersionName(state.Version), tls.CipherSuiteName(state.CipherSuite))
}

// validateLine refuses a command carrying a line break. Addresses reach this
// package parsed, so this is the second lock on the same door: a newline in a
// command is how one SMTP session is turned into two.
func validateLine(line string) error {
	if strings.ContainsAny(line, "\n\r") {
		return errors.New("SMTP command contains a line break")
	}
	return nil
}
