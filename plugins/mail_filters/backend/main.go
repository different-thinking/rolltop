// File overview: Runtime backend plugin for search-driven mail filters.

package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"rolltop/backend/plugins"
	"rolltop/backend/store"
)

const (
	apiPath          = "plugins/mail_filters"
	retentionWindow  = 30 * 24 * time.Hour
	scheduledBatch   = 100
	backfillBatch    = 500
	historyLimit     = 100
	forwarderHeader  = "X-Rolltop-Forwarded-By"
	statusScheduled  = "scheduled"
	statusMatched    = "matched"
	statusNotMatched = "not_matched"
	statusSkipped    = "skipped_scope"
	statusFailed     = "action_failed"
	statusLoop       = "loop_prevented"
	pluginID         = "mail_filters"
	// The three passes a rule runs in. A pass is recorded on every evaluation
	// row, and for a rule that only forwards newly arrived mail it is also what
	// decides whether the forward happens at all.
	phaseInbound   = "inbound"
	phaseBackfill  = "backfill"
	phaseScheduled = "scheduled"
	// forwardSkippedNew is what the audit records instead of "ok" when a rule
	// forwards new mail only and the message was already in the mailbox. It is
	// not a failure: the rule matched, and its move -- if it has one -- ran.
	forwardSkippedNew = "skipped_existing_mail"
	// moveRoleTrash and moveRoleArchive are destinations named relative to the
	// message's own account, so one rule can say "Trash" and mean each
	// account's own Trash. Deleting mail is exactly this move: Rolltop never
	// flags \Deleted or expunges outside of emptying a Trash folder, so the
	// most a filter can do to mail is put it where a manual delete puts it.
	moveRoleTrash   = "trash"
	moveRoleArchive = "archive"
	// zeroDateUnix is what a message with no parseable Date is stored as:
	// store.CreateMessage writes m.Date.UTC().Unix(), and Go's zero time is
	// this. It is not 0, so a cursor or a guard that tests for 0 misses it.
	zeroDateUnix = -62135596800
)

// filterScope is the mail a filter may act on, and it is the same mail the
// whole-account lists show (store.inPlayMailScope): folders that opt into All
// Mail, never Junk, never a hidden cross-account duplicate. Sent, Drafts and
// Trash default out of All Mail, which is what keeps "older than 30 days ->
// Trash" from emptying the reader's own Sent folder the first time they press
// Backfill. A rule that names a mailbox to move mail into is unaffected; this
// decides only what a rule reads.
// The copies of mail this Rolltop sent are out too, and for a forwarding rule
// they are the mail it would reach first: the provider files a copy of every
// forward, and a rule matching on the original's sender or subject matches that
// copy as readily as the original. The Sent and Inbox exemptions are the lists'
// too: a filter reads the mail the lists show, and mail delivered into the
// Inbox is mail that arrived whoever sent it.
//
// This is the store's own scope predicate, shared rather than restated so the
// two cannot drift; it assumes the messages table aliased m with its mailbox
// joined as mb, which every query below provides. It omits the archived-folder
// exclusion inPlayMailScope adds on purpose: filters reach archived mail.
const filterScope = store.InPlayMailScopeSQL

type mailFiltersBackend struct {
	mu     sync.Mutex
	routes []plugins.ProtectedAPIRouteHandle
	cancel context.CancelFunc
}

// RolltopPlugin is the symbol loaded by plugin.Open.
func RolltopPlugin() plugins.BackendPlugin {
	return &mailFiltersBackend{}
}

func (*mailFiltersBackend) ID() string { return pluginID }

func (p *mailFiltersBackend) Start(host plugins.BackendStartHost) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.unregisterRoutesLocked()
	if p.cancel != nil {
		p.cancel()
		p.cancel = nil
	}
	route := plugins.ProtectedAPIRoute{Path: apiPath, Prefix: true, Handle: p.handleAPI}
	handle, err := host.RegisterProtectedAPI(p.ID(), route)
	if err != nil {
		return err
	}
	p.routes = append(p.routes, handle)
	if filterHost, ok := host.(plugins.StoredMessageHost); ok {
		ctx, cancel := context.WithCancel(context.Background())
		p.cancel = cancel
		go scheduledLoop(ctx, filterHost)
	}
	return nil
}

func (p *mailFiltersBackend) Stop(plugins.BackendStartHost) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cancel != nil {
		p.cancel()
		p.cancel = nil
	}
	p.unregisterRoutesLocked()
	return nil
}

func (p *mailFiltersBackend) unregisterRoutesLocked() {
	for _, handle := range p.routes {
		handle.Unregister()
	}
	p.routes = nil
}

func scheduledLoop(ctx context.Context, host plugins.StoredMessageHost) {
	runAll := func() {
		st, ok := host.Store().(*store.Store)
		if !ok || st == nil {
			return
		}
		userIDs, err := st.ListUserIDsWithAccounts(ctx)
		if err != nil {
			return
		}
		for _, userID := range userIDs {
			db, err := st.UserDB(ctx, userID)
			if err != nil {
				continue
			}
			_, _ = runScheduled(ctx, host, db, userID, time.Now().UTC())
		}
	}
	runAll()
	ticker := time.NewTicker(15 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			runAll()
		}
	}
}

func (p *mailFiltersBackend) ImportStoredMessage(ctx context.Context, host plugins.StoredMessageHost, msg plugins.StoredMessageContext) error {
	db, err := userDB(ctx, host, msg.UserID)
	if err != nil {
		return err
	}
	// The retention sweep does not belong on this path. It runs once per stored
	// message, so mirroring a mailbox for the first time paid two DELETEs and an
	// anti-join against `messages` per message -- for a window that moves by one
	// day per day. The scheduled worker already sweeps every user every fifteen
	// minutes, which is often enough for a thirty-day window.
	inScope, err := messageInFilterScope(ctx, db, msg)
	if err != nil {
		return err
	}
	if !inScope {
		return nil
	}
	rules, err := listRules(ctx, db, msg.UserID, true)
	if err != nil {
		return err
	}
	for _, rule := range rules {
		moved, err := evaluateRule(ctx, host, db, rule, msg, inboundPass(), 0)
		if err != nil {
			return err
		}
		if moved {
			break
		}
	}
	return nil
}

