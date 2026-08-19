/**
 * File overview: The Activity view. Everything Rolltop is doing in the
 * background for the signed-in user, in one place: the folder syncs, the
 * workers behind them, and the Google syncs that keep their own schedule.
 *
 * It exists because none of this belonged where it used to live. A folder sync
 * was only visible from the folder it happened to be touching, a stalled worker
 * was visible only as a counter that would not move, and a Google contact sync
 * that had been failing for a week was three clicks into settings. A reader who
 * suspects something is stuck has one place to look and, for the things that
 * can be stopped, one place to stop them.
 */

import { useCallback, useEffect, useRef, useState } from "react";
import { api } from "../../api";
import type { AddToast, DatePrefs } from "../../appTypes";
import type { Activity, ActivityService, ActivityWorker, SyncRun } from "../../types";
import { Icon } from "../../components/Icon";
import { displayDateTime } from "../../lib/format";
import { messageFromError } from "../../lib/errors";

/**
 * The fallback poll. The event stream is the real change signal -- sync
 * progress updates the chrome props, and this view refreshes when they change --
 * so this only covers worker phases that change without a chrome event.
 */
const refreshIntervalMS = 10000;

export function ActivityView({
  csrf,
  datePrefs,
  activeSyncRuns,
  mailGeneration,
  navigate,
  addToast
}: {
  csrf: string;
  datePrefs: DatePrefs;
  /** SSE-fed run list from the chrome payload; a change here means new state to fetch. */
  activeSyncRuns: SyncRun[];
  /** Bumps when a sync stores messages, which is when worker phases move. */
  mailGeneration: number;
  navigate: (url: string) => void;
  addToast: AddToast;
}) {
  const [activity, setActivity] = useState<Activity | null>(null);
  const [error, setError] = useState("");
  const [busyKeys, setBusyKeys] = useState<Set<string>>(() => new Set());
  // A slow poll answer must not overwrite the fresh state a cancel just
  // fetched, so every response is checked against the newest request.
  const refreshSequence = useRef(0);

  const refresh = useCallback(async () => {
    const sequence = ++refreshSequence.current;
    try {
      const next = await api.activity();
      if (refreshSequence.current !== sequence) return;
      setActivity(next);
      setError("");
    } catch (err) {
      if (refreshSequence.current !== sequence) return;
      setError(messageFromError(err));
    }
  }, []);

  // The event stream already tells this browser when sync state changes, via
  // the chrome props above. They trigger a fetch; the interval is only the
  // fallback for changes no chrome event announces, and it stays quiet while
  // the tab is hidden.
  useEffect(() => {
    void refresh();
  }, [refresh, activeSyncRuns, mailGeneration]);

  useEffect(() => {
    const timer = window.setInterval(() => {
      if (!document.hidden) void refresh();
    }, refreshIntervalMS);
    return () => window.clearInterval(timer);
  }, [refresh]);

  // One busy set for every row action: a row is disabled while its own request
  // is in flight, so the poll that lands meanwhile cannot re-enable a button the
  // user has already pressed.
  const withBusy = useCallback(async (key: string, action: () => Promise<void>) => {
    setBusyKeys((current) => new Set([...current, key]));
    try {
      await action();
      await refresh();
    } catch (err) {
      addToast(messageFromError(err), "error");
    } finally {
      setBusyKeys((current) => {
        const next = new Set(current);
        next.delete(key);
        return next;
      });
    }
  }, [addToast, refresh]);

  const runs = activity?.sync_runs || [];
  const activeRuns = runs.filter((run) => run.status === "running");
  const pastRuns = runs.filter((run) => run.status !== "running");
  const workers = activity?.workers || [];
  const services = activity?.services || [];
  const categoriesPending = activity?.categories_pending || 0;
  const searchIndexPending = activity?.search_index_pending || 0;
  // One folder sync is a run row plus its runner-side worker rows, so summing
  // rows would count the same operation three times. The pill counts the runs
  // and only the workers that are not the runner's view of one.
  const independentWorkers = workers.filter(
    (worker) => !worker.waiting && worker.kind !== "mailbox_sync" && worker.kind !== "account_wide_sync"
  );
  const runningCount = activeRuns.length + independentWorkers.length;
  // idle is only knowable after the first answer: everything defaults to empty
  // before it, and "Nothing is running" over a busy server is the one flash
  // this view must never show.
  const idle =
    activity !== null &&
    activeRuns.length === 0 &&
    workers.length === 0 &&
    categoriesPending === 0 &&
    searchIndexPending === 0;

  return (
    <>
      <div className="content-head">
        <div>
          <h1>Activity</h1>
          <span className="label-pill">{runningCount.toLocaleString()} running</span>
        </div>
        <div className="activity-actions">
          <button className="secondary" type="button" onClick={() => void refresh()}>
            <Icon name="sync" />
            Refresh
          </button>
          <button
            className="secondary"
            type="button"
            disabled={pastRuns.length === 0 || busyKeys.has("history")}
            title="Remove every finished run from this list"
            onClick={() => void withBusy("history", async () => {
              const result = await api.clearSyncHistory(csrf);
              addToast(`Cleared ${result.removed.toLocaleString()} finished ${result.removed === 1 ? "run" : "runs"}.`);
            })}
          >
            <Icon name="delete" />
            Clear history
          </button>
        </div>
      </div>

      {error ? <div className="error">{error}</div> : null}

      {idle && !error ? (
        <section className="panel activity-idle">
          <Icon name="check" />
          <div>
            <strong>Nothing is running</strong>
            <p>Mail syncs, background workers and Google syncs all appear here while they work.</p>
          </div>
        </section>
      ) : null}

      {activeRuns.length > 0 ? (
        <section className="panel activity-section">
          <h2>Mail syncs</h2>
          <div className="activity-rows">
            {activeRuns.map((run) => (
              <ActivityRunRow
                key={run.id}
                run={run}
                datePrefs={datePrefs}
                busy={busyKeys.has(`run:${run.id}`)}
                navigate={navigate}
                onCancel={() => void withBusy(`run:${run.id}`, async () => {
                  await api.cancelSyncRun(csrf, run.id);
                  addToast("Sync run cancelled.");
                })}
              />
            ))}
          </div>
        </section>
      ) : null}

      {workers.length > 0 || categoriesPending > 0 || searchIndexPending > 0 ? (
        <section className="panel activity-section">
          <h2>Background workers</h2>
          {categoriesPending > 0 ? (
            <div className="activity-note">
              <Icon name="label" />
              <span>
                {categoriesPending.toLocaleString()} {categoriesPending === 1 ? "message is" : "messages are"} still
                waiting to be sorted into categories.
              </span>
            </div>
          ) : null}
          {/* Not work in flight, which is why it is a note rather than a row:
              these folders are indexed by the next sync that reaches them, and
              until then search answers without the mail they hold. */}
          {searchIndexPending > 0 ? (
            <div className="activity-note">
              <Icon name="search" />
              <span>
                {searchIndexPending.toLocaleString()} {searchIndexPending === 1 ? "folder is" : "folders are"} waiting
                to be added to the search index. Syncing fills them in; Settings &rsaquo; Storage rebuilds now.
              </span>
            </div>
          ) : null}
          <div className="activity-rows">
            {workers.map((worker) => (
              <ActivityWorkerRow
                key={worker.key}
                worker={worker}
                datePrefs={datePrefs}
                busy={busyKeys.has(worker.key)}
                onCancel={() => void withBusy(worker.key, async () => {
                  await api.cancelWorker(csrf, worker.key);
                  addToast("Background task stopped.");
                })}
              />
            ))}
          </div>
        </section>
      ) : null}

      {services.length > 0 ? (
        <section className="panel activity-section">
          <h2>Connected services</h2>
          <div className="activity-rows">
            {services.map((service) => (
              <ActivityServiceRow key={`${service.kind}:${service.connection_id}`} service={service} datePrefs={datePrefs} />
            ))}
          </div>
        </section>
      ) : null}

      {pastRuns.length > 0 ? (
        <section className="panel activity-section">
          <h2>Recently finished</h2>
          <div className="activity-rows">
            {pastRuns.map((run) => (
              <ActivityRunRow
                key={run.id}
                run={run}
                datePrefs={datePrefs}
                busy={busyKeys.has(`run:${run.id}`)}
                navigate={navigate}
                onDelete={() => void withBusy(`run:${run.id}`, () => api.deleteSyncRun(csrf, run.id).then(() => undefined))}
              />
            ))}
          </div>
        </section>
      ) : null}
    </>
  );
}

