// File overview: One held IMAP connection reused across the batches of one purge.

package imapclient

import (
	"context"
	"errors"

	"github.com/emersion/go-imap/client"

	"rolltop/backend/store"
	"rolltop/backend/syncer"
)

var (
	_ syncer.ExpungeSessionFetcher = (*Fetcher)(nil)
	_ syncer.ExpungeSession        = (*ExpungeSession)(nil)
)

// ExpungeSession owns one authenticated connection for the batches of a single
// folder purge. Its methods must be called sequentially; go-imap does not
// permit overlapping commands on one client connection.
type ExpungeSession struct {
	client      *client.Client
	stopContext func()
}

// OpenExpungeSession logs in once so the batches that follow only cost their own
// IMAP commands.
func (f *Fetcher) OpenExpungeSession(ctx context.Context, account store.MailAccount) (syncer.ExpungeSession, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if f == nil {
		return nil, errors.New("open expunge session requires a fetcher")
	}
	c, err := f.loginWithinContext(ctx, account)
	if err != nil {
		return nil, err
	}
	return &ExpungeSession{client: c, stopContext: watchClientContext(ctx, c)}, nil
}

// ExpungeMessages deletes one batch on the held connection. Each batch still
// selects the folder and proves its generation, so reusing the connection costs
// nothing in safety: only the login is saved.
func (s *ExpungeSession) ExpungeMessages(ctx context.Context, mailbox string, uids []uint32, expectedUIDValidity uint32, scope syncer.ExpungeScope) ([]uint32, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s == nil || s.client == nil {
		return nil, errors.New("expunge session is closed")
	}
	return expungeMessages(ctx, s.client, mailbox, uids, expectedUIDValidity, scope)
}

// Close drops the held connection. It is safe to call more than once once all
// session operations have stopped.
func (s *ExpungeSession) Close() error {
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
