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
import type { Toast } from "../../../appTypes";
import type { DatabaseOverview, DatabaseStatus } from "../../../types";

const JOB_POLL_MS = 1500;
const IDLE_POLL_MS = 15000;

function formatBytes(bytes: number): string {
  const value = Number(bytes || 0);
  if (!Number.isFinite(value) || value <= 0) return "0 B";
  const units = ["B", "KB", "MB", "GB", "TB"];
  const exponent = Math.min(units.length - 1, Math.floor(Math.log(value) / Math.log(1024)));
  const scaled = value / Math.pow(1024, exponent);
  return `${scaled >= 100 || exponent === 0 ? Math.round(scaled) : scaled.toFixed(1)} ${units[exponent]}`;
}

function formatTimestamp(value?: string): string {
  if (!value) return "";
  const parsed = new Date(value);
  if (Number.isNaN(parsed.getTime()) || parsed.getFullYear() < 1980) return "";
  return parsed.toLocaleString();
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

/**
 * AdminDatabaseView polls the maintenance overview. Polling is fast while a job
 * runs so its log streams, and slow otherwise so an idle admin tab is cheap.
 */
export function AdminDatabaseView({
  csrf,
  addToast
}: {
  csrf: string;
  addToast: (message: string, kind?: Toast["kind"]) => number;
}) {
  const [overview, setOverview] = useState<DatabaseOverview | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const [confirmRepair, setConfirmRepair] = useState<DatabaseStatus | null>(null);
  const mounted = useRef(true);

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

  useEffect(() => {
    mounted.current = true;
    let timer: number | undefined;
    const tick = async () => {
      const data = await load();
      if (!mounted.current) return;
      const running = Boolean(data?.job?.running);
      timer = window.setTimeout(() => void tick(), running ? JOB_POLL_MS : IDLE_POLL_MS);
    };
    void tick();
    return () => {
      mounted.current = false;
      if (timer) window.clearTimeout(timer);
    };
  }, [load]);

  async function runCheck(userID: number) {
    setBusy(true);
    try {
      await api.checkDatabases(csrf, userID);
      addToast(userID ? "Checking this database." : "Checking all databases.");
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
      await api.backupDatabases(csrf, 0);
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
      await load();
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
            <button type="button" className="secondary" disabled={busy || jobRunning} onClick={() => void runCheck(0)}>
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
                          Requested {formatTimestamp(database.repair_requested_at) || "recently"}; runs at the next start.
                        </small>
                      ) : null}
                      {database.last_repair ? (
                        <small className="database-detail">
                          {database.last_repair.succeeded
                            ? `Last repair ${formatTimestamp(database.last_repair.finished_at)}: ${database.last_repair.report.RowsCopied} rows recovered, ${database.last_repair.report.RowsSkipped} unreadable, ${database.last_repair.report.Gaps} damaged range(s).`
                            : `Last repair failed: ${database.last_repair.error || "unknown error"}`}
                        </small>
                      ) : null}
                    </td>
                    <td className="database-actions">
                      <button
                        type="button"
                        className="secondary"
                        disabled={busy || jobRunning || database.missing}
                        onClick={() => void runCheck(database.scope === "user" ? database.user_id : 0)}
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
                          disabled={busy || database.missing}
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
                    <small>{formatTimestamp(backup.created_at)}</small>
                  </li>
                ))}
              </ul>
            ) : (
              <p className="settings-hint">No backups yet.</p>
            )}
          </div>
        </>
      ) : null}

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
              <button type="button" className="secondary" disabled={busy} onClick={() => setConfirmRepair(null)}>
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
