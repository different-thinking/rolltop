// File overview: Tests for saving mail accounts that authenticate with Google,
// including tenant isolation of the connection reference and the sync cutoff.

package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"rolltop/backend/store"
)

func googleAccountInput(connectionID int64) accountSettingsInput {
	return accountSettingsInput{
		Email:              "user@gmail.example.test",
		AuthType:           store.AuthTypeGoogleOAuth,
		GoogleConnectionID: connectionID,
	}
}

func TestSavingAGoogleAccountUsesGmailEndpointsAndStoresNoPassword(t *testing.T) {
	env := newGoogleTestEnv(t)
	connection := env.connect(t, env.owner)
	account, message, err := env.server.saveMailAccountFromInput(
		context.Background(), env.owner.ID, googleAccountInput(connection.ID))
	if err != nil {
		t.Fatalf("save: %v (%s)", err, message)
	}
	if account.Host != gmailIMAPHost || account.Port != gmailIMAPPort || !account.UseTLS {
		t.Fatalf("IMAP endpoint = %s:%d tls=%t", account.Host, account.Port, account.UseTLS)
	}
	if account.SMTPHost != gmailSMTPHost || account.SMTPPort != gmailSMTPPort || !account.SMTPUseTLS {
		t.Fatalf("SMTP endpoint = %s:%d tls=%t", account.SMTPHost, account.SMTPPort, account.SMTPUseTLS)
	}
	// A password column that still holds something would keep working after the
	// user believes they moved the account to Google sign-in.
	if account.EncryptedPassword != "" || account.EncryptedSMTPPassword != "" {
		t.Fatal("a Google account stored a password")
	}
	if !account.UsesGoogleOAuth() || account.GoogleConnectionID != connection.ID {
		t.Fatalf("auth = %q connection = %d", account.AuthType, account.GoogleConnectionID)
	}
}

// The submission host is fixed and the port defaults to 587: the sender opens
// TLS directly only on 465 and upgrades with STARTTLS everywhere else
// (backend/smtpclient/sender.go), Google serves both, and 465 is the one
// hosters block against spam -- which on such a network left an account timing
// out with nothing its owner could change. The choice stays available, because
// a network that blocks 587 instead would be the same dead end reversed, and
// it is a choice between Google's two ports rather than a free field: a third
// port would be a typo, not a setting.
func TestGmailSubmissionDefaultsTo587AndKeeps465WhenAsked(t *testing.T) {
	for _, test := range []struct {
		name      string
		submitted int
		want      int
	}{
		{name: "nothing chosen", submitted: 0, want: 587},
		{name: "the default", submitted: 587, want: 587},
		{name: "implicit TLS", submitted: 465, want: 465},
		{name: "a port Google does not serve", submitted: 25, want: 587},
	} {
		t.Run(test.name, func(t *testing.T) {
			host := "typed.example.test"
			port := test.submitted
			var useTLS bool
			password := "kept"
			applyGmailSMTPEndpoint(&host, &port, &useTLS, &password)
			if host != "smtp.gmail.com" || port != test.want || !useTLS {
				t.Fatalf("endpoint = %s:%d tls=%t, want smtp.gmail.com:%d with TLS", host, port, useTLS, test.want)
			}
			if password != "" {
				t.Fatalf("a Google endpoint kept a password: %q", password)
			}
		})
	}
}

