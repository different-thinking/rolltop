// File overview: Admin database page. It reports where the mirror is stored and
// how that storage is doing — the PostgreSQL connection and its size, and the
// data volume that still holds the blobs and the search index.
//
// The three operations it used to offer (verify, back up, repair) were all
// SQLite maintenance and went with it: integrity is the database server's
// problem now, and backups are `pg_dump` on a schedule rather than a button
// that writes into the volume it is protecting against.
//
// Two cards below still act, each deliberately its own. The search index card
// rebuilds one tenant's index, which is derived state on the volume that can
// break on its own and costs nothing to throw away; taking that with the SQLite
// buttons left an operator without a shell no way to repair search at all. The
// PostgreSQL migration console creates and drops a schema.

import { useCallback, useEffect, useRef, useState } from "react";

import { api } from "../../../api";
import { Icon } from "../../../components/Icon";
import { messageFromError } from "../../../lib/errors";
import { displayDateTime, displayLogTimestamp, formatBytes } from "../../../lib/format";
import type { DatePrefs } from "../../../appTypes";
import type {
  DatabaseOverview,
  DatabaseStatus,
  PostgresPreflightCheck,
  PostgresPreflightReport,
  PostgresSchemaAction,
  PostgresState,
  SearchIndexTenant,
  ServerLogLine
} from "../../../types";

const IDLE_POLL_MS = 15000;

// databaseLabel and databaseTone follow the same convention as preflightState
// and stageState below: status in, a label and a tone out, rendered through the
// shared `database-state is-${tone}` badge.
function databaseLabel(database: DatabaseStatus): string {
  if (!database.reachable) return "Unreachable";
  if (database.in_recovery) return "Read-only replica";
  return "Connected";
}

function databaseTone(database: DatabaseStatus): "ok" | "warn" | "bad" {
  if (!database.reachable) return "bad";
  if (database.in_recovery) return "warn";
  return "ok";
}

function preflightState(status: PostgresPreflightCheck["status"]): { label: string; tone: "ok" | "warn" | "bad" } {
  if (status === "pass") return { label: "Pass", tone: "ok" };
  if (status === "fail") return { label: "Fail", tone: "bad" };
  return { label: "Info", tone: "warn" };
}

// Mirrors databaseState and preflightState so all three keep one convention.
function stageState(stage: PostgresState["stage"]): { label: string; tone: "ok" | "warn" | "bad" } {
  if (stage === "empty") return { label: "Empty", tone: "ok" };
  if (stage === "baseline") return { label: "Schema present", tone: "ok" };
  if (stage === "mismatch") return { label: "Other build", tone: "warn" };
  return { label: "Not ours", tone: "bad" };
}

/**
 * PostgresMigrationCard is the staged console for the SQLite-to-PostgreSQL
 * migration. Each step is run and checked on its own against the real target,
 * so the migration is rehearsed rather than attempted once:
 *
 *   1. Preflight — can this database do what the port needs?
 *   2. Schema — create the generated schema here, look at it, drop it, repeat.
 *
 * The connection string is entered once, shared by both steps, held in
 * component state for the request only, and sent nowhere else; the server
 * neither stores nor logs it, and redacts it out of driver errors.
 */
