// File overview: Admin database maintenance page. It surfaces the SQLite files
// behind the mirror with their integrity state, and runs the three maintenance
// operations that previously needed a shell: verify, back up, and repair.
// Verify and back up run in the background on the live server; repair is
// scheduled and runs during the restart it triggers, because a tenant database
// can only be replaced while nothing holds a handle on it.

import { useCallback, useEffect, useRef, useState } from "react";

import { api } from "../../../api";
import { Icon } from "../../../components/Icon";
import { messageFromError } from "../../../lib/errors";
import { displayDateTime, displayLogTimestamp, formatBytes } from "../../../lib/format";
import type { DatePrefs, Toast } from "../../../appTypes";
import type {
  DatabaseOverview,
  DatabaseStatus,
  PostgresPreflightCheck,
  PostgresPreflightReport,
  PostgresSchemaAction,
  PostgresState,
  ServerLogLine
} from "../../../types";

const JOB_POLL_MS = 1500;
const IDLE_POLL_MS = 15000;

// Go zero times arrive as year 1; they mean "never happened", not a date.
function formatTimestamp(value: string | undefined, datePrefs?: DatePrefs): string {
  if (!value) return "";
  const parsed = new Date(value);
  if (Number.isNaN(parsed.getTime()) || parsed.getFullYear() < 1980) return "";
  return displayDateTime(value, datePrefs);
}

function databaseLabel(database: DatabaseStatus): string {
  if (database.scope === "system") return "Installation database";
  return database.email ? `User ${database.user_id} — ${database.email}` : `User ${database.user_id}`;
}

function databaseState(database: DatabaseStatus): { label: string; tone: "ok" | "warn" | "bad" } {
  if (database.missing) return { label: "Missing", tone: "warn" };
  if (database.corrupt) return { label: "Damaged", tone: "bad" };
  if (database.repair_scheduled) return { label: "Repair scheduled", tone: "warn" };
  return { label: "No problems reported", tone: "ok" };
}

