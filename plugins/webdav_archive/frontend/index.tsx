// File overview: The plugin's two pages -- the settings page where a WebDAV
// target is configured and its queue watched, and the file browser at /files.

import { useCallback, useEffect, useMemo, useState } from "react";
import type { FormEvent } from "react";
import type { AddToast, LocationState, Navigate } from "../../../frontend/src/appTypes";
import { Icon } from "../../../frontend/src/components/Icon";
import { SettingsEmpty, SettingsError, SettingsLoading, SettingsPage } from "../../../frontend/src/features/settings/SettingsUI";
import { displayDateTime } from "../../../frontend/src/lib/format";
import type { AccountSettingsRuntimePlugin, AppRouteRuntimePlugin } from "../../../frontend/src/plugins/runtime";
import type { Mailbox, User } from "../../../frontend/src/types";
import "./styles.css";

const apiBase = "/api/plugins/webdav_archive";

type Target = {
  id: number;
  name: string;
  enabled: boolean;
  base_url: string;
  username: string;
  has_password: boolean;
  watch_mailbox_id: number;
  content_types: string;
  path_template: string;
  include_inline: boolean;
  last_error: string;
  last_success_at: number;
  uploaded_total: number;
};

type MailboxOption = { id: number; name: string; role: string; label: string };

type Upload = {
  id: number;
  target_id: number;
  message_id: number;
  filename: string;
  content_type: string;
  size: number;
  remote_path: string;
  status: string;
  attempts: number;
  next_attempt_at: number;
  last_error: string;
  subject: string;
  from_addr: string;
  message_date: number;
  created_at: number;
  completed_at: number;
};

type ResourceEntry = {
  name: string;
  path: string;
  is_dir: boolean;
  size: number;
  content_type: string;
  modified_at: number;
};

type BrowseResponse = { target_id: number; path: string; parent: string; entries: ResourceEntry[] };

type SettingsContext = {
  csrf: string;
  user: User;
  mailboxes: Mailbox[];
  location: LocationState;
  navigate: Navigate;
  addToast: AddToast;
};

/**
 * statusLabels name what a queue row means, in the reader's terms rather than
 * the table's. "Waiting" rather than "queued" because what it is waiting for --
 * the next sweep, or a server that is switched off -- is the same wait.
 */
const statusLabels: Record<string, string> = {
  queued: "Waiting",
  uploading: "Uploading",
  done: "On the server",
  duplicate: "Already there",
  failed: "Retrying",
  abandoned: "Given up"
};

const emptyForm = {
  id: 0,
  name: "",
  base_url: "",
  username: "",
  password: "",
  watch_mailbox_id: 0,
  content_types: "audio/",
  path_template: "{yyyy}/{mm}/{filename}",
  include_inline: false,
  enabled: true
};

type FormState = typeof emptyForm;

