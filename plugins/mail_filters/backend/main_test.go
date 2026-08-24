package main

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"rolltop/backend/plugins"
	"rolltop/backend/store"
	"rolltop/backend/store/storetest"
)

// openFilterStore opens a store carrying this plugin's own migrations, not only
// the baseline the shared test template was built from. The baseline predates
// them: it was translated once from a fully migrated schema and is frozen,
// because its checksum is what an in-service database is recognised by. So a
// migration added after that point reaches a test database only this way, and
// the partial unique index behind the one-wait invariant is one of them.
func openFilterStore(t *testing.T) *store.Store {
	t.Helper()
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate the mail filters test source")
	}
	manifests, err := plugins.LoadManifests(filepath.Clean(filepath.Join(filepath.Dir(currentFile), "..", "..")))
	if err != nil {
		t.Fatal(err)
	}
	var selected []plugins.Manifest
	for _, manifest := range manifests {
		if manifest.ID == pluginID {
			selected = append(selected, manifest)
		}
	}
	if len(selected) != 1 {
		t.Fatalf("mail filter manifests = %d, want 1", len(selected))
	}
	st, err := storetest.OpenWithManifests(t, selected)
	if err != nil {
		t.Fatalf("open store through the plugin's own migrations: %v", err)
	}
	return st
}

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
	st := openFilterStore(t)
	db := st.DB()
	ctx := context.Background()
	// The tables are part of the baseline the test database carries; only the
	// rows this test reasons about are set up here.
	user, account, mailbox := mailFilterFixture(t, st, "filters@example.test")
	now := time.Now().UTC().Unix()
	blob, err := st.CreateBlob(ctx, store.BlobRecord{
		UserID: user.ID, Kind: "message", Path: "users/x/message.eml", SHA256: "sha", Size: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO plugin_mail_filter_rules (id, user_id, name, query, enabled, scope_mode, actions_json, position, created_at, updated_at) VALUES (10, ?, 'Yoga cleanup', 'older_than:7d yoga', 1, 'all_accounts', '{}', 0, ?, ?)`, user.ID, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO messages (id, user_id, account_id, mailbox_id, blob_id, blob_path,
		message_id_header, in_reply_to, references_header, thread_key, subject, from_addr, to_addr, cc_addr,
		body_text, body_html, date_unix, internal_date_unix, uid, size, created_at, updated_at)
		VALUES (100, ?, ?, ?, ?, '', '', '', '', '', 'Yoga booking', 'studio@example.test', '', '',
		'', '', ?, ?, 1, 10, ?, ?)`, user.ID, account.ID, mailbox.ID, blob.ID, now, now, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO plugin_mail_filter_evaluations
		(id, user_id, rule_id, message_id, account_id, mailbox_id, phase, status, matched, due_at, evaluated_at, terms_json, fields_json, actions_json, error, created_at)
		VALUES
		(1, ?, 10, 100, ?, ?, 'backfill', 'not_matched', 0, 0, ?, '[]', '[]', '{}', '', ?),
		(2, ?, 10, 100, ?, ?, 'backfill', 'matched', 1, 0, ?, '[]', '[]', '{"move":"ok"}', '', ?)`,
		user.ID, account.ID, mailbox.ID, now-1, now-1, user.ID, account.ID, mailbox.ID, now, now); err != nil {
		t.Fatal(err)
	}
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
	st := openFilterStore(t)
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
	movedTo  []int64
	forwards []string
	// archiveMailboxID is the account's chosen Archive folder, and zero stands
	// for an account whose reader never named one.
	archiveMailboxID int64
	archiveErr       error
}

func (h *fakeFilterHost) Store() any        { return h.store }
func (h *fakeFilterHost) MasterKey() []byte { return nil }

func (h *fakeFilterHost) PluginEnabled(context.Context, string) bool { return true }

func (h *fakeFilterHost) MatchMessageSearch(_ context.Context, _, _ int64, query string) (plugins.SearchMatchResult, error) {
	h.searched = append(h.searched, query)
	return plugins.SearchMatchResult{Matched: h.matches[query]}, nil
}

func (h *fakeFilterHost) StarMessage(context.Context, int64, int64, bool) error { return nil }

func (h *fakeFilterHost) MoveMessage(_ context.Context, _, messageID, destID int64) error {
	h.moved = append(h.moved, messageID)
	h.movedTo = append(h.movedTo, destID)
	return nil
}

func (h *fakeFilterHost) ArchiveMailboxID(context.Context, int64, int64) (int64, error) {
	return h.archiveMailboxID, h.archiveErr
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
	st := openFilterStore(t)
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
	st := openFilterStore(t)
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
	st := openFilterStore(t)
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
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM plugin_mail_filter_evaluations WHERE user_id = ? AND rule_id = ? AND message_id = ? AND status = ?`,
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
	st := openFilterStore(t)
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
	st := openFilterStore(t)
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
	st := openFilterStore(t)
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

	rows, err := db.QueryContext(ctx, `SELECT message_id FROM plugin_mail_filter_evaluations WHERE user_id = ? ORDER BY id`, user.ID)
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
	if _, err := db.ExecContext(ctx, `INSERT INTO messages (id, user_id, account_id, mailbox_id, blob_id, blob_path,
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
	if err := db.QueryRowContext(context.Background(), `SELECT status, due_at FROM plugin_mail_filter_evaluations
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
	st := openFilterStore(t)
	db := st.DB()
	ctx := context.Background()
	user, account, mailbox := mailFilterFixture(t, st, "no-condition@example.test")
	host := &fakeFilterHost{store: st, matches: map[string]bool{}}
	// saveRule refuses this, so the row is written the way a hand-edited
	// database or an older release could leave one behind.
	now := time.Now().UTC().Unix()
	if _, err := db.ExecContext(ctx, `INSERT INTO plugin_mail_filter_rules (id, user_id, name, query, enabled, scope_mode, actions_json, position, created_at, updated_at)
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

// A rule's actions are carried out where it matches, so evaluating the same
// message twice forwards it twice. Backfill now walks the whole mailbox, which
// made a second Backfill click a second copy of every match.
func TestRepeatedBackfillDoesNotActTwice(t *testing.T) {
	st := openFilterStore(t)
	db := st.DB()
	ctx := context.Background()
	user, account, mailbox := mailFilterFixture(t, st, "repeat-backfill@example.test")
	host := &fakeFilterHost{store: st, matches: map[string]bool{"from:studio@example.test": true}}
	rule := insertRule(t, db, user.ID, "from:studio@example.test", Actions{ForwardTo: "archive@example.test"})
	insertMessage(t, st, db, user.ID, account.ID, mailbox.ID, 300, time.Now().UTC().Add(-24*time.Hour))

	total := 0
	for range 3 {
		n, _, _, err := backfillRule(ctx, host, db, rule, backfillCursor{})
		if err != nil {
			t.Fatal(err)
		}
		total += n
	}

	if total != 1 {
		t.Fatalf("processed %d messages over three runs, want 1", total)
	}
	if len(host.forwards) != 1 {
		t.Fatalf("forwards = %v, want the message forwarded once", host.forwards)
	}
	var rowCount int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM plugin_mail_filter_evaluations WHERE user_id = ? AND rule_id = ?`, user.ID, rule.ID).Scan(&rowCount); err != nil {
		t.Fatal(err)
	}
	if rowCount != 1 {
		t.Fatalf("evaluation rows = %d, want 1", rowCount)
	}
}

// Editing a rule is the reader saying "decide again", so the messages it
// already decided on go back in front of it.
func TestEditingARulePutsDecidedMailBackInFrontOfIt(t *testing.T) {
	st := openFilterStore(t)
	db := st.DB()
	ctx := context.Background()
	user, account, mailbox := mailFilterFixture(t, st, "reedit@example.test")
	host := &fakeFilterHost{store: st, matches: map[string]bool{}}
	rule := insertRule(t, db, user.ID, "from:studio@example.test", Actions{})
	insertMessage(t, st, db, user.ID, account.ID, mailbox.ID, 310, time.Now().UTC().Add(-24*time.Hour))
	if n, _, _, err := backfillRule(ctx, host, db, rule, backfillCursor{}); err != nil || n != 1 {
		t.Fatalf("first backfill processed %d (%v), want 1", n, err)
	}

	// saveRule stamps updated_at from the wall clock, which has the same second
	// resolution as evaluated_at; move it forward so the edit is observable.
	if _, err := db.ExecContext(ctx, `UPDATE plugin_mail_filter_rules SET query = ?, updated_at = ? WHERE user_id = ? AND id = ?`,
		"from:other@example.test", time.Now().UTC().Add(time.Minute).Unix(), user.ID, rule.ID); err != nil {
		t.Fatal(err)
	}
	edited, err := getRule(ctx, db, user.ID, rule.ID)
	if err != nil {
		t.Fatal(err)
	}

	n, _, _, err := backfillRule(ctx, host, db, edited, backfillCursor{})
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("backfill after the edit processed %d, want the message reconsidered", n)
	}
}

// A message stored without a parseable Date carries a large negative date_unix,
// not zero, so a walk that started at (0, 0) stepped over every one of them.
func TestBackfillReachesMailWithNoDate(t *testing.T) {
	st := openFilterStore(t)
	db := st.DB()
	ctx := context.Background()
	user, account, mailbox := mailFilterFixture(t, st, "dateless@example.test")
	host := &fakeFilterHost{store: st, matches: map[string]bool{}}
	rule := insertRule(t, db, user.ID, "from:studio@example.test", Actions{})
	insertMessage(t, st, db, user.ID, account.ID, mailbox.ID, 320, time.Time{})
	insertMessage(t, st, db, user.ID, account.ID, mailbox.ID, 321, time.Now().UTC().Add(-24*time.Hour))

	n, _, done, err := backfillRule(ctx, host, db, rule, backfillCursor{})
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 || !done {
		t.Fatalf("processed = %d done = %t, want both messages", n, done)
	}
	// The dateless one is first in the walk, and its age must be left to the
	// index rather than read as 1970.
	if len(host.searched) == 0 || host.searched[0] != "from:studio@example.test" {
		t.Fatalf("searched = %v", host.searched)
	}
}

// "Older than 30 days -> Trash" is a rule about incoming mail. Left unscoped it
// reads the reader's own Sent folder too, and Backfill empties it.
func TestFiltersDoNotReachMailTheAccountListsHide(t *testing.T) {
	st := openFilterStore(t)
	db := st.DB()
	ctx := context.Background()
	user, account, inbox := mailFilterFixture(t, st, "scope@example.test")
	sent, err := st.GetOrCreateMailboxWithRole(ctx, user.ID, account.ID, "Sent", "sent")
	if err != nil {
		t.Fatal(err)
	}
	host := &fakeFilterHost{store: st, matches: map[string]bool{}}
	rule := insertRule(t, db, user.ID, "from:studio@example.test", Actions{})
	insertMessage(t, st, db, user.ID, account.ID, inbox.ID, 330, time.Now().UTC().Add(-24*time.Hour))
	insertMessage(t, st, db, user.ID, account.ID, sent.ID, 331, time.Now().UTC().Add(-24*time.Hour))

	n, _, _, err := backfillRule(ctx, host, db, rule, backfillCursor{})
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("processed = %d, want only the inbox message", n)
	}
	var seen int64
	if err := db.QueryRowContext(ctx, `SELECT message_id FROM plugin_mail_filter_evaluations WHERE user_id = ?`, user.ID).Scan(&seen); err != nil {
		t.Fatal(err)
	}
	if seen != 330 {
		t.Fatalf("evaluated message %d, want the inbox one", seen)
	}
	inScope, err := messageInFilterScope(ctx, db, storedMessage(user.ID, account.ID, sent.ID, 331, time.Now().UTC()))
	if err != nil {
		t.Fatal(err)
	}
	if inScope {
		t.Fatal("a message in Sent is in filter scope, want it left alone on arrival too")
	}
}

