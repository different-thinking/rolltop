// File overview: One held IMAP connection reused across a run of moves.

package imapclient

import (
	"context"
	"errors"

	"github.com/emersion/go-imap/client"

	"rolltop/backend/store"
	"rolltop/backend/syncer"
)

var _ syncer.MoveSessionFetcher = (*Fetcher)(nil)

// MoveSession owns one authenticated connection for a run of moves inside a
// single account. A whole-filter delete moves thousands of messages one command
// at a time, and a fresh TCP handshake, TLS negotiation and LOGIN for each of
// them dominates the run and is what mail hosts throttle. Its methods must be
// called sequentially; go-imap does not permit overlapping commands on one
// client connection.
type MoveSession struct {
	client      *client.Client
	stopContext func()
}

// OpenMoveSession logs in once so the moves that follow only cost their own
// IMAP commands.
func (f *Fetcher) OpenMoveSession(ctx context.Context, account store.MailAccount) (syncer.MoveSession, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if f == nil {
		return nil, errors.New("open move session requires a fetcher")
	}
	c, err := f.loginWithinContext(ctx, account)
	if err != nil {
		return nil, err
	}
	return &MoveSession{client: c, stopContext: watchClientContext(ctx, c)}, nil
}

// MoveMessageWithReceipt runs one move on the held connection. Each move still
// selects its source mailbox and proves the mailbox generation, so reusing the
// connection costs nothing in safety: only the login is saved.
func (s *MoveSession) MoveMessageWithReceipt(ctx context.Context, sourceMailbox, destMailbox string, uid uint32, expectedSourceUIDValidity uint32) (*syncer.MoveReceipt, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s == nil || s.client == nil {
		return nil, errors.New("move session is closed")
	}
	return moveMessageWithReceipt(ctx, s.client, sourceMailbox, destMailbox, uid, expectedSourceUIDValidity)
}

// Close drops the held connection. It is safe to call more than once once all
// session operations have stopped.
func (s *MoveSession) Close() error {
	if s == nil || s.client == nil {
		return nil
	}
	c := s.client
	s.client = nil
	if s.stopContext != nil {
		s.stopContext()
		s.stopContext = nil
	}
	return terminateClient(c)
}
