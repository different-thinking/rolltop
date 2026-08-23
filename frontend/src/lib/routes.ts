// File overview: Client-side route parser and URL builder. It owns the friendly mailbox/search
// slugs and preserves safe return URLs for message-detail back navigation.

import type { LocationState } from "../appTypes";

/** Read the browser URL into the tiny route state object used by App. */
export function currentLocation(): LocationState {
  return { path: window.location.pathname, search: window.location.search };
}

/** routeWithSearch preserves a path plus query string as a safe return URL candidate. */
export function routeWithSearch(path: string, search = ""): string {
  return `${path}${search}`;
}

function positiveInt(value: string | null | undefined, fallback: number): number {
  const raw = value || "";
  const number = raw.startsWith("p") ? raw.slice(1) : raw;
  const parsed = Number.parseInt(number, 10);
  return Number.isFinite(parsed) && parsed > 0 ? parsed : fallback;
}

function decodePathSegment(value = ""): string {
  try {
    return decodeURIComponent(value);
  } catch {
    return "";
  }
}

/**
 * MailView names a whole-account list. All Mail is the unnamed default, and the
 * rest live under /mail/<view>. The fixed views are spelled out because the
 * frontend builds those links itself; category views are any other name, since
 * the server owns that set and publishes it in the chrome payload. Listing the
 * categories here as well would mean a category could exist in the sidebar and
 * still 404 in the router until this file was edited to match.
 */
export type MailView = string;

/** The views this frontend constructs on its own, rather than reading from chrome. */
const fixedMailViews = ["inbox", "sent", "drafts"];

/**
 * legacyMailViews keeps older URLs working. The Inbox list shipped as
 * /mail/unarchived, so bookmarks, the installed app's cached shell, and open
 * tabs still name it that way. A Map rather than an object literal: a path
 * segment like `constructor` would otherwise resolve against Object.prototype
 * and travel on as the view name.
 */
const legacyMailViews = new Map<string, MailView>([["unarchived", "inbox"]]);

/**
 * defaultMailURL is the list the app opens on. Relevant is what is left once
 * the machine-generated traffic has been named, which is the mail a reader
 * arrives wanting to see; All Mail stays one click away in the sidebar.
 *
 * It is a category view, so it is spelled as a URL rather than assembled from
 * mailURL: the router deliberately does not carry its own copy of the category
 * set, and this is the one name the frontend needs to know before chrome has
 * loaded.
 */
export const defaultMailURL = "/mail/relevant";

/** mailViewCategory reports the category a view names, or "" for the rest. */
export function mailViewCategory(view: MailView): string {
  return view && !fixedMailViews.includes(view) ? view : "";
}

/** pageSegment matches the /pN marker, which is the one thing in the view slot that is not a view. */
const pageSegment = /^p\d+$/;

/** Parse /mail, /mail/pN, /mail/<view>(/pN), /mailbox/:id, and /mailbox/:id/pN into list state. */
export function mailRoute(path: string): { mailboxID: string | null; page: number; view: MailView } {
  const parts = path.split("/").filter(Boolean);
  if (parts[0] === "mailbox") {
    const id = positiveInt(parts[1], 0);
    return { mailboxID: id > 0 ? String(id) : null, page: positiveInt(parts[2], 1), view: "" };
  }
  if (parts[0] !== "mail") return { mailboxID: null, page: 1, view: "" };
  // Anything in the view slot that is not a page marker is a view name, mapped
  // through the legacy names first. The server decides whether the name is a
  // list it renders, which is what keeps this router from needing its own copy
  // of the category set.
  const segment = parts[1] && !pageSegment.test(parts[1]) ? parts[1] : "";
  const named = segment ? legacyMailViews.get(segment) || segment : "";
  if (named) return { mailboxID: null, page: positiveInt(parts[2], 1), view: named };
  return { mailboxID: null, page: positiveInt(parts[1], 1), view: "" };
}

/** mailURL builds the friendly mailbox or whole-account list URL for a page. */
export function mailURL(mailboxID: string | number | null, page = 1, view: MailView = ""): string {
  const suffix = page > 1 ? `/p${page}` : "";
  if (mailboxID) return `/mailbox/${mailboxID}${suffix}`;
  return view ? `/mail/${view}${suffix}` : `/mail${suffix}`;
}

/**
 * mailRouteView reports whether a path reaches mail to read - a conversation
 * list or a thread. It is the authority on that question rather than a summary
 * of one: RouteView tests it before considering any of its own special routes,
 * so the two cannot answer differently, and the shell reads it to decide which
 * views take the reading measure.
 *
 * It has to name the special routes exactly as RouteView guards them, not by
 * their prefix. RouteView claims `/settings/account`, not all of `/settings`,
 * and it claims the admin screens only for an admin - so a stale `/admin/users`
 * a reader can no longer open, and `/settings` on its own, both end on the mail
 * list, and a prefix test would leave those lists unmeasured.
 */