func (p *mailFiltersBackend) handleAPI(host plugins.APIHost, path string, w http.ResponseWriter, r *http.Request) {
	cu, ok := host.RequireAPIAuth(w, r)
	if !ok {
		return
	}
	db, err := userDB(r.Context(), host, cu.UserID)
	if err != nil {
		host.ServerError(w, err)
		return
	}
	rest := strings.Trim(strings.TrimPrefix(path, apiPath), "/")
	switch {
	case rest == "rules" && r.Method == http.MethodGet:
		p.apiListRules(host, db, cu.UserID, w, r)
	case rest == "rules" && r.Method == http.MethodPost:
		p.apiSaveRule(host, db, cu.UserID, w, r)
	case strings.HasPrefix(rest, "rules/"):
		p.apiRuleAction(host, db, cu.UserID, rest, w, r)
	case strings.HasPrefix(rest, "messages/"):
		p.apiMessageAction(host, db, cu.UserID, rest, w, r)
	case rest == "history" && r.Method == http.MethodGet:
		p.apiHistory(host, db, cu.UserID, w, r)
	case rest == "scheduled/run" && r.Method == http.MethodPost:
		if !host.VerifyCSRF(w, r) {
			return
		}
		filterHost, ok := host.(plugins.StoredMessageHost)
		if !ok {
			host.WriteAPIError(w, http.StatusServiceUnavailable, "mail filter actions are not available")
			return
		}
		n, err := runScheduled(r.Context(), filterHost, db, cu.UserID, time.Now().UTC())
		if err != nil {
			host.ServerError(w, err)
			return
		}
		host.WriteJSON(w, map[string]any{"ok": true, "processed": n})
	default:
		host.WriteAPIError(w, http.StatusNotFound, "mail filter route not found")
	}
}

func (p *mailFiltersBackend) apiListRules(host plugins.APIHost, db *sql.DB, userID int64, w http.ResponseWriter, r *http.Request) {
	rules, err := listRules(r.Context(), db, userID, false)
	if err != nil {
		host.ServerError(w, err)
		return
	}
	host.WriteJSON(w, map[string]any{"rules": rules})
}

func (p *mailFiltersBackend) apiSaveRule(host plugins.APIHost, db *sql.DB, userID int64, w http.ResponseWriter, r *http.Request) {
	if !host.VerifyCSRF(w, r) {
		return
	}
	var in Rule
	if !host.DecodeJSON(w, r, &in) {
		return
	}
	rule, err := saveRule(r.Context(), db, userID, in)
	if err != nil {
		host.WriteAPIError(w, http.StatusBadRequest, err.Error())
		return
	}
	host.WriteJSON(w, map[string]any{"ok": true, "rule": rule})
}

// apiHistory answers with both halves of what a rule did: the actions it has
// already taken, and the ones it is still waiting to take. The waiting half is
// the one a reader needs before a rule that moves mail to Trash comes due,
// which is why it is not left to the per-message audit panel to discover.
func (p *mailFiltersBackend) apiHistory(host plugins.APIHost, db *sql.DB, userID int64, w http.ResponseWriter, r *http.Request) {
	recent, err := listRecentEvaluations(r.Context(), db, userID, historyLimit)
	if err != nil {
		host.ServerError(w, err)
		return
	}
	pending, err := listScheduledEvaluations(r.Context(), db, userID, historyLimit)
	if err != nil {
		host.ServerError(w, err)
		return
	}
	host.WriteJSON(w, map[string]any{"recent": recent, "pending": pending})
}

func (p *mailFiltersBackend) apiRuleAction(host plugins.APIHost, db *sql.DB, userID int64, rest string, w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.Trim(rest, "/"), "/")
	if len(parts) < 2 {
		host.WriteAPIError(w, http.StatusNotFound, "mail filter rule route not found")
		return
	}
	id, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || id <= 0 {
		host.WriteAPIError(w, http.StatusBadRequest, "invalid rule id")
		return
	}
	if len(parts) == 2 && r.Method == http.MethodDelete {
		if !host.VerifyCSRF(w, r) {
			return
		}
		if err := deleteRule(r.Context(), db, userID, id); err != nil {
			host.ServerError(w, err)
			return
		}
		host.WriteJSON(w, map[string]any{"ok": true})
		return
	}
	if len(parts) == 3 && parts[2] == "backfill" && r.Method == http.MethodPost {
		if !host.VerifyCSRF(w, r) {
			return
		}
		rule, err := getRule(r.Context(), db, userID, id)
		if err != nil {
			host.WriteAPIError(w, http.StatusNotFound, "filter rule not found")
			return
		}
		filterHost, ok := host.(plugins.StoredMessageHost)
		if !ok {
			host.WriteAPIError(w, http.StatusServiceUnavailable, "mail filter actions are not available")
			return
		}
		var from backfillCursor
		if !host.DecodeJSON(w, r, &from) {
			return
		}
		n, next, done, err := backfillRule(r.Context(), filterHost, db, rule, from)
		if err != nil && n == 0 {
			host.ServerError(w, err)
			return
		}
		if err != nil {
			// The messages before the failure were evaluated and their rows
			// committed, so this is a partial result rather than a failed
			// request. Reporting the cursor with it lets the walk resume where
			// it stopped instead of starting the mailbox again.
			host.WriteJSON(w, map[string]any{"ok": false, "processed": n, "done": false, "cursor": next, "error": err.Error()})
			return
		}
		host.WriteJSON(w, map[string]any{"ok": true, "processed": n, "done": done, "cursor": next})
		return
	}
	host.WriteAPIError(w, http.StatusNotFound, "mail filter rule route not found")
}

func (p *mailFiltersBackend) apiMessageAction(host plugins.APIHost, db *sql.DB, userID int64, rest string, w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.Trim(rest, "/"), "/")
	if len(parts) != 3 || parts[0] != "messages" || parts[2] != "evaluations" || r.Method != http.MethodGet {
		host.WriteAPIError(w, http.StatusNotFound, "mail filter message route not found")
		return
	}
	messageID, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || messageID <= 0 {
		host.WriteAPIError(w, http.StatusBadRequest, "invalid message id")
		return
	}
	evals, err := listMessageEvaluations(r.Context(), db, userID, messageID, 200)
	if err != nil {
		host.ServerError(w, err)
		return
	}
	host.WriteJSON(w, map[string]any{"evaluations": evals})
}