function WebDAVArchiveSettings({ csrf, navigate, addToast }: SettingsContext) {
  const [targets, setTargets] = useState<Target[] | null>(null);
  const [mailboxes, setMailboxes] = useState<MailboxOption[]>([]);
  const [counts, setCounts] = useState<Record<string, number>>({});
  const [uploads, setUploads] = useState<Upload[]>([]);
  const [error, setError] = useState("");
  const [form, setForm] = useState<FormState | null>(null);
  const [busy, setBusy] = useState(false);

  const load = useCallback(async () => {
    try {
      const data = await getJSON<{ targets: Target[]; mailboxes: MailboxOption[]; counts: Record<string, number> }>(`${apiBase}/targets`);
      setTargets(data.targets || []);
      setMailboxes(data.mailboxes || []);
      setCounts(data.counts || {});
      setError("");
    } catch (err) {
      setError(messageFromError(err));
    }
  }, []);

  const loadUploads = useCallback(async () => {
    try {
      const data = await getJSON<{ uploads: Upload[]; counts: Record<string, number> }>(`${apiBase}/uploads?limit=100`);
      setUploads(data.uploads || []);
      setCounts(data.counts || {});
    } catch (err) {
      addToast(messageFromError(err), "error");
    }
  }, [addToast]);

  useEffect(() => {
    void load();
    void loadUploads();
  }, [load, loadUploads]);

  const save = useCallback(async (event: FormEvent) => {
    event.preventDefault();
    if (!form) return;
    setBusy(true);
    try {
      const payload = {
        name: form.name,
        enabled: form.enabled,
        base_url: form.base_url,
        username: form.username,
        password: form.password,
        watch_mailbox_id: Number(form.watch_mailbox_id) || 0,
        content_types: form.content_types,
        path_template: form.path_template,
        include_inline: form.include_inline
      };
      if (form.id > 0) await sendJSON(`${apiBase}/targets/${form.id}`, csrf, payload, "PUT");
      else await sendJSON(`${apiBase}/targets`, csrf, payload, "POST");
      setForm(null);
      await load();
      addToast(form.id > 0 ? "Target saved." : "Target added.");
    } catch (err) {
      addToast(messageFromError(err), "error");
    } finally {
      setBusy(false);
    }
  }, [addToast, csrf, form, load]);

  const test = useCallback(async (target: Target) => {
    setBusy(true);
    try {
      const result = await sendJSON<{ ok: boolean; error?: string }>(`${apiBase}/targets/${target.id}/test`, csrf, {}, "POST");
      if (result.ok) addToast("The WebDAV server answered.");
      else addToast(result.error || "The WebDAV server did not answer.", "error");
      await load();
    } catch (err) {
      addToast(messageFromError(err), "error");
    } finally {
      setBusy(false);
    }
  }, [addToast, csrf, load]);

  const toggle = useCallback(async (target: Target) => {
    try {
      await sendJSON(`${apiBase}/targets/${target.id}/enabled`, csrf, { enabled: !target.enabled }, "POST");
      await load();
    } catch (err) {
      addToast(messageFromError(err), "error");
    }
  }, [addToast, csrf, load]);

  const remove = useCallback(async (target: Target) => {
    if (!window.confirm(`Remove ${target.name || target.base_url}? Files already on the server are left alone.`)) return;
    try {
      await sendJSON(`${apiBase}/targets/${target.id}`, csrf, null, "DELETE");
      await load();
      await loadUploads();
      addToast("Target removed.");
    } catch (err) {
      addToast(messageFromError(err), "error");
    }
  }, [addToast, csrf, load, loadUploads]);

  const retry = useCallback(async (item: Upload) => {
    try {
      await sendJSON(`${apiBase}/uploads/${item.id}/retry`, csrf, {}, "POST");
      await loadUploads();
      addToast("Queued again.");
    } catch (err) {
      addToast(messageFromError(err), "error");
    }
  }, [addToast, csrf, loadUploads]);

  const runNow = useCallback(async () => {
    try {
      await sendJSON(`${apiBase}/run`, csrf, {}, "POST");
      addToast("Working through the queue.");
      window.setTimeout(() => void loadUploads(), 1500);
    } catch (err) {
      addToast(messageFromError(err), "error");
    }
  }, [addToast, csrf, loadUploads]);

  const pending = (counts.queued || 0) + (counts.failed || 0) + (counts.uploading || 0);

  if (targets === null && !error) return <SettingsLoading label="Loading WebDAV targets..." />;

  return (
    <SettingsPage
      title="WebDAV archive"
      description="Attachments from a watched folder are copied onto a WebDAV server. What cannot be delivered stays queued until the server answers."
      backPath="/settings/account"
      navigate={navigate}
      actions={
        <>
          <button className="secondary" type="button" onClick={() => navigate("/files")}>
            <Icon name="folder" />Browse files
          </button>
          <button className="secondary" type="button" onClick={() => setForm({ ...emptyForm })}>
            <Icon name="add" />Add target
          </button>
        </>
      }
    >
      {error ? <SettingsError message={error} onRetry={() => void load()} /> : null}

      {form ? (
        <form className="panel webdav-form" onSubmit={save}>
          <h2>{form.id > 0 ? "Edit target" : "New target"}</h2>
          <label>
            <span>Name</span>
            <input value={form.name} placeholder="Nextcloud" onChange={(e) => setForm({ ...form, name: e.target.value })} />
          </label>
          <label>
            <span>WebDAV address</span>
            <input
              required
              value={form.base_url}
              placeholder="https://cloud.example.org/remote.php/dav/files/me/Recordings/"
              onChange={(e) => setForm({ ...form, base_url: e.target.value })}
            />
            <small>The folder everything is filed under. Rolltop never writes above it.</small>
          </label>
          <label>
            <span>User name</span>
            <input value={form.username} autoComplete="off" onChange={(e) => setForm({ ...form, username: e.target.value })} />
          </label>
          <label>
            <span>Password</span>
            <input
              type="password"
              value={form.password}
              autoComplete="new-password"
              placeholder={form.id > 0 ? "Unchanged" : ""}
              onChange={(e) => setForm({ ...form, password: e.target.value })}
            />
            <small>Stored encrypted. Leave empty to keep the one already saved. An app password is safer than the account password.</small>
          </label>
          <label>
            <span>Watched folder</span>
            <select
              value={form.watch_mailbox_id}
              onChange={(e) => setForm({ ...form, watch_mailbox_id: Number(e.target.value) })}
            >
              <option value={0}>Every folder</option>
              {mailboxes.map((mailbox) => (
                <option key={mailbox.id} value={mailbox.id}>{mailbox.label}</option>
              ))}
            </select>
            <small>Sort the mail into this folder with a filter, and what lands here is what gets filed.</small>
          </label>
          <label>
            <span>Attachment types</span>
            <input
              value={form.content_types}
              placeholder="audio/"
              onChange={(e) => setForm({ ...form, content_types: e.target.value })}
            />
            <small>Comma-separated prefixes. <code>audio/</code> takes every recording; <code>audio/mpeg, application/pdf</code> takes two exact types.</small>
          </label>
          <label>
            <span>Where the files go</span>
            <input
              value={form.path_template}
              placeholder="{yyyy}/{mm}/{filename}"
              onChange={(e) => setForm({ ...form, path_template: e.target.value })}
            />
            <small>
              Placeholders: <code>{"{yyyy}"}</code> <code>{"{mm}"}</code> <code>{"{dd}"}</code> <code>{"{date}"}</code>{" "}
              <code>{"{filename}"}</code> <code>{"{basename}"}</code> <code>{"{ext}"}</code> <code>{"{subject}"}</code>{" "}
              <code>{"{from}"}</code>. Folders are created as needed.
            </small>
          </label>
          <label className="webdav-check">
            <input type="checkbox" checked={form.include_inline} onChange={(e) => setForm({ ...form, include_inline: e.target.checked })} />
            <span>Also file inline parts (signatures, embedded images)</span>
          </label>
          <div className="webdav-form-actions">
            <button type="submit" disabled={busy}>Save</button>
            <button className="secondary" type="button" onClick={() => setForm(null)}>Cancel</button>
          </div>
        </form>
      ) : null}

      {targets && targets.length === 0 && !form ? (
        <SettingsEmpty
          icon="folder"
          title="No WebDAV target yet"
          description="Add the server the attachments should be filed onto, then point a mail filter at the folder you want watched."
          action={<button type="button" onClick={() => setForm({ ...emptyForm })}><Icon name="add" />Add target</button>}
        />
      ) : null}

      {(targets || []).map((target) => (
        <section className="panel webdav-target" key={target.id}>
          <div className="panel-headline">
            <div>
              <h2>{target.name || target.base_url}</h2>
              <div className="muted">{target.base_url}</div>
            </div>
            <div className="webdav-target-actions">
              <button className="secondary" type="button" disabled={busy} onClick={() => void test(target)}>
                <Icon name="sync" />Test
              </button>
              <button className="secondary" type="button" onClick={() => void toggle(target)}>
                {target.enabled ? "Pause" : "Resume"}
              </button>
              <button
                className="secondary"
                type="button"
                onClick={() => setForm({
                  id: target.id,
                  name: target.name,
                  base_url: target.base_url,
                  username: target.username,
                  password: "",
                  watch_mailbox_id: target.watch_mailbox_id,
                  content_types: target.content_types,
                  path_template: target.path_template,
                  include_inline: target.include_inline,
                  enabled: target.enabled
                })}
              >
                <Icon name="edit" />Edit
              </button>
              <button className="secondary" type="button" onClick={() => void remove(target)}>
                <Icon name="delete" />Remove
              </button>
            </div>
          </div>
          <dl className="webdav-target-facts">
            <div><dt>Watching</dt><dd>{mailboxLabel(mailboxes, target.watch_mailbox_id)}</dd></div>
            <div><dt>Types</dt><dd>{target.content_types}</dd></div>
            <div><dt>Filed</dt><dd>{target.uploaded_total.toLocaleString()}</dd></div>
            <div><dt>State</dt><dd>{target.enabled ? "Active" : "Paused"}</dd></div>
          </dl>
          {target.last_error ? <div className="error webdav-target-error">{target.last_error}</div> : null}
        </section>
      ))}

      <section className="panel webdav-queue">
        <div className="panel-headline">
          <div>
            <h2>Queue</h2>
            <div className="muted">
              {pending > 0
                ? `${pending.toLocaleString()} waiting · ${(counts.done || 0).toLocaleString()} filed`
                : `${(counts.done || 0).toLocaleString()} filed`}
            </div>
          </div>
          <div className="webdav-target-actions">
            <button className="secondary" type="button" onClick={() => void runNow()}><Icon name="sync" />Run now</button>
            <button className="secondary" type="button" onClick={() => void loadUploads()}>Refresh</button>
          </div>
        </div>
        {uploads.length === 0 ? (
          <p className="muted">Nothing has been queued yet.</p>
        ) : (
          <div className="webdav-rows">
            {uploads.map((item) => (
              <div className={`webdav-row status-${item.status}`} key={item.id}>
                <span className="webdav-row-icon"><Icon name={item.status === "done" || item.status === "duplicate" ? "check" : "clock"} /></span>
                <div className="webdav-row-text">
                  <strong>{item.filename || "(unnamed)"}</strong>
                  <div className="webdav-row-meta">
                    <span>{statusLabels[item.status] || item.status}</span>
                    {item.remote_path ? <span title={item.remote_path}>{item.remote_path}</span> : null}
                    {item.subject ? <span>{item.subject}</span> : null}
                    {item.size > 0 ? <span>{formatBytes(item.size)}</span> : null}
                  </div>
                  {item.last_error ? <div className="webdav-row-error">{item.last_error}</div> : null}
                </div>
                {item.status === "failed" || item.status === "abandoned" ? (
                  <button className="secondary" type="button" onClick={() => void retry(item)}>Retry</button>
                ) : null}
              </div>
            ))}
          </div>
        )}
      </section>
    </SettingsPage>
  );
}

