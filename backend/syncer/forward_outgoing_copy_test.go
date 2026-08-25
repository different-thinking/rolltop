package syncer

import (
	"context"
	"testing"

	"rolltop/backend/blob"
	"rolltop/backend/smtpclient"
	"rolltop/backend/store"
)

// forwardTestSender answers a send and reports what the ledger held while the
// message was on its way out. The order is the property under test: the provider
// files its own copy the moment it accepts the message, so an id recorded only
// after the send can be recorded after that copy has already been mirrored as
// ordinary incoming mail.
type forwardTestSender struct {
	store       *store.Store
	userID      int64
	accountID   int64
	sent        []smtpclient.Message
	knownAtSend []bool
}

func (s *forwardTestSender) Send(ctx context.Context, _ store.MailAccount, msg smtpclient.Message) ([]byte, error) {
	s.sent = append(s.sent, msg)
	s.knownAtSend = append(s.knownAtSend, outgoingMessageIDRecorded(ctx, s.store, s.userID, s.accountID, msg.MessageID))
	return nil, nil
}

func outgoingMessageIDRecorded(ctx context.Context, st *store.Store, userID, accountID int64, messageID string) bool {
	var found int64
	err := st.DB().QueryRowContext(ctx, `SELECT 1 FROM outgoing_message_ids
		WHERE user_id = ? AND account_id = ? AND message_id_header = ?`, userID, accountID, messageID).Scan(&found)
	return err == nil && found == 1
}

// An automatic forward leaves through the account's own SMTP, and the provider
// keeps its copy: on a Gmail account that mirrors All Mail, that copy comes back
// through sync. It is recognised as the reader's own outgoing mail only if the
// Message-ID was remembered before it went out.
func TestForwardRecordsItsMessageIDBeforeSending(t *testing.T) {
	ctx := context.Background()
	fixture := newMoveTestFixture(t)
	fixture.service.Blobs = blob.New(t.TempDir())
	message := storeRawForMessage(t, ctx, fixture, fixture.service.Blobs, fixture.message,
		[]byte("From: studio@example.test\r\nTo: reader@example.test\r\nSubject: Receipt\r\n\r\nBody\r\n"))
	smtpAccount, err := fixture.store.CreateSMTPAccount(ctx, store.SMTPAccount{
		UserID: fixture.userID, Label: "Outgoing", Host: "smtp.example.test", Port: 587,
		Username: "move-hook@example.test", EncryptedPassword: "encrypted-test-value", UseTLS: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.CreateMailIdentityForUser(ctx, fixture.userID, store.MailIdentity{
		Email: "move-hook@example.test", DisplayName: "Move Hook",
		IMAPAccountID: fixture.account.ID, SMTPAccountID: smtpAccount.ID,
	}); err != nil {
		t.Fatal(err)
	}
	sender := &forwardTestSender{store: fixture.store, userID: fixture.userID, accountID: fixture.account.ID}
	fixture.service.Sender = sender

	if err := fixture.service.ForwardMessage(ctx, fixture.userID, message.ID, "books@example.test", nil); err != nil {
		t.Fatal(err)
	}

	if len(sender.sent) != 1 {
		t.Fatalf("sent %d messages, want 1", len(sender.sent))
	}
	if !sender.knownAtSend[0] {
		t.Fatal("the forward's Message-ID was not recorded before the send, so the provider's copy of it comes back as incoming mail")
	}
}

// The identity a forward leaves through is preferably the message's own
// account's, but the fallback pass takes any identity with an outgoing server.
// It is the account the mail actually leaves through that keeps the copy, so
// that is the account the id has to be recorded for -- recorded against the
// forwarded message's account instead, the copy comes back unrecognised.
func TestForwardRecordsItsMessageIDForTheSendingAccount(t *testing.T) {
	ctx := context.Background()
	fixture := newMoveTestFixture(t)
	fixture.service.Blobs = blob.New(t.TempDir())
	message := storeRawForMessage(t, ctx, fixture, fixture.service.Blobs, fixture.message,
		[]byte("From: studio@example.test\r\nTo: reader@example.test\r\nSubject: Receipt\r\n\r\nBody\r\n"))
	sending, err := fixture.store.CreateMailAccount(ctx, store.MailAccount{
		UserID: fixture.userID, Email: "sender@example.test", Host: "imap.example.test", Port: 993,
		Username: "sender", EncryptedPassword: "encrypted-test-value", UseTLS: true, Mailbox: store.DefaultMailboxPattern,
	})
	if err != nil {
		t.Fatal(err)
	}
	smtpAccount, err := fixture.store.CreateSMTPAccount(ctx, store.SMTPAccount{
		UserID: fixture.userID, Label: "Outgoing", Host: "smtp.example.test", Port: 587,
		Username: "sender@example.test", EncryptedPassword: "encrypted-test-value", UseTLS: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	// The only identity there is belongs to the other account, so the fallback
	// pass is what answers -- the message's own account has none.
	if _, err := fixture.store.CreateMailIdentityForUser(ctx, fixture.userID, store.MailIdentity{
		Email: "sender@example.test", DisplayName: "Sender",
		IMAPAccountID: sending.ID, SMTPAccountID: smtpAccount.ID,
	}); err != nil {
		t.Fatal(err)
	}
	sender := &forwardTestSender{store: fixture.store, userID: fixture.userID, accountID: sending.ID}
	fixture.service.Sender = sender

	if err := fixture.service.ForwardMessage(ctx, fixture.userID, message.ID, "books@example.test", nil); err != nil {
		t.Fatal(err)
	}

	if len(sender.sent) != 1 {
		t.Fatalf("sent %d messages, want 1", len(sender.sent))
	}
	if !sender.knownAtSend[0] {
		t.Fatal("the id was not recorded for the account the forward left through")
	}
	if outgoingMessageIDRecorded(ctx, fixture.store, fixture.userID, fixture.account.ID, sender.sent[0].MessageID) {
		t.Fatal("the id was recorded for the forwarded message's account, which never sees the copy")
	}
}