// Actions is what a rule does to the mail it matches. MoveRole names a
// destination relative to the message's own account -- Trash or Archive -- and
// MoveMailboxID names one exact folder instead, which only fits a rule that
// stays inside that folder's account, because a move cannot cross accounts.
type Actions struct {
	MoveMailboxID int64  `json:"move_mailbox_id"`
	MoveRole      string `json:"move_role"`
	ForwardTo     string `json:"forward_to"`
	// ForwardNewOnly limits forwarding to mail that reached the rule as it
	// arrived. The rest of the rule is unaffected: Backfill still walks the mail
	// already in the mailbox, still matches it, still moves it and still records
	// what it decided -- it just does not forward it. A mailbox holding years of
	// mail is the normal case, and a forward is the one action a rule takes that
	// leaves the account: a first Backfill without this sent hundreds of copies
	// of mail the reader had long since dealt with, and every copy came back
	// through the sending account's own Sent copy.
	ForwardNewOnly bool `json:"forward_new_only"`
}

// pass is which walk brought a message in front of a rule, and -- for a message
// released from the age queue -- which walk first did. The two differ exactly
// once: a message that arrived while the rule was running and then waited for
// an age condition is released by the scheduled pass, and it is still mail this
// rule saw arrive. Forwarding asks Origin for that reason; everything recorded
// on the evaluation row uses Phase, so the audit still says which pass acted.
type pass struct {
	Phase  string
	Origin string
}

func inboundPass() pass  { return pass{Phase: phaseInbound, Origin: phaseInbound} }
func backfillPass() pass { return pass{Phase: phaseBackfill, Origin: phaseBackfill} }

// scheduledPass carries the phase the waiting row was written in. A row from
// before this field existed, or one whose phase is unreadable, is treated as a
// backfill: the conservative answer is the one that does not forward mail the
// reader may never have wanted forwarded.
func scheduledPass(origin string) pass {
	if strings.TrimSpace(origin) != phaseInbound {
		origin = phaseBackfill
	}
	return pass{Phase: phaseScheduled, Origin: origin}
}

// forwards reports whether these actions may forward in this pass.
func (a Actions) forwards(p pass) bool {
	if strings.TrimSpace(a.ForwardTo) == "" {
		return false
	}
	if !a.ForwardNewOnly {
		return true
	}
	return p.Origin == phaseInbound
}

type Rule struct {
	ID         int64   `json:"id"`
	UserID     int64   `json:"user_id"`
	Name       string  `json:"name"`
	Query      string  `json:"query"`
	Enabled    bool    `json:"enabled"`
	ScopeMode  string  `json:"scope_mode"`
	AccountIDs []int64 `json:"account_ids"`
	Actions    Actions `json:"actions"`
	Position   int64   `json:"position"`
	CreatedAt  int64   `json:"created_at"`
	UpdatedAt  int64   `json:"updated_at"`
}

type Evaluation struct {
	ID        int64    `json:"id"`
	UserID    int64    `json:"user_id"`
	RuleID    int64    `json:"rule_id"`
	MessageID int64    `json:"message_id"`
	AccountID int64    `json:"account_id"`
	MailboxID int64    `json:"mailbox_id"`
	Phase     string   `json:"phase"`
	Status    string   `json:"status"`
	Matched   bool     `json:"matched"`
	DueAt     int64    `json:"due_at"`
	Evaluated int64    `json:"evaluated_at"`
	Terms     []string `json:"terms"`
	Fields    []string `json:"fields"`
	Actions   string   `json:"actions_json"`
	Error     string   `json:"error"`
	CreatedAt int64    `json:"created_at"`
	RuleName  string   `json:"rule_name"`
	Subject   string   `json:"subject"`
	From      string   `json:"from_addr"`
}

func userDB(ctx context.Context, host plugins.BackendHost, userID int64) (*sql.DB, error) {
	st, ok := host.Store().(*store.Store)
	if !ok || st == nil {
		return nil, errors.New("plugin host store is unavailable")
	}
	return st.UserDB(ctx, userID)
}

