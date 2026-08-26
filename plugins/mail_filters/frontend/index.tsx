// File overview: Runtime frontend settings view for the mail filters plugin.

import { useEffect, useMemo, useState } from "react";
import type { FormEvent, MouseEvent } from "react";
import type { DatePrefs, LocationState, Toast } from "../../../frontend/src/appTypes";
import { Icon } from "../../../frontend/src/components/Icon";
import { SettingsEmpty, SettingsError, SettingsLoading, SettingsPage } from "../../../frontend/src/features/settings/SettingsUI";
import { displayDateTime } from "../../../frontend/src/lib/format";
import type { AccountSettingsRuntimePlugin, RuntimeMessageQuickActionContext } from "../../../frontend/src/plugins/runtime";
import type { Mailbox, ThreadMessage, User } from "../../../frontend/src/types";
import "./styles.css";

// A destination is either relative to the message's own account -- Trash or
// Archive, so one rule covering several accounts lands in each account's own
// folder -- or one exact folder. The two are mutually exclusive, which is why
// the editor holds them as a single select value rather than two fields that
// can disagree.
type Actions = {
  move_mailbox_id: number;
  move_role: string;
  // mark_read marks the mail read the way opening it would. It is the one
  // action that adds to the others rather than replacing them: mail a rule
  // forwarded, filed or deleted is mail with no unread badge left to answer.
  mark_read: boolean;
  forward_to: string;
  // forward_new_only limits the forward to mail that reaches the rule as it
  // arrives. Everything else about the rule is unchanged: Backfill still walks
  // the mail already in the mailbox, matches it, moves it and records it.
  forward_new_only: boolean;
};

type Rule = {
  id: number;
  name: string;
  query: string;
  enabled: boolean;
  scope_mode: "all_accounts" | "selected_accounts" | string;
  account_ids: number[];
  actions: Actions;
  position: number;
};

type Evaluation = {
  id: number;
  actions_json: string;
  rule_id: number;
  message_id: number;
  phase: string;
  status: string;
  matched: boolean;
  due_at: number;
  evaluated_at: number;
  rule_name: string;
  subject: string;
  from_addr: string;
  error: string;
};

type Context = {
  csrf: string;
  user: User;
  mailboxes: Mailbox[];
  location: LocationState;
  navigate: (url: string) => void;
  addToast: (message: string, kind?: Toast["kind"]) => number;
};

type MessageActionContext = {
  csrf: string;
  item: ThreadMessage;
  datePrefs: DatePrefs;
  activePanel: string;
  openPanel: (panelID: string) => void;
  closePanel: () => void;
  navigate: (url: string) => void;
  addToast: (message: string, kind?: Toast["kind"]) => number;
};

// A rule stores one search string, and that is what the engine evaluates and
// what an advanced reader edits directly. Conditions are a reading of that same
// string for the three rules people actually write -- this sender, this
// subject, older than this many days -- so they can be written without knowing
// the search syntax. Anything the three fields do not claim stays in "rest",
// which is why parsing and rebuilding a query does not lose the parts they
// cannot express.
type Conditions = {
  from: string;
  subject: string;
  olderThanDays: string;
  rest: string;
};

const blankConditions: Conditions = { from: "", subject: "", olderThanDays: "", rest: "" };

type BackfillResult = {
  ok?: boolean;
  error?: string;
  processed: number;
  done: boolean;
  cursor: { date_unix: number; id: number };
};

function tokenizeQuery(query: string) {
  const tokens: string[] = [];
  let current = "";
  let quoted = false;
  for (const char of query) {
    if (char === '"') {
      quoted = !quoted;
      current += char;
      continue;
    }
    if (!quoted && /\s/.test(char)) {
      if (current) tokens.push(current);
      current = "";
      continue;
    }
    current += char;
  }
  if (current) tokens.push(current);
  return tokens;
}

function parseConditions(query: string): Conditions {
  const out: Conditions = { ...blankConditions };
  const rest: string[] = [];
  for (const token of tokenizeQuery(query)) {
    const lower = token.toLowerCase();
    // Only the first of each operator is lifted into a field. A query that
    // names two senders keeps the second one in the advanced box rather than
    // losing it to a field that can hold one.
    if (!out.from && lower.startsWith("from:")) {
      out.from = unquote(token.slice("from:".length));
      continue;
    }
    if (!out.subject && lower.startsWith("subject:")) {
      out.subject = unquote(token.slice("subject:".length));
      continue;
    }
    if (!out.olderThanDays && lower.startsWith("older_than:")) {
      const days = ageDays(unquote(token.slice("older_than:".length)));
      if (days > 0) {
        out.olderThanDays = String(days);
        continue;
      }
    }
    rest.push(token);
  }
  out.rest = rest.join(" ");
  return out;
}

