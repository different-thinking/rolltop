// File overview: Settings section for connected Google accounts. Connecting is a
// full-page navigation into Google's consent screen, so this view reads the
// result out of the callback's query string rather than from a fetch response.

import { useCallback, useEffect, useState } from "react";
import { deleteJSON, postJSON } from "../../api";
import { Icon } from "../../components/Icon";
import type { Toast } from "../../appTypes";
import { messageFromError } from "../../lib/errors";
import { SettingsEmpty, SettingsError, SettingsLoading } from "./SettingsUI";
import { loadGoogleConnections, type GoogleConnection } from "./googleConnections";

// Scope URLs are unreadable in a badge, so they are shown as capabilities.
// Google offers narrower variants of each scope (".readonly", ".other" and so
// on), so capability scopes match on prefix; the bare identity scopes are exact
// values and match exactly.
const scopeLabels: ReadonlyArray<{ prefixes?: string[]; exact?: string[]; label: string }> = [
  { prefixes: ["https://mail.google.com/", "https://www.googleapis.com/auth/gmail"], label: "Gmail" },
  { prefixes: ["https://www.googleapis.com/auth/contacts"], label: "Contacts" },
  { prefixes: ["https://www.googleapis.com/auth/calendar"], label: "Calendar" },
  { exact: ["email", "https://www.googleapis.com/auth/userinfo.email"], label: "Email address" },
  { exact: ["openid"], label: "Sign-in" }
];

function scopeBadges(scopes: string[]): string[] {
  const badges: string[] = [];
  for (const { prefixes, exact, label } of scopeLabels) {
    const matched = scopes.some(
      (scope) =>
        (exact?.includes(scope) ?? false) || (prefixes?.some((prefix) => scope.startsWith(prefix)) ?? false)
    );
    if (matched) badges.push(label);
  }
  return badges;
}

// The callback redirects here with either connected=<email> or error=<code>.
function messageFromCallback(search: string): { text: string; kind: Toast["kind"] } | null {
  const params = new URLSearchParams(search);
  const connected = params.get("connected");
  if (connected) return { text: `Connected ${connected}.`, kind: "success" };
  const error = params.get("error");
  if (!error) return null;
  if (error === "access_denied") return { text: "Google sign-in was cancelled.", kind: "error" };
  if (error === "expired") return { text: "That Google sign-in took too long. Try connecting again.", kind: "error" };
  if (error === "invalid_response") return { text: "Google returned an incomplete response. Try connecting again.", kind: "error" };
  if (error === "unavailable") return { text: "Google is not available on this server right now.", kind: "error" };
  return { text: "Connecting the Google account failed. Try again.", kind: "error" };
}

/** ContactsSyncLine describes what contact sync is doing for one connection.
 *
 * A connection authorized before contact sync existed still works for mail, so
 * the missing scope is stated as something to fix rather than as a fault. */
function ContactsSyncLine({ connection }: { connection: GoogleConnection }) {
  if (!connection.has_contacts_scope) {
    return <small className="muted">Contacts are not synced. Reauthorize this account to include them.</small>;
  }
  const sync = connection.contacts_sync;
  // A failure is reported before anything else. A connection whose syncs have
  // only ever failed -- the People API not enabled for the project is the usual
  // reason -- has never succeeded either, and "not synced yet" would hide the
  // one line that says what to fix.
  if (sync && sync.status === "error") {
    return <small className="settings-status-error">{sync.status_detail || "The last contact sync failed."}</small>;
  }
  if (!sync || !sync.ever_synced) {
    return <small className="muted">Contacts have not been synced yet.</small>;
  }
  const count = `${sync.contact_count.toLocaleString()} contact${sync.contact_count === 1 ? "" : "s"}`;
  return <small className="muted">{count} synced from Google. Last sync {formatSyncTime(sync.last_success_at)}.</small>;
}

/** CalendarSyncLine is the same report for calendars. It is a separate line
 * rather than one combined status because a grant can cover contacts and not
 * calendars, and one line would have to hide half the truth. */