// A wait is a promise to act on a message. Once the message is deleted the
// promise can never be kept, and the retention sweep skips waits by design, so
// the row sat in the pending queue for good with a due date long past.
func TestPurgeClearsWaitsWhoseMessageIsGone(t *testing.T) {
	st := openFilterStore(t)
	db := st.DB()
	ctx := context.Background()
	user, account, mailbox := mailFilterFixture(t, st, "orphan-wait@example.test")
	host := &fakeFilterHost{store: st, matches: map[string]bool{"from:studio@example.test": true}}
	rule := insertRule(t, db, user.ID, "from:studio@example.test older_than:7d", Actions{MoveRole: "trash"})
	insertMessage(t, st, db, user.ID, account.ID, mailbox.ID, 340, time.Now().UTC().Add(-24*time.Hour))
	msg := storedMessage(user.ID, account.ID, mailbox.ID, 340, time.Now().UTC().Add(-24*time.Hour))
	if _, err := evaluateRule(ctx, host, db, rule, msg, "backfill", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM messages WHERE user_id = ? AND id = ?`, user.ID, 340); err != nil {
		t.Fatal(err)
	}

	if err := purgeOldEvaluations(ctx, db, user.ID); err != nil {
		t.Fatal(err)
	}

	pending, err := listScheduledEvaluations(ctx, db, user.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("pending = %+v, want the orphaned wait cleared", pending)
	}
}

// A search that fails is recorded with matched = false, so a history filtered
// on matched alone never showed the reader the one row they need to see.
func TestRecentActionsShowFailures(t *testing.T) {
	st := openFilterStore(t)
	db := st.DB()
	ctx := context.Background()
	user, account, mailbox := mailFilterFixture(t, st, "failures@example.test")
	host := &failingSearchHost{fakeFilterHost{store: st, matches: map[string]bool{}}}
	rule := insertRule(t, db, user.ID, "from:studio@example.test", Actions{})
	insertMessage(t, st, db, user.ID, account.ID, mailbox.ID, 350, time.Now().UTC().Add(-24*time.Hour))
	msg := storedMessage(user.ID, account.ID, mailbox.ID, 350, time.Now().UTC().Add(-24*time.Hour))
	if _, err := evaluateRule(ctx, host, db, rule, msg, "backfill", 0); err != nil {
		t.Fatal(err)
	}

	recent, err := listRecentEvaluations(ctx, db, user.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(recent) != 1 || recent[0].Status != statusFailed || recent[0].Error == "" {
		t.Fatalf("recent = %+v, want the failed evaluation with its error", recent)
	}
}

type failingSearchHost struct{ fakeFilterHost }

func (h *failingSearchHost) MatchMessageSearch(context.Context, int64, int64, string) (plugins.SearchMatchResult, error) {
	return plugins.SearchMatchResult{}, errors.New("search is not configured")
}

// Retention now rides entirely on the scheduled pass: the import hook used to
// sweep as well, once per stored message, which is why it no longer does. This
// pins the pass that is left as the one that actually sweeps.
func TestScheduledPassSweepsRetention(t *testing.T) {
	st := openFilterStore(t)
	db := st.DB()
	ctx := context.Background()
	user, account, mailbox := mailFilterFixture(t, st, "sweep@example.test")
	host := &fakeFilterHost{store: st, matches: map[string]bool{}}
	rule := insertRule(t, db, user.ID, "from:studio@example.test", Actions{})
	insertMessage(t, st, db, user.ID, account.ID, mailbox.ID, 360, time.Now().UTC().Add(-24*time.Hour))
	stale := time.Now().UTC().Add(-retentionWindow - 24*time.Hour).Unix()
	if _, err := db.ExecContext(ctx, `INSERT INTO plugin_mail_filter_evaluations
		(user_id, rule_id, message_id, account_id, mailbox_id, phase, status, matched, due_at, evaluated_at, terms_json, fields_json, actions_json, error, created_at)
		VALUES (?, ?, 360, ?, ?, 'backfill', ?, 1, 0, ?, '[]', '[]', '{}', '', ?)`,
		user.ID, rule.ID, account.ID, mailbox.ID, statusMatched, stale, stale); err != nil {
		t.Fatal(err)
	}

	if _, err := runScheduled(ctx, host, db, user.ID, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}

	var remaining int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM plugin_mail_filter_evaluations WHERE user_id = ?`, user.ID).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 0 {
		t.Fatalf("evaluation rows = %d, want the stale row swept", remaining)
	}
}

