// File overview: The Message-IDs this Rolltop sent, remembered per account so
// the provider's own copy of them is recognised whenever it is mirrored.
//
// Every list that shows "mail still in play" already keeps Sent out of itself,
// but it does so through the folder (`show_in_all_mail`), and a Gmail account
// that mirrors [Gmail]/All Mail has one folder holding both what arrived and
// what the user sent. Without this ledger every reply and every filter forward
// came back into Inbox and into the categories as mail waiting on the reader.
//
// What is remembered is only what this installation sent: the Message-ID it
// generated, for the account it sent from. "Mail from my own address" would be
// the wrong test -- a printer, a monitoring system or a spoof can send as the
// reader -- and a message delivered to another account of theirs is mail they
// really received, which is why the account is part of the key.

package store

import (
	"context"
	"errors"
	"strings"
)

// RecordOutgoingMessageID remembers a Message-ID this installation is about to
// send. It is called before the send rather than after: the provider can file
// its copy the moment the message is accepted, and a sync that reaches it first
// would store it as ordinary incoming mail. A recorded id whose send then fails
// costs nothing -- no message will ever carry it.
//
// The row is kept for as long as the account is, and goes with it. A copy is
// not mirrored only once: a folder whose UIDVALIDITY the server resets is
// re-imported from scratch, and an arrival years later has to reach the same
// conclusion the first one did. An expiring window would have quietly put every
// older send back into the reader's lists the next time that happened.
func (s *Store) RecordOutgoingMessageID(ctx context.Context, userID, accountID int64, messageIDHeader string) error {
	messageIDHeader = strings.TrimSpace(messageIDHeader)
	if userID <= 0 || accountID <= 0 {
		return errors.New("invalid outgoing message scope")
	}
	if messageIDHeader == "" {
		return errors.New("outgoing message id is required")
	}
	db, err := s.dataDB(ctx, userID)
	if err != nil {
		return err
	}
	_, err = db.ExecContext(ctx, `INSERT INTO outgoing_message_ids (user_id, account_id, message_id_header, created_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT (user_id, account_id, message_id_header) DO NOTHING`, userID, accountID, messageIDHeader, nowUnix())
	return err
}