function CalendarSyncLine({ connection }: { connection: GoogleConnection }) {
  if (!connection.has_calendar_scope) {
    return <small className="muted">Calendars are not synced. Reauthorize this account to include them.</small>;
  }
  const sync = connection.calendar_sync;
  // A failure is reported before anything else, for the same reason it is on
  // the contacts line: the Calendar API not being enabled for the project fails
  // every sync, and "not synced yet" would hide the one line that says so.
  if (sync && sync.status === "error") {
    return <small className="settings-status-error">{sync.status_detail || "The last calendar sync failed."}</small>;
  }
  if (!sync || !sync.ever_synced) {
    return <small className="muted">Calendars have not been synced yet.</small>;
  }
  const count = `${sync.calendar_count.toLocaleString()} calendar${sync.calendar_count === 1 ? "" : "s"}`;
  return <small className="muted">{count} synced from Google. Last sync {formatSyncTime(sync.last_success_at)}.</small>;
}

function formatSyncTime(value: string): string {
  const parsed = new Date(value);
  if (!value || Number.isNaN(parsed.getTime())) return "at an unknown time";
  return parsed.toLocaleString();
}

/** GoogleAccountsSettings lists Google connections and manages their lifecycle. */
export function GoogleAccountsSettings({
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
  const [connections, setConnections] = useState<GoogleConnection[]>([]);
  const [loading, setLoading] = useState(true);
  const [loadError, setLoadError] = useState("");
  // Keyed per connection and operation: a single shared id would be cleared by
  // whichever request finished first, re-enabling buttons still in flight.
  const [busy, setBusy] = useState<Record<string, boolean>>({});
  const isBusy = (connectionID: number) =>
    Boolean(busy[`test:${connectionID}`] || busy[`disconnect:${connectionID}`] || busy[`contacts:${connectionID}`]);
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
      // Always a fresh read: this view is where connections are added and
      // removed, so it owns the state the server forms then read from cache.
      const data = await loadGoogleConnections(true);
      setConfigured(data.configured);
      setConnections(data.connections);
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

  // Report the outcome of a completed consent redirect exactly once, then drop
  // the query string so a reload does not repeat the toast.
  useEffect(() => {
    const message = messageFromCallback(search);
    if (!message) return;
    addToast(message.text, message.kind);
    // Clear the callback query through the router so App's own location state
    // stops reporting a query the URL no longer has.
    replaceRoute("/settings/account/google");
  }, [search, addToast, replaceRoute]);

  async function connect(connectionID?: number) {
    const key = connectionID ? `connect:${connectionID}` : "connect";
    setOperationBusy(key, true);
    try {
      const body = connectionID ? { connection_id: connectionID } : {};
      const result = await postJSON<{ authorization_url: string }>("/api/google/connect", csrf, body);
      window.location.assign(result.authorization_url);
    } catch (error) {
      addToast(messageFromError(error), "error");
      setOperationBusy(key, false);
    }
  }

  async function disconnect(connection: GoogleConnection) {
    const confirmed = window.confirm(
      `Disconnect ${connection.email}?\n\nRolltop will revoke its access at Google and remove the stored authorization.`
    );
    if (!confirmed) return;
    const key = `disconnect:${connection.id}`;
    setOperationBusy(key, true);
    try {
      const result = await deleteJSON<{ disconnected: boolean; warning?: string }>(
        `/api/google/connections/${connection.id}`,
        csrf
      );
      addToast(result.warning || `Disconnected ${connection.email}.`, result.warning ? "error" : "success");
      await load();
    } catch (error) {
      addToast(messageFromError(error), "error");
    } finally {
      setOperationBusy(key, false);
    }
  }

  async function syncContacts(connection: GoogleConnection) {
    const key = `contacts:${connection.id}`;
    setOperationBusy(key, true);
    try {
      const result = await postJSON<{ created: number; updated: number; deleted: number }>(
        `/api/google/connections/${connection.id}/contacts/sync`,
        csrf
      );
      addToast(
        `${connection.email}: ${result.created} added, ${result.updated} updated, ${result.deleted} removed.`,
        "success"
      );
    } catch (error) {
      addToast(messageFromError(error), "error");
    } finally {
      setOperationBusy(key, false);
      // Reload either way: a failed sync is recorded against the connection and
      // that record is the thing the user needs to see.
      await load();
    }
  }

  async function syncCalendars(connection: GoogleConnection) {
    const key = `calendar:${connection.id}`;
    setOperationBusy(key, true);
    try {
      const result = await postJSON<{ calendars: number; created: number; updated: number; deleted: number }>(
        `/api/google/connections/${connection.id}/calendar/sync`,
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
      // Reload either way: a failed sync is recorded against the connection and
      // that record is the thing the user needs to see.
      await load();
    }
  }

  async function testConnection(connection: GoogleConnection) {
    const key = `test:${connection.id}`;
    setOperationBusy(key, true);
    try {
      const result = await postJSON<{ ok: boolean; email: string }>(
        `/api/google/connections/${connection.id}/test`,
        csrf
      );
      addToast(`${result.email} responded. The connection works.`, "success");
      await load();
    } catch (error) {
      addToast(messageFromError(error), "error");
      await load();
    } finally {
      setOperationBusy(key, false);
    }
  }

  if (loading && connections.length === 0 && !loadError) {
    return <SettingsLoading label="Loading Google accounts..." />;
  }

  return (
    <>
      {loadError ? <SettingsError message={loadError} onRetry={() => void load()} /> : null}
      {!configured ? (
        <div className="notice">
          Google is not configured on this server. Set <code>ROLLTOP_GOOGLE_CLIENT_ID</code>,{" "}
          <code>ROLLTOP_GOOGLE_CLIENT_SECRET</code>, and <code>ROLLTOP_GOOGLE_REDIRECT_URLS</code>, then restart Rolltop.
        </div>
      ) : null}

      <section className="settings-index-group">
        <div className="settings-index-heading">
          <div>
            <h2>Connected accounts</h2>
            <p>Each connection authorizes one Google account for mail, contacts, and calendar.</p>
          </div>
          <button className="secondary" type="button" disabled={!configured || busy.connect} onClick={() => void connect()}>
            <Icon name="link" />
            Connect Google account
          </button>
        </div>

        {connections.length === 0 ? (
          <SettingsEmpty
            icon="key"
            title="No Google accounts connected"
            description="Connect a Google account to use it without an app password."
          />
        ) : (
          <div className="settings-index" role="list" aria-label="Connected Google accounts">
            {connections.map((connection) => {
              const badges = scopeBadges(connection.scopes);
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
                          ? "Google revoked this authorization. Reauthorize to resume access."
                          : badges.length > 0
                            ? `Authorized for ${badges.join(", ")}.`
                            : "No capabilities authorized."}
                      </small>
                      {badges.length > 0 ? (
                        <span className="settings-badges">
                          {badges.map((badge) => (
                            <span key={badge} className="settings-badge">
                              {badge}
                            </span>
                          ))}
                        </span>
                      ) : null}
                      {connection.needs_reauth ? null : <ContactsSyncLine connection={connection} />}
                      {connection.needs_reauth ? null : <CalendarSyncLine connection={connection} />}
                    </span>
                    <span className="settings-connection-actions">
                      {!connection.needs_reauth && connection.has_contacts_scope ? (
                        <button
                          className="secondary"
                          type="button"
                          disabled={rowBusy}
                          onClick={() => void syncContacts(connection)}
                        >
                          {busy[`contacts:${connection.id}`] ? "Syncing..." : "Sync contacts"}
                        </button>
                      ) : null}
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
                      {/* Always offered, never conditional. Consent is the only
                          way to add a scope, to replace a revoked grant, and to
                          recover a connection Google has quietly stopped
                          answering for -- and that last state looks, from here,
                          exactly like a healthy one. Hiding the control until
                          Rolltop can name the fault left the one account whose
                          syncs were failing with no way to reconnect it. */}
                      <button
                        className="secondary"
                        type="button"
                        title={`Send ${connection.email} through Google's consent screen again`}
                        disabled={!configured || reconnecting}
                        onClick={() => void connect(connection.id)}
                      >
                        {reconnecting ? "Opening Google..." : "Reauthorize"}
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
