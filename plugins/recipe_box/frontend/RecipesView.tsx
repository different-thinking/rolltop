// File overview: The cookbook -- every recipe the reader mailed themselves, as
// a recipe rather than as a mail with a link in it.
//
// Two pages share one route subtree: the cards (/apps/recipes) and one recipe
// opened (/apps/recipes/<id>). The links that did not turn out to be a recipe
// are notes now, with an overview of their own (/apps/notes).

import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import type { FormEvent, MouseEvent, ReactNode } from "react";
import type { DatePrefs } from "../../../frontend/src/appTypes";
import { Icon } from "../../../frontend/src/components/Icon";
import { displayDateTime } from "../../../frontend/src/lib/format";
import { messageURL } from "../../../frontend/src/lib/routes";
import type { RuntimeAppViewContext } from "../../../frontend/src/plugins/runtime";
import {
  hostOf,
  messageFromError,
  minutesLabel,
  recipeAPI,
  type RecipeCounts,
  type RecipeDetail,
  type RecipeSummary,
  type RecipeView,
  type SettingsResponse
} from "./api";

export const recipesPath = "/apps/recipes";
export const settingsPath = "/settings/account/plugins/recipes";
export const notesPath = "/apps/notes";

/** A dish counts as quick at this many minutes or fewer. */
const quickMinutes = 30;

export function RecipesView(context: RuntimeAppViewContext) {
  const rest = context.location.path.slice(recipesPath.length).replace(/^\/+|\/+$/g, "");
  if (rest === "links") return <MovedToNotes navigate={context.navigate} />;
  const id = Number(rest);
  if (rest && Number.isInteger(id) && id > 0) return <RecipePage key={id} id={id} {...context} />;
  return <CookbookPage {...context} />;
}

/** MovedToNotes answers the old not-recognised list, whose links are notes now. */
function MovedToNotes({ navigate }: { navigate: (url: string) => void }) {
  useEffect(() => {
    navigate(notesPath);
  }, [navigate]);
  return null;
}

export function useLink(navigate: (url: string) => void) {
  return (url: string) => (event: MouseEvent) => {
    if (event.button !== 0 || event.metaKey || event.ctrlKey || event.shiftKey || event.altKey) return;
    event.preventDefault();
    navigate(url);
  };
}

export function dateLabel(unix: number, prefs: DatePrefs): string {
  if (!unix) return "";
  return displayDateTime(new Date(unix * 1000).toISOString(), prefs);
}

/**
 * useStatus reads the box's progress. While a scan or a fetch is still
 * running the page asks again on a short interval, which is the one thing
 * here that cannot wait for a mail change: links resolve without any mail
 * arriving.
 */
export function useStatus(mailGeneration: number) {
  const [status, setStatus] = useState<SettingsResponse | null>(null);
  const [tick, setTick] = useState(0);
  useEffect(() => {
    let cancelled = false;
    recipeAPI.settings().then((next) => {
      if (!cancelled) setStatus(next);
    }).catch(() => undefined);
    return () => {
      cancelled = true;
    };
  }, [mailGeneration, tick]);
  const busy = Boolean(status && status.settings.enabled && (
    !status.settings.scan_finished_at || (status.status.links.pending || 0) > 0
  ));
  useEffect(() => {
    if (!busy) return;
    const timer = window.setTimeout(() => setTick((value) => value + 1), 8000);
    return () => window.clearTimeout(timer);
  }, [busy, tick]);
  return { status, busy, tick };
}

/**
 * useCurrentLoad keeps one view's list current. A change of what is asked for
 * (`key`: the search, the shelf, a reload after an action) supersedes the
 * request in flight, so a slow earlier answer never draws over a later
 * question. A background refresh (`refresh`: the status tick, a mail change)
 * does not: it waits for the request in flight and is asked once after it.
 * Superseding on every refresh meant a list slower to arrive than the status
 * tick never arrived at all -- a phone on a mobile connection showed
 * "Loading…" for as long as the box was busy.
 */
