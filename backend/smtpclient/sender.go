// File overview: SMTP send implementation for composed messages.

package smtpclient

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"mime"
	"net"
	"net/mail"
	"net/smtp"
	"strings"
	"time"

	"rolltop/backend/buildinfo"
	mmcrypto "rolltop/backend/crypto"
	"rolltop/backend/googletoken"
	"rolltop/backend/smtplog"
	"rolltop/backend/store"
	"rolltop/backend/xoauth2"
)

// Attachment is an outgoing MIME attachment or inline part prepared by compose.
type Attachment struct {
	Filename    string
	ContentType string
	ContentID   string
	Inline      bool
	Data        []byte
}

// Header is an extra outbound RFC822 header prepared by compose or a plugin.
type Header struct {
	Name  string
	Value string
}

// MIMEBodyOverride lets compose supply a fully prepared root MIME body.
type MIMEBodyOverride struct {
	ContentType string
	Body        string
}

// Message is the normalized outbound compose payload passed to the SMTP sender.
type Message struct {
	From             string
	To               []string
	Cc               []string
	Bcc              []string
	Subject          string
	BodyText         string
	BodyHTML         string
	MessageID        string
	InReplyTo        string
	References       string
	Date             time.Time
	ExtraHeaders     []Header
	MIMEBodyOverride *MIMEBodyOverride
	Attachments      []Attachment
}

// Sender sends compose messages through an encrypted Rolltop SMTP account.
type Sender struct {
	MasterKey []byte
	Timeout   time.Duration
	// Tokens mints Google access tokens for accounts that authenticate with
	// OAuth. Nil only fails accounts that actually need it.
	Tokens googletoken.TokenSource
	// Log records the conversation of every attempt so the settings page can
	// show why a send failed. A nil recorder records nothing, which is what a
	// test that only asserts on delivery wants.
	Log *smtplog.Recorder
}

// smtpHelloName is what Rolltop calls itself in EHLO. Submission servers
// authenticate the client rather than trusting this name, and a made-up
// hostname is likelier to be refused than the loopback name every client
// library sends by default.
const smtpHelloName = "localhost"

type smtpIdleDeadlineConn struct {
	net.Conn
	timeout time.Duration
}

func (c *smtpIdleDeadlineConn) Read(p []byte) (int, error) {
	if c.timeout > 0 {
		if err := c.Conn.SetReadDeadline(time.Now().Add(c.timeout)); err != nil {
			return 0, err
		}
	}
	return c.Conn.Read(p)
}

func (c *smtpIdleDeadlineConn) Write(p []byte) (int, error) {
	if c.timeout > 0 {
		if err := c.Conn.SetWriteDeadline(time.Now().Add(c.timeout)); err != nil {
			return 0, err
		}
	}
	return c.Conn.Write(p)
}

// Send builds a MIME message from the compose form and sends it through the configured SMTP account.
func (s *Sender) Send(ctx context.Context, account store.MailAccount, msg Message) ([]byte, error) {
	raw, recipients, err := BuildRaw(msg)
	if err != nil {
		return nil, err
	}
	if err := s.SendRaw(ctx, account, recipients, raw); err != nil {
		return nil, err
	}
	return raw, nil
}

// SendRaw sends an already-built RFC822 payload to all recipients using the configured SMTP account.
//
// The whole attempt is recorded as one session even when it authenticates
// twice: a refreshed token is a second login on a second connection, and a
// reader looking at why their mail did not leave needs to see that as one
// story rather than as two unexplained halves.
func (s *Sender) SendRaw(ctx context.Context, account store.MailAccount, recipients []string, raw []byte) (err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	trace := s.Log.Start(smtplog.Session{
		UserID:    account.UserID,
		AccountID: account.SMTPAccountID,
		Kind:      smtplog.KindSend,
		Host:      account.SMTPHost,
		Port:      account.SMTPPort,
		Username:  account.SMTPUsername,
		From:      account.Email,
	})
	defer func() { trace.Finish(err) }()
	trace.Note(fmt.Sprintf("sending %d bytes to %d recipient(s)", len(raw), len(recipients)))
	if len(recipients) == 0 {
		return errors.New("message has no recipients")
	}
	if account.UsesGoogleOAuth() {
		return s.sendWithGoogleToken(ctx, account, recipients, raw, trace)
	}
	password, err := mmcrypto.DecryptString(s.MasterKey, account.EncryptedSMTPPassword)
	if err != nil {
		return fmt.Errorf("decrypt SMTP password: %w", err)
	}
	return s.dialAndSend(ctx, account, passwordAuth(account, password), recipients, raw, trace)
}

