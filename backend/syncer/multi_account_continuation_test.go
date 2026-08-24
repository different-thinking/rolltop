// File overview: One failing account must not block the remaining accounts' sync.

package syncer

import (
	"context"
	"errors"
	"strings"
	"testing"

	"rolltop/backend/store"
	"rolltop/backend/store/storetest"

	"rolltop/internal/testlog"
)

// splitAccountFetcher fails everything for one host and syncs an empty INBOX
// for every other.
type splitAccountFetcher struct {
	*moveTestFetcher
	failingHost  string
	fetchedHosts []string
}

func (f *splitAccountFetcher) MailboxStatus(_ context.Context, account store.MailAccount, _ string) (MailboxStatus, error) {
	if account.Host == f.failingHost {
		return MailboxStatus{}, errors.New("host is unreachable")
	}
	return MailboxStatus{UIDNext: 1, UIDValidity: 1}, nil
}

func (f *splitAccountFetcher) FetchMailboxWithUIDValidity(_ context.Context, account store.MailAccount, _ string, _ uint32, _ uint32, _ func(FetchedMessage) error) error {
	f.fetchedHosts = append(f.fetchedHosts, account.Host)
	return nil
}

func (f *splitAccountFetcher) FetchUIDsWithUIDValidity(context.Context, store.MailAccount, string, []uint32, uint32, func(FetchedMessage) error) error {
	return errors.New("unexpected sparse fetch")
}

func TestSyncUserContinuesPastAFailingAccount(t *testing.T) {
	testlog.Capture(t)
	ctx := context.Background()
	db, err := storetest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	user, err := db.CreateUser(ctx, "multi-account@example.test", "Multi Account", "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	broken, err := db.CreateMailAccount(ctx, store.MailAccount{
		UserID: user.ID, Email: "broken@example.test", Host: "broken.example.test", Port: 993,
		Username: "broken", EncryptedPassword: "encrypted-test-value", UseTLS: true, Mailbox: "INBOX",
	})
	if err != nil {
		t.Fatal(err)
	}
	healthy, err := db.CreateMailAccount(ctx, store.MailAccount{
		UserID: user.ID, Email: "healthy@example.test", Host: "healthy.example.test", Port: 993,
		Username: "healthy", EncryptedPassword: "encrypted-test-value", UseTLS: true, Mailbox: "INBOX",
	})
	if err != nil {
		t.Fatal(err)
	}
	fetcher := &splitAccountFetcher{moveTestFetcher: &moveTestFetcher{}, failingHost: broken.Host}
	var outcomes []error
	service := &Service{Store: db, Fetcher: fetcher,
		AccountSyncOutcome: func(_, _ int64, err error) { outcomes = append(outcomes, err) }}

	_, err = service.SyncUserMailboxes(ctx, user.ID, []string{"INBOX"})
	if err == nil || !strings.Contains(err.Error(), "unreachable") {
		t.Fatalf("sync error = %v, want the broken account's failure reported", err)
	}
	// The healthy account was still visited and fetched: a single unreachable
	// server used to abort the loop for every account behind it.
	if len(fetcher.fetchedHosts) != 1 || fetcher.fetchedHosts[0] != healthy.Host {
		t.Fatalf("fetched hosts = %v, want only %q", fetcher.fetchedHosts, healthy.Host)
	}
	if len(outcomes) != 2 {
		t.Fatalf("account outcomes = %d, want one per account", len(outcomes))
	}
	if outcomes[0] == nil || outcomes[1] != nil {
		t.Fatalf("outcomes = [%v, %v], want the broken account failed and the healthy one clean", outcomes[0], outcomes[1])
	}
}
