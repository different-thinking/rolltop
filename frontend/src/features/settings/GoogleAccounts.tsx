// File overview: Settings section for connected Google accounts. Connecting is a
// full-page navigation into Google's consent screen, so this view reads the
// result out of the callback's query string rather than from a fetch response.

import { useCallback, useEffect, useState } from "react";
import { deleteJSON, getJSON, postJSON } from "../../api";
import { Icon } from "../../components/Icon";
import type { Toast } from "../../appTypes";
import { SettingsEmpty, SettingsError, SettingsLoading } from "./SettingsUI";

export type GoogleConnection = {
  id: number;
  email: string;
  scopes: string[];
  status: string;
  status_detail: string;
  needs_reauth: boolean;
  has_mail_scope: boolean;
  connected_at: string;
  last_updated_at: string;
};

type GoogleConnectionsResponse = {
  configured: boolean;
  connections: GoogleConnection[];
};

// Scope URLs are unreadable in a badge, so they are shown as capabilities.
const scopeLabels: ReadonlyArray<{ match: string; label: string }> = [
  { match: "https://mail.google.com/", label: "Gmail" },
  { match: "/auth/contacts", label: "Contacts" },
  { match: "/auth/calendar", label: "Calendar" },
  { match: "email", label: "Email address" },
  { match: "openid", label: "Sign-in" }
];

function scopeBadges(scopes: string[]): string[] {
  const badges: string[] = [];
  for (const { match, label } of scopeLabels) {
    if (scopes.some((scope) => scope === match || scope.endsWith(match))) badges.push(label);
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
  return { text: "Connecting the Google account failed. Try again.", kind: "error" };
}

/** GoogleAccountsSettings lists Google connections and manages their lifecycle. */
export function GoogleAccountsSettings({
  csrf,
  search,
  addToast
}: {
  csrf: string;
  search: string;
  addToast: (message: string, kind?: Toast["kind"]) => number;
}) {
  const [configured, setConfigured] = useState(true);
  const [connections, setConnections] = useState<GoogleConnection[]>([]);
  const [loading, setLoading] = useState(true);
  const [loadError, setLoadError] = useState("");
  const [busyID, setBusyID] = useState<number | null>(null);

  const load = useCallback(async () => {
    setLoading(true);
    try {
      const data = await getJSON<GoogleConnectionsResponse>("/api/google/connections");
      setConfigured(data.configured);
      setConnections(data.connections || []);
      setLoadError("");
    } catch (error) {
      setLoadError(error instanceof Error ? error.message : "Could not load Google accounts.");
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
    window.history.replaceState(null, "", "/settings/account/google");
  }, [search, addToast]);

  function connect(connectionID?: number) {
    const suffix = connectionID ? `?connection_id=${connectionID}` : "";
    window.location.assign(`/api/google/connect${suffix}`);
  }

  async function disconnect(connection: GoogleConnection) {
    const confirmed = window.confirm(
      `Disconnect ${connection.email}?\n\nRolltop will revoke its access at Google and remove the stored authorization.`
    );
    if (!confirmed) return;
    setBusyID(connection.id);
    try {
      const result = await deleteJSON<{ disconnected: boolean; warning?: string }>(
        `/api/google/connections/${connection.id}`,
        csrf
      );
      addToast(result.warning || `Disconnected ${connection.email}.`, result.warning ? "error" : "success");
      await load();
    } catch (error) {
      addToast(error instanceof Error ? error.message : "Could not disconnect the account.", "error");
    } finally {
      setBusyID(null);
    }
  }

  async function testConnection(connection: GoogleConnection) {
    setBusyID(connection.id);
    try {
      const result = await postJSON<{ ok: boolean; email: string }>(
        `/api/google/connections/${connection.id}/test`,
        csrf
      );
      addToast(`${result.email} responded. The connection works.`, "success");
      await load();
    } catch (error) {
      addToast(error instanceof Error ? error.message : "The connection test failed.", "error");
      await load();
    } finally {
      setBusyID(null);
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
          <button className="secondary" type="button" disabled={!configured} onClick={() => connect()}>
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
              const busy = busyID === connection.id;
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
                    </span>
                    <span className="settings-connection-actions">
                      {connection.needs_reauth ? (
                        <button className="secondary" type="button" disabled={!configured} onClick={() => connect(connection.id)}>
                          Reauthorize
                        </button>
                      ) : (
                        <button className="secondary" type="button" disabled={busy} onClick={() => void testConnection(connection)}>
                          {busy ? "Testing..." : "Test connection"}
                        </button>
                      )}
                      <button className="danger" type="button" disabled={busy} onClick={() => void disconnect(connection)}>
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