function buildQuery(conditions: Conditions) {
  const parts: string[] = [];
  if (conditions.from.trim()) parts.push(`from:${quoteValue(conditions.from)}`);
  if (conditions.subject.trim()) parts.push(`subject:${quoteValue(conditions.subject)}`);
  const days = olderThanDays(conditions);
  if (days > 0) parts.push(`older_than:${days}d`);
  if (conditions.rest.trim()) parts.push(conditions.rest.trim());
  return parts.join(" ");
}

// olderThanDays reads the age field the way the engine reads the term it
// composes: whole days, at least one. Both parseAgeDuration and the search's
// own relative-date parser reject anything else, so a value the field accepts
// but they do not would drop the age out of the query silently -- turning
// "trash this in 30 days" into "trash this now". A rejected value returns 0 and
// olderThanInvalid below blocks the save rather than letting that happen.
function olderThanDays(conditions: Conditions) {
  const raw = conditions.olderThanDays.trim();
  if (!raw) return 0;
  const days = Number(raw);
  if (!Number.isInteger(days) || days < 1) return 0;
  return days;
}

function olderThanInvalid(conditions: Conditions) {
  return conditions.olderThanDays.trim() !== "" && olderThanDays(conditions) === 0;
}

function unquote(value: string) {
  return value.replace(/^"|"$/g, "");
}

function quoteValue(value: string) {
  // Quotes are dropped rather than escaped: the search grammar has no escape,
  // and a stray one would swallow the rest of the query into one token.
  const trimmed = value.trim().replaceAll('"', "");
  return /\s/.test(trimmed) ? `"${trimmed}"` : trimmed;
}

// The engine reads d, w, m and y; the field speaks days, so the other three are
// converted rather than refused. The conversions match parseAgeDuration.
function ageDays(value: string) {
  const match = /^(\d+)([dwmy])$/.exec(value.trim().toLowerCase());
  if (!match) return 0;
  const amount = Number(match[1]);
  const perUnit: Record<string, number> = { d: 1, w: 7, m: 30, y: 365 };
  return amount * (perUnit[match[2]] || 0);
}

// The destination select carries one value for what are two fields on the
// rule, so the two can never both be set. "role:" names a folder relative to
// the message's own account and "mailbox:" names one exact folder.
function destinationValue(actions: Actions) {
  if (actions.move_mailbox_id > 0) return `mailbox:${actions.move_mailbox_id}`;
  if (actions.move_role) return `role:${actions.move_role}`;
  return "";
}

function destinationActions(value: string): Pick<Actions, "move_role" | "move_mailbox_id"> {
  if (value.startsWith("role:")) return { move_role: value.slice("role:".length), move_mailbox_id: 0 };
  if (value.startsWith("mailbox:")) return { move_role: "", move_mailbox_id: Number(value.slice("mailbox:".length)) || 0 };
  return { move_role: "", move_mailbox_id: 0 };
}

// actionSummary says what a saved rule does, in the words the editor uses, so
// the list answers "what will this actually do to my mail" without opening it.
function actionSummary(actions: Actions, mailboxes: Mailbox[]) {
  const parts: string[] = [];
  if (actions.forward_to.trim()) {
    parts.push(actions.forward_new_only ? `Forward new mail to ${actions.forward_to.trim()}` : `Forward to ${actions.forward_to.trim()}`);
  }
  if (actions.move_role === "trash") parts.push("Delete");
  else if (actions.move_role === "archive") parts.push("Archive");
  else if (actions.move_mailbox_id > 0) {
    const folder = mailboxes.find((mailbox) => mailbox.id === actions.move_mailbox_id);
    parts.push(folder ? `Move to ${folder.account_label || folder.account_email} / ${folder.name}` : "Move to a folder that is gone");
  }
  if (actions.mark_read) parts.push("Mark as read");
  return parts.length > 0 ? parts.join(" \u00b7 ") : "Record matches only";
}

// A From header carries a display name as often as not, and the search's from:
// operator wants the address. Take what the angle brackets hold when there is a
// pair, and the whole trimmed value otherwise.
function senderAddress(fromAddr: string) {
  const angled = /<([^<>]+)>/.exec(fromAddr);
  return (angled ? angled[1] : fromAddr).trim().toLowerCase();
}

// A reply prefix is not part of what the subject is about, and a filter written
// from one reply should reach the rest of the thread, so the prefixes come off.
function subjectStem(subject: string) {
  return subject.replace(/^\s*(?:(?:re|aw|fwd?|wg|antw)\s*(?:\[\d+\])?\s*:\s*)+/i, "").trim();
}

const blankRule: Rule = {
  id: 0,
  name: "",
  query: "",
  enabled: true,
  scope_mode: "all_accounts",
  account_ids: [],
  // A new rule forwards new mail only. A mailbox holds years of mail and a
  // forward is the one action that leaves the account, so the first Backfill of
  // a rule written without thinking about it must not send hundreds of copies of
  // mail the reader dealt with long ago. Turning it off is one click.
  actions: { move_mailbox_id: 0, move_role: "", mark_read: false, forward_to: "", forward_new_only: true },
  position: 0
};