export function useCurrentLoad<T>(key: string, refresh: string, load: () => Promise<T>, apply: (value: T) => void, fail: (err: unknown) => void) {
  const handlers = useRef({ load, apply, fail });
  handlers.current = { load, apply, fail };
  const latest = useRef(0);
  const inFlight = useRef(false);
  const again = useRef(false);
  const [round, setRound] = useState(0);

  useEffect(() => {
    const id = ++latest.current;
    inFlight.current = true;
    again.current = false;
    handlers.current.load().then((value) => {
      if (latest.current === id) handlers.current.apply(value);
    }, (err) => {
      if (latest.current === id) handlers.current.fail(err);
    }).finally(() => {
      if (latest.current !== id) return;
      inFlight.current = false;
      if (again.current) {
        again.current = false;
        setRound((value) => value + 1);
      }
    });
  }, [key, round]);

  const lastRefresh = useRef(refresh);
  useEffect(() => {
    if (lastRefresh.current === refresh) return;
    lastRefresh.current = refresh;
    if (inFlight.current) again.current = true;
    else setRound((value) => value + 1);
  }, [refresh]);

  // An answer landing after the view is gone is dropped.
  useEffect(() => () => {
    latest.current++;
  }, []);
}

function CookbookPage({ csrf, navigate, addToast, mailGeneration }: RuntimeAppViewContext) {
  const [query, setQuery] = useState("");
  const [appliedQuery, setAppliedQuery] = useState("");
  const [view, setView] = useState<RecipeView>("");
  const [recipes, setRecipes] = useState<RecipeSummary[] | null>(null);
  const [counts, setCounts] = useState<RecipeCounts | null>(null);
  const [error, setError] = useState("");
  const [filter, setFilter] = useState("");
  const [adding, setAdding] = useState(false);
  const [addURL, setAddURL] = useState("");
  const [addBusy, setAddBusy] = useState(false);
  const { status, busy, tick } = useStatus(mailGeneration);
  const link = useLink(navigate);

  useEffect(() => {
    const timer = window.setTimeout(() => setAppliedQuery(query), 250);
    return () => window.clearTimeout(timer);
  }, [query]);

  // Each load answers only for the query and view it was asked with: typing
  // fires one per pause, and a slow earlier answer landing after a later one
  // would otherwise draw the results of a search the reader already changed.
  const [reloadKey, setReloadKey] = useState(0);
  const load = useCallback(() => setReloadKey((value) => value + 1), []);

  useCurrentLoad(
    JSON.stringify([appliedQuery, view, reloadKey]),
    `${mailGeneration}:${tick}`,
    () => recipeAPI.recipes(appliedQuery, view),
    (next) => {
      setRecipes(next.recipes);
      setCounts(next.counts || null);
      setError("");
    },
    (err) => setError(messageFromError(err)),
  );

  const filters = useMemo(() => recipeFilters(recipes || []), [recipes]);
  const shown = useMemo(() => (recipes || []).filter((recipe) => matchesFilter(recipe, filter)), [recipes, filter]);
  const enabled = status?.settings.enabled;
  const notes = status?.status.notes || 0;

  async function add(event: FormEvent) {
    event.preventDefault();
    if (!addURL.trim()) return;
    setAddBusy(true);
    try {
      const result = await recipeAPI.addRecipe(csrf, addURL.trim());
      setAddURL("");
      setAdding(false);
      addToast(result.status === "recipe" ? "That recipe is already in the cookbook." : "Added. The recipe appears as soon as its page has answered.");
      void load();
    } catch (err) {
      addToast(messageFromError(err), "error");
    } finally {
      setAddBusy(false);
    }
  }

  return (
    <div className="recipe-box">
      <div className="content-head recipe-box-head">
        <div>
          <h1>Recipes</h1>
          {recipes ? <span className="label-pill">{recipes.length.toLocaleString()}{viewSuffix[view]}</span> : null}
        </div>
        <div className="recipe-box-head-actions">
          <button className="secondary" type="button" onClick={() => setAdding((value) => !value)}>
            <Icon name="add" />Add link
          </button>
          <a className="icon-button" href={settingsPath} title="Recipe box settings" aria-label="Recipe box settings" onClick={link(settingsPath)}>
            <Icon name="settings" />
          </a>
        </div>
      </div>

      {adding ? (
        <form className="panel recipe-add" onSubmit={add}>
          <label>
            <span className="settings-field-label">Recipe page</span>
            <input type="url" value={addURL} onChange={(event) => setAddURL(event.target.value)} placeholder="https://…" autoFocus required />
          </label>
          <button type="submit" disabled={addBusy}>{addBusy ? "Adding…" : "Add"}</button>
        </form>
      ) : null}

      {status && !enabled ? (
        <section className="panel recipe-idle">
          <Icon name="cooking_pot" />
          <div>
            <strong>The recipe box is switched off.</strong>
            <p>
              Switch it on and Rolltop looks through the mail you sent yourself for links to recipes, keeps each
              recipe here with its picture, ingredients and steps, and files those mails out of your Inbox.
            </p>
            <a className="button" href={settingsPath} onClick={link(settingsPath)}>Set it up</a>
          </div>
        </section>
      ) : null}

      {enabled && busy ? (
        <p className="muted recipe-progress">
          <span className="spinner" aria-hidden="true" />
          {status && !status.settings.scan_finished_at
            ? `Looking through your mail… ${percent(status.status.messages_scanned, status.status.messages_total)}`
            : `Reading ${(status?.status.links.pending || 0).toLocaleString()} shared ${(status?.status.links.pending || 0) === 1 ? "page" : "pages"}…`}
        </p>
      ) : null}

      <div className="recipe-toolbar">
        <label className="recipe-search">
          <Icon name="search" />
          <input
            type="search"
            value={query}
            onChange={(event) => setQuery(event.target.value)}
            placeholder="Search dishes and ingredients"
            aria-label="Search recipes"
          />
        </label>
        <div className="recipe-tabs" role="tablist">
          {recipeViews.map((entry) => {
            const count = counts ? counts[entry.count] : 0;
            return (
              <button key={entry.view} type="button" role="tab" aria-selected={view === entry.view} className={view === entry.view ? "active" : ""} onClick={() => { setView(entry.view); setFilter(""); }}>
                {entry.label}{count ? <span className="recipe-tab-count">{count.toLocaleString()}</span> : null}
              </button>
            );
          })}
          <a className={`recipe-tab-link${notes ? "" : " empty"}`} href={notesPath} onClick={link(notesPath)}>
            <Icon name="draft" />Notes{notes ? <span className="recipe-tab-count">{notes.toLocaleString()}</span> : null}
          </a>
        </div>
      </div>

      {filters.length > 0 && view !== "dismissed" ? (
        <div className="recipe-filters" aria-label="Filter recipes">
          <button type="button" className={filter === "" ? "active" : ""} onClick={() => setFilter("")}>All</button>
          {filters.map((item) => (
            <button type="button" key={item.key} className={filter === item.key ? "active" : ""} onClick={() => setFilter(filter === item.key ? "" : item.key)}>
              {item.label}
            </button>
          ))}
        </div>
      ) : null}

      {error ? <div className="error">{error}</div> : null}
      {recipes === null && !error ? <p className="muted">Loading…</p> : null}
      {recipes !== null && shown.length === 0 && enabled ? (
        <p className="muted recipe-empty">
          {appliedQuery || filter
            ? "No recipe matches."
            : view === "dismissed"
              ? "Nothing removed."
              : view === "cooked"
                ? "Nothing cooked yet. Open a recipe you made and mark it cooked."
                : (counts?.cooked || 0) > 0
                  ? "Everything here has been cooked. Send yourself the next recipe."
                  : "No recipes yet. Send yourself a link to a recipe, and it appears here."}
        </p>
      ) : null}

      <div className="recipe-grid">
        {shown.map((recipe) => (
          <RecipeCard key={recipe.id} recipe={recipe} href={`${recipesPath}/${recipe.id}`} onOpen={link(`${recipesPath}/${recipe.id}`)} />
        ))}
      </div>
    </div>
  );
}