type FilesContext = {
  csrf: string;
  user: User;
  mailboxes: Mailbox[];
  location: LocationState;
  navigate: Navigate;
  addToast: AddToast;
};

/**
 * FilesView browses what the archive filed. It is a view of the WebDAV server
 * rather than of the queue: what is here is what is actually on the server,
 * including anything put there by something other than Rolltop.
 *
 * The current folder lives in the URL after /files, so a folder can be linked
 * to and the browser's own Back button walks back up.
 */
function FilesView({ csrf, location, navigate, addToast }: FilesContext) {
  const [targets, setTargets] = useState<Target[] | null>(null);
  const [targetID, setTargetID] = useState(0);
  const [listing, setListing] = useState<BrowseResponse | null>(null);
  const [error, setError] = useState("");
  const [loading, setLoading] = useState(false);

  const currentPath = useMemo(() => decodeFilesPath(location.path), [location.path]);

  useEffect(() => {
    void (async () => {
      try {
        const data = await getJSON<{ targets: Target[] }>(`${apiBase}/targets`);
        const enabled = (data.targets || []).filter((item) => item.enabled);
        const usable = enabled.length > 0 ? enabled : (data.targets || []);
        setTargets(usable);
        setTargetID((current) => (current > 0 ? current : usable[0]?.id || 0));
      } catch (err) {
        setError(messageFromError(err));
        setTargets([]);
      }
    })();
  }, []);

  useEffect(() => {
    if (targetID <= 0) return;
    let cancelled = false;
    setLoading(true);
    void (async () => {
      try {
        const data = await getJSON<BrowseResponse>(`${apiBase}/browse?target=${targetID}&path=${encodeURIComponent(currentPath)}`);
        if (cancelled) return;
        setListing(data);
        setError("");
      } catch (err) {
        if (cancelled) return;
        setListing(null);
        setError(messageFromError(err));
      } finally {
        if (!cancelled) setLoading(false);
      }
    })();
    return () => { cancelled = true; };
  }, [currentPath, targetID]);

  const openFolder = useCallback((path: string) => {
    navigate(filesURL(path));
  }, [navigate]);

  const remove = useCallback(async (entry: ResourceEntry) => {
    if (!window.confirm(`Delete ${entry.name} from the WebDAV server? This cannot be undone.`)) return;
    try {
      await sendJSON(`${apiBase}/file?target=${targetID}&path=${encodeURIComponent(entry.path)}`, csrf, null, "DELETE");
      addToast(`${entry.name} deleted.`);
      const data = await getJSON<BrowseResponse>(`${apiBase}/browse?target=${targetID}&path=${encodeURIComponent(currentPath)}`);
      setListing(data);
    } catch (err) {
      addToast(messageFromError(err), "error");
    }
  }, [addToast, csrf, currentPath, targetID]);

  const entries = listing?.entries || [];
  const folders = entries.filter((entry) => entry.is_dir);
  const files = entries.filter((entry) => !entry.is_dir);

  return (
    <>
      <div className="content-head">
        <div>
          <h1>Files</h1>
          {targets && targets.length > 1 ? (
            <select className="webdav-target-picker" value={targetID} onChange={(e) => { setTargetID(Number(e.target.value)); navigate(filesURL("")); }}>
              {targets.map((target) => <option key={target.id} value={target.id}>{target.name || target.base_url}</option>)}
            </select>
          ) : null}
        </div>
      </div>

      {targets && targets.length === 0 ? (
        <section className="panel webdav-idle">
          <Icon name="folder" />
          <div>
            <strong>No WebDAV target configured.</strong>
            <p>Add one in settings, and the attachments filed onto it appear here.</p>
            <button className="secondary" type="button" onClick={() => navigate("/settings/account/plugins/webdav")}>
              <Icon name="settings" />Open settings
            </button>
          </div>
        </section>
      ) : null}

      {error ? <div className="error">{error}</div> : null}

      {targetID > 0 ? (
        <section className="panel webdav-browser">
          <nav className="webdav-crumbs" aria-label="Folder path">
            <button type="button" className="link-button" onClick={() => openFolder("")}>
              <Icon name="folder" />Top
            </button>
            {breadcrumbs(currentPath).map((crumb) => (
              <span key={crumb.path}>
                <Icon name="chevron_right" />
                <button type="button" className="link-button" onClick={() => openFolder(crumb.path)}>{crumb.label}</button>
              </span>
            ))}
          </nav>

          {loading && !listing ? <p className="muted" aria-busy="true">Reading the folder...</p> : null}

          {listing && entries.length === 0 && !loading ? <p className="muted">This folder is empty.</p> : null}

          <div className="webdav-entries">
            {folders.map((entry) => (
              <button className="webdav-entry" type="button" key={entry.path} onClick={() => openFolder(entry.path)}>
                <Icon name="folder" weight="fill" />
                <span className="webdav-entry-name">{entry.name}</span>
                <span className="webdav-entry-meta">{unixLabel(entry.modified_at)}</span>
              </button>
            ))}
            {files.map((entry) => (
              <div className="webdav-entry webdav-entry-file" key={entry.path}>
                <Icon name={fileIcon(entry.content_type, entry.name)} />
                <span className="webdav-entry-name">{entry.name}</span>
                <span className="webdav-entry-meta">
                  {formatBytes(entry.size)}
                  {entry.modified_at ? ` · ${unixLabel(entry.modified_at)}` : ""}
                </span>
                <span className="webdav-entry-actions">
                  <a
                    className="secondary"
                    href={`${apiBase}/download?target=${targetID}&path=${encodeURIComponent(entry.path)}`}
                    download={entry.name}
                  >
                    <Icon name="download" />
                  </a>
                  <button className="secondary" type="button" title="Delete" onClick={() => void remove(entry)}>
                    <Icon name="delete" />
                  </button>
                </span>
                {playable(entry.content_type, entry.name) ? (
                  <audio
                    className="webdav-entry-player"
                    controls
                    preload="none"
                    src={`${apiBase}/download?target=${targetID}&path=${encodeURIComponent(entry.path)}&inline=1`}
                  />
                ) : null}
              </div>
            ))}
          </div>
        </section>
      ) : null}
    </>
  );
}