export function MailFilterSettings({ csrf, user, mailboxes, location, navigate, addToast }: Context) {
  const [rules, setRules] = useState<Rule[]>([]);
  const [draft, setDraft] = useState<Rule>(blankRule);
  const [recent, setRecent] = useState<Evaluation[]>([]);
  const [pending, setPending] = useState<Evaluation[]>([]);
  const [loading, setLoading] = useState(true);
  const [loadError, setLoadError] = useState("");
  const [busy, setBusy] = useState(false);
  const [advanced, setAdvanced] = useState(false);
  // The fields hold their own text while the reader types, because the query
  // they compose is normalized -- a name half-typed as "John " would lose the
  // space it needs to become "John Smith" if the field read itself back out of
  // the composed string on every keystroke. Every place that replaces the whole
  // draft re-reads them from that draft's query, which stays the stored truth.
  const [conditions, setConditions] = useState<Conditions>(blankConditions);
  const ageInvalid = olderThanInvalid(conditions);
  const ageDaysValue = olderThanDays(conditions);

  const accounts = useMemo(() => {
    const seen = new Map<number, string>();
    mailboxes.forEach((mailbox) => {
      if (!seen.has(mailbox.account_id)) {
        seen.set(mailbox.account_id, mailbox.account_label || mailbox.account_email || `Account ${mailbox.account_id}`);
      }
    });
    return [...seen.entries()].map(([id, label]) => ({ id, label }));
  }, [mailboxes]);

  // Folders are grouped under the account that owns them, because a folder only
  // means anything inside its own account: a move cannot cross accounts.
  const folderGroups = useMemo(() => {
    const groups = new Map<number, { label: string; folders: Mailbox[] }>();
    mailboxes.forEach((mailbox) => {
      const group = groups.get(mailbox.account_id) || {
        label: mailbox.account_label || mailbox.account_email || `Account ${mailbox.account_id}`,
        folders: []
      };
      group.folders.push(mailbox);
      groups.set(mailbox.account_id, group);
    });
    return [...groups.entries()].map(([id, group]) => ({ id, ...group }));
  }, [mailboxes]);

  // A rule that names one account's folder but reaches other accounts would
  // fail on every message from those accounts, one failed action at a time, and
  // the reader would only find out from the audit. Say it in the editor.
  const destinationFolder = draft.actions.move_mailbox_id > 0
    ? mailboxes.find((mailbox) => mailbox.id === draft.actions.move_mailbox_id)
    : undefined;
  const ruleAccountIDs = draft.scope_mode === "selected_accounts"
    ? (draft.account_ids || [])
    : accounts.map((account) => account.id);
  const strandedAccounts = destinationFolder
    ? accounts.filter((account) => account.id !== destinationFolder.account_id && ruleAccountIDs.includes(account.id))
    : [];
  const destinationMissing = draft.actions.move_mailbox_id > 0 && !destinationFolder;

  async function load(quiet = false) {
    if (!quiet) {
      setLoading(true);
      setLoadError("");
    }
    try {
      const [ruleData, history] = await Promise.all([
        getJSON<{ rules: Rule[] }>("/api/plugins/mail_filters/rules"),
        getJSON<{ recent: Evaluation[]; pending: Evaluation[] }>("/api/plugins/mail_filters/history")
      ]);
      setRules(ruleData.rules || []);
      setRecent(history.recent || []);
      setPending(history.pending || []);
    } catch (err) {
      const message = messageFromError(err);
      if (quiet) addToast(message, "error");
      else setLoadError(message);
    } finally {
      if (!quiet) setLoading(false);
    }
  }

  useEffect(() => {
    // Arriving with a query -- from a search, or from an open message -- means
    // "make a filter for this", never "rewrite the rule I had open", so the
    // draft starts blank rather than inheriting whatever id was being edited.
    const initialQuery = new URLSearchParams(location.search).get("query") || "";
    if (initialQuery) {
      setDraft({ ...blankRule, query: initialQuery, name: `Filter: ${initialQuery}` });
      setConditions(parseConditions(initialQuery));
    }
    void load();
  }, [location.search]);

  function edit(rule: Rule) {
    applyDraft({
      ...rule,
      account_ids: rule.account_ids || [],
      actions: { ...blankRule.actions, ...(rule.actions || {}) }
    });
  }

  function resetDraft() {
    applyDraft(blankRule);
  }

  async function save(event: FormEvent) {
    event.preventDefault();
    setBusy(true);
    try {
      const data = await postJSON<{ rule: Rule }>("/api/plugins/mail_filters/rules", csrf, draft);
      applyDraft(data.rule);
      addToast("Filter saved.");
      await load(true);
    } catch (err) {
      addToast(messageFromError(err), "error");
    } finally {
      setBusy(false);
    }
  }

  async function remove(rule: Rule) {
    if (!window.confirm(`Delete ${rule.name || rule.query}?`)) return;
    setBusy(true);
    try {
      await deleteJSON(`/api/plugins/mail_filters/rules/${rule.id}`, csrf);
      if (draft.id === rule.id) resetDraft();
      addToast("Filter deleted.");
      await load(true);
    } catch (err) {
      addToast(messageFromError(err), "error");
    } finally {
      setBusy(false);
    }
  }

  // The backend walks one page of stored mail per request, oldest first, so the
  // whole mailbox is covered by following the cursor it hands back rather than
  // by one request long enough to time out.
  async function backfill(rule: Rule) {
    if (!window.confirm(`Apply ${rule.name || rule.query} to existing mail?`)) return;
    setBusy(true);
    try {
      let cursor = { date_unix: 0, id: 0 };
      let processed = 0;
      for (;;) {
        const data = await postJSON<BackfillResult>(`/api/plugins/mail_filters/rules/${rule.id}/backfill`, csrf, cursor);
        processed += data.processed || 0;
        // A page that failed part way still committed what it evaluated, so the
        // count is reported rather than thrown away with the error.
        if (data.ok === false) throw new Error(`${data.error || "Backfill stopped early."} Checked ${processed} so far.`);
        // A cursor that did not move would ask for the same page forever, so
        // the loop stops on it rather than trusting "done" alone.
        if (data.done || !data.cursor || (data.cursor.date_unix === cursor.date_unix && data.cursor.id === cursor.id)) break;
        cursor = data.cursor;
      }
      addToast(`Backfill checked ${processed} ${processed === 1 ? "message" : "messages"}.`);
      await load(true);
    } catch (err) {
      addToast(messageFromError(err), "error");
    } finally {
      setBusy(false);
    }
  }

  async function runDue() {
    setBusy(true);
    try {
      const data = await postJSON<{ processed: number }>("/api/plugins/mail_filters/scheduled/run", csrf, {});
      addToast(`Processed ${data.processed || 0} due scheduled filters.`);
      await load(true);
    } catch (err) {
      addToast(messageFromError(err), "error");
    } finally {
      setBusy(false);
    }
  }

  function setAction(patch: Partial<Actions>) {
    setDraft((current) => ({ ...current, actions: { ...current.actions, ...patch } }));
  }

  // Editing a field recomposes the whole query from all of them, so the draft
  // the Save button posts is always what the fields currently say.
  function setCondition(patch: Partial<Conditions>) {
    const next = { ...conditions, ...patch };
    setConditions(next);
    setDraft((rule) => ({ ...rule, query: buildQuery(next) }));
  }

  // Whatever replaces the draft wholesale -- opening a rule, resetting, a save
  // response, a hand-written search -- reseeds the fields from that query.
  function applyDraft(rule: Rule) {
    setDraft(rule);
    setConditions(parseConditions(rule.query));
  }

  function toggleAccount(id: number) {
    setDraft((current) => {
      const selected = new Set(current.account_ids || []);
      selected.has(id) ? selected.delete(id) : selected.add(id);
      return { ...current, account_ids: [...selected] };
    });
  }

  return (
    <SettingsPage
      title="Mail filters"
      description="Rules that forward, archive or delete mail by sender, subject or age, for new and existing mail."
      backPath="/settings/account/plugins"
      navigate={navigate}
      className="mail-filter-settings"
    >
      {loading ? <SettingsLoading label="Loading filters..." /> : null}
      {!loading && loadError ? <SettingsError message={loadError} onRetry={() => void load()} /> : null}
      {!loading && !loadError ? (
        <>
      <form className="panel mail-filter-editor" onSubmit={save}>
        <div className="panel-headline">
          <div>
            <h2>{draft.id ? "Edit filter" : "New filter"}</h2>
          </div>
          <button className="secondary" type="button" onClick={resetDraft}>New</button>
        </div>
        <label>
          <span className="settings-field-label">Name</span>
          <input type="text" value={draft.name} onChange={(event) => setDraft({ ...draft, name: event.target.value })} placeholder="Yoga reservations cleanup" />
        </label>
        <fieldset className="mail-filter-conditions">
          <legend>When mail matches</legend>
          <div className="mail-filter-grid">
            <label>
              <span className="settings-field-label">From</span>
              <input type="text" value={conditions.from} onChange={(event) => setCondition({ from: event.target.value })} placeholder="studio@example.com" />
            </label>
            <label>
              <span className="settings-field-label">Subject contains</span>
              <input type="text" value={conditions.subject} onChange={(event) => setCondition({ subject: event.target.value })} placeholder="Reservation" />
            </label>
            <label>
              <span className="settings-field-label">Older than</span>
              <input type="number" min={1} step={1} value={conditions.olderThanDays} onChange={(event) => setCondition({ olderThanDays: event.target.value })} placeholder="days" />
              {ageInvalid ? <small className="error-text">Whole days, one or more. Anything else would drop the age and let the rule act at once.</small> : null}
            </label>
          </div>
          <p className="muted mail-filter-query-preview">
            {draft.query ? <>Search: <code>{draft.query}</code></> : "Fill in at least one condition, or write a search below."}
          </p>
          <button className="ghost" type="button" onClick={() => setAdvanced((current) => !current)}>
            {advanced ? "Hide search" : "Edit search directly"}
          </button>
          {advanced ? (
            <label>
              <span className="settings-field-label">Search</span>
              <input type="text" value={draft.query} onChange={(event) => applyDraft({ ...draft, query: event.target.value })} placeholder='from:studio@example.com older_than:7d' required />
            </label>
          ) : null}
        </fieldset>
        <label>
          <span className="settings-field-label">Applies to</span>
          <select value={draft.scope_mode} onChange={(event) => setDraft({ ...draft, scope_mode: event.target.value })}>
            <option value="all_accounts">All accounts</option>
            <option value="selected_accounts">Selected accounts</option>
          </select>
        </label>
        {draft.scope_mode === "selected_accounts" ? (
          <div className="mail-filter-account-list">
            {accounts.map((account) => (
              <label key={account.id}>
                <input type="checkbox" checked={(draft.account_ids || []).includes(account.id)} onChange={() => toggleAccount(account.id)} />
                <span>{account.label}</span>
              </label>
            ))}
          </div>
        ) : null}
        <fieldset className="mail-filter-conditions">
          <legend>Then do this</legend>
          <div className="mail-filter-grid">
            <label>
              <span className="settings-field-label">Move it to</span>
              <select value={destinationValue(draft.actions)} onChange={(event) => setAction(destinationActions(event.target.value))}>
                <option value="">Nowhere &mdash; leave it where it is</option>
                <optgroup label={accounts.length > 1 ? "Each message\u2019s own account" : "This account"}>
                  <option value="role:trash">Trash &mdash; this deletes it</option>
                  <option value="role:archive">Archive</option>
                </optgroup>
                {folderGroups.map((group) => (
                  <optgroup label={group.label} key={group.id}>
                    {group.folders.map((folder) => <option value={`mailbox:${folder.id}`} key={folder.id}>{folder.name}</option>)}
                  </optgroup>
                ))}
                {destinationMissing ? (
                  // A folder the rule still names but the account no longer has
                  // needs an option of its own, or the select falls back to its
                  // first entry and shows "leave it where it is" over a rule
                  // that is in fact still trying to move mail into it.
                  <option value={`mailbox:${draft.actions.move_mailbox_id}`}>A folder that is gone</option>
                ) : null}
              </select>
            </label>
            <label className="mail-filter-forward">
              <span className="settings-field-label">Forward to</span>
              <input type="email" value={draft.actions.forward_to} onChange={(event) => setAction({ forward_to: event.target.value })} placeholder="name@example.com" />
            </label>
          </div>
          <label className="mail-filter-mark-read">
            <input type="checkbox" checked={draft.actions.mark_read} onChange={(event) => setAction({ mark_read: event.target.checked })} />
            <span>Mark it as read</span>
          </label>
          {draft.actions.mark_read ? (
            <p className="muted">
              Marking read happens before the forward and the move, so mail this filter files or deletes leaves nothing unread behind. Backfill marks the mail already in the mailbox read as well &mdash; on a filter that matches years of mail, that is one pass over all of it.
            </p>
          ) : null}
          {draft.actions.forward_to.trim() ? (
            <label className="mail-filter-forward-scope">
              <input type="checkbox" checked={draft.actions.forward_new_only} onChange={(event) => setAction({ forward_new_only: event.target.checked })} />
              <span>Forward new mail only</span>
            </label>
          ) : null}
          {draft.actions.forward_to.trim() ? (
            <p className="muted">
              {draft.actions.forward_new_only
                ? "Only mail that arrives from now on is forwarded. Backfill still walks the mail already in the mailbox \u2014 it matches it, moves it and records it, it just does not send a copy of it."
                : "Everything this filter matches is forwarded, the mail already in the mailbox included, the next time you press Backfill. Your provider keeps its own copy of every forward, so a walk over years of mail puts all of them back in your lists."}
            </p>
          ) : null}
          {destinationMissing ? (
            <p className="error-text">
              This filter moves mail into a folder this account no longer has, so every move it makes fails. Choose another destination.
            </p>
          ) : null}
          {draft.actions.move_role === "trash" ? (
            <p className="muted">
              Deleting puts mail in the account&rsquo;s own Trash, the same as deleting it by hand. Rolltop never erases mail on the server.
            </p>
          ) : null}
          {draft.actions.move_role === "archive" ? (
            <p className="muted">
              Archiving uses each account&rsquo;s chosen Archive folder &mdash; the one its identity settings name. An account without one records a failure rather than guessing. Name the folder, save this filter again, then press Backfill: saving is what puts already-decided mail back in front of the rule, and Backfill is what walks it.
            </p>
          ) : null}
          {strandedAccounts.length > 0 ? (
            <p className="error-text">
              Mail cannot move between accounts. {strandedAccounts.map((account) => account.label).join(", ")} {strandedAccounts.length === 1 ? "is also covered by this rule and its mail" : "are also covered by this rule and their mail"} would fail to move.
              Choose Delete or Archive to mean each account&rsquo;s own folder, or narrow this rule to {destinationFolder ? (accounts.find((account) => account.id === destinationFolder.account_id)?.label || "one account") : "one account"}.
            </p>
          ) : null}
          {ageDaysValue > 0 && draft.actions.move_role === "trash" ? (
            <p className="muted">
              Matching mail is deleted {ageDaysValue} {ageDaysValue === 1 ? "day" : "days"} after it was sent, not after the rule saw it. Anything already older goes on the next pass; the rest waits in the queue below.
            </p>
          ) : null}
        </fieldset>
        <div className="mail-filter-actions">
          <label><input type="checkbox" checked={draft.enabled} onChange={(event) => setDraft({ ...draft, enabled: event.target.checked })} /> Enabled</label>
        </div>
        <div className="form-actions">
          <button disabled={busy || ageInvalid || !draft.query.trim()}><Icon name="label" />Save filter</button>
        </div>
      </form>
      <section className="panel">
        <div className="panel-headline">
          <div>
            <h2>Rules</h2>
            <div className="muted">{rules.length === 1 ? "1 filter" : `${rules.length} filters`}</div>
          </div>
          <button className="secondary" type="button" disabled={busy} onClick={() => void runDue()}><Icon name="clock" />Run due</button>
        </div>
        <div className="mail-filter-rule-list">
          {rules.map((rule) => (
            <div className="mail-filter-rule-row" key={rule.id}>
              <button type="button" onClick={() => edit(rule)}>
                <strong>{rule.name || rule.query}</strong>
                <small>{rule.query}</small>
                <small>{actionSummary({ ...blankRule.actions, ...(rule.actions || {}) }, mailboxes)}</small>
              </button>
              <button className="secondary" type="button" disabled={busy} onClick={() => void backfill(rule)}><Icon name="sync" />Backfill</button>
              <button className="icon-button" type="button" disabled={busy} onClick={() => void remove(rule)} title="Delete filter"><Icon name="delete" /></button>
            </div>
          ))}
          {rules.length === 0 ? (
            <SettingsEmpty
              icon="label"
              title="No filters yet"
              description="Create a filter above to automate matching mail."
            />
          ) : null}
        </div>
      </section>
      <EvaluationPanel
        title="Waiting on age"
        description="Matched mail an age condition has not released yet. Nothing has been moved or forwarded for these."
        emptyTitle="Nothing waiting"
        emptyDescription="Filters with an age condition queue their matches here until the mail is old enough."
        evaluations={pending}
        datePrefs={user}
      />
      <EvaluationPanel
        title="Recent actions"
        description="What the filters have done over the last 30 days."
        emptyTitle="No filter actions yet"
        emptyDescription="Matches appear here once a filter acts on one."
        evaluations={recent}
        datePrefs={user}
      />
        </>
      ) : null}
    </SettingsPage>
  );
}