function ActivityRunRow({
  run,
  datePrefs,
  busy,
  navigate,
  onCancel,
  onDelete
}: {
  run: SyncRun;
  datePrefs: DatePrefs;
  busy: boolean;
  navigate: (url: string) => void;
  onCancel?: () => void;
  onDelete?: () => void;
}) {
  const running = run.status === "running";
  return (
    <div className={`activity-row ${running ? "running" : ""}`}>
      <span className={`activity-state ${running ? "running" : run.status === "ok" ? "ok" : "error"}`}>
        <Icon name={running ? "sync" : run.status === "ok" ? "check" : "error"} />
      </span>
      <div className="activity-row-copy">
        <strong>{run.current_mailbox || "Account-wide check"}</strong>
        <small>
          {run.messages_stored.toLocaleString()} processed · {run.messages_skipped.toLocaleString()} skipped ·{" "}
          {displayDateTime(run.updated_at, datePrefs)}
        </small>
        {run.error ? <small className="activity-row-error">{run.error}</small> : null}
      </div>
      <div className="activity-row-actions">
        <button className="ghost text-link" type="button" onClick={() => navigate(`/sync-runs/${run.id}`)}>Details</button>
        {onCancel ? (
          <button className="secondary" type="button" disabled={busy} onClick={onCancel}>
            {busy ? "Stopping..." : "Cancel"}
          </button>
        ) : null}
        {onDelete ? (
          <button className="ghost" type="button" title="Remove from this list" aria-label="Remove from this list" disabled={busy} onClick={onDelete}>
            <Icon name="delete" />
          </button>
        ) : null}
      </div>
    </div>
  );
}

