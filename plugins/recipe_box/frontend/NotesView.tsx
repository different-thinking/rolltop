// File overview: Notes -- every note the reader mailed themselves that is not
// a recipe, sorted onto shelves by what it points at.
//
// The overview is the point of this page: with nothing chosen it draws every
// shelf as a section with its newest notes, so a reader sees at a glance that
// they have six videos to watch and three things they meant to buy. Choosing a
// shelf, or searching, turns it into one list. A note is done when the reader
// says so; done notes keep their own tab rather than disappearing.

import { useCallback, useEffect, useMemo, useState } from "react";
import type { MouseEvent } from "react";
import type { DatePrefs } from "../../../frontend/src/appTypes";
import { Icon } from "../../../frontend/src/components/Icon";
import { displayDateTime } from "../../../frontend/src/lib/format";
import { messageURL } from "../../../frontend/src/lib/routes";
import type { RuntimeAppViewContext } from "../../../frontend/src/plugins/runtime";
import { hostOf, messageFromError, recipeAPI, type Note, type NoteLink, type NotesResponse } from "./api";
import { dateLabel, recipesPath, settingsPath, useLink, useStatus } from "./RecipesView";

/** The shelves, in the order the overview draws them, with what they are called. */
export const noteKinds: { kind: string; label: string; icon: string }[] = [
  { kind: "text", label: "Text notes", icon: "draft" },
  { kind: "article", label: "Articles", icon: "newspaper" },
  { kind: "video", label: "Videos", icon: "video" },
  { kind: "product", label: "Shopping", icon: "shopping_bag" },
  { kind: "place", label: "Places & travel", icon: "map_pin" },
  { kind: "event", label: "Events", icon: "ticket" },
  { kind: "music", label: "Music & podcasts", icon: "music" },
  { kind: "media", label: "Books & films", icon: "book" },
  { kind: "tech", label: "Tech & apps", icon: "code" },
  { kind: "social", label: "Social", icon: "forum" },
  { kind: "docs", label: "Documents", icon: "file_text" },
  { kind: "link", label: "Other links", icon: "link" }
];

const kindByName = new Map(noteKinds.map((entry) => [entry.kind, entry]));

function kindInfo(kind: string) {
  return kindByName.get(kind) || kindByName.get("link")!;
}

/** How many notes a shelf shows in the overview before "Show all". */
const overviewPerShelf = 4;

type NoteAction = "done" | "reopen" | "remove" | "delete" | "recipe" | "retry";