function EvaluationPanel({
  title,
  description,
  emptyTitle,
  emptyDescription,
  evaluations,
  datePrefs
}: {
  title: string;
  description: string;
  emptyTitle: string;
  emptyDescription: string;
  evaluations: Evaluation[];
  datePrefs: DatePrefs;
}) {
  return (
    <section className="panel">
      <div className="panel-headline">
        <div>
          <h2>{title}</h2>
          <div className="muted">{description}</div>
        </div>
      </div>
      {evaluations.length === 0 ? (
        <SettingsEmpty icon="clock" title={emptyTitle} description={emptyDescription} />
      ) : (
        <div className="mail-filter-message-evaluations">
          {evaluations.map((ev) => (
            <div className="mail-filter-message-evaluation" key={ev.id}>
              <span className={`mail-filter-status ${ev.status}`}>{statusLabel(ev.status)}</span>
              <div>
                <strong>{ev.subject || "(no subject)"}</strong>
                <small>{[ev.from_addr, ev.rule_name].filter(Boolean).join(" · ")}</small>
                <small>{evaluationDetail(ev, datePrefs)}</small>
                {ev.error ? <small className="error-text">{ev.error}</small> : null}
              </div>
            </div>
          ))}
        </div>
      )}
    </section>
  );
}

function MessageFilterEvaluationsPanel({ item, datePrefs, activePanel, closePanel, addToast }: MessageActionContext) {
  const [evaluations, setEvaluations] = useState<Evaluation[]>([]);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState("");
  const open = activePanel === "mail-filter-evaluations";

  useEffect(() => {
    if (!open) return;
    let cancelled = false;
    setLoading(true);
    setError("");
    getJSON<{ evaluations: Evaluation[] }>(`/api/plugins/mail_filters/messages/${item.message.id}/evaluations`)
      .then((data) => {
        if (!cancelled) setEvaluations(data.evaluations || []);
      })
      .catch((err) => {
        const message = messageFromError(err);
        if (!cancelled) setError(message);
        addToast(message, "error");
      })
      .finally(() => {
        if (!cancelled) setLoading(false);
      });
    return () => {
      cancelled = true;
    };
  }, [addToast, item.message.id, open]);

  if (!open) return null;
  return (
    <section className="search-explanation mail-filter-message-panel" aria-live="polite">
      <div className="search-explanation-head mail-filter-message-panel-head">
        <div>
          <strong>Filter evaluations</strong>
          <span>{evaluations.length === 1 ? "1 evaluation" : `${evaluations.length} evaluations`}</span>
        </div>
        <button className="ghost search-explanation-close" type="button" title="Close" aria-label="Close filter evaluations" onClick={closePanel}>
          <Icon name="close" />
        </button>
      </div>
      {loading ? <p>Loading filter evaluations...</p> : null}
      {error ? <p className="error-text">{error}</p> : null}
      {!loading && !error && evaluations.length === 0 ? <p>No filters have evaluated this message yet.</p> : null}
      {!loading && !error && evaluations.length > 0 ? (
        <div className="mail-filter-message-evaluations">
          {evaluations.map((ev) => (
            <div className="mail-filter-message-evaluation" key={ev.id}>
              <span className={`mail-filter-status ${ev.status}`}>{statusLabel(ev.status)}</span>
              <div>
                <strong>{ev.rule_name}</strong>
                <small>{evaluationDetail(ev, datePrefs)}</small>
                {ev.error ? <small className="error-text">{ev.error}</small> : null}
              </div>
            </div>
          ))}
        </div>
      ) : null}
    </section>
  );
}

