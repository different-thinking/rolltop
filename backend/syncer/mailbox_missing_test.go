// File overview: A folder job carries a name, not an account. What the accounts
// that have no such folder do with it -- which used to be fail their whole sync.

package syncer

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"rolltop/backend/store"
	"rolltop/backend/store/storetest"

	"rolltop/internal/testlog"
)

// missingMailboxFetcher answers for the folders each host really has and
// refuses the rest the way a mail server does.
type missingMailboxFetcher struct {
	*moveTestFetcher
	folders     map[string][]string
	statusCalls []string
	fetched     []string
}

func (f *missingMailboxFetcher) MailboxStatus(_ context.Context, account store.MailAccount, mailbox string) (MailboxStatus, error) {
	f.statusCalls = append(f.statusCalls, account.Host+":"+mailbox)
	if !slices.Contains(f.folders[account.Host], mailbox) {
		return MailboxStatus{}, MailboxGone(fmt.Errorf("status mailbox %q: Mailbox doesn't exist: %s", mailbox, mailbox))
	}
	return MailboxStatus{UIDNext: 1, UIDValidity: 1}, nil
}

func (f *missingMailboxFetcher) FetchMailboxWithUIDValidity(_ context.Context, account store.MailAccount, mailbox string, _ uint32, _ uint32, _ func(FetchedMessage) error) error {
	f.fetched = append(f.fetched, account.Host+":"+mailbox)
	return nil
}

func (f *missingMailboxFetcher) FetchUIDsWithUIDValidity(context.Context, store.MailAccount, string, []uint32, uint32, func(FetchedMessage) error) error {
	return errors.New("unexpected sparse fetch")
}