// Mirrors databaseState above so the shared .database-state badge keeps one
// mapping convention: status in, {label, tone} out, rendered through the same
// `database-state is-${tone}` template.
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
    try {
      setState(await api.postgresSchema(csrf, dsn, action));
      setDropArmed(false);
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
export function AdminDatabaseView({
  csrf,
  datePrefs,
  addToast
}: {
  csrf: string;
  datePrefs?: DatePrefs;
  addToast: (message: string, kind?: Toast["kind"]) => number;
}) {
  const [overview, setOverview] = useState<DatabaseOverview | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const [confirmRepair, setConfirmRepair] = useState<DatabaseStatus | null>(null);
  const [logLines, setLogLines] = useState<ServerLogLine[] | null>(null);
  const [logError, setLogError] = useState("");
  const [logBusy, setLogBusy] = useState(false);
  const mounted = useRef(true);
  const cancelRef = useRef<HTMLButtonElement>(null);
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

  useEffect(() => {
    mounted.current = true;
    let timer: number | undefined;
    // A running job only changes its own log, so the fast poll asks for the job
    // alone; the full overview walks every backup directory and reads a marker
    // per tenant and stays on the slow cadence.
    const tick = async (jobOnly: boolean) => {
      let running = false;
      if (jobOnly) {
        try {
          const { job: current } = await api.databaseJob();
          if (!mounted.current) return;
          running = Boolean(current?.running);
          setOverview((previous) => (previous ? { ...previous, job: current } : previous));
        } catch {
          running = false;
        }
        if (!running) await load();
      } else {
        const data = await load();
        running = Boolean(data?.job?.running);
      }
      if (!mounted.current) return;
      timer = window.setTimeout(() => void tick(running), running ? JOB_POLL_MS : IDLE_POLL_MS);
    };
    void tick(false);
    return () => {
      mounted.current = false;
      if (timer) window.clearTimeout(timer);
    };
  }, [load]);

  // aria-modal tells assistive technology the page behind the overlay is
  // inert, so focus has to move into the dialog and Escape has to leave it.
  useEffect(() => {
    if (!confirmRepair) return;
    cancelRef.current?.focus();
    function onKeyDown(event: KeyboardEvent) {
      if (event.key === "Escape" && !busy) setConfirmRepair(null);
    }
    window.addEventListener("keydown", onKeyDown);
    return () => window.removeEventListener("keydown", onKeyDown);
  }, [confirmRepair, busy]);

  async function runCheck(scope: string, userID: number) {
    setBusy(true);
    try {
      await api.checkDatabases(csrf, scope, userID);
      addToast(scope ? "Checking this database." : "Checking all databases.");
      await load();
    } catch (err) {
      addToast(messageFromError(err), "error");
    } finally {
      setBusy(false);
    }
  }

  async function runBackup() {
    setBusy(true);
    try {
      await api.backupDatabases(csrf);
      addToast("Backup started.");
      await load();
    } catch (err) {
      addToast(messageFromError(err), "error");
    } finally {
      setBusy(false);
    }
  }

  async function scheduleRepair(database: DatabaseStatus) {
    setBusy(true);
    try {
      const result = await api.scheduleDatabaseRepair(csrf, database.user_id);
      setConfirmRepair(null);
      addToast(
        result.restarting
          ? "Repair scheduled. Rolltop is restarting to run it."
          : "Repair scheduled. It runs the next time Rolltop starts."
      );
      // The server is already going down; a reload here would only surface a
      // fetch error right after the success toast.
      if (!result.restarting) await load();
    } catch (err) {
      addToast(messageFromError(err), "error");
    } finally {
      setBusy(false);
    }
  }

  async function cancelRepair(database: DatabaseStatus) {
    setBusy(true);
    try {
      await api.cancelDatabaseRepair(csrf, database.user_id);
      addToast("Scheduled repair cancelled.");
      await load();
    } catch (err) {
      addToast(messageFromError(err), "error");
    } finally {
      setBusy(false);
    }
  }

  const job = overview?.job || null;
  const jobRunning = Boolean(job?.running);
  const backups = overview?.backups || [];
  const freePercent =
    overview && overview.total_bytes > 0 ? Math.round((overview.free_bytes / overview.total_bytes) * 100) : null;
  const lowDisk = freePercent !== null && freePercent < 10;

  return (
    <section className="settings-page database-admin">
      <header className="settings-page-header">
        <h1>Database</h1>
        <p>
          Verify, back up, and repair the SQLite files behind the local mirror. Verifying and backing up run while
          Rolltop keeps serving. A repair replaces the file, so it is scheduled and runs during a restart.
        </p>
      </header>

      {error ? <p className="settings-error">{error}</p> : null}
      {loading && !overview ? <p className="settings-hint">Loading database status…</p> : null}

      {overview ? (
        <>
          <div className="database-toolbar">
            <button type="button" className="secondary" disabled={busy || jobRunning} onClick={() => void runCheck("", 0)}>
              <Icon name="search" />
              Check all databases
            </button>
            <button type="button" className="secondary" disabled={busy || jobRunning} onClick={() => void runBackup()}>
              <Icon name="archive" />
              Create backup
            </button>
            <span className={`database-disk${lowDisk ? " is-low" : ""}`}>
              {overview.total_bytes > 0
                ? `${formatBytes(overview.free_bytes)} free of ${formatBytes(overview.total_bytes)}${
                    freePercent !== null ? ` (${freePercent}%)` : ""
                  }`
                : "Free space unavailable"}
            </span>
          </div>
          {lowDisk ? (
            <p className="settings-error">
              The data volume is nearly full. Running out of space while SQLite writes is one of the few conditions
              that can actually damage these files.
            </p>
          ) : null}

          <table className="database-table">
            <thead>
              <tr>
                <th>Database</th>
                <th>Size</th>
                <th>Status</th>
                <th aria-label="Actions" />
              </tr>
            </thead>
            <tbody>
              {overview.databases.map((database) => {
                const state = databaseState(database);
                return (
                  <tr key={`${database.scope}-${database.user_id}`}>
                    <td>
                      <strong>{databaseLabel(database)}</strong>
                      <small className="database-path">{database.path}</small>
                    </td>
                    <td>
                      {formatBytes(database.bytes)}
                      {database.wal_bytes > 0 ? <small>WAL {formatBytes(database.wal_bytes)}</small> : null}
                    </td>
                    <td>
                      <span className={`database-state is-${state.tone}`}>{state.label}</span>
                      {database.corrupt_detail ? <small className="database-detail">{database.corrupt_detail}</small> : null}
                      {database.repair_scheduled ? (
                        <small className="database-detail">
                          Requested {formatTimestamp(database.repair_requested_at, datePrefs) || "recently"}; runs at the next start.
                        </small>
                      ) : null}
                      {database.last_repair ? (
                        <small className="database-detail">
                          {database.last_repair.succeeded
                            ? `Last repair ${formatTimestamp(database.last_repair.finished_at, datePrefs) || "recently"}: ${database.last_repair.report.rows_copied} rows recovered, ${database.last_repair.report.rows_skipped} unreadable, ${database.last_repair.report.gaps} damaged range(s).`
                            : `Last repair failed: ${database.last_repair.error || "unknown error"}`}
                        </small>
                      ) : null}
                    </td>
                    <td className="database-actions">
                      <button
                        type="button"
                        className="secondary"
                        disabled={busy || jobRunning || database.missing}
                        onClick={() => void runCheck(database.scope, database.user_id)}
                      >
                        Check
                      </button>
                      {database.scope === "user" && database.repair_scheduled ? (
                        <button type="button" className="secondary" disabled={busy} onClick={() => void cancelRepair(database)}>
                          Cancel repair
                        </button>
                      ) : null}
                      {database.scope === "user" && !database.repair_scheduled ? (
                        <button
                          type="button"
                          className="secondary danger"
                          disabled={busy || jobRunning || database.missing}
                          onClick={() => setConfirmRepair(database)}
                        >
                          Repair…
                        </button>
                      ) : null}
                    </td>
                  </tr>
                );
              })}
            </tbody>
          </table>

          {job ? (
            <div className={`database-job${job.running ? " is-running" : ""}`}>
              <h2>
                {job.kind === "check" ? "Integrity check" : "Backup"} — {job.running ? "running" : "finished"}
              </h2>
              <p className="database-job-detail">{job.detail}</p>
              {job.error ? <p className="settings-error">{job.error}</p> : null}
              {!job.running && job.kind === "check" ? (
                <p className={job.problems > 0 ? "settings-error" : "settings-hint"}>
                  {job.problems > 0
                    ? `${job.problems} problem(s) found. Schedule a repair for each damaged user database.`
                    : "No problems found."}
                </p>
              ) : null}
              {job.log.length ? (
                <pre className="database-job-log">{job.log.join("\n")}</pre>
              ) : null}
            </div>
          ) : null}

          <div className="database-backups">
            <h2>Backups</h2>
            <p className="settings-hint">
              Written to {overview.backup_dir}. Copies are consistent snapshots of the databases only — message
              blobs and the search index are not included. Move them off this volume to survive losing it.
            </p>
            {backups.length ? (
              <ul>
                {backups.map((backup) => (
                  <li key={backup.name}>
                    <strong>{backup.name}</strong>
                    <span>{formatBytes(backup.bytes)}</span>
                    <small>{formatTimestamp(backup.created_at, datePrefs)}</small>
                  </li>
                ))}
              </ul>
            ) : (
              <p className="settings-hint">No backups yet.</p>
            )}
          </div>
        </>
      ) : null}

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

      {confirmRepair ? (
        <div className="database-confirm" role="dialog" aria-modal="true" aria-label="Confirm database repair">
          <div className="database-confirm-panel">
            <h2>Repair {databaseLabel(confirmRepair)}?</h2>
            <p>
              Rolltop copies every readable row into a new database and moves the damaged file aside. Rows on
              damaged pages cannot be recovered: mail is re-downloaded from IMAP on the next sync, but locally
              created state on those pages — contacts, snoozes, identities, pending flag changes — is lost.
            </p>
            <p>
              {overview?.restart_supported
                ? "Rolltop restarts immediately to run the repair. It is unavailable for the length of the restart."
                : "The repair runs the next time Rolltop starts."}
            </p>
            <p className="settings-hint">Create a backup first if you have not already.</p>
            <div className="database-confirm-actions">
              <button ref={cancelRef} type="button" className="secondary" disabled={busy} onClick={() => setConfirmRepair(null)}>
                Cancel
              </button>
              <button type="button" className="secondary danger" disabled={busy} onClick={() => void scheduleRepair(confirmRepair)}>
                Schedule repair and restart
              </button>
            </div>
          </div>
        </div>
      ) : null}
    </section>
  );
}