function statusLabel(status: string) {
  return status.replaceAll("_", " ");
}

// actionOutcome reads what one action recorded on an evaluation row. The two
// outcomes the detail line spells out are the ones that are neither a success
// nor a failure, and a row that shows "matched" over either of them says the
// opposite of what happened.
function actionOutcome(ev: Evaluation, action: string) {
  if (!ev.actions_json) return "";
  try {
    // A row that recorded no actions holds "{}", and one written before this
    // column meant anything can hold "null" -- neither is a failure to parse,
    // so neither should be answered by throwing.
    const actions = JSON.parse(ev.actions_json) as Record<string, string> | null;
    return actions?.[action] || "";
  } catch {
    return "";
  }
}

function evaluationDetail(ev: Evaluation, datePrefs: DatePrefs) {
  // A scheduled row is recorded with matched = false because its rule has not
  // acted yet, not because the message failed to match -- it matched everything
  // but the age, which is why it is waiting at all. Saying "did not match" next
  // to a queued deletion reads as the opposite of what is about to happen.
  const outcome = ev.status === "scheduled" ? "waiting on age" : ev.matched ? "matched" : "did not match";
  const parts = [
    ev.phase ? statusLabel(ev.phase) : "",
    ev.evaluated_at ? `evaluated ${displayDateTime(new Date(ev.evaluated_at * 1000).toISOString(), datePrefs)}` : "",
    ev.due_at ? `due ${displayDateTime(new Date(ev.due_at * 1000).toISOString(), datePrefs)}` : "",
    outcome,
    // A row that matched and forwarded nothing has to say why, or the audit
    // reads as a forward that quietly failed.
    actionOutcome(ev, "forward") === "skipped_existing_mail" ? "forward skipped \u2014 this mail was already in the mailbox" : "",
    // Read locally, with the flag still to reach the server. Saying "marked as
    // read" here would claim the mail is read on every other client too.
    actionOutcome(ev, "read") === "queued" ? "marked read here \u2014 the flag reaches the server on the next sync" : ""
  ].filter(Boolean);
  return parts.join(" · ");
}

