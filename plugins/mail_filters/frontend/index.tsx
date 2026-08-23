// File overview: Runtime frontend settings view for the mail filters plugin.

import { useEffect, useMemo, useState } from "react";
import type { FormEvent } from "react";
import type { DatePrefs, LocationState, Toast } from "../../../frontend/src/appTypes";
import { Icon } from "../../../frontend/src/components/Icon";
import { SettingsEmpty, SettingsError, SettingsLoading, SettingsPage } from "../../../frontend/src/features/settings/SettingsUI";
import { displayDateTime } from "../../../frontend/src/lib/format";
import type { AccountSettingsRuntimePlugin } from "../../../frontend/src/plugins/runtime";
import type { Mailbox, ThreadMessage, User } from "../../../frontend/src/types";
import "./styles.css";

type Actions = {
  star: boolean;
  move_mailbox_id: number;
  move_role: string;
  forward_to: string;
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
  const days = Number(conditions.olderThanDays);
  if (Number.isFinite(days) && days > 0) parts.push(`older_than:${Math.floor(days)}d`);
  if (conditions.rest.trim()) parts.push(conditions.rest.trim());
  return parts.join(" ");
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

const blankRule: Rule = {
  id: 0,
  name: "",
  query: "",
  enabled: true,
  scope_mode: "all_accounts",
  account_ids: [],
  actions: { star: false, move_mailbox_id: 0, move_role: "", forward_to: "" },
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

  const accounts = useMemo(() => {
    const seen = new Map<number, string>();
    mailboxes.forEach((mailbox) => {
      if (!seen.has(mailbox.account_id)) {
        seen.set(mailbox.account_id, mailbox.account_label || mailbox.account_email || `Account ${mailbox.account_id}`);
      }
    });
    return [...seen.entries()].map(([id, label]) => ({ id, label }));
  }, [mailboxes]);

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
    const initialQuery = new URLSearchParams(location.search).get("query") || "";
    if (initialQuery) {
      setDraft((current) => ({ ...current, query: initialQuery, name: `Filter: ${initialQuery}` }));
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
      description="Create search-based rules for new and existing mail."
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
              <input type="number" min={0} step={1} value={conditions.olderThanDays} onChange={(event) => setCondition({ olderThanDays: event.target.value })} placeholder="days" />
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
        <div className="mail-filter-grid">
          <label>
            <span className="settings-field-label">Scope</span>
            <select value={draft.scope_mode} onChange={(event) => setDraft({ ...draft, scope_mode: event.target.value })}>
              <option value="all_accounts">All accounts</option>
              <option value="selected_accounts">Selected accounts</option>
            </select>
          </label>
          <label>
            <span className="settings-field-label">Move</span>
            <select value={draft.actions.move_mailbox_id || draft.actions.move_role} onChange={(event) => {
              const value = event.target.value;
              if (value === "trash") setAction({ move_role: "trash", move_mailbox_id: 0 });
              else setAction({ move_role: "", move_mailbox_id: Number(value || 0) });
            }}>
              <option value="">Do not move</option>
              <option value="trash">Source account Trash</option>
              {mailboxes.map((mailbox) => <option value={mailbox.id} key={mailbox.id}>{mailbox.account_label || mailbox.account_email} / {mailbox.name}</option>)}
            </select>
          </label>
        </div>
        {conditions.olderThanDays && draft.actions.move_role === "trash" ? (
          <p className="muted">
            Matching mail moves to Trash {conditions.olderThanDays} {conditions.olderThanDays === "1" ? "day" : "days"} after it was sent. Until then it waits in the queue below.
          </p>
        ) : null}
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
        <div className="mail-filter-actions">
          <label><input type="checkbox" checked={draft.enabled} onChange={(event) => setDraft({ ...draft, enabled: event.target.checked })} /> Enabled</label>
          <label><input type="checkbox" checked={draft.actions.star} onChange={(event) => setAction({ star: event.target.checked })} /> Star matches</label>
          <label className="mail-filter-forward">
            <span className="settings-field-label">Forward to</span>
            <input type="email" value={draft.actions.forward_to} onChange={(event) => setAction({ forward_to: event.target.value })} placeholder="name@example.com" />
          </label>
        </div>
        <div className="form-actions">
          <button disabled={busy || !draft.query.trim()}><Icon name="label" />Save filter</button>
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
        description="Matched mail an age condition has not released yet. Nothing has been moved, starred or forwarded for these."
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

function evaluationDetail(ev: Evaluation, datePrefs: DatePrefs) {
  const parts = [
    ev.phase ? statusLabel(ev.phase) : "",
    ev.evaluated_at ? `evaluated ${displayDateTime(new Date(ev.evaluated_at * 1000).toISOString(), datePrefs)}` : "",
    ev.due_at ? `due ${displayDateTime(new Date(ev.due_at * 1000).toISOString(), datePrefs)}` : "",
    ev.matched ? "matched" : "did not match"
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
      description: "Search-based rules for new and existing mail.",
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
          <div className="muted">Search-based automation for starring, forwarding, moving, and age-based cleanup.</div>
        </div>
        <button className="secondary" type="button" onClick={() => navigate("/settings/account/plugins/filters")}><Icon name="label" />Manage</button>
      </div>
      <button className="server-row" type="button" onClick={() => navigate("/settings/account/plugins/filters")}>
        <span className="server-row-icon"><Icon name="label" /></span>
        <strong>Filters</strong>
        <small>Create filters from searches and review the 30-day match audit.</small>
      </button>
    </section>
  ),
  renderSearchActions: ({ query, navigate }: { query: string; navigate: (url: string) => void }) => (
    <button className="secondary" type="button" onClick={() => navigate(`/settings/account/plugins/filters?query=${encodeURIComponent(query)}`)}>
      <Icon name="label" />Create filter
    </button>
  ),
  renderMessageMenuActions: ({ activePanel, openPanel, closePanel }: MessageActionContext) => (
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
  ),
  renderMessageActionPanels: (context: MessageActionContext) => <MessageFilterEvaluationsPanel {...context} />
} satisfies AccountSettingsRuntimePlugin;