/** The cookbook's three shelves: what is still to try, what was made, and what was taken out. */
const recipeViews: { view: RecipeView; label: string; count: keyof RecipeCounts }[] = [
  { view: "", label: "To cook", count: "to_cook" },
  { view: "cooked", label: "Cooked", count: "cooked" },
  { view: "dismissed", label: "Removed", count: "dismissed" }
];

const viewSuffix: Record<RecipeView, string> = { "": " to cook", cooked: " cooked", dismissed: " removed" };

function percent(done: number, total: number): string {
  if (!total) return "";
  return `${Math.min(100, Math.floor((done / total) * 100))}%`;
}

type FilterOption = { key: string; label: string };

/**
 * recipeFilters offers the ways the reader's own collection is most often
 * filed, read off the recipes themselves: the categories and cuisines their
 * sites gave them, most common first, and a quick-dish filter when the times
 * say there are any. A filter only one recipe would match is not offered.
 */
function recipeFilters(recipes: RecipeSummary[]): FilterOption[] {
  const counts = new Map<string, { label: string; count: number }>();
  for (const recipe of recipes) {
    const seen = new Set<string>();
    for (const term of [...recipe.categories, ...recipe.cuisines]) {
      const key = term.trim().toLowerCase();
      if (!key || seen.has(key)) continue;
      seen.add(key);
      const entry = counts.get(key) || { label: term.trim(), count: 0 };
      entry.count += 1;
      counts.set(key, entry);
    }
  }
  const options: FilterOption[] = [];
  const quick = recipes.filter((recipe) => recipe.total_minutes > 0 && recipe.total_minutes <= quickMinutes).length;
  if (quick > 1) options.push({ key: "@quick", label: `Up to ${quickMinutes} min` });
  return options.concat(
    Array.from(counts.entries())
      .filter(([, entry]) => entry.count > 1)
      .sort((a, b) => b[1].count - a[1].count || a[1].label.localeCompare(b[1].label))
      .slice(0, 10)
      .map(([key, entry]) => ({ key, label: entry.label }))
  );
}

