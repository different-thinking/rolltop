// File overview: Settings section for connected Microsoft 365 accounts. Like the
// Google section, connecting is a full-page navigation into Microsoft's sign-in,
// so the outcome is read back from the callback's query string.

import { useCallback, useEffect, useState } from "react";
import { deleteJSON, getJSON, postJSON } from "../../api";
import { Icon } from "../../components/Icon";
import type { Toast } from "../../appTypes";
import { messageFromError } from "../../lib/errors";
import { SettingsEmpty, SettingsError, SettingsLoading } from "./SettingsUI";

/** MicrosoftCalendarSync is the state of one connection's calendar sync. */
type MicrosoftCalendarSync = {
  status: string;
  status_detail: string;
  last_sync_at: string;
  last_success_at: string;
  calendar_count: number;
  ever_synced: boolean;
};

/** MicrosoftConnection is one Microsoft account this user has authorized. */
type MicrosoftConnection = {
  id: number;
  email: string;
  display_name: string;
  scopes: string[];
  needs_reauth: boolean;
  has_calendar_scope: boolean;
  calendar_sync: MicrosoftCalendarSync | null;
};

type MicrosoftConnectionsResponse = {
  configured: boolean;
  connections: MicrosoftConnection[];
};

const settingsPath = "/settings/account/microsoft";

// The callback redirects here with either connected=<email> or error=<code>.
function messageFromCallback(search: string): { text: string; kind: Toast["kind"] } | null {
  const params = new URLSearchParams(search);
  const connected = params.get("connected");
  if (connected) return { text: `Connected ${connected}. Its calendars are being synced.`, kind: "success" };
  const error = params.get("error");
  if (!error) return null;
  if (error === "access_denied") return { text: "Microsoft sign-in was cancelled.", kind: "error" };
  if (error === "consent_required" || error === "interaction_required")
    return { text: "Microsoft needs consent for Rolltop first. An administrator of your organisation may have to approve it.", kind: "error" };
  if (error === "expired") return { text: "That Microsoft sign-in took too long. Try connecting again.", kind: "error" };
  if (error === "invalid_response") return { text: "Microsoft returned an incomplete response. Try connecting again.", kind: "error" };
  if (error === "unavailable") return { text: "Microsoft 365 is not available on this server right now.", kind: "error" };
  return { text: "Connecting the Microsoft account failed. Try again.", kind: "error" };
}

/** CalendarSyncLine reports what the calendar sync is doing for one account. */
function CalendarSyncLine({ connection }: { connection: MicrosoftConnection }) {
  if (!connection.has_calendar_scope) {
    return <small className="muted">Calendars are not synced. Sign in again and allow calendar access.</small>;
  }
  const sync = connection.calendar_sync;
  // A failure comes first: an organisation that blocks the app fails every
  // sync, and "not synced yet" would hide the one line that says so.
  if (sync && sync.status === "error") {
    return <small className="settings-status-error">{sync.status_detail || "The last calendar sync failed."}</small>;
  }
  if (!sync || !sync.ever_synced) {
    return <small className="muted">Calendars have not been synced yet.</small>;
  }
  const count = `${sync.calendar_count.toLocaleString()} calendar${sync.calendar_count === 1 ? "" : "s"}`;
  return <small className="muted">{count} synced from Microsoft 365. Last sync {formatSyncTime(sync.last_success_at)}.</small>;
}

function formatSyncTime(value: string): string {
  const parsed = new Date(value);
  if (!value || Number.isNaN(parsed.getTime())) return "at an unknown time";
  return parsed.toLocaleString();
}