export function NotesView({ csrf, navigate, addToast, datePrefs, mailGeneration }: RuntimeAppViewContext) {
  const [state, setState] = useState<"open" | "done">("open");
  const [query, setQuery] = useState("");
  const [appliedQuery, setAppliedQuery] = useState("");
  const [kind, setKind] = useState("");
  const [data, setData] = useState<NotesResponse | null>(null);
  const [error, setError] = useState("");
  const [busyIDs, setBusyIDs] = useState<Set<number>>(() => new Set());
  const [reloadKey, setReloadKey] = useState(0);
  const { status, busy, tick } = useStatus(mailGeneration);
  const link = useLink(navigate);

  useEffect(() => {
    const timer = window.setTimeout(() => setAppliedQuery(query), 250);
    return () => window.clearTimeout(timer);
  }, [query]);

  // Each answer is for the state and search it was asked with; a slower
  // earlier one must not replace what the reader is now looking at.
  useEffect(() => {
    let cancelled = false;
    recipeAPI.notes(state, appliedQuery).then((next) => {
      if (cancelled) return;
      setData(next);
      setError("");
    }).catch((err) => {
      if (!cancelled) setError(messageFromError(err));
    });
    return () => {
      cancelled = true;
    };
  }, [state, appliedQuery, mailGeneration, tick, reloadKey]);

  const reload = useCallback(() => setReloadKey((value) => value + 1), []);
  const notes = data?.notes || [];
  const shown = useMemo(() => (kind ? notes.filter((note) => note.kind === kind) : notes), [notes, kind]);
  const shelves = useMemo(() => noteKinds.filter((entry) => (data?.counts[entry.kind] || 0) > 0), [data]);
  const overview = !kind && !appliedQuery.trim();

  function setBusy(id: number, on: boolean) {
    setBusyIDs((current) => {
      const next = new Set(current);
      if (on) next.add(id);
      else next.delete(id);
      return next;
    });
  }

  // A note acted on leaves the list at once; the reload that follows brings
  // the counts back in line with the server.
  function drop(id: number) {
    setData((current) => {
      if (!current) return current;
      const note = current.notes.find((entry) => entry.id === id);
      const counts = { ...current.counts };
      if (note) counts[note.kind] = Math.max(0, (counts[note.kind] || 1) - 1);
      return { ...current, notes: current.notes.filter((entry) => entry.id !== id), counts };
    });
  }

  async function act(note: Note, action: NoteAction, target?: NoteLink) {
    setBusy(note.id, true);
    try {
      if (action === "done" || action === "reopen") {
        await recipeAPI.setNoteDone(csrf, note.id, action === "done");
        drop(note.id);
        addToast(action === "done" ? "Marked as done." : "Back among the open notes.");
      } else if (action === "remove") {
        await recipeAPI.dismissNote(csrf, note.id);
        drop(note.id);
        addToast("Removed from the notes.");
      } else if (action === "delete") {
        const result = await recipeAPI.deleteNote(csrf, note.id);
        drop(note.id);
        // A note whose mail is already gone has nothing left to move.
        addToast(result.moved ? "Moved to Trash." : "Removed from the notes.");
      } else if (action === "recipe" && target) {
        await recipeAPI.promoteLink(csrf, target.id);
        drop(note.id);
        addToast("Filed as a recipe.");
      } else if (action === "retry" && target) {
        await recipeAPI.retryLink(csrf, target.id);
        addToast("It will be read again shortly.");
      }
      reload();
    } catch (err) {
      addToast(messageFromError(err), "error");
    } finally {
      setBusy(note.id, false);
    }
  }

  const rowProps = { datePrefs, navigate, link, busyIDs, onAction: act };
  const enabled = status?.settings.enabled;

  return (
    <div className="recipe-box notes-view">
      <div className="content-head recipe-box-head">
        <div>
          <h1>Notes</h1>
          {data ? <span className="label-pill">{(state === "open" ? data.totals.open : data.totals.done).toLocaleString()}{state === "done" ? " done" : " open"}</span> : null}
        </div>
        <div className="recipe-box-head-actions">
          <a className="button secondary" href={recipesPath} onClick={link(recipesPath)}><Icon name="cooking_pot" />Recipes</a>
          <a className="icon-button" href={settingsPath} title="Recipe box settings" aria-label="Recipe box settings" onClick={link(settingsPath)}>
            <Icon name="settings" />
          </a>
        </div>
      </div>

      {status && !enabled ? (
        <section className="panel recipe-idle">
          <Icon name="draft" />
          <div>
            <strong>The recipe box is switched off.</strong>
            <p>Notes are the mails you sent yourself that are not recipes. Switch the recipe box on to collect both.</p>
            <a className="button" href={settingsPath} onClick={link(settingsPath)}>Set it up</a>
          </div>
        </section>
      ) : null}

      {enabled && busy ? (
        <p className="muted recipe-progress">
          <span className="spinner" aria-hidden="true" />
          {status && !status.settings.scan_finished_at ? "Looking through your mail…" : "Reading the pages your notes link to…"}
        </p>
      ) : null}

      <div className="recipe-toolbar">
        <label className="recipe-search">
          <Icon name="search" />
          <input type="search" value={query} onChange={(event) => setQuery(event.target.value)} placeholder="Search notes" aria-label="Search notes" />
        </label>
        <div className="recipe-tabs" role="tablist">
          <button type="button" role="tab" aria-selected={state === "open"} className={state === "open" ? "active" : ""} onClick={() => { setState("open"); setKind(""); }}>
            Open{data?.totals.open ? <span className="recipe-tab-count">{data.totals.open.toLocaleString()}</span> : null}
          </button>
          <button type="button" role="tab" aria-selected={state === "done"} className={state === "done" ? "active" : ""} onClick={() => { setState("done"); setKind(""); }}>
            Done{data?.totals.done ? <span className="recipe-tab-count">{data.totals.done.toLocaleString()}</span> : null}
          </button>
        </div>
      </div>

      {shelves.length > 0 ? (
        <div className="recipe-filters notes-shelves" aria-label="Shelves">
          <button type="button" className={kind === "" ? "active" : ""} onClick={() => setKind("")}>All</button>
          {shelves.map((entry) => (
            <button type="button" key={entry.kind} className={kind === entry.kind ? "active" : ""} onClick={() => setKind(kind === entry.kind ? "" : entry.kind)}>
              <Icon name={entry.icon} />{entry.label}<span className="notes-shelf-count">{(data?.counts[entry.kind] || 0).toLocaleString()}</span>
            </button>
          ))}
        </div>
      ) : null}

      {error ? <div className="error">{error}</div> : null}
      {!data && !error ? <p className="muted">Loading…</p> : null}
      {data && shown.length === 0 && enabled ? (
        <p className="muted recipe-empty">
          {appliedQuery || kind ? "No note matches." : state === "done" ? "Nothing marked as done yet." : "No open notes. Everything you mail yourself that is not a recipe appears here."}
        </p>
      ) : null}

      {overview ? (
        shelves.map((entry) => {
          const items = notes.filter((note) => note.kind === entry.kind);
          return (
            <section className="notes-shelf" key={entry.kind}>
              <h2>
                <Icon name={entry.icon} />{entry.label}
                <span className="deliveries-section-count">{items.length.toLocaleString()}</span>
              </h2>
              <div className="notes-list">
                {items.slice(0, overviewPerShelf).map((note) => <NoteRow key={note.id} note={note} {...rowProps} />)}
              </div>
              {items.length > overviewPerShelf ? (
                <button className="ghost notes-show-all" type="button" onClick={() => setKind(entry.kind)}>
                  Show all {items.length.toLocaleString()}
                </button>
              ) : null}
            </section>
          );
        })
      ) : (
        <div className="notes-list">
          {shown.map((note) => <NoteRow key={note.id} note={note} {...rowProps} />)}
        </div>
      )}
    </div>
  );
}