/** decodeFilesPath reads the folder out of /files/<path>. */
function decodeFilesPath(path: string): string {
  const rest = path.replace(/^\/files\/?/, "");
  if (!rest) return "";
  return rest.split("/").map((segment) => {
    try {
      return decodeURIComponent(segment);
    } catch {
      return segment;
    }
  }).join("/");
}

function filesURL(path: string): string {
  const clean = path.replace(/^\/+|\/+$/g, "");
  if (!clean) return "/files";
  return `/files/${clean.split("/").map(encodeURIComponent).join("/")}`;
}

function breadcrumbs(path: string): { label: string; path: string }[] {
  const segments = path.replace(/^\/+|\/+$/g, "").split("/").filter(Boolean);
  const out: { label: string; path: string }[] = [];
  let running = "";
  for (const segment of segments) {
    running = running ? `${running}/${segment}` : segment;
    out.push({ label: segment, path: running });
  }
  return out;
}

function mailboxLabel(mailboxes: MailboxOption[], id: number): string {
  if (id <= 0) return "Every folder";
  return mailboxes.find((mailbox) => mailbox.id === id)?.label || "A folder that is gone";
}

function fileIcon(contentType: string, name: string): string {
  const value = (contentType || "").toLowerCase();
  if (value.startsWith("image/")) return "image";
  if (value.startsWith("audio/") || /\.(mp3|m4a|ogg|opus|wav|aac|flac)$/i.test(name)) return "attach_file";
  return "file_text";
}