function matchesFilter(recipe: RecipeSummary, filter: string): boolean {
  if (!filter) return true;
  if (filter === "@quick") return recipe.total_minutes > 0 && recipe.total_minutes <= quickMinutes;
  return [...recipe.categories, ...recipe.cuisines].some((term) => term.trim().toLowerCase() === filter);
}

function RecipeCard({ recipe, href, onOpen }: { recipe: RecipeSummary; href: string; onOpen: (event: MouseEvent) => void }) {
  const time = minutesLabel(recipe.total_minutes);
  const summary = recipe.note || recipe.description;
  return (
    <a className={`recipe-card${recipe.cooked_at ? " is-cooked" : ""}`} href={href} onClick={onOpen}>
      <div className="recipe-card-image">
        {recipe.image_url ? <img src={recipe.image_url} alt="" loading="lazy" /> : <Icon name="cooking_pot" />}
        {recipe.cooked_at ? <span className="recipe-cooked-badge"><Icon name="check" />Cooked</span> : null}
      </div>
      <div className="recipe-card-body">
        <strong className="recipe-card-title">{recipe.title || hostOf(recipe.url)}</strong>
        <span className="recipe-card-site">{recipe.site_name || hostOf(recipe.url)}</span>
        {summary ? <p className={`recipe-card-summary${recipe.note ? " is-note" : ""}`}>{summary}</p> : null}
        <span className="recipe-card-meta">
          {time ? <span><Icon name="timer" />{time}</span> : null}
          {recipe.yield ? <span><Icon name="fork_knife" />{recipe.yield}</span> : null}
          {recipe.ingredient_count ? <span>{recipe.ingredient_count} ingredients</span> : null}
        </span>
      </div>
    </a>
  );
}