/** MicrosoftAccountsSettings lists Microsoft connections and manages them. */
export function MicrosoftAccountsSettings({
  csrf,
  search,
  replaceRoute,
  addToast
}: {
  csrf: string;
  search: string;
  replaceRoute: (url: string) => void;
  addToast: (message: string, kind?: Toast["kind"]) => number;
}) {
  const [configured, setConfigured] = useState(true);
  const [connections, setConnections] = useState<MicrosoftConnection[]>([]);
  const [loading, setLoading] = useState(true);
  const [loadError, setLoadError] = useState("");
  // Keyed per connection and operation, for the reason the Google section
  // gives: one shared flag would be cleared by whichever request ended first.
  const [busy, setBusy] = useState<Record<string, boolean>>({});
  const isBusy = (connectionID: number) =>
    Boolean(
      busy[`test:${connectionID}`] ||
        busy[`disconnect:${connectionID}`] ||
        busy[`calendar:${connectionID}`] ||
        busy[`connect:${connectionID}`]
    );
  const setOperationBusy = (key: string, running: boolean) =>
    setBusy((current) => {
      if (!running) {
        const { [key]: _removed, ...rest } = current;
        return rest;
      }
      return { ...current, [key]: true };
    });

  const load = useCallback(async () => {
    setLoading(true);
    try {
      const data = await getJSON<MicrosoftConnectionsResponse>("/api/microsoft/connections");
      setConfigured(Boolean(data.configured));
      setConnections(data.connections || []);
      setLoadError("");
    } catch (error) {
      setLoadError(messageFromError(error));
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    void load();
  }, [load]);

  // Report a completed sign-in once, then drop the query string so a reload
  // does not repeat the toast.
  useEffect(() => {
    const message = messageFromCallback(search);
    if (!message) return;
    addToast(message.text, message.kind);
    replaceRoute(settingsPath);
  }, [search, addToast, replaceRoute]);

  async function connect(connectionID?: number) {
    const key = connectionID ? `connect:${connectionID}` : "connect";
    setOperationBusy(key, true);
    try {
      const body = connectionID ? { connection_id: connectionID } : {};
      const result = await postJSON<{ authorization_url: string }>("/api/microsoft/connect", csrf, body);
      window.location.assign(result.authorization_url);
    } catch (error) {
      addToast(messageFromError(error), "error");
      setOperationBusy(key, false);
    }
  }

  async function disconnect(connection: MicrosoftConnection) {
    const confirmed = window.confirm(
      `Disconnect ${connection.email}?\n\nRolltop removes the stored authorization and the calendars mirrored from this account. Nothing is deleted in Microsoft 365.`
    );
    if (!confirmed) return;
    const key = `disconnect:${connection.id}`;
    setOperationBusy(key, true);
    try {
      const result = await deleteJSON<{ disconnected: boolean; notice?: string }>(
        `/api/microsoft/connections/${connection.id}`,
        csrf
      );
      addToast(result.notice || `Disconnected ${connection.email}.`, "success");
      await load();
    } catch (error) {
      addToast(messageFromError(error), "error");
    } finally {
      setOperationBusy(key, false);
    }
  }

  async function syncCalendars(connection: MicrosoftConnection) {
    const key = `calendar:${connection.id}`;
    setOperationBusy(key, true);
    try {
      const result = await postJSON<{ calendars: number; created: number; updated: number; deleted: number }>(
        `/api/microsoft/connections/${connection.id}/calendar/sync`,
        csrf
      );
      addToast(
        `${connection.email}: ${result.calendars} calendar${result.calendars === 1 ? "" : "s"}, ` +
          `${result.created} events added, ${result.updated} updated, ${result.deleted} removed.`,
        "success"
      );
    } catch (error) {
      addToast(messageFromError(error), "error");
    } finally {
      setOperationBusy(key, false);
      // Reload either way: a failed sync is recorded against the connection.
      await load();
    }
  }

  async function testConnection(connection: MicrosoftConnection) {
    const key = `test:${connection.id}`;
    setOperationBusy(key, true);
    try {
      const result = await postJSON<{ ok: boolean; email: string }>(
        `/api/microsoft/connections/${connection.id}/test`,
        csrf
      );
      addToast(`${result.email} responded. The connection works.`, "success");
    } catch (error) {
      addToast(messageFromError(error), "error");
    } finally {
      setOperationBusy(key, false);
      await load();
    }
  }

  if (loading && connections.length === 0 && !loadError) {
    return <SettingsLoading label="Loading Microsoft accounts..." />;
  }

  return (
    <>
      {loadError ? <SettingsError message={loadError} onRetry={() => void load()} /> : null}
      {!configured ? (
        <div className="notice">
          Microsoft 365 is not configured on this server. Register an app in Microsoft Entra, then set{" "}
          <code>ROLLTOP_MICROSOFT_CLIENT_ID</code>, <code>ROLLTOP_MICROSOFT_CLIENT_SECRET</code> and{" "}
          <code>ROLLTOP_MICROSOFT_REDIRECT_URLS</code> and restart Rolltop.
        </div>
      ) : null}

      <section className="settings-index-group">
        <div className="settings-index-heading">
          <div>
            <h2>Connected accounts</h2>
            <p>
              Each connection authorizes one work, school or personal Microsoft account. Its calendars appear in the
              calendar beside Google's, and new events can be Teams meetings.
            </p>
          </div>
          <button className="secondary" type="button" disabled={!configured || busy.connect} onClick={() => void connect()}>
            <Icon name="link" />
            Connect Microsoft account
          </button>
        </div>

        {connections.length === 0 ? (
          <SettingsEmpty
            icon="key"
            title="No Microsoft accounts connected"
            description="Connect a Microsoft 365 account to see and edit its calendar here."
          />
        ) : (
          <div className="settings-index" role="list" aria-label="Connected Microsoft accounts">
            {connections.map((connection) => {
              const rowBusy = isBusy(connection.id);
              const reconnecting = Boolean(busy[`connect:${connection.id}`]);
              return (
                <div key={connection.id} className="settings-index-item" role="listitem">
                  <div className="settings-connection-row">
                    <span className="settings-index-icon">
                      <Icon name={connection.needs_reauth ? "shield_warning" : "key"} />
                    </span>
                    <span className="settings-index-copy">
                      <strong>{connection.email}</strong>
                      <small>
                        {connection.needs_reauth
                          ? "Microsoft stopped accepting this authorization. Sign in again to resume."
                          : connection.display_name
                            ? `Signed in as ${connection.display_name}.`
                            : "Signed in."}
                      </small>
                      {connection.has_calendar_scope ? (
                        <span className="settings-badges">
                          <span className="settings-badge">Calendar</span>
                          <span className="settings-badge">Teams meetings</span>
                        </span>
                      ) : null}
                      {connection.needs_reauth ? null : <CalendarSyncLine connection={connection} />}
                    </span>
                    <span className="settings-connection-actions">
                      {!connection.needs_reauth && connection.has_calendar_scope ? (
                        <button
                          className="secondary"
                          type="button"
                          disabled={rowBusy}
                          onClick={() => void syncCalendars(connection)}
                        >
                          {busy[`calendar:${connection.id}`] ? "Syncing..." : "Sync calendars"}
                        </button>
                      ) : null}
                      {/* Always offered: signing in again is the only way to
                          replace a grant Microsoft stopped honouring, and that
                          state can look healthy from here. */}
                      <button
                        className="secondary"
                        type="button"
                        title={`Sign ${connection.email} in with Microsoft again`}
                        disabled={!configured || rowBusy}
                        onClick={() => void connect(connection.id)}
                      >
                        {reconnecting ? "Opening Microsoft..." : "Sign in again"}
                      </button>
                      {connection.needs_reauth ? null : (
                        <button className="secondary" type="button" disabled={rowBusy} onClick={() => void testConnection(connection)}>
                          {busy[`test:${connection.id}`] ? "Testing..." : "Test connection"}
                        </button>
                      )}
                      <button className="danger" type="button" disabled={rowBusy} onClick={() => void disconnect(connection)}>
                        Disconnect
                      </button>
                    </span>
                  </div>
                </div>
              );
            })}
          </div>
        )}
      </section>
    </>
  );
}