// The connection identifier arrives from the browser. Accepting one that
// belongs to another tenant would authenticate this account as that tenant's
// mailbox, which is exactly the isolation rule the project is built on.
func TestSavingAGoogleAccountRejectsAnotherTenantsConnection(t *testing.T) {
	env := newGoogleTestEnv(t)
	foreign := env.connect(t, env.other)
	_, message, err := env.server.saveMailAccountFromInput(
		context.Background(), env.owner.ID, googleAccountInput(foreign.ID))
	if err == nil {
		t.Fatal("saved an account pointing at another tenant's Google connection")
	}
	if !strings.Contains(message, "not connected") {
		t.Fatalf("message = %q, want it to report the connection as unavailable", message)
	}
	accounts, err := env.db.ListMailAccountsForUser(context.Background(), env.owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(accounts) != 0 {
		t.Fatalf("stored accounts = %d, want none", len(accounts))
	}
}

func TestSavingAGoogleAccountRequiresAConnection(t *testing.T) {
	env := newGoogleTestEnv(t)
	_, message, err := env.server.saveMailAccountFromInput(
		context.Background(), env.owner.ID, googleAccountInput(0))
	if err == nil {
		t.Fatal("saved a Google account with no connection")
	}
	if !strings.Contains(message, "Choose which connected Google account") {
		t.Fatalf("message = %q", message)
	}
}

// Onboarding creates the outgoing server. Requiring a password there would have
// left a Google mailbox able to receive but not send.
func TestOnboardingGivesAGoogleAccountAnOAuthOutgoingServer(t *testing.T) {
	env := newGoogleTestEnv(t)
	connection := env.connect(t, env.owner)
	ctx := context.Background()
	account, message, err := env.server.saveMailAccountFromInput(ctx, env.owner.ID, googleAccountInput(connection.ID))
	if err != nil {
		t.Fatalf("save: %v (%s)", err, message)
	}
	if err := env.server.ensureMailAccountOnboarding(ctx, env.owner, account, true); err != nil {
		t.Fatalf("onboarding: %v", err)
	}
	outgoing, err := env.db.ListSMTPAccountsForUser(ctx, env.owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(outgoing) != 1 {
		t.Fatalf("outgoing servers = %d, want 1", len(outgoing))
	}
	if !outgoing[0].UsesGoogleOAuth() || outgoing[0].GoogleConnectionID != connection.ID {
		t.Fatalf("outgoing server auth = %q connection = %d", outgoing[0].AuthType, outgoing[0].GoogleConnectionID)
	}
	if outgoing[0].EncryptedPassword != "" {
		t.Fatal("the outgoing server stored a password for an OAuth account")
	}
}

// Switching back has to demand a password: the OAuth row stores an empty one,
// and reusing it would produce an account that silently cannot authenticate.
func TestSwitchingAGoogleAccountBackToPasswordRequiresOne(t *testing.T) {
	env := newGoogleTestEnv(t)
	connection := env.connect(t, env.owner)
	ctx := context.Background()
	account, _, err := env.server.saveMailAccountFromInput(ctx, env.owner.ID, googleAccountInput(connection.ID))
	if err != nil {
		t.Fatal(err)
	}
	_, message, err := env.server.saveMailAccountFromInput(ctx, env.owner.ID, accountSettingsInput{
		ID:       account.ID,
		Email:    account.Email,
		Host:     "imap.example.test",
		Port:     993,
		Username: account.Email,
		AuthType: store.AuthTypePassword,
	})
	if err == nil {
		t.Fatal("switched to password authentication without a password")
	}
	if !strings.Contains(message, "Enter an IMAP password") {
		t.Fatalf("message = %q", message)
	}
	saved, err := env.db.GetMailAccountForUser(ctx, env.owner.ID, account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !saved.UsesGoogleOAuth() {
		t.Fatal("the rejected switch still changed the stored account")
	}
}

func TestSavingAGoogleAccountKeepsTheSyncStartDate(t *testing.T) {
	env := newGoogleTestEnv(t)
	connection := env.connect(t, env.owner)
	input := googleAccountInput(connection.ID)
	input.SyncStartAt = "2024-03-01"
	account, message, err := env.server.saveMailAccountFromInput(context.Background(), env.owner.ID, input)
	if err != nil {
		t.Fatalf("save: %v (%s)", err, message)
	}
	want := time.Date(2024, time.March, 1, 0, 0, 0, 0, time.UTC)
	if !account.SyncStartAt.Equal(want) {
		t.Fatalf("sync start = %s, want %s", account.SyncStartAt, want)
	}
	if got := apiAccountFromStore(account).SyncStartAt; got != "2024-03-01" {
		t.Fatalf("presented sync start = %q, want 2024-03-01", got)
	}
}

// XOAUTH2 sends the login name next to the token and Google rejects the pair
// when they disagree. The email field is the user's From address and may well
// be an alias, so it must not become the login name.
func TestGoogleAccountAuthenticatesAsTheConnectedMailboxNotTheTypedAddress(t *testing.T) {
	env := newGoogleTestEnv(t)
	connection := env.connect(t, env.owner)
	input := googleAccountInput(connection.ID)
	input.Email = "alias@company.example.test"
	account, message, err := env.server.saveMailAccountFromInput(context.Background(), env.owner.ID, input)
	if err != nil {
		t.Fatalf("save: %v (%s)", err, message)
	}
	if account.Email != "alias@company.example.test" {
		t.Fatalf("email = %q, want the alias the user chose to send as", account.Email)
	}
	if account.Username != connection.GoogleEmail || account.SMTPUsername != connection.GoogleEmail {
		t.Fatalf("login names = %q/%q, want %q", account.Username, account.SMTPUsername, connection.GoogleEmail)
	}
}

// The date picker sends the browser's local date, so east of UTC "today" is
// already tomorrow by UTC and would be refused as a future date.
func TestSyncStartDateAcceptsTomorrowForTimezonesAheadOfUTC(t *testing.T) {
	env := newGoogleTestEnv(t)
	connection := env.connect(t, env.owner)
	input := googleAccountInput(connection.ID)
	input.SyncStartAt = time.Now().UTC().AddDate(0, 0, 1).Format("2006-01-02")
	if _, message, err := env.server.saveMailAccountFromInput(
		context.Background(), env.owner.ID, input); err != nil {
		t.Fatalf("save with tomorrow's date: %v (%s)", err, message)
	}
}

// A cutoff in the future mirrors nothing at all, which reads as a broken
// account rather than a setting.
func TestSyncStartDateRejectsUnusableValues(t *testing.T) {
	env := newGoogleTestEnv(t)
	connection := env.connect(t, env.owner)
	for name, value := range map[string]string{
		"not a date": "last tuesday",
		"future":     time.Now().UTC().AddDate(0, 0, 3).Format("2006-01-02"),
	} {
		t.Run(name, func(t *testing.T) {
			input := googleAccountInput(connection.ID)
			input.SyncStartAt = value
			if _, message, err := env.server.saveMailAccountFromInput(
				context.Background(), env.owner.ID, input); err == nil {
				t.Fatalf("accepted %q with message %q", value, message)
			}
		})
	}
}

// The outgoing server is saved by its own handler, which had no equivalent of
// the incoming side's guard: an OAuth row stores an empty password, so carrying
// it over produced a password account that fails at the next send with a
// decryption error and nothing on screen explaining it.
func TestSwitchingAGoogleSMTPServerBackToPasswordRequiresOne(t *testing.T) {
	env := newGoogleTestEnv(t)
	connection := env.connect(t, env.owner)
	created := env.send(t, env.owner, http.MethodPost, "/api/account/smtp",
		[]byte(fmt.Sprintf(`{"label":"Gmail","auth_type":"google_oauth","google_connection_id":%d}`, connection.ID)))
	if created.Code != http.StatusOK {
		t.Fatalf("create status=%d body=%s", created.Code, created.Body.String())
	}
	var payload struct {
		SMTPAccount apiSMTPAccount `json:"smtp_account"`
	}
	if err := json.NewDecoder(created.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if payload.SMTPAccount.Username != connection.GoogleEmail {
		t.Fatalf("login name = %q, want the connected mailbox %q", payload.SMTPAccount.Username, connection.GoogleEmail)
	}

	switched := env.send(t, env.owner, http.MethodPost, "/api/account/smtp",
		[]byte(fmt.Sprintf(`{"id":%d,"label":"Gmail","host":"smtp.example.test","port":587,"username":"someone","auth_type":"password"}`,
			payload.SMTPAccount.ID)))
	if switched.Code != http.StatusBadRequest {
		t.Fatalf("switch status=%d body=%s, want 400", switched.Code, switched.Body.String())
	}
	if !strings.Contains(switched.Body.String(), "Enter a password") {
		t.Fatalf("body = %s, want it to ask for a password", switched.Body.String())
	}
	stored, err := env.db.GetSMTPAccountForUser(context.Background(), env.owner.ID, payload.SMTPAccount.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !stored.UsesGoogleOAuth() {
		t.Fatal("the rejected switch still changed the stored server")
	}
}

// The outgoing-server route accepts the same browser-supplied connection id as
// the incoming one. Both go through googleConnectionForAccount, but the guard is
// worth pinning at each entry point: a later refactor of one handler must not be
// able to open a path to another tenant's mailbox.
func TestSavingAGoogleSMTPServerRejectsAnotherTenantsConnection(t *testing.T) {
	env := newGoogleTestEnv(t)
	foreign := env.connect(t, env.other)
	response := env.send(t, env.owner, http.MethodPost, "/api/account/smtp",
		[]byte(fmt.Sprintf(`{"label":"Gmail","auth_type":"google_oauth","google_connection_id":%d}`, foreign.ID)))
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s, want 400", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "not connected") {
		t.Fatalf("body = %s, want it to report the connection as unavailable", response.Body.String())
	}
	servers, err := env.db.ListSMTPAccountsForUser(context.Background(), env.owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(servers) != 0 {
		t.Fatalf("stored outgoing servers = %d, want none", len(servers))
	}
}

// An account list that cannot name the Google identity leaves the user unable
// to tell which of several connected accounts a mailbox signs in as.
func TestAccountListNamesTheConnectedGoogleAddress(t *testing.T) {
	env := newGoogleTestEnv(t)
	connection := env.connect(t, env.owner)
	ctx := context.Background()
	account, _, err := env.server.saveMailAccountFromInput(ctx, env.owner.ID, googleAccountInput(connection.ID))
	if err != nil {
		t.Fatal(err)
	}
	presented := env.server.apiAccountsWithGoogleIdentity(ctx, env.owner.ID, []store.MailAccount{account})
	if len(presented) != 1 {
		t.Fatalf("presented accounts = %d", len(presented))
	}
	if presented[0].GoogleEmail != connection.GoogleEmail {
		t.Fatalf("google email = %q, want %q", presented[0].GoogleEmail, connection.GoogleEmail)
	}
	if presented[0].AuthType != store.AuthTypeGoogleOAuth {
		t.Fatalf("auth type = %q", presented[0].AuthType)
	}
}