function playable(contentType: string, name: string): boolean {
  const value = (contentType || "").toLowerCase();
  return value.startsWith("audio/") || /\.(mp3|m4a|ogg|opus|wav|aac|flac)$/i.test(name);
}

/** unixLabel renders a WebDAV timestamp with the app's own date formatter,
 * which takes the string form a date is carried in everywhere else. */
function unixLabel(seconds: number): string {
  if (!seconds || seconds <= 0) return "";
  return displayDateTime(new Date(seconds * 1000).toISOString());
}

function formatBytes(size: number): string {
  if (!size || size <= 0) return "";
  const units = ["B", "kB", "MB", "GB"];
  let value = size;
  let unit = 0;
  while (value >= 1024 && unit < units.length - 1) {
    value /= 1024;
    unit += 1;
  }
  return `${value < 10 && unit > 0 ? value.toFixed(1) : Math.round(value)} ${units[unit]}`;
}

async function getJSON<T>(url: string): Promise<T> {
  const res = await fetch(url, { credentials: "same-origin" });
  if (!res.ok) throw new Error(await errorText(res));
  return res.json();
}

async function sendJSON<T>(url: string, csrf: string, payload: unknown, method: string): Promise<T> {
  const res = await fetch(url, {
    method,
    credentials: "same-origin",
    headers: payload === null
      ? { "X-CSRF-Token": csrf }
      : { "Content-Type": "application/json", "X-CSRF-Token": csrf },
    body: payload === null ? undefined : JSON.stringify(payload)
  });
  if (!res.ok) throw new Error(await errorText(res));
  // A DELETE that answers 204 has nothing to parse, and a caller that ignores
  // the result should not be handed a parse failure instead.
  const text = await res.text();
  return (text ? JSON.parse(text) : {}) as T;
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
      path: "/settings/account/plugins/webdav",
      title: "WebDAV archive",
      label: "WebDAV archive",
      description: "File attachments from a watched folder onto a WebDAV server, and browse what landed there.",
      icon: "folder",
      section: "plugins",
      render: (context: SettingsContext) => <WebDAVArchiveSettings {...context} />
    }
  ],
  appRoutes: [
    {
      path: "/files",
      nested: true,
      label: "Files",
      icon: "folder",
      render: (context: FilesContext) => <FilesView {...context} />
    }
  ],
  renderAccountSettingsSummary: ({ navigate }: SettingsContext) => (
    <section className="panel account-list-panel">
      <div className="panel-headline">
        <div>
          <h2>WebDAV archive</h2>
          <div className="muted">Attachments from a watched folder, filed onto a server you run.</div>
        </div>
        <button className="secondary" type="button" onClick={() => navigate("/settings/account/plugins/webdav")}>
          <Icon name="folder" />Manage
        </button>
      </div>
    </section>
  )
} satisfies AccountSettingsRuntimePlugin & AppRouteRuntimePlugin;
