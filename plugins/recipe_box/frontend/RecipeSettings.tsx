// File overview: The recipe box's settings: which mail it may read, where it
// files what it found, and how far the backfill has come.

import { useCallback, useEffect, useState } from "react";
import type { FormEvent } from "react";
import { Icon } from "../../../frontend/src/components/Icon";
import { SettingsError, SettingsLoading, SettingsPage } from "../../../frontend/src/features/settings/SettingsUI";
import type { AccountSettingsRouteContext } from "../../../frontend/src/plugins/runtime";
import { messageFromError, recipeAPI, type Settings, type SettingsResponse } from "./api";
import { recipesPath } from "./RecipesView";

export function RecipeSettings({ csrf, navigate, addToast }: AccountSettingsRouteContext) {
  const [data, setData] = useState<SettingsResponse | null>(null);
  const [draft, setDraft] = useState<Settings | null>(null);
  const [loadError, setLoadError] = useState("");
  const [busy, setBusy] = useState(false);

  const load = useCallback(async () => {
    try {
      const next = await recipeAPI.settings();
      setData(next);
      setDraft((current) => current || next.settings);
      setLoadError("");
    } catch (err) {
      setLoadError(messageFromError(err));
    }
  }, []);

  useEffect(() => {
    void load();
  }, [load]);

  // While the backfill or the page reads run, the numbers below move on their
  // own; the page follows them rather than asking the reader to reload.
  const running = Boolean(data?.settings.enabled && (!data.settings.scan_finished_at || (data.status.links.pending || 0) > 0));
  useEffect(() => {
    if (!running) return;
    const timer = window.setInterval(() => void load(), 5000);
    return () => window.clearInterval(timer);
  }, [running, load]);

  async function save(event: FormEvent) {
    event.preventDefault();
    if (!draft) return;
    setBusy(true);
    try {
      const next = await recipeAPI.saveSettings(csrf, draft);
      setData(next);
      setDraft(next.settings);
      addToast(next.settings.enabled ? "Saved. The recipe box is looking through your mail." : "Saved. The recipe box is switched off.");
    } catch (err) {
      addToast(messageFromError(err), "error");
    } finally {
      setBusy(false);
    }
  }

  async function rescan() {
    setBusy(true);
    try {
      setData(await recipeAPI.restartScan(csrf));
      addToast("Looking through all of your mail again.");
    } catch (err) {
      addToast(messageFromError(err), "error");
    } finally {
      setBusy(false);
    }
  }

  async function hideFolder(mailboxID: number) {
    setBusy(true);
    try {
      setData(await recipeAPI.hideFolder(csrf, mailboxID));
      addToast("The folder is no longer listed in All Mail.");
    } catch (err) {
      addToast(messageFromError(err), "error");
    } finally {
      setBusy(false);
    }
  }

  const status = data?.status;
  const settings = data?.settings;
  const scanned = status && status.messages_total > 0 ? Math.min(100, Math.floor((status.messages_scanned / status.messages_total) * 100)) : 0;

  return (
    <SettingsPage
      title="Recipe box"
      description="Recipes you send yourself as links, kept as recipes - with picture, ingredients and steps."
      backPath="/settings/account/plugins"
      navigate={navigate}
      className="recipe-box-settings"
      actions={<button className="secondary" type="button" onClick={() => navigate(recipesPath)}><Icon name="cooking_pot" />Open recipes</button>}
    >
      {!data && !loadError ? <SettingsLoading label="Loading the recipe box..." /> : null}
      {loadError ? <SettingsError message={loadError} onRetry={() => void load()} /> : null}
      {draft && settings && status ? (
        <>
          <form className="panel recipe-settings-form" onSubmit={save}>
            <label className="recipe-settings-check">
              <input type="checkbox" checked={draft.enabled} onChange={(event) => setDraft({ ...draft, enabled: event.target.checked })} />
              <span>
                <strong>Collect recipes from my mail</strong>
                <small>
                  Mail is read only when one of your own addresses sent it. Every link in it is opened once, from this
                  server, to see whether the page publishes a recipe. Newsletters and mail from anybody else are never read.
                </small>
              </span>
            </label>
            <label className="recipe-settings-check">
              <input type="checkbox" checked={draft.own_recipients_only} onChange={(event) => setDraft({ ...draft, own_recipients_only: event.target.checked })} />
              <span>
                <strong>Only mail from my addresses to my addresses</strong>
                <small>A mail that also went to somebody else is left alone. Your own addresses are your accounts and your sending identities.</small>
              </span>
            </label>
            <label className="recipe-settings-check">
              <input type="checkbox" checked={draft.move_enabled} onChange={(event) => setDraft({ ...draft, move_enabled: event.target.checked })} />
              <span>
                <strong>File recipe mails out of the Inbox</strong>
                <small>
                  A mail is filed once, into the folder below in its own account, which is created on first use and kept out
                  of All Mail. Moving one back is your call; the recipe box does not file it again.
                </small>
              </span>
            </label>
            {draft.move_enabled ? (
              <label>
                <span className="settings-field-label">Folder</span>
                <input type="text" value={draft.folder_name} onChange={(event) => setDraft({ ...draft, folder_name: event.target.value })} placeholder="Recipes" maxLength={120} />
              </label>
            ) : null}
            <div className="recipe-settings-actions">
              <button type="submit" disabled={busy}>{busy ? "Saving…" : "Save"}</button>
            </div>
          </form>

          <section className="panel recipe-settings-status">
            <div className="panel-headline">
              <div>
                <h2>Progress</h2>
                <div className="muted">
                  {!settings.enabled
                    ? "Switched off."
                    : !settings.scan_finished_at
                      ? `Looking through your mail: ${scanned}%`
                      : "All mail looked through. New mail is read as it arrives."}
                </div>
              </div>
              <button className="secondary" type="button" disabled={busy || !settings.enabled} onClick={() => void rescan()}>
                <Icon name="sync" />Look through everything again
              </button>
            </div>
            {settings.enabled && !settings.scan_finished_at ? (
              <div className="recipe-progress-bar" role="progressbar" aria-valuemin={0} aria-valuemax={100} aria-valuenow={scanned}>
                <span style={{ width: `${scanned}%` }} />
              </div>
            ) : null}
            <dl className="recipe-facts">
              <div><dt>Notes to self with links</dt><dd>{status.notes_found.toLocaleString()}</dd></div>
              <div><dt>Recipes</dt><dd>{status.recipes.toLocaleString()}</dd></div>
              <div><dt>Pages still to read</dt><dd>{((status.links.pending || 0) + (status.links.retrying || 0)).toLocaleString()}</dd></div>
              <div><dt>Not a recipe</dt><dd>{(status.links.page || 0).toLocaleString()}</dd></div>
              <div><dt>Could not be read</dt><dd>{(status.links.failed || 0).toLocaleString()}</dd></div>
              <div><dt>Mails filed</dt><dd>{(status.mails.moved || 0).toLocaleString()}</dd></div>
              {status.mails.failed ? <div><dt>Could not be filed</dt><dd>{status.mails.failed.toLocaleString()}</dd></div> : null}
            </dl>
            {(status.links.page || 0) + (status.links.failed || 0) > 0 ? (
              <button className="ghost" type="button" onClick={() => navigate(`${recipesPath}/links`)}>Review links that were not recognised</button>
            ) : null}
          </section>

          {settings.move_enabled && status.folders.length > 0 ? (
            <section className="panel recipe-settings-folders">
              <h2>Recipe folders</h2>
              <ul>
                {status.folders.map((folder) => (
                  <li key={folder.account_id}>
                    <span>
                      <strong>{folder.account}</strong>
                      <small className="muted">
                        {folder.mailbox_id
                          ? folder.show_in_all_mail ? `${folder.name} - still listed in All Mail` : `${folder.name} - kept out of All Mail`
                          : "Created when the first recipe mail is filed"}
                      </small>
                    </span>
                    {folder.mailbox_id && folder.show_in_all_mail ? (
                      <button className="secondary" type="button" disabled={busy} onClick={() => void hideFolder(folder.mailbox_id)}>Keep out of All Mail</button>
                    ) : null}
                  </li>
                ))}
              </ul>
            </section>
          ) : null}
        </>
      ) : null}
    </SettingsPage>
  );
}