func listRules(ctx context.Context, db *sql.DB, userID int64, enabledOnly bool) ([]Rule, error) {
	query := `SELECT id, user_id, name, query, enabled, scope_mode, actions_json, position, created_at, updated_at
		FROM plugin_mail_filter_rules WHERE user_id = ?`
	if enabledOnly {
		query += ` AND enabled = 1`
	}
	query += ` ORDER BY position, id`
	rows, err := db.QueryContext(ctx, query, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var rules []Rule
	for rows.Next() {
		rule, err := scanRule(rows)
		if err != nil {
			return nil, err
		}
		rules = append(rules, rule)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range rules {
		accounts, err := ruleAccounts(ctx, db, userID, rules[i].ID)
		if err != nil {
			return nil, err
		}
		rules[i].AccountIDs = accounts
	}
	return rules, nil
}

func getRule(ctx context.Context, db *sql.DB, userID, id int64) (Rule, error) {
	row := db.QueryRowContext(ctx, `SELECT id, user_id, name, query, enabled, scope_mode, actions_json, position, created_at, updated_at
		FROM plugin_mail_filter_rules WHERE user_id = ? AND id = ?`, userID, id)
	rule, err := scanRule(row)
	if err != nil {
		return Rule{}, err
	}
	rule.AccountIDs, err = ruleAccounts(ctx, db, userID, rule.ID)
	return rule, err
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanRule(row rowScanner) (Rule, error) {
	var rule Rule
	var enabled int
	var actionsJSON string
	if err := row.Scan(&rule.ID, &rule.UserID, &rule.Name, &rule.Query, &enabled, &rule.ScopeMode, &actionsJSON, &rule.Position, &rule.CreatedAt, &rule.UpdatedAt); err != nil {
		return Rule{}, err
	}
	rule.Enabled = enabled != 0
	_ = json.Unmarshal([]byte(actionsJSON), &rule.Actions)
	return rule, nil
}

func ruleAccounts(ctx context.Context, db *sql.DB, userID, ruleID int64) ([]int64, error) {
	rows, err := db.QueryContext(ctx, `SELECT account_id FROM plugin_mail_filter_rule_accounts WHERE user_id = ? AND rule_id = ? ORDER BY account_id`, userID, ruleID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func saveRule(ctx context.Context, db *sql.DB, userID int64, in Rule) (Rule, error) {
	in.Name = strings.TrimSpace(in.Name)
	in.Query = strings.TrimSpace(in.Query)
	in.ScopeMode = strings.TrimSpace(in.ScopeMode)
	if in.ScopeMode == "" {
		in.ScopeMode = "all_accounts"
	}
	if in.ScopeMode != "all_accounts" && in.ScopeMode != "selected_accounts" {
		return Rule{}, errors.New("invalid account scope")
	}
	if in.Name == "" {
		in.Name = in.Query
	}
	if in.Query == "" {
		return Rule{}, errors.New("search query is required")
	}
	in.Actions.MoveRole = strings.ToLower(strings.TrimSpace(in.Actions.MoveRole))
	switch in.Actions.MoveRole {
	case "", moveRoleTrash, moveRoleArchive:
	default:
		return Rule{}, errors.New("a filter moves mail to Trash, to Archive, or to a folder you name")
	}
	in.Actions.ForwardTo = strings.TrimSpace(in.Actions.ForwardTo)
	if in.Actions.ForwardTo == "" {
		in.Actions.ForwardNewOnly = false
	}
	if in.Actions.MoveMailboxID < 0 {
		return Rule{}, errors.New("invalid destination folder")
	}
	// A named destination and a named folder are two answers to one question,
	// and guessing which one the reader meant is how a rule ends up deleting
	// mail it was supposed to file. Say so instead.
	if in.Actions.MoveRole != "" && in.Actions.MoveMailboxID > 0 {
		return Rule{}, errors.New("choose either Trash or Archive or one folder, not both")
	}
	actionsJSON, err := json.Marshal(in.Actions)
	if err != nil {
		return Rule{}, err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return Rule{}, err
	}
	defer tx.Rollback()
	now := time.Now().UTC().Unix()
	if in.ID > 0 {
		res, err := tx.ExecContext(ctx, `UPDATE plugin_mail_filter_rules SET name = ?, query = ?, enabled = ?, scope_mode = ?, actions_json = ?, position = ?, updated_at = ? WHERE user_id = ? AND id = ?`,
			in.Name, in.Query, boolInt(in.Enabled), in.ScopeMode, string(actionsJSON), in.Position, now, userID, in.ID)
		if err != nil {
			return Rule{}, err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return Rule{}, err
		}
		if n == 0 {
			return Rule{}, store.ErrNotFound
		}
	} else {
		if err := tx.QueryRowContext(ctx, `INSERT INTO plugin_mail_filter_rules (user_id, name, query, enabled, scope_mode, actions_json, position, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
			RETURNING id`, userID, in.Name, in.Query, boolInt(in.Enabled), in.ScopeMode, string(actionsJSON), in.Position, now, now).Scan(&in.ID); err != nil {
			return Rule{}, err
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM plugin_mail_filter_rule_accounts WHERE user_id = ? AND rule_id = ?`, userID, in.ID); err != nil {
		return Rule{}, err
	}
	if in.ScopeMode == "selected_accounts" {
		for _, accountID := range uniqueIDs(in.AccountIDs) {
			if _, err := tx.ExecContext(ctx, `INSERT INTO plugin_mail_filter_rule_accounts (rule_id, user_id, account_id) VALUES (?, ?, ?)`, in.ID, userID, accountID); err != nil {
				return Rule{}, err
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return Rule{}, err
	}
	return getRule(ctx, db, userID, in.ID)
}

func deleteRule(ctx context.Context, db *sql.DB, userID, id int64) error {
	_, err := db.ExecContext(ctx, `DELETE FROM plugin_mail_filter_rules WHERE user_id = ? AND id = ?`, userID, id)
	return err
}

// evaluateRule decides one rule against one message and, when it matches,
// carries out the rule's actions.
//
// An `older_than:` term is the one term that is not a search: it names the
// moment the rule may act rather than something to look for in the message, and
// the message's own date already answers it. So the term is taken out of the
// query and compared against that date here, and what is left decides whether
// the rule matches at all. Two things follow. A message that matches the rest
// of the query but is still too young is recorded as scheduled and re-evaluated
// once it is old enough, in whichever phase it was first seen -- a rule created
// today has to reach the mail already in the mailbox, which is the whole point
// of an age rule and what restricting this to newly arrived mail used to
// prevent. And a rule whose only term is the age matches every message its
// scope reaches, because the age was the entire condition; asking the search
// index for the empty string that remains would answer no to all of them.
func evaluateRule(ctx context.Context, host plugins.StoredMessageHost, db *sql.DB, rule Rule, msg plugins.StoredMessageContext, p pass, evalID int64) (bool, error) {
	phase := p.Phase
	if rule.ScopeMode == "selected_accounts" && !containsID(rule.AccountIDs, msg.AccountID) {
		return false, recordEvaluation(ctx, db, evalID, rule, msg, phase, statusSkipped, false, time.Time{}, nil, nil, "{}", "")
	}
	query := rule.Query
	aged := false
	// A message with no date of its own cannot be aged here, so the age term
	// stays in the query and the search index answers it from the stored date.
	if age, ok := olderThanClause(query); ok && !msg.Date.IsZero() {
		query = strings.TrimSpace(age.QueryWithoutClause)
		aged = true
		if dueAt := msg.Date.Add(age.Duration); time.Now().UTC().Before(dueAt) {
			result, err := matchMessage(ctx, host, msg, query, aged)
			if err != nil {
				return false, recordEvaluation(ctx, db, evalID, rule, msg, phase, statusFailed, false, time.Time{}, nil, nil, "{}", err.Error())
			}
			if !result.Matched {
				return false, recordEvaluation(ctx, db, evalID, rule, msg, phase, statusNotMatched, false, time.Time{}, result.Terms, result.Fields, "{}", "")
			}
			return false, scheduleEvaluation(ctx, db, evalID, rule, msg, phase, dueAt, result)
		}
	}
	result, err := matchMessage(ctx, host, msg, query, aged)
	if err != nil {
		return false, recordEvaluation(ctx, db, evalID, rule, msg, phase, statusFailed, false, time.Time{}, nil, nil, "{}", err.Error())
	}
	if !result.Matched {
		return false, recordEvaluation(ctx, db, evalID, rule, msg, phase, statusNotMatched, false, time.Time{}, result.Terms, result.Fields, "{}", "")
	}
	actionJSON, moved, status, errText := applyActions(ctx, host, db, rule, msg, p)
	if errText != "" {
		status = statusFailed
	}
	if status == "" {
		status = statusMatched
	}
	if err := recordEvaluation(ctx, db, evalID, rule, msg, phase, status, true, time.Time{}, result.Terms, result.Fields, actionJSON, errText); err != nil {
		return moved, err
	}
	return moved, nil
}

// messageInFilterScope reports whether a newly stored message is mail a filter
// may act on at all. It asks the same question filterScope asks of the backfill
// walk, one message at a time, so an arrival in Sent or Junk is left alone by
// both paths rather than only by one of them.
func messageInFilterScope(ctx context.Context, db *sql.DB, msg plugins.StoredMessageContext) (bool, error) {
	var found int64
	err := db.QueryRowContext(ctx, `SELECT 1 FROM messages m
		JOIN mailboxes mb ON mb.id = m.mailbox_id AND mb.user_id = m.user_id
		WHERE m.user_id = ? AND m.id = ?`+filterScope, msg.UserID, msg.MessageID).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// matchMessage answers what is left of a rule's query. When an age term was
// taken out of it, an empty remainder is not a search: the age was the whole
// condition, so every message the rule's scope reaches satisfies it. An empty
// query that no age term emptied is a rule with no condition at all -- the
// editor refuses to save one -- and it matches nothing rather than everything,
// because the alternative is a malformed rule moving a whole mailbox to Trash.
func matchMessage(ctx context.Context, host plugins.StoredMessageHost, msg plugins.StoredMessageContext, query string, aged bool) (plugins.SearchMatchResult, error) {
	if aged && strings.TrimSpace(query) == "" {
		return plugins.SearchMatchResult{Matched: true}, nil
	}
	return host.MatchMessageSearch(ctx, msg.UserID, msg.MessageID, query)
}

// scheduleEvaluation records that a rule is waiting for a message to grow old
// enough. Exactly one row may wait per rule and message: the same message
// reaches this from an arrival and from a backfill, and two rows would run the
// rule's move twice, the second time against a message already in Trash. The
// insert says so to the database rather than only to itself -- a lookup first
// would still let a concurrent arrival and worker pass each other between the
// SELECT and the INSERT.
func scheduleEvaluation(ctx context.Context, db *sql.DB, evalID int64, rule Rule, msg plugins.StoredMessageContext, phase string, dueAt time.Time, result plugins.SearchMatchResult) error {
	if evalID > 0 {
		return recordEvaluation(ctx, db, evalID, rule, msg, phase, statusScheduled, false, dueAt, result.Terms, result.Fields, "{}", "")
	}
	now := time.Now().UTC().Unix()
	_, err := db.ExecContext(ctx, `INSERT INTO plugin_mail_filter_evaluations
		(user_id, rule_id, message_id, account_id, mailbox_id, phase, status, matched, due_at, evaluated_at, terms_json, fields_json, actions_json, error, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, 0, ?, ?, ?, ?, '{}', '', ?)
		ON CONFLICT (user_id, rule_id, message_id) WHERE status = 'scheduled'
		-- An arrival is not un-seen by a later pass over the same message. The
		-- wait keeps the phase it was written in when that phase was the
		-- arrival, or a rule edited while mail waits on its age -- which puts
		-- every message back in front of the rule, backfill included -- would
		-- turn mail this rule watched arrive into mail it merely found, and a
		-- rule that forwards new mail only would then not forward it.
		DO UPDATE SET phase = CASE WHEN plugin_mail_filter_evaluations.phase = 'inbound'
				THEN plugin_mail_filter_evaluations.phase ELSE EXCLUDED.phase END,
			due_at = EXCLUDED.due_at, evaluated_at = EXCLUDED.evaluated_at,
			terms_json = EXCLUDED.terms_json, fields_json = EXCLUDED.fields_json, error = ''`,
		msg.UserID, rule.ID, msg.MessageID, msg.AccountID, msg.MailboxID, phase, statusScheduled,
		unixOrZero(dueAt), now, mustJSON(result.Terms), mustJSON(result.Fields), now)
	return err
}

// moveDestination resolves the folder a rule's move sends one message to, in
// that message's own account. A destination that cannot be resolved is an
// error rather than a silent zero: a rule that says "delete" against an account
// with no Trash folder has not done what it said, and reporting it as a match
// with nothing recorded would hide that from the reader for as long as the rule
// keeps running.
func moveDestination(ctx context.Context, host plugins.StoredMessageHost, db *sql.DB, actions Actions, msg plugins.StoredMessageContext) (int64, error) {
	if actions.MoveMailboxID > 0 {
		return actions.MoveMailboxID, nil
	}
	switch strings.ToLower(strings.TrimSpace(actions.MoveRole)) {
	case "":
		return 0, nil
	case moveRoleTrash:
		id := mailboxIDByRole(ctx, db, msg.UserID, msg.AccountID, moveRoleTrash)
		if id == 0 {
			return 0, errors.New("this account has no Trash folder to delete into")
		}
		return id, nil
	case moveRoleArchive:
		// Archive is a choice rather than a role, so the host is asked for the
		// same folder the header's Archive button uses.
		id, err := host.ArchiveMailboxID(ctx, msg.UserID, msg.AccountID)
		if err != nil {
			return 0, err
		}
		if id == 0 {
			return 0, errors.New("this account has no Archive folder chosen; name one in its identity settings")
		}
		return id, nil
	}
	return 0, fmt.Errorf("unknown move destination %q", actions.MoveRole)
}

func applyActions(ctx context.Context, host plugins.StoredMessageHost, db *sql.DB, rule Rule, msg plugins.StoredMessageContext, p pass) (string, bool, string, string) {
	results := map[string]string{}
	switch {
	case rule.Actions.forwards(p):
		forwarderID, err := ensureForwarderID(ctx, db, msg.UserID, msg.AccountID)
		if err != nil {
			results["forward"] = "failed"
			return mustJSON(results), false, statusFailed, err.Error()
		}
		err = host.ForwardMessage(ctx, msg.UserID, msg.MessageID, rule.Actions.ForwardTo, []plugins.MailHeader{{Name: forwarderHeader, Value: forwarderID}})
		if err != nil {
			results["forward"] = "failed"
			// A forward that would loop is an expected outcome, not a failure;
			// match the host's sentinel rather than its error text so a reworded
			// message cannot silently reclassify a prevented loop as a failure.
			if errors.Is(err, plugins.ErrAlreadyForwarded) {
				return mustJSON(results), false, statusLoop, err.Error()
			}
			return mustJSON(results), false, statusFailed, err.Error()
		}
		results["forward"] = "ok"
	case strings.TrimSpace(rule.Actions.ForwardTo) != "":
		// The rule forwards new mail only and this message was already in the
		// mailbox. Recording the skip is the point: the audit otherwise shows a
		// match that forwarded nothing and says nothing about why.
		results["forward"] = forwardSkippedNew
	}
	destID, err := moveDestination(ctx, host, db, rule.Actions, msg)
	if err != nil {
		results["move"] = "failed"
		return mustJSON(results), false, statusFailed, err.Error()
	}
	if destID > 0 {
		if err := host.MoveMessage(ctx, msg.UserID, msg.MessageID, destID); err != nil {
			results["move"] = "failed"
			return mustJSON(results), false, statusFailed, err.Error()
		}
		results["move"] = "ok"
		return mustJSON(results), true, statusMatched, ""
	}
	return mustJSON(results), false, statusMatched, ""
}

func recordEvaluation(ctx context.Context, db *sql.DB, evalID int64, rule Rule, msg plugins.StoredMessageContext, phase, status string, matched bool, dueAt time.Time, terms, fields []string, actionJSON, errText string) error {
	termsJSON := mustJSON(terms)
	fieldsJSON := mustJSON(fields)
	now := time.Now().UTC().Unix()
	if evalID > 0 {
		_, err := db.ExecContext(ctx, `UPDATE plugin_mail_filter_evaluations SET phase = ?, status = ?, matched = ?, due_at = ?, evaluated_at = ?, terms_json = ?, fields_json = ?, actions_json = ?, error = ? WHERE user_id = ? AND id = ?`,
			phase, status, boolInt(matched), unixOrZero(dueAt), now, termsJSON, fieldsJSON, actionJSON, errText, msg.UserID, evalID)
		return err
	}
	_, err := db.ExecContext(ctx, `INSERT INTO plugin_mail_filter_evaluations
		(user_id, rule_id, message_id, account_id, mailbox_id, phase, status, matched, due_at, evaluated_at, terms_json, fields_json, actions_json, error, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		msg.UserID, rule.ID, msg.MessageID, msg.AccountID, msg.MailboxID, phase, status, boolInt(matched), unixOrZero(dueAt), now, termsJSON, fieldsJSON, actionJSON, errText, now)
	return err
}

func listRecentEvaluations(ctx context.Context, db *sql.DB, userID int64, limit int) ([]Evaluation, error) {
	rows, err := db.QueryContext(ctx, `SELECT e.id, e.user_id, e.rule_id, e.message_id, e.account_id, e.mailbox_id, e.phase, e.status, e.matched, e.due_at, e.evaluated_at, e.terms_json, e.fields_json, e.actions_json, e.error, e.created_at, r.name, COALESCE(m.subject, ''), COALESCE(m.from_addr, '')
		FROM plugin_mail_filter_evaluations e
		JOIN plugin_mail_filter_rules r ON r.id = e.rule_id AND r.user_id = e.user_id
		LEFT JOIN messages m ON m.id = e.message_id AND m.user_id = e.user_id
		WHERE e.user_id = ? AND (e.matched = 1 OR e.status IN (?, ?))
		ORDER BY e.id DESC LIMIT ?`, userID, statusFailed, statusLoop, limit)
	return scanEvaluations(rows, err)
}

func listMessageEvaluations(ctx context.Context, db *sql.DB, userID, messageID int64, limit int) ([]Evaluation, error) {
	rows, err := db.QueryContext(ctx, `SELECT e.id, e.user_id, e.rule_id, e.message_id, e.account_id, e.mailbox_id, e.phase, e.status, e.matched, e.due_at, e.evaluated_at, e.terms_json, e.fields_json, e.actions_json, e.error, e.created_at, r.name, COALESCE(m.subject, ''), COALESCE(m.from_addr, '')
		FROM plugin_mail_filter_evaluations e
		JOIN plugin_mail_filter_rules r ON r.id = e.rule_id AND r.user_id = e.user_id
		LEFT JOIN messages m ON m.id = e.message_id AND m.user_id = e.user_id
		WHERE e.user_id = ? AND e.message_id = ?
		ORDER BY e.evaluated_at DESC, e.id DESC LIMIT ?`, userID, messageID, limit)
	return scanEvaluations(rows, err)
}

// listScheduledEvaluations returns the rules still waiting on a message's age,
// soonest first, so the queue can be read before it acts rather than after.
func listScheduledEvaluations(ctx context.Context, db *sql.DB, userID int64, limit int) ([]Evaluation, error) {
	rows, err := db.QueryContext(ctx, `SELECT e.id, e.user_id, e.rule_id, e.message_id, e.account_id, e.mailbox_id, e.phase, e.status, e.matched, e.due_at, e.evaluated_at, e.terms_json, e.fields_json, e.actions_json, e.error, e.created_at, r.name, COALESCE(m.subject, ''), COALESCE(m.from_addr, '')
		FROM plugin_mail_filter_evaluations e
		JOIN plugin_mail_filter_rules r ON r.id = e.rule_id AND r.user_id = e.user_id
		LEFT JOIN messages m ON m.id = e.message_id AND m.user_id = e.user_id
		WHERE e.user_id = ? AND e.status = ? AND e.due_at > 0 AND r.enabled = 1
		ORDER BY e.due_at, e.id LIMIT ?`, userID, statusScheduled, limit)
	return scanEvaluations(rows, err)
}

func scanEvaluations(rows *sql.Rows, err error) ([]Evaluation, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Evaluation
	for rows.Next() {
		var ev Evaluation
		var matched int
		var termsJSON, fieldsJSON string
		if err := rows.Scan(&ev.ID, &ev.UserID, &ev.RuleID, &ev.MessageID, &ev.AccountID, &ev.MailboxID, &ev.Phase, &ev.Status, &matched, &ev.DueAt, &ev.Evaluated, &termsJSON, &fieldsJSON, &ev.Actions, &ev.Error, &ev.CreatedAt, &ev.RuleName, &ev.Subject, &ev.From); err != nil {
			return nil, err
		}
		ev.Matched = matched != 0
		_ = json.Unmarshal([]byte(termsJSON), &ev.Terms)
		_ = json.Unmarshal([]byte(fieldsJSON), &ev.Fields)
		out = append(out, ev)
	}
	return out, rows.Err()
}

// backfillCursor is a position in the user's mail ordered oldest first. Date
// and id together, because two messages can carry the same date and a cursor
// that cannot separate them either repeats a page or skips one.
type backfillCursor struct {
	DateUnix int64 `json:"date_unix"`
	ID       int64 `json:"id"`
}

// before renders the position the first page starts from. A zero cursor means
// "before everything", which is not (0, 0): a message with no parseable Date is
// stored with a large negative date_unix, so starting at zero walked past every
// dateless message in the mailbox. Message ids are positive, so a zero id is an
// unambiguous marker for the start.
func (c backfillCursor) before() (int64, int64) {
	if c.ID <= 0 {
		return math.MinInt64, 0
	}
	return c.DateUnix, c.ID
}

// backfillRule applies one rule to one page of the mail that is already stored,
// oldest first, and returns where to continue. Two things decide that shape.
// The messages an age rule exists to clean up are the oldest ones, which a
// newest-first page of two thousand left out entirely on any mailbox larger
// than that; and matching is one search per message, so a walk long enough to
// cover a real mailbox cannot be a single request. The caller presses on with
// the returned cursor until the walk reports itself done.
func backfillRule(ctx context.Context, host plugins.StoredMessageHost, db *sql.DB, rule Rule, from backfillCursor) (int, backfillCursor, bool, error) {
	messages, err := backfillPage(ctx, db, rule, from)
	if err != nil {
		return 0, from, false, err
	}
	if len(messages) == 0 {
		return 0, from, true, nil
	}
	processed := 0
	next := from
	for _, msg := range messages {
		if _, err := evaluateRule(ctx, host, db, rule, msg, backfillPass(), 0); err != nil {
			return processed, next, false, err
		}
		processed++
		next = backfillCursor{DateUnix: storedDateUnix(msg.Date), ID: msg.MessageID}
	}
	return processed, next, len(messages) < backfillBatch, nil
}

// backfillPage reads the next page of mail this rule has not decided on yet.
// Skipping what it already decided is not an optimization: a rule's actions are
// carried out where it matches, so a second Backfill over the same message
// forwarded it a second time and wrote a second audit row. `evaluated_at` is
// compared against the rule's own `updated_at`, so editing a rule still puts
// every message back in front of it.
func backfillPage(ctx context.Context, db *sql.DB, rule Rule, from backfillCursor) ([]plugins.StoredMessageContext, error) {
	dateUnix, id := from.before()
	rows, err := db.QueryContext(ctx, `SELECT m.id, m.user_id, m.account_id, m.mailbox_id, m.subject, m.from_addr, m.to_addr, m.cc_addr, m.date_unix, m.uid, m.is_read, m.is_starred
		FROM messages m
		JOIN mailboxes mb ON mb.id = m.mailbox_id AND mb.user_id = m.user_id
		WHERE m.user_id = ? AND (m.date_unix, m.id) > (?, ?)`+filterScope+`
		AND NOT EXISTS (
			SELECT 1 FROM plugin_mail_filter_evaluations e
			WHERE e.user_id = m.user_id AND e.rule_id = ? AND e.message_id = m.id AND e.evaluated_at >= ?
		)
		ORDER BY m.date_unix, m.id LIMIT ?`, rule.UserID, dateUnix, id, rule.ID, rule.UpdatedAt, backfillBatch)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var messages []plugins.StoredMessageContext
	for rows.Next() {
		var msg plugins.StoredMessageContext
		var dateUnix int64
		var read, starred int
		if err := rows.Scan(&msg.MessageID, &msg.UserID, &msg.AccountID, &msg.MailboxID, &msg.Subject, &msg.From, &msg.To, &msg.CC, &dateUnix, &msg.UID, &read, &starred); err != nil {
			return nil, err
		}
		// A message the mirror stored without a date keeps the zero time, so
		// evaluateRule leaves its age to the search index instead of reading it
		// as 1970 and acting at once.
		if dateUnix != zeroDateUnix {
			msg.Date = time.Unix(dateUnix, 0).UTC()
		}
		msg.IsRead = read != 0
		msg.IsStarred = starred != 0
		messages = append(messages, msg)
	}
	return messages, rows.Err()
}

// storedDateUnix is the inverse of the read above: it returns the date_unix the
// message was read from, so the cursor lands back on the same row.
func storedDateUnix(date time.Time) int64 {
	if date.IsZero() {
		return zeroDateUnix
	}
	return date.UTC().Unix()
}

func runScheduled(ctx context.Context, host plugins.StoredMessageHost, db *sql.DB, userID int64, now time.Time) (int, error) {
	rows, err := db.QueryContext(ctx, `SELECT e.id, e.rule_id, e.phase, m.id, m.user_id, m.account_id, m.mailbox_id, m.subject, m.from_addr, m.to_addr, m.cc_addr, m.date_unix, m.uid, m.is_read, m.is_starred
		FROM plugin_mail_filter_evaluations e
		JOIN plugin_mail_filter_rules r ON r.id = e.rule_id AND r.user_id = e.user_id
		JOIN messages m ON m.id = e.message_id AND m.user_id = e.user_id
		WHERE e.user_id = ? AND e.status = ? AND e.due_at > 0 AND e.due_at <= ? AND r.enabled = 1
		ORDER BY e.due_at, e.id LIMIT ?`, userID, statusScheduled, now.Unix(), scheduledBatch)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	type due struct {
		evalID int64
		ruleID int64
		// origin is the phase the wait was written in. A rule that forwards new
		// mail only still forwards a message that arrived while the rule was
		// running and then waited on its age; one the backfill found does not.
		origin string
		msg    plugins.StoredMessageContext
	}
	var dueRows []due
	for rows.Next() {
		var item due
		var dateUnix int64
		var read, starred int
		if err := rows.Scan(&item.evalID, &item.ruleID, &item.origin, &item.msg.MessageID, &item.msg.UserID, &item.msg.AccountID, &item.msg.MailboxID, &item.msg.Subject, &item.msg.From, &item.msg.To, &item.msg.CC, &dateUnix, &item.msg.UID, &read, &starred); err != nil {
			return 0, err
		}
		if dateUnix > 0 {
			item.msg.Date = time.Unix(dateUnix, 0).UTC()
		}
		item.msg.IsRead = read != 0
		item.msg.IsStarred = starred != 0
		dueRows = append(dueRows, item)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	processed := 0
	for _, item := range dueRows {
		rule, err := getRule(ctx, db, userID, item.ruleID)
		if err != nil {
			return processed, err
		}
		if _, err := evaluateRule(ctx, host, db, rule, item.msg, scheduledPass(item.origin), item.evalID); err != nil {
			return processed, err
		}
		processed++
	}
	return processed, purgeOldEvaluations(ctx, db, userID)
}

// purgeOldEvaluations drops what the audit no longer owes anyone: decided rows
// past the retention window, and waits whose message has since been deleted.
// A wait is a promise to act on a message, so once the message is gone the row
// can never resolve -- runScheduled joins `messages` and finds nothing -- and
// the retention sweep skips waits by design, which left them in the pending
// queue for good, sorted to the front by a due date long past.
func purgeOldEvaluations(ctx context.Context, db *sql.DB, userID int64) error {
	cutoff := time.Now().UTC().Add(-retentionWindow).Unix()
	if _, err := db.ExecContext(ctx, `DELETE FROM plugin_mail_filter_evaluations
		WHERE user_id = ? AND status <> ? AND evaluated_at > 0 AND evaluated_at < ?`, userID, statusScheduled, cutoff); err != nil {
		return err
	}
	_, err := db.ExecContext(ctx, `DELETE FROM plugin_mail_filter_evaluations e
		WHERE e.user_id = ? AND e.status = ?
		AND NOT EXISTS (SELECT 1 FROM messages m WHERE m.id = e.message_id AND m.user_id = e.user_id)`,
		userID, statusScheduled)
	return err
}

type ageClause struct {
	Duration           time.Duration
	QueryWithoutClause string
}

func olderThanClause(query string) (ageClause, bool) {
	fields := strings.Fields(query)
	out := make([]string, 0, len(fields))
	for _, field := range fields {
		lower := strings.ToLower(strings.Trim(field, `"`))
		if strings.HasPrefix(lower, "older_than:") {
			if d, ok := parseAgeDuration(strings.TrimPrefix(lower, "older_than:")); ok {
				return ageClause{Duration: d, QueryWithoutClause: strings.Join(append(out, fields[len(out)+1:]...), " ")}, true
			}
		}
		out = append(out, field)
	}
	return ageClause{}, false
}

func parseAgeDuration(value string) (time.Duration, bool) {
	value = strings.Trim(strings.TrimSpace(value), `"`)
	if len(value) < 2 {
		return 0, false
	}
	n, err := strconv.Atoi(value[:len(value)-1])
	if err != nil || n <= 0 {
		return 0, false
	}
	switch value[len(value)-1] {
	case 'd':
		return time.Duration(n) * 24 * time.Hour, true
	case 'w':
		return time.Duration(n) * 7 * 24 * time.Hour, true
	case 'm':
		return time.Duration(n) * 30 * 24 * time.Hour, true
	case 'y':
		return time.Duration(n) * 365 * 24 * time.Hour, true
	}
	return 0, false
}

func ensureForwarderID(ctx context.Context, db *sql.DB, userID, accountID int64) (string, error) {
	var existing string
	err := db.QueryRowContext(ctx, `SELECT forwarder_id FROM plugin_mail_filter_forwarders WHERE user_id = ? AND account_id = ?`, userID, accountID).Scan(&existing)
	if err == nil && strings.TrimSpace(existing) != "" {
		return existing, nil
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		return "", err
	}
	id := "rtf-" + hex.EncodeToString(random)
	_, err = db.ExecContext(ctx, `INSERT INTO plugin_mail_filter_forwarders (user_id, account_id, forwarder_id, created_at) VALUES (?, ?, ?, ?)`, userID, accountID, id, time.Now().UTC().Unix())
	return id, err
}

func mailboxIDByRole(ctx context.Context, db *sql.DB, userID, accountID int64, role string) int64 {
	var id int64
	_ = db.QueryRowContext(ctx, `SELECT id FROM mailboxes WHERE user_id = ? AND account_id = ? AND role = ? ORDER BY id LIMIT 1`, userID, accountID, strings.TrimSpace(role)).Scan(&id)
	return id
}

func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

func containsID(ids []int64, needle int64) bool {
	for _, id := range ids {
		if id == needle {
			return true
		}
	}
	return false
}

func uniqueIDs(ids []int64) []int64 {
	seen := map[int64]bool{}
	out := make([]int64, 0, len(ids))
	for _, id := range ids {
		if id <= 0 || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

func mustJSON(value any) string {
	raw, err := json.Marshal(value)
	if err != nil {
		return "{}"
	}
	return string(raw)
}

func unixOrZero(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UTC().Unix()
}