export function mailRouteView(path: string, isAdmin: boolean): boolean {
  const claimed = path === "/compose"
    || path === "/calendar" || path.startsWith("/calendar/")
    || path === "/contacts"
    || path === "/settings/account" || path.startsWith("/settings/account/")
    || ((path === "/admin/users" || path === "/admin/database") && isAdmin)
    || path === "/activity"
    || path.startsWith("/sync-runs/");
  return !claimed;
}

/** Parse /search/q/:query/pN slugs into search state. */
export function searchRoute(path: string): { query: string; page: number } {
  const parts = path.split("/").filter(Boolean);
  if (parts[0] === "search" && parts[1]?.startsWith("p")) {
    return { query: "", page: positiveInt(parts[1], 1) };
  }
  if (parts[0] !== "search" || parts[1] !== "q") return { query: "", page: 1 };
  const query = decodePathSegment(parts[2]);
  let page = 1;
  for (const part of parts.slice(3)) {
    if (part.startsWith("p")) page = positiveInt(part, page);
  }
  return { query, page };
}

/** searchURL builds the friendly search URL for a query/page pair. */
export function searchURL(query: string, page = 1): string {
  const trimmed = query.trim();
  if (!trimmed) return page > 1 ? `/search/p${page}` : "/search";
  const pagePart = page > 1 ? `/p${page}` : "";
  return `/search/q/${encodeURIComponent(trimmed)}${pagePart}`;
}

/** Keep back links internal before they are reflected into message URLs. */
export function safeInternalURL(value: string | null | undefined, fallback = "/mail"): string {
  if (!value) return fallback;
  try {
    const url = new URL(value, window.location.origin);
    if (url.origin !== window.location.origin) return fallback;
    return `${url.pathname}${url.search}${url.hash}`;
  } catch {
    return fallback;
  }
}

/**
 * messageBackURL extracts the safe return target a view was opened with. The
 * `back` parameter is one convention across message detail and compose, so it is
 * read in one place; the fallback differs, because a composer that was reached
 * without one has no conversation to fall back beside.
 */
export function messageBackURL(location: LocationState, fallback = "/mail"): string {
  return safeInternalURL(new URLSearchParams(location.search).get("back"), fallback);
}

/**
 * composeURL builds the full-page composer's URL, with the list to return to.
 *
 * It is here beside messageURL rather than at its call site because the two
 * spell the same `back` convention, and a caller assembling this by hand has to
 * get the separator right as well: `/compose` carries no query of its own until
 * something is being replied to.
 */
export function composeURL(options: { replyID?: number; draftID?: number; backURL?: string }): string {
  const params = new URLSearchParams();
  if (options.replyID) params.set("reply", String(options.replyID));
  if (options.draftID) params.set("draft", String(options.draftID));
  if (options.backURL) params.set("back", safeInternalURL(options.backURL));
  const query = params.toString();
  return query ? `/compose?${query}` : "/compose";
}

/** messageURL builds a message-detail URL with search highlight terms and back target. */
export function messageURL(messageID: number, searchQuery = "", matchTerms: string[] = [], backURL = "", searchHitID = 0): string {
  const query = searchQuery.trim();
  if (!query && matchTerms.length === 0 && !backURL && !searchHitID) return `/messages/${messageID}`;
  const params = new URLSearchParams();
  if (query) params.set("q", query);
  if (searchHitID > 0) params.set("hit", String(searchHitID));
  matchTerms.slice(0, 10).forEach((term) => {
    if (term.trim()) params.append("term", term.trim());
  });
  if (backURL) params.set("back", safeInternalURL(backURL));
  return `/messages/${messageID}?${params}`;
}

/** messageHighlightQuery returns the raw query used to highlight message-detail text. */
export function messageHighlightQuery(location: LocationState): string {
  const params = new URLSearchParams(location.search);
  return params.get("q") || params.get("highlight") || "";
}

/** messageSearchHitID returns the exact Bleve hit that opened this message view. */
export function messageSearchHitID(location: LocationState): number {
  const raw = new URLSearchParams(location.search).get("hit") || "";
  const id = Number.parseInt(raw, 10);
  return Number.isFinite(id) && id > 0 ? id : 0;
}

/** messageHighlightTerms returns explicit Bleve-reported terms carried by a message URL. */
export function messageHighlightTerms(location: LocationState): string[] {
  return new URLSearchParams(location.search).getAll("term");
}