function NoteRow({
  note,
  datePrefs,
  link,
  busyIDs,
  onAction
}: {
  note: Note;
  datePrefs: DatePrefs;
  navigate: (url: string) => void;
  link: (url: string) => (event: MouseEvent) => void;
  busyIDs: Set<number>;
  onAction: (note: Note, action: NoteAction, target?: NoteLink) => void;
}) {
  const [expanded, setExpanded] = useState(false);
  const info = kindInfo(note.kind);
  const first = note.links[0];
  const href = first ? first.final_url || first.url : "";
  const title = (first && first.title) || note.subject || firstLine(note.body) || (first ? titleFromLink(href) : "Note");
  const siteName = first ? first.site_name || hostOf(href) : "";
  const site = siteName !== title ? siteName : "";
  const detail = first ? detailLabel(first, datePrefs) : "";
  const busy = busyIDs.has(note.id);
  // A note that is all text shows its text; a link note shows what the reader
  // wrote beside the link, and the page's description only when they wrote
  // nothing.
  const text = note.body || (first ? first.description : "");
  const long = text.length > 220 || text.split("\n").length > 4;
  // Only a page that could be a recipe offers to be one: a shop, a map or a
  // concert never is, and a button on every row would bury the ones that are.
  const promotable = first && (first.status === "page" || first.status === "failed") && recipeShaped.has(first.kind || "link");

  return (
    <article className={`notes-row${note.done_at ? " is-done" : ""}`}>
      <span className={`notes-row-icon kind-${info.kind}`} title={info.label}><Icon name={info.icon} /></span>
      <div className="notes-row-body">
        <div className="notes-row-title">
          {href
            ? <a href={href} target="_blank" rel="noopener noreferrer"><strong>{title}</strong></a>
            : <strong>{title}</strong>}
          {detail ? <span className="notes-detail">{detail}</span> : null}
        </div>
        <span className="notes-row-meta">
          {site ? <span>{site}</span> : null}
          {note.date_unix ? <span>{dateLabel(note.date_unix, datePrefs)}</span> : null}
          {note.message_id ? <a href={messageURL(note.message_id)} onClick={link(messageURL(note.message_id))}>{note.subject && first ? note.subject : "Open mail"}</a> : null}
          {note.filed ? <span>filed</span> : null}
          {first && first.status === "pending" ? <span>reading the page…</span> : null}
        </span>
        {text ? (
          <p className={`notes-row-text${note.body ? " is-own" : ""}${expanded ? " is-expanded" : ""}`}>{text}</p>
        ) : null}
        {long ? (
          <button className="ghost notes-more" type="button" onClick={() => setExpanded((value) => !value)}>{expanded ? "Less" : "More"}</button>
        ) : null}
        {first && first.error ? <p className="error-text">{first.error}</p> : null}
        {note.links.length > 1 ? (
          <ul className="notes-more-links">
            {note.links.slice(1).map((item) => (
              <li key={item.id}>
                <Icon name={kindInfo(item.kind).icon} />
                <a href={item.final_url || item.url} target="_blank" rel="noopener noreferrer">{item.title || hostOf(item.final_url || item.url)}</a>
              </li>
            ))}
          </ul>
        ) : null}
      </div>
      <div className="notes-row-actions">
        {note.done_at
          ? <button className="secondary" type="button" disabled={busy} onClick={() => onAction(note, "reopen")}>Reopen</button>
          : <button className="secondary" type="button" disabled={busy} onClick={() => onAction(note, "done")}><Icon name="check" />Done</button>}
        {promotable ? (
          <button className="ghost" type="button" disabled={busy} title="File this link as a recipe" onClick={() => onAction(note, "recipe", first)}><Icon name="cooking_pot" />Recipe</button>
        ) : null}
        {first && first.status === "failed" ? (
          <button className="ghost" type="button" disabled={busy} onClick={() => onAction(note, "retry", first)}><Icon name="sync" />Read again</button>
        ) : null}
        <button className="ghost icon-only" type="button" disabled={busy} title="Delete the mail (moves it to Trash)" aria-label="Delete" onClick={() => onAction(note, "delete")}><Icon name="delete" /></button>
        <button className="ghost icon-only" type="button" disabled={busy} title="Remove from the notes" aria-label="Remove from the notes" onClick={() => onAction(note, "remove")}><Icon name="close" /></button>
      </div>
    </article>
  );
}

