// File overview: User-scoped persistence for the mail list's date direction. The choice follows the
// reader across All Mail and every folder, and a missing or corrupt entry means newest first.

/** MailSortOrder is the date direction of a conversation list page. */
export type MailSortOrder = "newest" | "oldest";

export const defaultMailSortOrder: MailSortOrder = "newest";

const sortOrderPrefix = "rolltop.mail.sortOrder.v1.";

function sortOrderStorageKey(userID: number): string {
  return `${sortOrderPrefix}${userID}`;
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

/** clearOtherMailSortOrders drops the list preference of other users on a shared browser. */
export function clearOtherMailSortOrders(keepUserID: number): void {
  const keep = positiveUserID(keepUserID) ? sortOrderStorageKey(keepUserID) : "";
  try {
    const stale: string[] = [];
    for (let index = 0; index < localStorage.length; index++) {
      const key = localStorage.key(index);
      if (key && key.startsWith(sortOrderPrefix) && key !== keep) stale.push(key);
    }
    stale.forEach((key) => localStorage.removeItem(key));
  } catch {
    // Storage access failures leave the stale entries in place; they are inert.
  }
}
