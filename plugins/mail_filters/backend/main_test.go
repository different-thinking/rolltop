package main

import (
	"context"
	"database/sql"
	"strconv"
	"testing"
	"time"

	"rolltop/backend/plugins"
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

// fakeFilterHost stands in for the sync service so the scheduling decisions can
// be tested without a search index behind them. It records what the rule asked
// the mailbox to do and answers matches from a fixed set of queries.
type fakeFilterHost struct {
	store    *store.Store
	matches  map[string]bool
	searched []string
	moved    []int64
	starred  []int64
	forwards []string
}

func (h *fakeFilterHost) Store() any        { return h.store }
func (h *fakeFilterHost) MasterKey() []byte { return nil }

func (h *fakeFilterHost) PluginEnabled(context.Context, string) bool { return true }

func (h *fakeFilterHost) MatchMessageSearch(_ context.Context, _, _ int64, query string) (plugins.SearchMatchResult, error) {
	h.searched = append(h.searched, query)
	return plugins.SearchMatchResult{Matched: h.matches[query]}, nil
}

func (h *fakeFilterHost) StarMessage(_ context.Context, _, messageID int64, _ bool) error {
	h.starred = append(h.starred, messageID)
	return nil
}

func (h *fakeFilterHost) MoveMessage(_ context.Context, _, messageID, _ int64) error {
	h.moved = append(h.moved, messageID)
	return nil
}

func (h *fakeFilterHost) ForwardMessage(_ context.Context, _, _ int64, to string, _ []plugins.MailHeader) error {
	h.forwards = append(h.forwards, to)
	return nil
}

// A rule created today has to reach the mail already in the mailbox. Scheduling
// used to be reserved for newly arrived mail, so a "delete mail from this sender
// after seven days" rule silently never fired on anything that was too young at
// the moment the rule was saved.
func TestBackfillSchedulesMailThatIsNotOldEnoughYet(t *testing.T) {
	st, err := storetest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	db := st.DB()
	ctx := context.Background()
	user, account, mailbox := mailFilterFixture(t, st, "backfill-schedule@example.test")
	host := &fakeFilterHost{store: st, matches: map[string]bool{"from:studio@example.test": true}}
	rule := insertRule(t, db, user.ID, "from:studio@example.test older_than:7d", Actions{MoveRole: "trash"})
	msg := storedMessage(user.ID, account.ID, mailbox.ID, 100, time.Now().UTC().Add(-2*24*time.Hour))

	if _, err := evaluateRule(ctx, host, db, rule, msg, "backfill", 0); err != nil {
		t.Fatal(err)
	}

	if len(host.moved) != 0 {
		t.Fatalf("moved %v, want nothing moved before the rule comes due", host.moved)
	}
	status, dueAt := evaluationState(t, db, user.ID, rule.ID, msg.MessageID)
	if status != statusScheduled {
		t.Fatalf("status = %q, want %q", status, statusScheduled)
	}
	if want := msg.Date.Add(7 * 24 * time.Hour).Unix(); dueAt != want {
		t.Fatalf("due_at = %d, want %d", dueAt, want)
	}
}

// The age is the whole condition of a rule that carries nothing else, so the
// empty string left after it is taken out must not be handed to the search
// index, which answers no to it and would drop every message on the floor.
func TestAgeOnlyRuleSchedulesWithoutSearching(t *testing.T) {
	st, err := storetest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	db := st.DB()
	ctx := context.Background()
	user, account, mailbox := mailFilterFixture(t, st, "age-only@example.test")
	host := &fakeFilterHost{store: st, matches: map[string]bool{}}
	rule := insertRule(t, db, user.ID, "older_than:30d", Actions{MoveRole: "trash"})
	msg := storedMessage(user.ID, account.ID, mailbox.ID, 101, time.Now().UTC().Add(-24*time.Hour))

	if _, err := evaluateRule(ctx, host, db, rule, msg, "inbound", 0); err != nil {
		t.Fatal(err)
	}

	if len(host.searched) != 0 {
		t.Fatalf("searched %v, want no search for an age-only rule", host.searched)
	}
	if status, _ := evaluationState(t, db, user.ID, rule.ID, msg.MessageID); status != statusScheduled {
		t.Fatalf("status = %q, want %q", status, statusScheduled)
	}
}

// The same message reaches the scheduler from its arrival and from every
// backfill of the same rule. Only one row may wait for it, or a rule that moves
// mail to Trash runs its move again against a message already sitting there.
func TestRepeatedSchedulingKeepsOneWaitingRow(t *testing.T) {
	st, err := storetest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	db := st.DB()
	ctx := context.Background()
	user, account, mailbox := mailFilterFixture(t, st, "one-wait@example.test")
	host := &fakeFilterHost{store: st, matches: map[string]bool{"from:studio@example.test": true}}
	rule := insertRule(t, db, user.ID, "from:studio@example.test older_than:7d", Actions{MoveRole: "trash"})
	msg := storedMessage(user.ID, account.ID, mailbox.ID, 102, time.Now().UTC().Add(-24*time.Hour))

	for range 3 {
		if _, err := evaluateRule(ctx, host, db, rule, msg, "backfill", 0); err != nil {
			t.Fatal(err)
		}
	}

	var waiting int
	if err := db.QueryRow(`SELECT COUNT(*) FROM plugin_mail_filter_evaluations WHERE user_id = ? AND rule_id = ? AND message_id = ? AND status = ?`,
		user.ID, rule.ID, msg.MessageID, statusScheduled).Scan(&waiting); err != nil {
		t.Fatal(err)
	}
	if waiting != 1 {
		t.Fatalf("waiting rows = %d, want 1", waiting)
	}
}

// Once the message is old enough the age term is spent, so what is left of the
// query decides the match and the rule's actions run.
func TestDueMessageMatchesOnTheRemainingQueryAndActs(t *testing.T) {
	st, err := storetest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	db := st.DB()
	ctx := context.Background()
	user, account, mailbox := mailFilterFixture(t, st, "due@example.test")
	host := &fakeFilterHost{store: st, matches: map[string]bool{"from:studio@example.test": true}}
	rule := insertRule(t, db, user.ID, "from:studio@example.test older_than:7d", Actions{ForwardTo: "archive@example.test"})
	msg := storedMessage(user.ID, account.ID, mailbox.ID, 103, time.Now().UTC().Add(-30*24*time.Hour))

	if _, err := evaluateRule(ctx, host, db, rule, msg, "scheduled", 0); err != nil {
		t.Fatal(err)
	}

	if len(host.searched) != 1 || host.searched[0] != "from:studio@example.test" {
		t.Fatalf("searched %v, want the query without its age term", host.searched)
	}
	if len(host.forwards) != 1 || host.forwards[0] != "archive@example.test" {
		t.Fatalf("forwards = %v", host.forwards)
	}
	if status, _ := evaluationState(t, db, user.ID, rule.ID, msg.MessageID); status != statusMatched {
		t.Fatalf("status = %q, want %q", status, statusMatched)
	}
}

// A message the mirror stored without a date cannot be aged against its own
// date, so the age term stays in the query for the index to answer rather than
// being treated as satisfied.
func TestMessageWithoutADateLeavesTheAgeToTheIndex(t *testing.T) {
	st, err := storetest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	db := st.DB()
	ctx := context.Background()
	user, account, mailbox := mailFilterFixture(t, st, "no-date@example.test")
	host := &fakeFilterHost{store: st, matches: map[string]bool{}}
	rule := insertRule(t, db, user.ID, "from:studio@example.test older_than:7d", Actions{MoveRole: "trash"})
	msg := storedMessage(user.ID, account.ID, mailbox.ID, 104, time.Time{})

	if _, err := evaluateRule(ctx, host, db, rule, msg, "backfill", 0); err != nil {
		t.Fatal(err)
	}

	if len(host.searched) != 1 || host.searched[0] != "from:studio@example.test older_than:7d" {
		t.Fatalf("searched %v, want the whole query including its age term", host.searched)
	}
}

// The mail an age rule exists to clean up is the oldest in the mailbox, which
// is exactly what a newest-first page used to leave out.
func TestBackfillWalksOldestFirstAndReportsWhereItStopped(t *testing.T) {
	st, err := storetest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	db := st.DB()
	ctx := context.Background()
	user, account, mailbox := mailFilterFixture(t, st, "oldest-first@example.test")
	host := &fakeFilterHost{store: st, matches: map[string]bool{}}
	rule := insertRule(t, db, user.ID, "from:studio@example.test", Actions{})
	base := time.Now().UTC().Add(-400 * 24 * time.Hour)
	for i := range 3 {
		insertMessage(t, st, db, user.ID, account.ID, mailbox.ID, int64(200+i), base.Add(time.Duration(i)*24*time.Hour))
	}

	processed, cursor, done, err := backfillRule(ctx, host, db, rule, backfillCursor{})
	if err != nil {
		t.Fatal(err)
	}
	if processed != 3 || !done {
		t.Fatalf("processed = %d done = %t, want 3 and done", processed, done)
	}
	if cursor.ID != 202 {
		t.Fatalf("cursor = %+v, want the newest message of the page", cursor)
	}

	rows, err := db.Query(`SELECT message_id FROM plugin_mail_filter_evaluations WHERE user_id = ? ORDER BY id`, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var order []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		order = append(order, id)
	}
	if len(order) != 3 || order[0] != 200 || order[2] != 202 {
		t.Fatalf("evaluation order = %v, want oldest message first", order)
	}
}

func insertRule(t *testing.T, db *sql.DB, userID int64, query string, actions Actions) Rule {
	t.Helper()
	rule, err := saveRule(context.Background(), db, userID, Rule{Name: query, Query: query, Enabled: true, Actions: actions})
	if err != nil {
		t.Fatal(err)
	}
	return rule
}

func storedMessage(userID, accountID, mailboxID, messageID int64, date time.Time) plugins.StoredMessageContext {
	return plugins.StoredMessageContext{
		UserID: userID, AccountID: accountID, MailboxID: mailboxID, MessageID: messageID,
		Subject: "Reservation", From: "studio@example.test", Date: date,
	}
}

func insertMessage(t *testing.T, st *store.Store, db *sql.DB, userID, accountID, mailboxID, messageID int64, date time.Time) {
	t.Helper()
	ctx := context.Background()
	blob, err := st.CreateBlob(ctx, store.BlobRecord{
		UserID: userID, Kind: "message", Path: "users/x/message-" + strconv.FormatInt(messageID, 10) + ".eml", SHA256: "sha", Size: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Unix()
	if _, err := db.Exec(`INSERT INTO messages (id, user_id, account_id, mailbox_id, blob_id, blob_path,
		message_id_header, in_reply_to, references_header, thread_key, subject, from_addr, to_addr, cc_addr,
		body_text, body_html, date_unix, internal_date_unix, uid, size, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, '', '', '', '', '', 'Reservation', 'studio@example.test', '', '',
		'', '', ?, ?, ?, 10, ?, ?)`, messageID, userID, accountID, mailboxID, blob.ID, date.Unix(), date.Unix(), messageID, now, now); err != nil {
		t.Fatal(err)
	}
}

func evaluationState(t *testing.T, db *sql.DB, userID, ruleID, messageID int64) (string, int64) {
	t.Helper()
	var status string
	var dueAt int64
	if err := db.QueryRow(`SELECT status, due_at FROM plugin_mail_filter_evaluations
		WHERE user_id = ? AND rule_id = ? AND message_id = ? ORDER BY id DESC LIMIT 1`,
		userID, ruleID, messageID).Scan(&status, &dueAt); err != nil {
		t.Fatal(err)
	}
	return status, dueAt
}

// An empty query that no age term emptied is a rule with no condition at all.
// It has to match nothing: the shortcut that lets an age-only rule skip the
// index would otherwise let a malformed rule move a whole mailbox to Trash.
func TestQueryWithNoConditionMatchesNothing(t *testing.T) {
	st, err := storetest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	db := st.DB()
	ctx := context.Background()
	user, account, mailbox := mailFilterFixture(t, st, "no-condition@example.test")
	host := &fakeFilterHost{store: st, matches: map[string]bool{}}
	// saveRule refuses this, so the row is written the way a hand-edited
	// database or an older release could leave one behind.
	now := time.Now().UTC().Unix()
	if _, err := db.Exec(`INSERT INTO plugin_mail_filter_rules (id, user_id, name, query, enabled, scope_mode, actions_json, position, created_at, updated_at)
		VALUES (900, ?, 'Empty', '   ', 1, 'all_accounts', '{"move_role":"trash"}', 0, ?, ?)`, user.ID, now, now); err != nil {
		t.Fatal(err)
	}
	rule, err := getRule(ctx, db, user.ID, 900)
	if err != nil {
		t.Fatal(err)
	}
	msg := storedMessage(user.ID, account.ID, mailbox.ID, 105, time.Now().UTC().Add(-24*time.Hour))

	if _, err := evaluateRule(ctx, host, db, rule, msg, "backfill", 0); err != nil {
		t.Fatal(err)
	}

	if len(host.moved) != 0 {
		t.Fatalf("moved %v, want a rule with no condition to move nothing", host.moved)
	}
	if status, _ := evaluationState(t, db, user.ID, rule.ID, msg.MessageID); status != statusNotMatched {
		t.Fatalf("status = %q, want %q", status, statusNotMatched)
	}
}