// missingMailboxAccounts sets up the shape the bug needs: one account whose
// folder list is where the name comes from, and one plain host that has never
// heard of it.
func missingMailboxAccounts(t *testing.T) (*store.Store, store.User, store.MailAccount, store.MailAccount) {
	t.Helper()
	ctx := context.Background()
	db, err := storetest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	user, err := db.CreateUser(ctx, "folders@example.test", "Folders", "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	labelled, err := db.CreateMailAccount(ctx, store.MailAccount{
		UserID: user.ID, Email: "labelled@example.test", Host: "imap.gmail.test", Port: 993,
		Username: "labelled", EncryptedPassword: "encrypted-test-value", UseTLS: true, Mailbox: "*",
	})
	if err != nil {
		t.Fatal(err)
	}
	plain, err := db.CreateMailAccount(ctx, store.MailAccount{
		UserID: user.ID, Email: "plain@example.test", Host: "plain.kasserver.test", Port: 993,
		Username: "plain", EncryptedPassword: "encrypted-test-value", UseTLS: true, Mailbox: "*",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"INBOX", "[Gmail]/Gesendet"} {
		if _, err := db.GetOrCreateMailbox(ctx, user.ID, labelled.ID, name); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.GetOrCreateMailbox(ctx, user.ID, plain.ID, "INBOX"); err != nil {
		t.Fatal(err)
	}
	return db, user, labelled, plain
}

func TestSyncSkipsAFolderTheAccountDoesNotHave(t *testing.T) {
	testlog.Capture(t)
	ctx := context.Background()
	db, user, labelled, plain := missingMailboxAccounts(t)
	// The row a previous run left behind on the plain account: created from the
	// name alone, before any server was asked about it.
	if _, err := db.GetOrCreateMailbox(ctx, user.ID, plain.ID, "[Gmail]/Gesendet"); err != nil {
		t.Fatal(err)
	}
	fetcher := &missingMailboxFetcher{moveTestFetcher: &moveTestFetcher{}, folders: map[string][]string{
		labelled.Host: {"INBOX", "[Gmail]/Gesendet"},
		plain.Host:    {"INBOX"},
	}}
	service := &Service{Store: db, Fetcher: fetcher}

	if _, err := service.SyncUserMailboxes(ctx, user.ID, []string{"[Gmail]/Gesendet"}); err != nil {
		t.Fatalf("sync error = %v, want the missing folder skipped rather than reported", err)
	}
	// The account that has the folder still mirrored it.
	if want := labelled.Host + ":[Gmail]/Gesendet"; !slices.Contains(fetcher.fetched, want) {
		t.Fatalf("fetched = %v, want %q", fetcher.fetched, want)
	}
	if slices.ContainsFunc(fetcher.fetched, func(call string) bool { return call == plain.Host+":[Gmail]/Gesendet" }) {
		t.Fatalf("fetched = %v, want no fetch on the account without the folder", fetcher.fetched)
	}
	// The row that only ever existed locally is gone, so the folder stops being
	// listed under an account that does not have it.
	if _, err := db.GetMailbox(ctx, user.ID, plain.ID, "[Gmail]/Gesendet"); !store.IsNotFound(err) {
		t.Fatalf("stale mailbox row lookup = %v, want not found", err)
	}
	// The folder the plain account does have is untouched by the skip.
	if _, err := db.GetMailbox(ctx, user.ID, plain.ID, "INBOX"); err != nil {
		t.Fatalf("plain INBOX lookup = %v, want the account's own folder kept", err)
	}
}

func TestSyncDoesNotAskAnAccountForAnotherAccountsFolder(t *testing.T) {
	testlog.Capture(t)
	ctx := context.Background()
	db, user, labelled, plain := missingMailboxAccounts(t)
	fetcher := &missingMailboxFetcher{moveTestFetcher: &moveTestFetcher{}, folders: map[string][]string{
		labelled.Host: {"INBOX", "[Gmail]/Gesendet"},
		plain.Host:    {"INBOX"},
	}}
	service := &Service{Store: db, Fetcher: fetcher}

	if _, err := service.SyncUserMailboxes(ctx, user.ID, []string{"[Gmail]/Gesendet"}); err != nil {
		t.Fatalf("sync error = %v, want a clean run", err)
	}
	// An account whose folders are known is not asked about a name that is not
	// among them: the round trip is spent, and refused, for nothing.
	if slices.Contains(fetcher.statusCalls, plain.Host+":[Gmail]/Gesendet") {
		t.Fatalf("status calls = %v, want none for the account without the folder", fetcher.statusCalls)
	}
	if _, err := db.GetMailbox(ctx, user.ID, plain.ID, "[Gmail]/Gesendet"); !store.IsNotFound(err) {
		t.Fatalf("mailbox row lookup = %v, want no row created for another account's folder", err)
	}
}

// The union that feeds folder jobs keeps one spelling per name, so an account
// whose own folder differs only in case must still be asked for it -- under its
// own spelling, which is what its server answers to.
func TestSyncAsksAnAccountForItsOwnSpellingOfAFolder(t *testing.T) {
	testlog.Capture(t)
	ctx := context.Background()
	db, user, labelled, plain := missingMailboxAccounts(t)
	if _, err := db.GetOrCreateMailbox(ctx, user.ID, plain.ID, "Gesendet"); err != nil {
		t.Fatal(err)
	}
	fetcher := &missingMailboxFetcher{moveTestFetcher: &moveTestFetcher{}, folders: map[string][]string{
		labelled.Host: {"INBOX", "[Gmail]/Gesendet"},
		plain.Host:    {"INBOX", "Gesendet"},
	}}
	service := &Service{Store: db, Fetcher: fetcher}

	// The name arrives folded, the way the cross-account union hands it over.
	if _, err := service.SyncUserMailboxes(ctx, user.ID, []string{"gesendet"}); err != nil {
		t.Fatalf("sync error = %v, want a clean run", err)
	}
	if want := plain.Host + ":Gesendet"; !slices.Contains(fetcher.fetched, want) {
		t.Fatalf("fetched = %v, want %q", fetcher.fetched, want)
	}
	if slices.Contains(fetcher.statusCalls, plain.Host+":gesendet") {
		t.Fatalf("status calls = %v, want the account's own spelling", fetcher.statusCalls)
	}
}

// A folder that really was on this account and holds mirrored mail cannot be
// dropped, so skipping it silently would leave it stale forever with nothing
// said. The run says it -- without failing the account, which would put every
// other folder it has behind a backoff.
func TestSyncReportsAFolderThatLeftTheServerWithMailBehind(t *testing.T) {
	testlog.Capture(t)
	ctx := context.Background()
	db, user, _, plain := missingMailboxAccounts(t)
	archive, err := db.GetOrCreateMailbox(ctx, user.ID, plain.ID, "Archiv")
	if err != nil {
		t.Fatal(err)
	}
	blob, err := db.CreateBlob(ctx, store.BlobRecord{UserID: user.ID, Kind: "message",
		Path: "users/1/blobs/accounts/1/mailboxes/Archiv/uid-1.eml", SHA256: "deadbeef", Size: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateMessage(ctx, store.CreateMessage{
		UserID: user.ID, AccountID: plain.ID, MailboxID: archive.ID, BlobID: blob.ID,
		MessageIDHeader: "<mirrored@example.test>", Date: time.Now().UTC(), InternalDate: time.Now().UTC(),
		UID: 1, Size: blob.Size, BlobPath: blob.Path,
	}); err != nil {
		t.Fatal(err)
	}
	fetcher := &missingMailboxFetcher{moveTestFetcher: &moveTestFetcher{}, folders: map[string][]string{
		plain.Host: {"INBOX"},
	}}
	var outcomes []error
	service := &Service{Store: db, Fetcher: fetcher,
		AccountSyncOutcome: func(_, _ int64, err error) { outcomes = append(outcomes, err) }}

	if _, err := service.SyncUserMailboxes(ctx, user.ID, []string{"Archiv", "INBOX"}); err != nil {
		t.Fatalf("sync error = %v, want the account reported healthy", err)
	}
	for _, outcome := range outcomes {
		if outcome != nil {
			t.Fatalf("account outcome = %v, want no account put into backoff", outcome)
		}
	}
	// The folder it does have was still synced.
	if want := plain.Host + ":INBOX"; !slices.Contains(fetcher.fetched, want) {
		t.Fatalf("fetched = %v, want %q", fetcher.fetched, want)
	}
	// The mirrored mail stays where it is.
	if _, err := db.GetMailbox(ctx, user.ID, plain.ID, "Archiv"); err != nil {
		t.Fatalf("mailbox lookup = %v, want the folder holding mail kept", err)
	}
	runs, err := db.ListSyncRunsForUser(ctx, user.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	said := false
	for _, run := range runs {
		if run.AccountID != plain.ID {
			continue
		}
		if strings.Contains(run.Error, "Archiv") {
			said = true
			if run.Status != "ok" {
				t.Fatalf("run status = %q, want the run itself reported clean", run.Status)
			}
		}
	}
	if !said {
		t.Fatalf("no sync run named the folder that left the server: %+v", runs)
	}
}