// Verify runs the half of a send that a misconfigured account fails in --
// connect, greet, upgrade, authenticate -- and stops before offering a
// message, so pressing the button in settings cannot deliver mail. It returns
// the id of the recorded session so the page can show the conversation that
// just happened rather than the newest one, which on a busy account is
// somebody else's.
func (s *Sender) Verify(ctx context.Context, account store.MailAccount) (sessionID int64, err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	trace := s.Log.Start(smtplog.Session{
		UserID:    account.UserID,
		AccountID: account.SMTPAccountID,
		Kind:      smtplog.KindTest,
		Host:      account.SMTPHost,
		Port:      account.SMTPPort,
		Username:  account.SMTPUsername,
		From:      account.Email,
	})
	defer func() { trace.Finish(err) }()
	trace.Note("connection test: no message is offered")
	if strings.TrimSpace(account.SMTPHost) == "" {
		return trace.Ref(), errors.New("this outgoing server has no host configured")
	}
	if account.UsesGoogleOAuth() {
		username, nameErr := smtpAuthUsername(account)
		if nameErr != nil {
			return trace.Ref(), nameErr
		}
		err = googletoken.WithFreshToken(ctx, s.Tokens, account.UserID, account.GoogleConnectionID,
			func(token string) error {
				return s.dialAndVerify(ctx, account, xoauth2.NewSMTPAuth(username, token), trace)
			})
		return trace.Ref(), err
	}
	password, decryptErr := mmcrypto.DecryptString(s.MasterKey, account.EncryptedSMTPPassword)
	if decryptErr != nil {
		return trace.Ref(), fmt.Errorf("decrypt SMTP password: %w", decryptErr)
	}
	err = s.dialAndVerify(ctx, account, passwordAuth(account, password), trace)
	return trace.Ref(), err
}

// sendWithGoogleToken mirrors the IMAP path: one retry against a forcibly
// refreshed token. Retrying is safe because authentication precedes MAIL FROM,
// so a rejected login cannot have delivered anything.
func (s *Sender) sendWithGoogleToken(ctx context.Context, account store.MailAccount, recipients []string, raw []byte, trace *smtplog.Recording) error {
	username, err := smtpAuthUsername(account)
	if err != nil {
		return err
	}
	attempts := 0
	return googletoken.WithFreshToken(ctx, s.Tokens, account.UserID, account.GoogleConnectionID,
		func(token string) error {
			attempts++
			if attempts > 1 {
				trace.Note("retrying on a new connection with a refreshed Google token")
			}
			return s.dialAndSend(ctx, account, xoauth2.NewSMTPAuth(username, token), recipients, raw, trace)
		})
}

// passwordAuth returns nil for an account without a user name, which is how a
// relay that takes no credentials has always been configured.
func passwordAuth(account store.MailAccount, password string) smtp.Auth {
	if strings.TrimSpace(account.SMTPUsername) == "" {
		return nil
	}
	return smtp.PlainAuth("", account.SMTPUsername, password, account.SMTPHost)
}

// smtpAuthUsername is the connected Google mailbox, which the account handlers
// copy out of the connection when the server is saved.
//
// There is deliberately no fallback to the envelope address: that is the
// identity being sent as, which for a send-as alias is a different mailbox than
// the one the token belongs to. Authenticating with a mismatched pair fails at
// Google with a credentials error that names neither problem, so an empty name
// is reported here instead.
func smtpAuthUsername(account store.MailAccount) (string, error) {
	name := strings.TrimSpace(account.SMTPUsername)
	if name == "" {
		return "", fmt.Errorf("google SMTP account %d has no connected mailbox to authenticate as", account.ID)
	}
	return name, nil
}