function ActivityWorkerRow({
  worker,
  datePrefs,
  busy,
  onCancel
}: {
  worker: ActivityWorker;
  datePrefs: DatePrefs;
  busy: boolean;
  onCancel: () => void;
}) {
  const where = [worker.mailbox, worker.phase].filter(Boolean).join(" · ");
  return (
    <div className={`activity-row ${worker.waiting ? "waiting" : "running"}`}>
      <span className={`activity-state ${worker.waiting ? "waiting" : "running"}`}>
        <Icon name={worker.waiting ? "clock" : "sync"} />
      </span>
      <div className="activity-row-copy">
        <strong>{worker.label}</strong>
        <small>
          {worker.waiting ? "Waiting for a turn" : where || "Working"}
          {worker.started_at ? ` · since ${displayDateTime(worker.started_at, datePrefs)}` : ""}
        </small>
      </div>
      <div className="activity-row-actions">
        {worker.cancellable ? (
          <button
            className="secondary"
            type="button"
            title="Stops the sync turn this task belongs to; its own scheduling decides when it comes back"
            disabled={busy}
            onClick={onCancel}
          >
            {busy ? "Stopping..." : "Stop"}
          </button>
        ) : (
          <span className="muted activity-row-hint">{worker.waiting ? "Queued" : "Cannot be stopped"}</span>
        )}
      </div>
    </div>
  );
}

function ActivityServiceRow({ service, datePrefs }: { service: ActivityService; datePrefs: DatePrefs }) {
  const failed = service.status !== "" && service.status !== "ok";
  return (
    <div className={`activity-row ${failed ? "failed" : ""}`}>
      <span className={`activity-state ${failed ? "error" : "ok"}`}>
        <Icon name={failed ? "error" : "check"} />
      </span>
      <div className="activity-row-copy">
        <strong>{service.label} · {service.account}</strong>
        <small>
          {service.ever_synced
            ? `${service.item_count.toLocaleString()} synced · last success ${displayDateTime(service.last_success_at, datePrefs)}`
            : "Has never completed a sync"}
        </small>
        {failed && service.status_detail ? <small className="activity-row-error">{service.status_detail}</small> : null}
      </div>
    </div>
  );
}