// evaluationError reads what a rule recorded when an action did not happen. A
// destination that cannot be resolved has to leave a reason behind: the reader
// finds out from the audit or not at all.
func evaluationError(t *testing.T, db *sql.DB, userID, ruleID, messageID int64) string {
	t.Helper()
	var text string
	if err := db.QueryRowContext(context.Background(), `SELECT error FROM plugin_mail_filter_evaluations
		WHERE user_id = ? AND rule_id = ? AND message_id = ? ORDER BY id DESC LIMIT 1`,
		userID, ruleID, messageID).Scan(&text); err != nil {
		t.Fatal(err)
	}
	return text
}

// Deleting is a move into the account's own Trash, resolved per message, so one
// rule over several accounts files each account's mail in its own Trash rather
// than in whichever one the rule happened to be written against.
func TestDeleteMovesMailIntoTheAccountsOwnTrash(t *testing.T) {
	st := openFilterStore(t)
	db := st.DB()
	ctx := context.Background()
	user, account, mailbox := mailFilterFixture(t, st, "delete-dest@example.test")
	trash, err := st.GetOrCreateMailboxWithRole(ctx, user.ID, account.ID, "Trash", "trash")
	if err != nil {
		t.Fatal(err)
	}
	host := &fakeFilterHost{store: st, matches: map[string]bool{"from:studio@example.test": true}}
	rule := insertRule(t, db, user.ID, "from:studio@example.test", Actions{MoveRole: moveRoleTrash})
	msg := storedMessage(user.ID, account.ID, mailbox.ID, 300, time.Now().UTC().Add(-time.Hour))

	if _, err := evaluateRule(ctx, host, db, rule, msg, "inbound", 0); err != nil {
		t.Fatal(err)
	}

	if len(host.movedTo) != 1 || host.movedTo[0] != trash.ID {
		t.Fatalf("moved to %v, want the account's Trash %d", host.movedTo, trash.ID)
	}
}