// SearchIndexCard is the one repair this page still offers, and the reason it
// exists: the search index is derived state on the data volume, it can be
// damaged on its own — an incomplete volume copy leaves one that no longer
// opens — and rebuilding it costs only the time to reindex, because the mail it
// describes is untouched in PostgreSQL. The offline `rolltop reset-search`
// command does the same thing, which is no help to anyone running this as a
// container without a shell.
//
// The server heals an index it finds unreadable on its own. This card is for
// the cases it cannot see: search that returns too little, an index left behind
// by a restore, a rebuild an operator simply wants to force.
function SearchIndexCard({ csrf }: { csrf: string }) {
  const [tenants, setTenants] = useState<SearchIndexTenant[] | null>(null);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState("");
  const [busyUser, setBusyUser] = useState(0);
  // Rebuilding is armed by a first click and performed by a second: it throws
  // an index away and re-reads a whole mailbox to build the next one.
  const [armedUser, setArmedUser] = useState(0);
  const [queued, setQueued] = useState<{ runs: number; busy: number } | null>(null);

  const load = useCallback(async () => {
    setLoading(true);
    setError("");
    try {
      const report = await api.searchIndex();
      setTenants(report.tenants);
    } catch (err) {
      setError(messageFromError(err));
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    void load();
  }, [load]);

  async function rebuild(userID: number) {
    setBusyUser(userID);
    setError("");
    setQueued(null);
    try {
      const report = await api.rebuildSearchIndex(csrf, userID);
      setTenants(report.tenants);
      setQueued({ runs: report.started_runs || 0, busy: report.busy_accounts || 0 });
    } catch (err) {
      setError(messageFromError(err));
    } finally {
      setBusyUser(0);
      setArmedUser(0);
    }
  }

  const armedTenant = tenants?.find((tenant) => tenant.user_id === armedUser) || null;

  return (
    <div className="database-log">
      <h2>Search index</h2>
      <p className="settings-hint">
        Each user has their own search index on the data volume. It is built from mail that is already stored, so
        rebuilding one loses nothing — search is incomplete for that user until the reindex finishes, and nothing
        else is affected. Rebuilding queues one run per mail server, the same work the folder settings offer per
        account. Rebuild when search is missing mail you know is there, when folders below report that they need
        it, or after restoring the volume from a copy.
      </p>
      <div className="database-log-actions">
        <button type="button" className="secondary" disabled={loading} onClick={() => void load()}>
          <Icon name="sync" />
          Reload
        </button>
      </div>
      {error ? <p className="settings-error">{error}</p> : null}
      {queued ? (
        <p className="settings-hint">
          Rebuilding. {queued.runs} mail server{queued.runs === 1 ? "" : "s"} started reindexing — follow it in
          Activity.
          {queued.busy > 0
            ? ` ${queued.busy} more ${queued.busy === 1 ? "was" : "were"} already syncing; try those again once it finishes.`
            : ""}
        </p>
      ) : null}
      {/* The armed state changes only a button label, which is not enough to
          describe what the second click does — and nothing at all for a screen
          reader. The sibling card names its target and consequence the same
          way. */}
      <div aria-live="polite">
        {armedTenant ? (
          <p className="settings-hint">
            This rebuilds the search index for <strong>{armedTenant.email}</strong>
            {armedTenant.present ? ` and discards the ${formatBytes(armedTenant.bytes)} it holds today` : ""}. Their
            mail is untouched, but their search stays incomplete until the reindex finishes. Click again to confirm.
          </p>
        ) : null}
      </div>
      {loading && !tenants ? <p className="settings-hint">Loading search index status…</p> : null}
      {tenants && tenants.length > 0 ? (
        <table className="database-table">
          <tbody>
            {tenants.map((tenant) => (
              <tr key={tenant.user_id}>
                <td>
                  <strong>{tenant.email}</strong>
                  {tenant.name ? <small className="database-path">{tenant.name}</small> : null}
                </td>
                <td>
                  {tenant.present ? formatBytes(tenant.bytes) : "No index yet"}
                  {tenant.error ? (
                    <small className="database-detail">{tenant.error}</small>
                  ) : (
                    <small>
                      {tenant.folders_needing_rebuild > 0
                        ? `${tenant.folders_needing_rebuild} folder${
                            tenant.folders_needing_rebuild === 1 ? "" : "s"
                          } need rebuilding`
                        : "Coverage verified"}
                    </small>
                  )}
                </td>
                <td>
                  {armedUser === tenant.user_id ? (
                    <button
                      type="button"
                      className="danger"
                      disabled={busyUser !== 0}
                      onClick={() => void rebuild(tenant.user_id)}
                    >
                      {busyUser === tenant.user_id ? "Starting…" : "Confirm rebuild"}
                    </button>
                  ) : (
                    <button
                      type="button"
                      className="secondary"
                      disabled={busyUser !== 0}
                      onClick={() => setArmedUser(tenant.user_id)}
                    >
                      Rebuild
                    </button>
                  )}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      ) : null}
      {tenants && tenants.length === 0 ? <p className="settings-hint">No users yet.</p> : null}
    </div>
  );
}

function PostgresMigrationCard({ csrf, datePrefs }: { csrf: string; datePrefs?: DatePrefs }) {
  const [dsn, setDsn] = useState("");
  const [running, setRunning] = useState<"" | "preflight" | PostgresSchemaAction>("");
  const [report, setReport] = useState<PostgresPreflightReport | null>(null);
  const [state, setState] = useState<PostgresState | null>(null);
  const [error, setError] = useState("");
  // The drop is the one irreversible step, so it is armed by a first click and
  // performed by a second. Any other action disarms it.
  const [dropArmed, setDropArmed] = useState(false);

  const busy = running !== "";
  const ready = Boolean(dsn.trim());

  // Everything shown describes the database the results came from. Editing the
  // connection string makes all of it stale at once, and leaving it on screen
  // is not merely untidy: the drop confirmation names a database and a row
  // count, so a stale one would describe the old target while the drop ran
  // against the new one.
  function changeDsn(next: string) {
    setDsn(next);
    setReport(null);
    setState(null);
    setError("");
    setDropArmed(false);
  }

  async function runPreflight() {
    setRunning("preflight");
    setError("");
    setDropArmed(false);
    try {
      setReport(await api.postgresPreflight(csrf, dsn));
    } catch (err) {
      setReport(null);
      setError(messageFromError(err));
    } finally {
      setRunning("");
    }
  }

  async function runSchema(action: PostgresSchemaAction) {
    setRunning(action);
    setError("");
    // Disarmed when the action starts, not when it succeeds. A failed inspect
    // or create left the drop armed, so the next single click performed it.
    setDropArmed(false);
    try {
      setState(await api.postgresSchema(csrf, dsn, action));
    } catch (err) {
      // The previous state is kept: a refused action means nothing changed, and
      // blanking the panel would hide what the refusal was about.
      setError(messageFromError(err));
    } finally {
      setRunning("");
    }
  }

  return (
    <div className="database-log postgres-preflight">
      <h2>PostgreSQL migration</h2>
      <p className="settings-hint">
        The staged path to PostgreSQL, run against the real target from inside this server — so the network
        path, the server&rsquo;s locale, and the role&rsquo;s privileges are the ones the migration will
        actually meet. The connection string is used for the one request and is not stored or logged. One
        action runs at a time.
      </p>
      <form
        className="database-log-actions"
        onSubmit={(event) => {
          event.preventDefault();
          void runPreflight();
        }}
      >
        <input
          type="password"
          value={dsn}
          onChange={(event) => changeDsn(event.target.value)}
          placeholder="postgres://user:password@host:5432/dbname"
          autoComplete="off"
          aria-label="PostgreSQL connection string"
          disabled={busy}
          style={{ flex: "1 1 24rem" }}
        />
      </form>
      {error ? <p className="settings-error">{error}</p> : null}

      <h3>Step 1 — Preflight</h3>
      <p className="settings-hint">
        Checks version and encoding, collation behavior, the extensions, UTF-8 strictness, the SQL features the
        port relies on, and the round-trip latency of this exact path. Changes nothing but the extensions,
        which the migration wants anyway.
      </p>
      <div className="database-log-actions">
        <button type="button" className="secondary" disabled={busy || !ready} onClick={() => void runPreflight()}>
          <Icon name="search" />
          {running === "preflight" ? "Running checks…" : "Run preflight"}
        </button>
      </div>
      {report ? (
        <>
          <p className={report.ok ? "settings-hint" : "settings-error"}>
            {report.ok
              ? `All checks passed in ${report.duration_ms} ms. This database is ready for the migration.`
              : "At least one check failed. This database is not ready for the migration."}
          </p>
          <table className="database-table">
            <tbody>
              {report.checks.map((check) => (
                <tr key={check.id}>
                  <td>
                    <span className={`database-state is-${preflightState(check.status).tone}`}>
                      {preflightState(check.status).label}
                    </span>
                  </td>
                  <td>
                    {check.title}
                    {check.detail ? <small className="database-detail">{check.detail}</small> : null}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </>
      ) : null}

      <h3>Step 2 — Schema</h3>
      <p className="settings-hint">
        Creates the generated schema — every table, index, foreign key and trigger the port needs — in an empty
        database, so it is proven against the real server before any data moves. Drop it and create it again as
        often as you like. A database holding anything that is not Rolltop&rsquo;s is refused outright, and the
        drop leaves installed extensions in place.
      </p>
      <div className="database-log-actions">
        <button type="button" className="secondary" disabled={busy || !ready} onClick={() => void runSchema("inspect")}>
          <Icon name="sync" />
          {running === "inspect" ? "Checking…" : "Check database"}
        </button>
        <button
          type="button"
          className="secondary"
          disabled={busy || !ready || !state?.can_create}
          onClick={() => void runSchema("create")}
        >
          <Icon name="add" />
          {running === "create" ? "Creating schema…" : "Create schema"}
        </button>
        <button
          type="button"
          className={dropArmed ? "danger" : "secondary"}
          disabled={busy || !ready || !state?.can_drop}
          onClick={() => {
            if (!dropArmed) {
              setDropArmed(true);
              return;
            }
            void runSchema("drop");
          }}
        >
          <Icon name="trash" />
          {running === "drop" ? "Dropping schema…" : dropArmed ? "Confirm: drop the schema" : "Drop schema"}
        </button>
      </div>
      {dropArmed && state ? (
        <p className="settings-error">
          This drops every Rolltop table in <strong>{state.database}</strong>
          {state.rows > 0 ? ` along with ${state.rows.toLocaleString()} rows of data` : " (no data rows)"}. It
          cannot be undone. Click again to confirm.
        </p>
      ) : null}
      {state ? (
        <table className="database-table">
          <tbody>
            <tr>
              <td>
                <span className={`database-state is-${stageState(state.stage).tone}`}>
                  {stageState(state.stage).label}
                </span>
              </td>
              <td>
                {state.summary}
                <small className="database-detail">
                  {state.database} as {state.user} — {state.server_version}
                </small>
                {state.blocking?.length ? (
                  <small className="database-detail">
                    Remove these by hand to create the schema here: {state.blocking.join(", ")}
                  </small>
                ) : null}
                {state.applied_at > 0 ? (
                  <small className="database-detail">
                    Schema created {displayDateTime(new Date(state.applied_at * 1000).toISOString(), datePrefs)}
                  </small>
                ) : null}
              </td>
            </tr>
          </tbody>
        </table>
      ) : null}
    </div>
  );
}

/**
 * AdminDatabaseView polls the maintenance overview. Polling is fast while a job
 * runs so its log streams, and slow otherwise so an idle admin tab is cheap.
 */
export function AdminDatabaseView({ csrf, datePrefs }: { csrf: string; datePrefs?: DatePrefs }) {
  const [overview, setOverview] = useState<DatabaseOverview | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const [logLines, setLogLines] = useState<ServerLogLine[] | null>(null);
  const [logError, setLogError] = useState("");
  const [logBusy, setLogBusy] = useState(false);
  const mounted = useRef(true);
  const logListRef = useRef<HTMLOListElement>(null);

  const load = useCallback(async () => {
    try {
      const data = await api.database();
      if (!mounted.current) return data;
      setOverview(data);
      setError("");
      return data;
    } catch (err) {
      if (mounted.current) setError(messageFromError(err));
      return null;
    } finally {
      if (mounted.current) setLoading(false);
    }
  }, []);

  // The tail is loaded on demand rather than polled: it is read when someone is
  // chasing a failure they just reproduced, and the rest of the time it would
  // only be an admin tab holding a connection open for nothing.
  const loadLog = useCallback(async () => {
    setLogBusy(true);
    try {
      const { lines } = await api.serverLog();
      if (!mounted.current) return;
      setLogLines(lines || []);
      setLogError("");
    } catch (err) {
      if (mounted.current) setLogError(messageFromError(err));
    } finally {
      if (mounted.current) setLogBusy(false);
    }
  }, []);

  // Lines read oldest first, so the failure someone just reproduced is the last
  // row in a box that shows a fraction of them. Land on it rather than making
  // every load start with a scroll to the bottom.
  useEffect(() => {
    const list = logListRef.current;
    if (!list || !logLines?.length) return;
    list.scrollTop = list.scrollHeight;
  }, [logLines]);

  // There is no job to follow any more, so this is a plain refresh. The status
  // query is one round trip against the database, which is also the number the
  // card reports.
  useEffect(() => {
    mounted.current = true;
    let timer: number | undefined;
    const tick = async () => {
      await load();
      if (!mounted.current) return;
      timer = window.setTimeout(() => void tick(), IDLE_POLL_MS);
    };
    void tick();
    return () => {
      mounted.current = false;
      if (timer) window.clearTimeout(timer);
    };
  }, [load]);

  const database = overview?.database || null;
  const volume = overview?.volume || null;
  const freePercent =
    volume && volume.total_bytes > 0 ? Math.round((volume.free_bytes / volume.total_bytes) * 100) : null;
  const lowDisk = freePercent !== null && freePercent < 10;

  return (
    <section className="settings-page database-admin">
      <header className="settings-page-header">
        <h1>Database</h1>
        <p>
          Where the mirror is stored and how it is doing. The mail metadata lives in PostgreSQL; the message
          blobs and the search indexes are still on this server&rsquo;s data volume.
        </p>
      </header>

      {error ? <p className="settings-error">{error}</p> : null}
      {loading && !overview ? <p className="settings-hint">Loading database status…</p> : null}

      {database ? (
        <table className="database-table">
          <tbody>
            <tr>
              <td>
                <strong>PostgreSQL</strong>
                <small className="database-path">{database.target}</small>
              </td>
              <td>
                {database.reachable ? formatBytes(database.bytes) : "—"}
                {database.reachable ? (
                  <small>{database.latency_millis.toFixed(2)} ms round trip</small>
                ) : null}
              </td>
              <td>
                <span className={`database-state is-${databaseTone(database)}`}>{databaseLabel(database)}</span>
                {database.error ? <small className="database-detail">{database.error}</small> : null}
                {database.reachable ? (
                  <small className="database-detail">
                    {database.server_version}
                    {" · "}
                    {database.connections} of {database.pool_max_conns} pooled connections in use
                  </small>
                ) : null}
              </td>
            </tr>
          </tbody>
        </table>
      ) : null}

      {volume ? (
        <>
          <div className="database-toolbar">
            <span className={`database-disk${lowDisk ? " is-low" : ""}`}>
              {volume.total_bytes > 0
                ? `${formatBytes(volume.free_bytes)} free of ${formatBytes(volume.total_bytes)}${
                    freePercent !== null ? ` (${freePercent}%)` : ""
                  }`
                : "Free space unavailable"}
            </span>
            <span className="database-disk">
              {volume.measured_at_unix > 0 ? (
                <>
                  {formatBytes(volume.blob_bytes)} message blobs · {formatBytes(volume.index_bytes)} search index
                  {volume.other_bytes > 0 ? ` · ${formatBytes(volume.other_bytes)} other` : ""}
                  {" · measured "}
                  {displayDateTime(new Date(volume.measured_at_unix * 1000).toISOString(), datePrefs)}
                </>
              ) : (
                "Measuring what the volume holds…"
              )}
            </span>
          </div>
          {lowDisk ? (
            <p className="settings-error">
              The data volume is nearly full. Message blobs and the search index are written to it, and both stop
              when it fills.
            </p>
          ) : null}
          <p className="settings-hint">
            Backups are taken with <code>pg_dump</code> against the connection string above, on whatever schedule
            you run it — see the README. There is no backup button here on purpose: writing a copy into the same
            volume it is meant to protect against is not a backup.
          </p>
        </>
      ) : null}

      <SearchIndexCard csrf={csrf} />

      <PostgresMigrationCard csrf={csrf} datePrefs={datePrefs} />

      {/* Outside the overview guard on purpose. When the overview itself fails
          — a system database that cannot even list its users — the tail is the
          only thing left that can say why, so it must still render. */}
      <div className="database-log">
        <h2>Server log</h2>
        <p className="settings-hint">
          The newest lines this process wrote, kept in memory. A request that answers 500 says only
          &ldquo;internal server error&rdquo; in the browser; the line naming the actual failure is here.
          Reproduce the problem, then load the tail. It is cleared on restart.
        </p>
        <div className="database-log-actions">
          <button type="button" className="secondary" disabled={logBusy} onClick={() => void loadLog()}>
            <Icon name="sync" />
            {logLines ? "Reload log" : "Load log"}
          </button>
        </div>
        {logError ? <p className="settings-error">{logError}</p> : null}
        {logLines && logLines.length === 0 ? <p className="settings-hint">Nothing logged yet.</p> : null}
        {logLines && logLines.length > 0 ? (
          <ol className="database-log-lines" ref={logListRef}>
            {logLines.map((line, index) => (
              <li key={`${line.time}-${index}`} className={line.error ? "is-error" : undefined}>
                <time dateTime={line.time}>{displayLogTimestamp(line.time, datePrefs) || line.time}</time>
                <pre>{line.message}</pre>
              </li>
            ))}
          </ol>
        ) : null}
      </div>

    </section>
  );
}