func (s *Sender) dialAndSend(ctx context.Context, account store.MailAccount, auth smtp.Auth, recipients []string, raw []byte, trace *smtplog.Recording) error {
	conn, err := s.dial(ctx, account, trace)
	if err != nil {
		return err
	}
	return sendRawOnConn(ctx, account, auth, recipients, raw, conn, trace)
}

// dialAndVerify is the connection test: the same login the send path performs,
// ended with QUIT instead of a message.
func (s *Sender) dialAndVerify(ctx context.Context, account store.MailAccount, auth smtp.Auth, trace *smtplog.Recording) error {
	conn, err := s.dial(ctx, account, trace)
	if err != nil {
		return err
	}
	defer conn.Close()
	stopContext := watchSMTPContext(ctx, conn)
	defer stopContext()
	conversation, err := startConversation(account, auth, conn, trace)
	if err != nil {
		return err
	}
	conversation.quit()
	return nil
}

// dial opens the connection and, for implicit TLS, completes the handshake.
// Both the dial and the handshake are recorded: an account pointed at a port
// that answers nothing, or one speaking TLS to a plaintext port, fails here and
// has nothing else to show for it.
func (s *Sender) dial(ctx context.Context, account store.MailAccount, trace *smtplog.Recording) (net.Conn, error) {
	timeout := s.Timeout
	if timeout == 0 {
		timeout = 60 * time.Second
	}
	addr := net.JoinHostPort(account.SMTPHost, fmt.Sprintf("%d", account.SMTPPort))
	trace.Note("connecting to " + addr)
	dialer := &net.Dialer{Timeout: timeout}
	rawConn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		trace.Note("connect failed: " + err.Error())
		return nil, fmt.Errorf("connect to SMTP server %s: %w", addr, err)
	}
	trace.Note("connected")
	deadlineConn := &smtpIdleDeadlineConn{Conn: rawConn, timeout: timeout}
	if !account.SMTPUseTLS || account.SMTPPort != 465 {
		return deadlineConn, nil
	}
	// Implicit TLS: the handshake precedes the greeting, so it is done here
	// rather than by the conversation. The deadline wrapper stays underneath
	// the TLS connection so a stalled handshake is still bounded.
	tlsConn := tls.Client(deadlineConn, &tls.Config{ServerName: account.SMTPHost, MinVersion: tls.VersionTLS12})
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		trace.Note("TLS handshake failed: " + err.Error())
		_ = tlsConn.Close()
		return nil, fmt.Errorf("start SMTP TLS: %w", err)
	}
	trace.Note("TLS established: " + tlsSummary(tlsConn.ConnectionState()))
	return tlsConn, nil
}

func sendRawOnConn(ctx context.Context, account store.MailAccount, auth smtp.Auth, recipients []string, raw []byte, conn net.Conn, trace *smtplog.Recording) error {
	defer conn.Close()
	stopContext := watchSMTPContext(ctx, conn)
	defer stopContext()

	conversation, err := startConversation(account, auth, conn, trace)
	if err != nil {
		return err
	}
	fromAddr, err := firstAddress(account.Email)
	if err != nil {
		return err
	}
	if err := conversation.mail(fromAddr); err != nil {
		return err
	}
	for _, recipient := range recipients {
		if err := conversation.rcpt(recipient); err != nil {
			return err
		}
	}
	// The reply to the end of DATA is the server's acceptance. Closing the
	// session without waiting for a QUIT reply avoids a round trip that cannot
	// change the outcome and cannot misreport an accepted message as failed.
	return conversation.data(raw)
}

