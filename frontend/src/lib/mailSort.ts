// File overview: User-scoped persistence for the list orders a reader picks. The mail list's date
// direction follows the reader across All Mail and every folder; the search results list keeps its
// own choice, because best match is a search-only order and the two lists answer different
// questions. A missing or corrupt entry means each list's own default.

/** MailSortOrder is the date direction of a conversation list page. */
export type MailSortOrder = "newest" | "oldest";

export const defaultMailSortOrder: MailSortOrder = "newest";

/**
 * SearchSortOrder is the order of a search results page. Best match is the
 * ranking search has always drawn, and the two date orders replace it.
 */
export type SearchSortOrder = "best" | "newest" | "oldest";

export const defaultSearchSortOrder: SearchSortOrder = "best";

const sortOrderPrefix = "rolltop.mail.sortOrder.v1.";
const searchSortOrderPrefix = "rolltop.search.sortOrder.v1.";

function sortOrderStorageKey(userID: number): string {
  return `${sortOrderPrefix}${userID}`;
}

function searchSortOrderStorageKey(userID: number): string {
  return `${searchSortOrderPrefix}${userID}`;
}

function positiveUserID(userID: number): boolean {
  return Number.isInteger(userID) && userID > 0;
}

/** mailSortOrder narrows any stored or server-reported value to a known order. */
export function mailSortOrder(value: unknown): MailSortOrder {
  return value === "oldest" ? "oldest" : defaultMailSortOrder;
}

export function loadMailSortOrder(userID: number): MailSortOrder {
  if (!positiveUserID(userID)) return defaultMailSortOrder;
  try {
    return mailSortOrder(localStorage.getItem(sortOrderStorageKey(userID)));
  } catch {
    return defaultMailSortOrder;
  }
}

export function saveMailSortOrder(userID: number, order: MailSortOrder): void {
  if (!positiveUserID(userID)) return;
  try {
    if (order === defaultMailSortOrder) {
      localStorage.removeItem(sortOrderStorageKey(userID));
      return;
    }
    localStorage.setItem(sortOrderStorageKey(userID), order);
  } catch {
    // Quota or privacy-mode failures leave sorting working without persistence.
  }
}

/** searchSortOrder narrows any stored or server-reported value to a known order. */
export function searchSortOrder(value: unknown): SearchSortOrder {
  return value === "newest" || value === "oldest" ? value : defaultSearchSortOrder;
}

export function loadSearchSortOrder(userID: number): SearchSortOrder {
  if (!positiveUserID(userID)) return defaultSearchSortOrder;
  try {
    return searchSortOrder(localStorage.getItem(searchSortOrderStorageKey(userID)));
  } catch {
    return defaultSearchSortOrder;
  }
}

export function saveSearchSortOrder(userID: number, order: SearchSortOrder): void {
  if (!positiveUserID(userID)) return;
  try {
    if (order === defaultSearchSortOrder) {
      localStorage.removeItem(searchSortOrderStorageKey(userID));
      return;
    }
    localStorage.setItem(searchSortOrderStorageKey(userID), order);
  } catch {
    // Quota or privacy-mode failures leave sorting working without persistence.
  }
}

/** clearOtherMailSortOrders drops the list preferences of other users on a shared browser. */
export function clearOtherMailSortOrders(keepUserID: number): void {
  const known = positiveUserID(keepUserID);
  const keep = new Set(known ? [sortOrderStorageKey(keepUserID), searchSortOrderStorageKey(keepUserID)] : []);
  try {
    const stale: string[] = [];
    for (let index = 0; index < localStorage.length; index++) {
      const key = localStorage.key(index);
      if (!key || keep.has(key)) continue;
      if (key.startsWith(sortOrderPrefix) || key.startsWith(searchSortOrderPrefix)) stale.push(key);
    }
    stale.forEach((key) => localStorage.removeItem(key));
  } catch {
    // Storage access failures leave the stale entries in place; they are inert.
  }
}
