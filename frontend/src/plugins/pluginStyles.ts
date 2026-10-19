// File overview: Keeps each runtime plugin's stylesheet in the document, and
// keeps asking for one that did not arrive.
//
// A plugin's module and its stylesheet are two requests, and a page can get
// one without the other: a dropped connection on a train, or the server failing
// to answer for a moment. A module that loaded draws its markup either way, so a
// stylesheet the browser gave up on left that markup unstyled for as long as the
// page lived - the spam marker spelt out across a phone row instead of a shield
// beside the subject. A `<link>` never retries by itself, and setting the same
// href again does not refetch it, so a failed one is replaced by a new element.

const linkPrefix = "runtime-plugin-css-";
const retryBaseMS = 2_000;
const retryMaxMS = 5 * 60_000;

type LinkState = "loading" | "loaded" | "failed";

type Retry = {
  failures: number;
  timer: ReturnType<typeof setTimeout> | 0;
  onOnline: (() => void) | null;
};

// By link id. Survives the link it describes, which is replaced on each retry.
const retries = new Map<string, Retry>();

export function pluginStylesheetRetryDelay(failures: number): number {
  return Math.min(retryBaseMS * 3 ** Math.max(0, failures - 1), retryMaxMS);
}

export function syncPluginStylesheets(definitions: readonly { id: string; css_url?: string }[], doc: Document = document) {
  const wanted = new Map<string, string>();
  definitions.forEach((definition) => {
    if (definition.css_url) wanted.set(`${linkPrefix}${definition.id}`, absoluteURL(definition.css_url, doc));
  });
  doc.querySelectorAll<HTMLLinkElement>(`link[id^="${linkPrefix}"]`).forEach((link) => {
    if (!wanted.has(link.id)) {
      cancelRetry(link.id, doc);
      link.remove();
    }
  });
  wanted.forEach((href, id) => {
    const existing = doc.getElementById(id) as HTMLLinkElement | null;
    if (existing && existing.dataset.pluginHref === href) {
      // A stylesheet already in flight or applied is left alone; one that
      // failed is asked for again now rather than waiting out its backoff,
      // because a plugin reload is the page saying it wants the plugin.
      if (existing.dataset.loadState !== "failed") return;
      clearTimer(id, doc);
      attach(doc, id, href);
      return;
    }
    cancelRetry(id, doc);
    if (existing && existing.dataset.loadState === "loaded") {
      // A new build's stylesheet replaces the old one in place, so the old one
      // keeps applying until the new one has arrived.
      existing.dataset.pluginHref = href;
      setState(existing, "loading");
      existing.href = href;
      return;
    }
    attach(doc, id, href);
  });
}

function attach(doc: Document, id: string, href: string) {
  const link = doc.createElement("link");
  link.id = id;
  link.rel = "stylesheet";
  link.dataset.pluginHref = href;
  setState(link, "loading");
  // The handlers read the href off the element: a loaded stylesheet is pointed
  // at a new build's in place, and outlives the href it was created with.
  link.addEventListener("load", () => {
    if (!isCurrent(doc, link)) return;
    setState(link, "loaded");
    cancelRetry(id, doc);
  });
  link.addEventListener("error", () => {
    if (!isCurrent(doc, link) || !link.dataset.pluginHref) return;
    setState(link, "failed");
    scheduleRetry(doc, id, link.dataset.pluginHref);
  });
  const failures = retries.get(id)?.failures || 0;
  // The retry carries its own query so no cache between here and the server
  // can hand back the answer that failed.
  link.href = failures > 0 ? withRetryParam(href, failures) : href;
  const existing = doc.getElementById(id);
  if (existing) existing.replaceWith(link);
  else doc.head.appendChild(link);
}

function scheduleRetry(doc: Document, id: string, href: string) {
  const retry = retries.get(id) || { failures: 0, timer: 0, onOnline: null };
  retry.failures += 1;
  retries.set(id, retry);
  clearTimer(id, doc);
  const again = () => {
    clearTimer(id, doc);
    const link = doc.getElementById(id) as HTMLLinkElement | null;
    if (!link || link.dataset.pluginHref !== href || link.dataset.loadState !== "failed") return;
    attach(doc, id, href);
  };
  retry.timer = setTimeout(again, pluginStylesheetRetryDelay(retry.failures));
  const view = doc.defaultView;
  if (view) {
    retry.onOnline = again;
    view.addEventListener("online", again, { once: true });
  }
}

function clearTimer(id: string, doc: Document) {
  const retry = retries.get(id);
  if (!retry) return;
  if (retry.timer) clearTimeout(retry.timer);
  retry.timer = 0;
  if (retry.onOnline) doc.defaultView?.removeEventListener("online", retry.onOnline);
  retry.onOnline = null;
}

function cancelRetry(id: string, doc: Document) {
  clearTimer(id, doc);
  retries.delete(id);
}

function isCurrent(doc: Document, link: HTMLLinkElement): boolean {
  return doc.getElementById(link.id) === link;
}

function setState(link: HTMLLinkElement, state: LinkState) {
  link.dataset.loadState = state;
}

function absoluteURL(value: string, doc: Document): string {
  return new URL(value, doc.defaultView?.location.href || doc.baseURI).href;
}

function withRetryParam(href: string, attempt: number): string {
  const url = new URL(href);
  url.searchParams.set("retry", String(attempt));
  return url.href;
}