// Archiving has no role behind it: the destination is the folder the reader
// named for that account, which only the host knows.
func TestArchiveMovesMailIntoTheFolderTheAccountChose(t *testing.T) {
	st := openFilterStore(t)
	db := st.DB()
	ctx := context.Background()
	user, account, mailbox := mailFilterFixture(t, st, "archive-dest@example.test")
	chosen, err := st.GetOrCreateMailbox(ctx, user.ID, account.ID, "Keep")
	if err != nil {
		t.Fatal(err)
	}
	host := &fakeFilterHost{store: st, matches: map[string]bool{"from:studio@example.test": true}, archiveMailboxID: chosen.ID}
	rule := insertRule(t, db, user.ID, "from:studio@example.test", Actions{MoveRole: moveRoleArchive})
	msg := storedMessage(user.ID, account.ID, mailbox.ID, 301, time.Now().UTC().Add(-time.Hour))

	if _, err := evaluateRule(ctx, host, db, rule, msg, "inbound", 0); err != nil {
		t.Fatal(err)
	}

	if len(host.movedTo) != 1 || host.movedTo[0] != chosen.ID {
		t.Fatalf("moved to %v, want the chosen Archive %d", host.movedTo, chosen.ID)
	}
}

// An account with no Archive folder chosen used to be indistinguishable from a
// rule that moved nothing on purpose: the destination resolved to zero, the
// move was skipped and the row still said "matched". The reader would have had
// no way to tell that the rule had never once archived anything.
func TestArchiveWithoutAChosenFolderIsRecordedAsAFailure(t *testing.T) {
	st := openFilterStore(t)
	db := st.DB()
	ctx := context.Background()
	user, account, mailbox := mailFilterFixture(t, st, "archive-missing@example.test")
	host := &fakeFilterHost{store: st, matches: map[string]bool{"from:studio@example.test": true}}
	rule := insertRule(t, db, user.ID, "from:studio@example.test", Actions{MoveRole: moveRoleArchive})
	msg := storedMessage(user.ID, account.ID, mailbox.ID, 302, time.Now().UTC().Add(-time.Hour))

	if _, err := evaluateRule(ctx, host, db, rule, msg, "inbound", 0); err != nil {
		t.Fatal(err)
	}

	if len(host.moved) != 0 {
		t.Fatalf("moved %v, want nothing moved without a destination", host.moved)
	}
	if status, _ := evaluationState(t, db, user.ID, rule.ID, msg.MessageID); status != statusFailed {
		t.Fatalf("status = %q, want %q", status, statusFailed)
	}
	if text := evaluationError(t, db, user.ID, rule.ID, msg.MessageID); !strings.Contains(text, "Archive") {
		t.Fatalf("error = %q, want it to name the missing Archive folder", text)
	}
}