// startConversation is everything both a send and a connection test do before
// they differ: greeting, EHLO, the TLS upgrade the account asks for, and
// authentication.
func startConversation(account store.MailAccount, auth smtp.Auth, conn net.Conn, trace *smtplog.Recording) (*conversation, error) {
	// Whether the connection is already encrypted is asked of the connection
	// itself rather than re-derived from the port, so the answer cannot drift
	// from what dial actually did.
	_, encrypted := conn.(*tls.Conn)
	conversation, err := newConversation(conn, account.SMTPHost, encrypted, trace)
	if err != nil {
		return nil, err
	}
	if err := conversation.hello(smtpHelloName); err != nil {
		return nil, err
	}
	if account.SMTPUseTLS && !conversation.tls {
		if _, ok := conversation.supports("STARTTLS"); !ok {
			return nil, errors.New("SMTP server does not advertise STARTTLS")
		}
		config := &tls.Config{ServerName: account.SMTPHost, MinVersion: tls.VersionTLS12}
		if err := conversation.startTLS(config, smtpHelloName); err != nil {
			return nil, err
		}
	}
	if auth != nil {
		if err := conversation.auth(auth); err != nil {
			return nil, googletoken.AuthError{Err: err}
		}
	}
	return conversation, nil
}

func watchSMTPContext(ctx context.Context, conn net.Conn) func() {
	if ctx == nil || conn == nil {
		return func() {}
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	return func() { stop() }
}

// BuildRaw constructs the RFC822 message, including text/html alternatives,
// headers, and attachments. It is used for real sends, so at least one
// recipient is required before SMTP is attempted.
func BuildRaw(msg Message) ([]byte, []string, error) {
	return buildRaw(msg, true)
}

// BuildDraftRaw constructs an unsent RFC822 draft. Drafts are allowed to be
// incomplete, so To/Cc/Bcc may all be empty while the MIME body and attachments
// are still preserved for IMAP APPEND.
func BuildDraftRaw(msg Message) ([]byte, error) {
	raw, _, err := buildRaw(msg, false)
	return raw, err
}

func buildRaw(msg Message, requireRecipients bool) ([]byte, []string, error) {
	if msg.Date.IsZero() {
		msg.Date = time.Now()
	}
	from, err := mail.ParseAddress(msg.From)
	if err != nil {
		return nil, nil, fmt.Errorf("from address: %w", err)
	}
	to, err := parseAddresses(msg.To)
	if err != nil {
		return nil, nil, fmt.Errorf("to address: %w", err)
	}
	cc, err := parseAddresses(msg.Cc)
	if err != nil {
		return nil, nil, fmt.Errorf("cc address: %w", err)
	}
	bcc, err := parseAddresses(msg.Bcc)
	if err != nil {
		return nil, nil, fmt.Errorf("bcc address: %w", err)
	}
	recipients := addressStrings(append(append(to, cc...), bcc...))
	if requireRecipients && len(recipients) == 0 {
		return nil, nil, errors.New("message has no recipients")
	}
	if strings.TrimSpace(msg.MessageID) == "" {
		msg.MessageID = NewMessageID(from.Address)
	}

	var b bytes.Buffer
	w := bufio.NewWriter(&b)
	writeHeader(w, "From", from.String())
	if len(to) > 0 {
		writeHeader(w, "To", addressListString(to))
	}
	if len(cc) > 0 {
		writeHeader(w, "Cc", addressListString(cc))
	}
	if !requireRecipients && len(bcc) > 0 {
		writeHeader(w, "Bcc", addressListString(bcc))
	}
	writeHeader(w, "Subject", mime.QEncoding.Encode("utf-8", strings.TrimSpace(msg.Subject)))
	writeHeader(w, "Date", msg.Date.Format(time.RFC1123Z))
	writeHeader(w, "Message-ID", msg.MessageID)
	writeHeader(w, "X-Mailer", xMailerHeaderValue())
	for _, header := range msg.ExtraHeaders {
		writeExtraHeader(w, header.Name, header.Value)
	}
	if strings.TrimSpace(msg.InReplyTo) != "" {
		writeHeader(w, "In-Reply-To", sanitizeHeaderValue(msg.InReplyTo))
	}
	if strings.TrimSpace(msg.References) != "" {
		writeHeader(w, "References", sanitizeHeaderValue(msg.References))
	}
	writeHeader(w, "MIME-Version", "1.0")
	writeRootBody(w, msg)
	if err := w.Flush(); err != nil {
		return nil, nil, err
	}
	return b.Bytes(), recipients, nil
}

func writeRootBody(w *bufio.Writer, msg Message) {
	if msg.MIMEBodyOverride != nil && strings.TrimSpace(msg.MIMEBodyOverride.ContentType) != "" {
		writeHeader(w, "Content-Type", msg.MIMEBodyOverride.ContentType)
		_, _ = w.WriteString("\r\n")
		body := normalizeCRLF(msg.MIMEBodyOverride.Body)
		_, _ = w.WriteString(body)
		if !strings.HasSuffix(body, "\r\n") {
			_, _ = w.WriteString("\r\n")
		}
		return
	}
	inlineAttachments, regularAttachments := splitAttachments(msg.Attachments)
	hasInlineHTML := len(inlineAttachments) > 0 && strings.TrimSpace(msg.BodyHTML) != ""
	if len(msg.Attachments) == 0 {
		writeBodyEntity(w, msg)
		return
	}
	if len(regularAttachments) > 0 {
		boundary := boundaryFor(msg, "mixed")
		writeHeader(w, "Content-Type", mime.FormatMediaType("multipart/mixed", map[string]string{"boundary": boundary}))
		_, _ = w.WriteString("\r\n")
		if hasInlineHTML {
			relatedBoundary := boundaryFor(msg, "related")
			_, _ = fmt.Fprintf(w, "--%s\r\n", boundary)
			writeHeader(w, "Content-Type", mime.FormatMediaType("multipart/related", map[string]string{"boundary": relatedBoundary}))
			_, _ = w.WriteString("\r\n")
			writeBodyEntityPart(w, relatedBoundary, msg)
			for _, attachment := range inlineAttachments {
				writeAttachmentPart(w, relatedBoundary, attachment)
			}
			_, _ = fmt.Fprintf(w, "--%s--\r\n", relatedBoundary)
		} else {
			writeBodyEntityPart(w, boundary, msg)
			regularAttachments = append(inlineAttachments, regularAttachments...)
		}
		for _, attachment := range regularAttachments {
			writeAttachmentPart(w, boundary, attachment)
		}
		_, _ = fmt.Fprintf(w, "--%s--\r\n", boundary)
		return
	}
	if hasInlineHTML {
		boundary := boundaryFor(msg, "related")
		writeHeader(w, "Content-Type", mime.FormatMediaType("multipart/related", map[string]string{"boundary": boundary}))
		_, _ = w.WriteString("\r\n")
		writeBodyEntityPart(w, boundary, msg)
		for _, attachment := range inlineAttachments {
			writeAttachmentPart(w, boundary, attachment)
		}
		_, _ = fmt.Fprintf(w, "--%s--\r\n", boundary)
		return
	}
	boundary := boundaryFor(msg, "mixed")
	writeHeader(w, "Content-Type", mime.FormatMediaType("multipart/mixed", map[string]string{"boundary": boundary}))
	_, _ = w.WriteString("\r\n")
	writeBodyEntityPart(w, boundary, msg)
	for _, attachment := range inlineAttachments {
		writeAttachmentPart(w, boundary, attachment)
	}
	_, _ = fmt.Fprintf(w, "--%s--\r\n", boundary)
}

func splitAttachments(attachments []Attachment) ([]Attachment, []Attachment) {
	var inlineAttachments []Attachment
	var regularAttachments []Attachment
	for _, attachment := range attachments {
		if attachment.Inline {
			inlineAttachments = append(inlineAttachments, attachment)
		} else {
			regularAttachments = append(regularAttachments, attachment)
		}
	}
	return inlineAttachments, regularAttachments
}

func writeBodyEntityPart(w *bufio.Writer, boundary string, msg Message) {
	_, _ = fmt.Fprintf(w, "--%s\r\n", boundary)
	writeBodyEntity(w, msg)
}

func writeBodyEntity(w *bufio.Writer, msg Message) {
	if strings.TrimSpace(msg.BodyHTML) != "" {
		boundary := boundaryFor(msg, "alt")
		writeHeader(w, "Content-Type", mime.FormatMediaType("multipart/alternative", map[string]string{"boundary": boundary}))
		_, _ = w.WriteString("\r\n")
		writePart(w, boundary, `text/plain; charset="utf-8"`, msg.BodyText)
		writePart(w, boundary, `text/html; charset="utf-8"`, msg.BodyHTML)
		_, _ = fmt.Fprintf(w, "--%s--\r\n", boundary)
		return
	}
	writeHeader(w, "Content-Type", `text/plain; charset="utf-8"`)
	writeHeader(w, "Content-Transfer-Encoding", "8bit")
	_, _ = w.WriteString("\r\n")
	body := normalizeCRLF(msg.BodyText)
	_, _ = w.WriteString(body)
	if !strings.HasSuffix(body, "\r\n") {
		_, _ = w.WriteString("\r\n")
	}
}

func writeAttachmentPart(w *bufio.Writer, boundary string, attachment Attachment) {
	_, _ = fmt.Fprintf(w, "--%s\r\n", boundary)
	filename := sanitizeAttachmentFilename(attachment.Filename)
	contentType := strings.TrimSpace(attachment.ContentType)
	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil || strings.TrimSpace(mediaType) == "" {
		mediaType = "application/octet-stream"
		params = map[string]string{}
	}
	if filename != "" {
		params["name"] = filename
	}
	writeHeader(w, "Content-Type", mime.FormatMediaType(mediaType, params))
	writeHeader(w, "Content-Transfer-Encoding", "base64")
	if attachment.Inline && strings.TrimSpace(attachment.ContentID) != "" {
		writeHeader(w, "Content-ID", contentIDHeader(attachment.ContentID))
	}
	disposition := "attachment"
	if attachment.Inline {
		disposition = "inline"
	}
	dispositionParams := map[string]string{}
	if filename != "" {
		dispositionParams["filename"] = filename
	}
	writeHeader(w, "Content-Disposition", mime.FormatMediaType(disposition, dispositionParams))
	_, _ = w.WriteString("\r\n")
	writeBase64Body(w, attachment.Data)
}

func writeBase64Body(w *bufio.Writer, data []byte) {
	encoded := make([]byte, base64.StdEncoding.EncodedLen(len(data)))
	base64.StdEncoding.Encode(encoded, data)
	for len(encoded) > 0 {
		lineLength := 76
		if len(encoded) < lineLength {
			lineLength = len(encoded)
		}
		_, _ = w.Write(encoded[:lineLength])
		_, _ = w.WriteString("\r\n")
		encoded = encoded[lineLength:]
	}
}

func boundaryFor(msg Message, kind string) string {
	return MIMEBoundary(msg.MessageID, kind)
}

// MIMEBoundary returns Rolltop's stable boundary format for a message and body
// kind so callers that prepare a MIMEBodyOverride can match normal compose.
func MIMEBoundary(messageID, kind string) string {
	boundary := "rolltop-" + kind + "-" + strings.Trim(messageID, "<>")
	return strings.NewReplacer("@", "-", ".", "-", "_", "-", "/", "-", "+", "-").Replace(boundary)
}

func contentIDHeader(contentID string) string {
	contentID = strings.Trim(strings.TrimSpace(contentID), "<>")
	return "<" + sanitizeHeaderValue(contentID) + ">"
}

func sanitizeAttachmentFilename(filename string) string {
	filename = strings.TrimSpace(filename)
	filename = strings.ReplaceAll(filename, "\x00", "")
	filename = strings.ReplaceAll(filename, "/", "_")
	filename = strings.ReplaceAll(filename, "\\", "_")
	if filename == "" {
		return "attachment"
	}
	return filename
}

// NewMessageID creates a local Message-ID suitable for outbound composed mail.
func NewMessageID(fromAddress string) string {
	domain := "rolltop.local"
	if _, host, ok := strings.Cut(fromAddress, "@"); ok && strings.TrimSpace(host) != "" {
		domain = strings.ToLower(strings.TrimSpace(host))
	}
	random := make([]byte, 12)
	if _, err := rand.Read(random); err != nil {
		return fmt.Sprintf("<%d@rolltop.%s>", time.Now().UnixNano(), domain)
	}
	return fmt.Sprintf("<%d.%s@%s>", time.Now().UnixNano(), hex.EncodeToString(random), domain)
}

func writePart(w *bufio.Writer, boundary, contentType, body string) {
	_, _ = fmt.Fprintf(w, "--%s\r\n", boundary)
	writeHeader(w, "Content-Type", contentType)
	writeHeader(w, "Content-Transfer-Encoding", "8bit")
	_, _ = w.WriteString("\r\n")
	body = normalizeCRLF(body)
	_, _ = w.WriteString(body)
	if !strings.HasSuffix(body, "\r\n") {
		_, _ = w.WriteString("\r\n")
	}
}

func normalizeCRLF(body string) string {
	body = strings.ReplaceAll(body, "\r\n", "\n")
	body = strings.ReplaceAll(body, "\r", "\n")
	return strings.ReplaceAll(body, "\n", "\r\n")
}

func parseAddresses(values []string) ([]*mail.Address, error) {
	var out []*mail.Address
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		addrs, err := mail.ParseAddressList(value)
		if err != nil {
			return nil, err
		}
		out = append(out, addrs...)
	}
	return out, nil
}