/** The kinds of page a recipe can hide in without saying so. */
const recipeShaped = new Set(["article", "video", "social", "link"]);

/**
 * titleFromLink names a link whose page gave no title: a map pin by the place
 * in its address, a Google document by what it is, anything else by its host.
 */
function titleFromLink(link: string): string {
  try {
    const url = new URL(link);
    const place = url.pathname.match(/\/maps\/place\/([^/]+)/);
    if (place) return decodeURIComponent(place[1].replace(/\+/g, " "));
    if (url.hostname === "docs.google.com") {
      if (url.pathname.startsWith("/spreadsheets")) return "Google Sheets spreadsheet";
      if (url.pathname.startsWith("/presentation")) return "Google Slides presentation";
      if (url.pathname.startsWith("/forms")) return "Google Form";
      return "Google Docs document";
    }
    if (url.hostname === "drive.google.com") return "Google Drive file";
  } catch {
    // Not a URL the browser can read: the host fallback below answers.
  }
  return hostOf(link);
}

function firstLine(text: string): string {
  const line = text.split("\n").find((entry) => entry.trim());
  if (!line) return "";
  return line.length > 90 ? `${line.slice(0, 90).trim()}…` : line;
}

/** detailLabel prints a product's price as it is and an event's start as a date. */
function detailLabel(link: NoteLink, datePrefs: DatePrefs): string {
  if (!link.detail) return "";
  if (link.kind === "event") {
    // A bare date names a day, not an instant: read as UTC midnight it would
    // print the evening before for anyone west of Greenwich.
    const day = link.detail.match(/^(\d{4})-(\d{2})-(\d{2})$/);
    if (day) {
      const local = new Date(Number(day[1]), Number(day[2]) - 1, Number(day[3]));
      const options: Intl.DateTimeFormatOptions = { year: "numeric", month: "short", day: "numeric" };
      try {
        return local.toLocaleDateString(datePrefs.date_locale?.trim() || undefined, options);
      } catch {
        // A locale the browser does not know: its own default answers.
        return local.toLocaleDateString(undefined, options);
      }
    }
    const when = new Date(link.detail);
    if (!Number.isNaN(when.getTime())) return displayDateTime(when.toISOString(), datePrefs);
  }
  return link.detail;
}