// The same holds for an account with no Trash folder: a rule that says "delete"
// and quietly does nothing is worse than one that says it failed.
func TestDeleteWithoutATrashFolderIsRecordedAsAFailure(t *testing.T) {
	st := openFilterStore(t)
	db := st.DB()
	ctx := context.Background()
	user, account, mailbox := mailFilterFixture(t, st, "trash-missing@example.test")
	host := &fakeFilterHost{store: st, matches: map[string]bool{"from:studio@example.test": true}}
	rule := insertRule(t, db, user.ID, "from:studio@example.test", Actions{MoveRole: moveRoleTrash})
	msg := storedMessage(user.ID, account.ID, mailbox.ID, 303, time.Now().UTC().Add(-time.Hour))

	if _, err := evaluateRule(ctx, host, db, rule, msg, "inbound", 0); err != nil {
		t.Fatal(err)
	}

	if len(host.moved) != 0 {
		t.Fatalf("moved %v, want nothing moved without a destination", host.moved)
	}
	if status, _ := evaluationState(t, db, user.ID, rule.ID, msg.MessageID); status != statusFailed {
		t.Fatalf("status = %q, want %q", status, statusFailed)
	}
}

// A destination and a folder are two answers to one question. Storing both and
// letting the resolver pick would make a saved rule mean something the editor
// never showed, so the save is refused instead.
func TestARuleCannotNameBothADestinationAndAFolder(t *testing.T) {
	st := openFilterStore(t)
	db := st.DB()
	user, _, mailbox := mailFilterFixture(t, st, "two-destinations@example.test")
	_, err := saveRule(context.Background(), db, user.ID, Rule{
		Name: "both", Query: "from:studio@example.test", Enabled: true,
		Actions: Actions{MoveRole: moveRoleTrash, MoveMailboxID: mailbox.ID},
	})
	if err == nil {
		t.Fatal("saved a rule naming both a destination and a folder")
	}
}

func TestARuleRefusesADestinationTheEngineCannotResolve(t *testing.T) {
	st := openFilterStore(t)
	db := st.DB()
	user, _, _ := mailFilterFixture(t, st, "unknown-destination@example.test")
	_, err := saveRule(context.Background(), db, user.ID, Rule{
		Name: "spam", Query: "from:studio@example.test", Enabled: true,
		Actions: Actions{MoveRole: "junk"},
	})
	if err == nil {
		t.Fatal("saved a rule naming a destination the engine cannot resolve")
	}
}