func addressStrings(addrs []*mail.Address) []string {
	out := make([]string, 0, len(addrs))
	for _, addr := range addrs {
		out = append(out, addr.Address)
	}
	return out
}

func addressListString(addrs []*mail.Address) string {
	parts := make([]string, 0, len(addrs))
	for _, addr := range addrs {
		parts = append(parts, addr.String())
	}
	return strings.Join(parts, ", ")
}

func writeHeader(w *bufio.Writer, name, value string) {
	_, _ = fmt.Fprintf(w, "%s: %s\r\n", name, sanitizeHeaderValue(value))
}

func writeExtraHeader(w *bufio.Writer, name, value string) {
	name = sanitizeHeaderName(name)
	value = sanitizeHeaderValue(value)
	if name == "" || value == "" {
		return
	}
	prefix := name + ": "
	_, _ = w.WriteString(prefix)
	firstLineRemaining := 76 - len(prefix)
	if firstLineRemaining < 16 {
		firstLineRemaining = 16
	}
	writeFoldedToken(w, value, firstLineRemaining, 76)
	_, _ = w.WriteString("\r\n")
}

func xMailerHeaderValue() string {
	info := buildinfo.Current()
	version := strings.TrimSpace(info.Version)
	if version == "" {
		version = "dev"
	}
	return "rolltop/" + version
}

func writeFoldedToken(w *bufio.Writer, value string, firstLine, nextLine int) {
	lineLimit := firstLine
	for len(value) > 0 {
		n := lineLimit
		if len(value) < n {
			n = len(value)
		}
		_, _ = w.WriteString(value[:n])
		value = value[n:]
		if value != "" {
			_, _ = w.WriteString("\r\n ")
			lineLimit = nextLine
		}
	}
}

func sanitizeHeaderName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	for _, r := range name {
		switch {
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
			continue
		default:
			return ""
		}
	}
	return name
}

func sanitizeHeaderValue(value string) string {
	value = strings.ReplaceAll(value, "\r", " ")
	value = strings.ReplaceAll(value, "\n", " ")
	return strings.Join(strings.Fields(value), " ")
}

func firstAddress(value string) (string, error) {
	addr, err := mail.ParseAddress(value)
	if err != nil {
		return "", fmt.Errorf("from address: %w", err)
	}
	return addr.Address, nil
}