function RecipePage({ id, csrf, navigate, addToast, datePrefs, mailGeneration }: RuntimeAppViewContext & { id: number }) {
  const [recipe, setRecipe] = useState<RecipeDetail | null>(null);
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const [checked, setChecked] = useState<Set<number>>(() => new Set());
  const link = useLink(navigate);
  const headingRef = useRef<HTMLHeadingElement>(null);

  useEffect(() => {
    let cancelled = false;
    recipeAPI.recipe(id).then((next) => {
      if (cancelled) return;
      setRecipe(next.recipe);
      setError("");
    }).catch((err) => {
      if (!cancelled) setError(messageFromError(err));
    });
    return () => {
      cancelled = true;
    };
  }, [id, mailGeneration]);

  useEffect(() => {
    if (recipe) headingRef.current?.focus();
  }, [recipe?.id]);

  async function setDismissed(dismissed: boolean) {
    if (!recipe) return;
    setBusy(true);
    try {
      const next = await recipeAPI.setDismissed(csrf, recipe.id, dismissed);
      setRecipe(next.recipe);
      addToast(dismissed ? "Removed from the cookbook." : "Back in the cookbook.");
      if (dismissed) navigate(recipesPath);
    } catch (err) {
      addToast(messageFromError(err), "error");
    } finally {
      setBusy(false);
    }
  }

  async function setCooked(cooked: boolean) {
    if (!recipe) return;
    setBusy(true);
    try {
      const next = await recipeAPI.setCooked(csrf, recipe.id, cooked);
      setRecipe(next.recipe);
      addToast(cooked ? "Marked as cooked." : "Back on the list to cook.");
    } catch (err) {
      addToast(messageFromError(err), "error");
    } finally {
      setBusy(false);
    }
  }

  function toggleIngredient(index: number) {
    setChecked((current) => {
      const next = new Set(current);
      if (next.has(index)) next.delete(index);
      else next.add(index);
      return next;
    });
  }

  const back = (
    <a className="recipe-back" href={recipesPath} onClick={link(recipesPath)}>
      <Icon name="arrow_back" />Recipes
    </a>
  );
  if (error) return <div className="recipe-box">{back}<div className="error">{error}</div></div>;
  if (!recipe) return <div className="recipe-box">{back}<p className="muted">Loading…</p></div>;

  const facts: { label: string; value: ReactNode }[] = [];
  if (recipe.total_minutes) facts.push({ label: "Total", value: minutesLabel(recipe.total_minutes) });
  if (recipe.prep_minutes) facts.push({ label: "Prep", value: minutesLabel(recipe.prep_minutes) });
  if (recipe.cook_minutes) facts.push({ label: "Cook", value: minutesLabel(recipe.cook_minutes) });
  if (recipe.yield) facts.push({ label: "Makes", value: recipe.yield });
  if (recipe.calories) facts.push({ label: "Energy", value: recipe.calories });
  if (recipe.rating_value > 0) {
    facts.push({
      label: "Rating",
      value: <>{recipe.rating_value.toFixed(1)} / 5{recipe.rating_count ? <small> ({recipe.rating_count.toLocaleString()})</small> : null}</>
    });
  }
  const notes = Array.from(new Set(recipe.mails.map((mail) => mail.note).filter(Boolean)));
  const tags = Array.from(new Set([...recipe.categories, ...recipe.cuisines, ...recipe.keywords])).slice(0, 14);

  return (
    <div className="recipe-box recipe-detail">
      {back}
      <article className="panel recipe-detail-card">
        <header className="recipe-detail-head">
          {recipe.image_url ? <img className="recipe-detail-image" src={recipe.image_url} alt="" /> : null}
          <div className="recipe-detail-heading">
            <h1 ref={headingRef} tabIndex={-1}>{recipe.title || hostOf(recipe.url)}</h1>
            <p className="recipe-detail-source">
              {recipe.site_name || hostOf(recipe.url)}
              {recipe.author ? ` · ${recipe.author}` : ""}
            </p>
            {recipe.description ? <p className="recipe-detail-description">{recipe.description}</p> : null}
            {notes.map((note) => (
              <blockquote className="recipe-detail-note" key={note}><Icon name="draft" />{note}</blockquote>
            ))}
            {recipe.cooked_at ? (
              <p className="recipe-cooked-line"><Icon name="check" />Cooked {dateLabel(recipe.cooked_at, datePrefs)}</p>
            ) : null}
            <div className="recipe-detail-actions">
              {recipe.cooked_at
                ? <button className="secondary" type="button" disabled={busy} onClick={() => void setCooked(false)}>Not cooked yet</button>
                : <button type="button" disabled={busy || recipe.dismissed} onClick={() => void setCooked(true)}><Icon name="check" />Mark as cooked</button>}
              <a className="button secondary" href={recipe.url} target="_blank" rel="noopener noreferrer"><Icon name="open_in_new" />Open original</a>
              {recipe.dismissed
                ? <button className="secondary" type="button" disabled={busy} onClick={() => void setDismissed(false)}>Put back in the cookbook</button>
                : <button className="secondary" type="button" disabled={busy} onClick={() => void setDismissed(true)}><Icon name="delete" />Not a recipe</button>}
            </div>
          </div>
        </header>

        {facts.length > 0 ? (
          <dl className="recipe-facts">
            {facts.map((fact) => (
              <div key={fact.label}><dt>{fact.label}</dt><dd>{fact.value}</dd></div>
            ))}
          </dl>
        ) : null}

        {tags.length > 0 ? <div className="recipe-tags">{tags.map((tag) => <span key={tag}>{tag}</span>)}</div> : null}

        <div className="recipe-detail-columns">
          {recipe.ingredients.length > 0 ? (
            <section className="recipe-ingredients">
              <h2>Ingredients</h2>
              <ul>
                {recipe.ingredients.map((item, index) => (
                  <li key={`${index}-${item}`}>
                    <label className={checked.has(index) ? "done" : ""}>
                      <input type="checkbox" checked={checked.has(index)} onChange={() => toggleIngredient(index)} />
                      <span>{item}</span>
                    </label>
                  </li>
                ))}
              </ul>
            </section>
          ) : null}
          {recipe.instructions.length > 0 ? (
            <section className="recipe-steps">
              <h2>Method</h2>
              {recipe.instructions.map((section, sectionIndex) => (
                <div key={`${sectionIndex}-${section.name}`}>
                  {section.name ? <h3>{section.name}</h3> : null}
                  <ol>{section.steps.map((step, index) => <li key={`${index}-${step.slice(0, 20)}`}>{step}</li>)}</ol>
                </div>
              ))}
            </section>
          ) : null}
        </div>
        {recipe.origin === "manual" ? (
          <p className="muted">This page publishes no recipe of its own, so only its preview is kept. The full recipe is on the original page.</p>
        ) : null}
      </article>

      {recipe.mails.length > 0 ? (
        <section className="panel recipe-mails">
          <h2>Shared in</h2>
          <ul>
            {recipe.mails.map((mail, index) => (
              <li key={`${mail.message_id}-${index}`}>
                {mail.message_id ? (
                  <a href={messageURL(mail.message_id)} onClick={link(messageURL(mail.message_id))}>{mail.subject || "(no subject)"}</a>
                ) : <span>{mail.subject || "(no subject)"}</span>}
                <small className="muted">
                  {dateLabel(mail.date_unix, datePrefs)}
                  {mail.move_state === "moved" ? " · filed" : ""}
                  {mail.move_state === "failed" ? ` · could not be filed${mail.move_error ? `: ${mail.move_error}` : ""}` : ""}
                </small>
              </li>
            ))}
          </ul>
        </section>
      ) : null}
    </div>
  );
}