async function getJSON<T>(url: string): Promise<T> {
  const res = await fetch(url, { credentials: "same-origin" });
  if (!res.ok) throw new Error(await errorText(res));
  return res.json();
}

async function postJSON<T>(url: string, csrf: string, payload: unknown): Promise<T> {
  const res = await fetch(url, {
    method: "POST",
    credentials: "same-origin",
    headers: { "Content-Type": "application/json", "X-CSRF-Token": csrf },
    body: JSON.stringify(payload)
  });
  if (!res.ok) throw new Error(await errorText(res));
  return res.json();
}

async function deleteJSON(url: string, csrf: string) {
  const res = await fetch(url, { method: "DELETE", credentials: "same-origin", headers: { "X-CSRF-Token": csrf } });
  if (!res.ok) throw new Error(await errorText(res));
}

async function errorText(res: Response) {
  try {
    const data = await res.json();
    return data.error || data.message || res.statusText;
  } catch {
    return res.statusText;
  }
}

function messageFromError(err: unknown) {
  return err instanceof Error ? err.message : "Request failed";
}

export default {
  accountSettingsRoutes: [
    {
      path: "/settings/account/plugins/filters",
      aliases: ["/settings/account/filters"],
      title: "Mail filters",
      label: "Filters",
      description: "Rules that forward, archive or delete mail by sender, subject or age.",
      icon: "label",
      section: "plugins",
      render: (context: Context) => <MailFilterSettings {...context} />
    }
  ],
  renderAccountSettingsSummary: ({ navigate }: Context) => (
    <section className="panel account-list-panel">
      <div className="panel-headline">
        <div>
          <h2>Mail filters</h2>
          <div className="muted">Rules that forward, archive, or delete matching mail, now and as it arrives.</div>
        </div>
        <button className="secondary" type="button" onClick={() => navigate("/settings/account/plugins/filters")}><Icon name="label" />Manage</button>
      </div>
      <button className="server-row" type="button" onClick={() => navigate("/settings/account/plugins/filters")}>
        <span className="server-row-icon"><Icon name="label" /></span>
        <strong>Filters</strong>
        <small>Filter by sender, subject or age; forward, archive or delete what matches.</small>
      </button>
    </section>
  ),
  renderSearchActions: ({ query, navigate }: { query: string; navigate: (url: string) => void }) => (
    <button className="secondary" type="button" onClick={() => navigate(`/settings/account/plugins/filters?query=${encodeURIComponent(query)}`)}>
      <Icon name="label" />Create filter
    </button>
  ),
  // A list row and the open message both carry the toolbar Reply and Archive
  // sit in, and a filter is written from a message far more often than from a
  // search box. One click has no menu to pick a condition from, so it makes the
  // rule people actually write from mail in front of them -- this sender -- and
  // the editor it lands in is where a subject or an age is added instead.
  renderMessageQuickActions: ({ message, buttonClassName, disabled, navigate, addToast }: RuntimeMessageQuickActionContext) => {
    const address = senderAddress(message.from_addr || "");
    return (
      <button
        className={buttonClassName}
        type="button"
        disabled={disabled}
        title={address ? `Create a filter for ${address}` : "Create a filter for this sender"}
        aria-label="Create a filter for this sender"
        onClick={() => {
          if (!address) {
            addToast("This message has no sender to filter on.", "error");
            return;
          }
          navigate(`/settings/account/plugins/filters?query=${encodeURIComponent(`from:${quoteValue(address)}`)}`);
        }}
      >
        <Icon name="filter" />
      </button>
    );
  },
  renderMessageMenuActions: ({ item, navigate, activePanel, openPanel, closePanel, addToast }: MessageActionContext) => {
    // The editor already reads a query out of the URL and lifts from: and
    // subject: back into its own fields, so an open message only has to hand it
    // one condition -- no second prefill path to keep in step with the parser.
    const openEditor = (event: MouseEvent<HTMLButtonElement>, condition: string, missing: string) => {
      event.currentTarget.closest("details")?.removeAttribute("open");
      if (!condition) {
        addToast(missing, "error");
        return;
      }
      navigate(`/settings/account/plugins/filters?query=${encodeURIComponent(condition)}`);
    };
    const address = senderAddress(item.message.from_addr || "");
    const subject = subjectStem(item.message.subject || "");
    return (
      <>
        <button type="button" onClick={(event) => openEditor(event, address ? `from:${quoteValue(address)}` : "", "This message has no sender to filter on.")}>
          <Icon name="label" />
          Filter mail from this sender
        </button>
        <button type="button" onClick={(event) => openEditor(event, subject ? `subject:${quoteValue(subject)}` : "", "This message has no subject to filter on.")}>
          <Icon name="label" />
          Filter mail with this subject
        </button>
        <button
          type="button"
          onClick={(event) => {
            event.currentTarget.closest("details")?.removeAttribute("open");
            if (activePanel === "mail-filter-evaluations") closePanel();
            else openPanel("mail-filter-evaluations");
          }}
        >
          <Icon name="rule" />
          Filter evaluations
        </button>
      </>
    );
  },
  renderMessageActionPanels: (context: MessageActionContext) => <MessageFilterEvaluationsPanel {...context} />
} satisfies AccountSettingsRuntimePlugin;
