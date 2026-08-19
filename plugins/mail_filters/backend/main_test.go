package main

import (
	"context"
	"testing"
	"time"

	"rolltop/backend/store"
	"rolltop/backend/store/storetest"
)

func TestOlderThanClauseStripsAgeOperator(t *testing.T) {
	age, ok := olderThanClause("from:studio@example.test older_than:7d subject:Yoga")
	if !ok {
		t.Fatal("older_than clause not found")
	}
	if got := age.Duration.Hours() / 24; got != 7 {
		t.Fatalf("duration days = %v, want 7", got)
	}
	if age.QueryWithoutClause != "from:studio@example.test subject:Yoga" {
		t.Fatalf("query without clause = %q", age.QueryWithoutClause)
	}
}

func TestEvaluationListsSeparateManagementFromMessageAudit(t *testing.T) {
	st, err := storetest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	db := st.DB()
	// The tables are part of the baseline the test database carries; only the
	// rows this test reasons about are set up here.
	user, account, mailbox := mailFilterFixture(t, st, "filters@example.test")
	now := time.Now().UTC().Unix()
	blob, err := st.CreateBlob(context.Background(), store.BlobRecord{
		UserID: user.ID, Kind: "message", Path: "users/x/message.eml", SHA256: "sha", Size: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO plugin_mail_filter_rules (id, user_id, name, query, enabled, scope_mode, actions_json, position, created_at, updated_at) VALUES (10, ?, 'Yoga cleanup', 'older_than:7d yoga', 1, 'all_accounts', '{}', 0, ?, ?)`, user.ID, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO messages (id, user_id, account_id, mailbox_id, blob_id, blob_path,
		message_id_header, in_reply_to, references_header, thread_key, subject, from_addr, to_addr, cc_addr,
		body_text, body_html, date_unix, internal_date_unix, uid, size, created_at, updated_at)
		VALUES (100, ?, ?, ?, ?, '', '', '', '', '', 'Yoga booking', 'studio@example.test', '', '',
		'', '', ?, ?, 1, 10, ?, ?)`, user.ID, account.ID, mailbox.ID, blob.ID, now, now, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO plugin_mail_filter_evaluations
		(id, user_id, rule_id, message_id, account_id, mailbox_id, phase, status, matched, due_at, evaluated_at, terms_json, fields_json, actions_json, error, created_at)
		VALUES
		(1, ?, 10, 100, ?, ?, 'backfill', 'not_matched', 0, 0, ?, '[]', '[]', '{}', '', ?),
		(2, ?, 10, 100, ?, ?, 'backfill', 'matched', 1, 0, ?, '[]', '[]', '{"move":"ok"}', '', ?)`,
		user.ID, account.ID, mailbox.ID, now-1, now-1, user.ID, account.ID, mailbox.ID, now, now); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	recent, err := listRecentEvaluations(ctx, db, 1, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(recent) != 1 || !recent[0].Matched {
		t.Fatalf("recent evaluations = %+v, want only matched rows", recent)
	}
	messageRows, err := listMessageEvaluations(ctx, db, 1, 100, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(messageRows) != 2 {
		t.Fatalf("message evaluations len = %d, want 2", len(messageRows))
	}
}

func TestEnsureForwarderIDIsStableAndOpaque(t *testing.T) {
	st, err := storetest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	db := st.DB()
	defer db.Close()
	ctx := context.Background()
	user, account, _ := mailFilterFixture(t, st, "forwarder@example.test")
	first, err := ensureForwarderID(ctx, db, user.ID, account.ID)
	if err != nil {
		t.Fatal(err)
	}
	second, err := ensureForwarderID(ctx, db, user.ID, account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("forwarder id changed: %q then %q", first, second)
	}
	if len(first) != len("rtf-")+32 {
		t.Fatalf("forwarder id = %q", first)
	}
}

// mailFilterFixture creates the tenant rows the plugin's foreign keys require.
// The schema these tests run against is the production one now, so a rule or an
// evaluation cannot reference an account and a mailbox that do not exist.
func mailFilterFixture(t *testing.T, st *store.Store, email string) (store.User, store.MailAccount, store.Mailbox) {
	t.Helper()
	ctx := context.Background()
	user, err := st.CreateUser(ctx, email, "Filters", "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	account, err := st.CreateMailAccount(ctx, store.MailAccount{
		UserID: user.ID, Email: email, Host: "imap.example.test", Port: 993,
		Username: email, EncryptedPassword: "secret", UseTLS: true, Mailbox: "*",
	})
	if err != nil {
		t.Fatal(err)
	}
	mailbox, err := st.GetOrCreateMailbox(ctx, user.ID, account.ID, "INBOX")
	if err != nil {
		t.Fatal(err)
	}
	return user, account, mailbox
}
