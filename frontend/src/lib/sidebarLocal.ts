// File overview: User-scoped persistence for the sidebar's own state - which account groups are
// collapsed, and whether the sidebar is hidden altogether. Storage is best-effort; a missing or
// corrupt entry means every account stays expanded and the sidebar stays visible.

const collapsedAccountsPrefix = "rolltop.sidebar.collapsedAccounts.v1.";
const hiddenSidebarPrefix = "rolltop.sidebar.hidden.v1.";

function collapsedAccountsStorageKey(userID: number): string {
  return `${collapsedAccountsPrefix}${userID}`;
}

function hiddenSidebarStorageKey(userID: number): string {
  return `${hiddenSidebarPrefix}${userID}`;
}

function positiveUserID(userID: number): boolean {
  return Number.isInteger(userID) && userID > 0;
}

export function loadCollapsedAccounts(userID: number): Set<string> {
  if (!positiveUserID(userID)) return new Set();
  try {
    const parsed = JSON.parse(localStorage.getItem(collapsedAccountsStorageKey(userID)) || "null") as unknown;
    if (Array.isArray(parsed)) return new Set(parsed.filter((key): key is string => typeof key === "string"));
  } catch {
    return new Set();
  }
  return new Set();
}

export function saveCollapsedAccounts(userID: number, collapsed: Set<string>): void {
  if (!positiveUserID(userID)) return;
  try {
    if (collapsed.size === 0) {
      localStorage.removeItem(collapsedAccountsStorageKey(userID));
      return;
    }
    localStorage.setItem(collapsedAccountsStorageKey(userID), JSON.stringify(Array.from(collapsed)));
  } catch {
    // Quota or privacy-mode failures leave the sidebar working without persistence.
  }
}

/**
 * loadSidebarHidden reports whether this reader hid the sidebar. Only an explicit
 * "true" hides it, so an unreadable or absent entry opens the app with the
 * folders in view rather than with a shell the reader has to discover a button
 * to fill.
 */
export function loadSidebarHidden(userID: number): boolean {
  if (!positiveUserID(userID)) return false;
  try {
    return localStorage.getItem(hiddenSidebarStorageKey(userID)) === "true";
  } catch {
    return false;
  }
}

export function saveSidebarHidden(userID: number, hidden: boolean): void {
  if (!positiveUserID(userID)) return;
  try {
    if (!hidden) {
      localStorage.removeItem(hiddenSidebarStorageKey(userID));
      return;
    }
    localStorage.setItem(hiddenSidebarStorageKey(userID), "true");
  } catch {
    // Quota or privacy-mode failures leave the sidebar working without persistence.
  }
}

/** clearOtherSidebarState drops sidebar state belonging to other users on a shared browser. */
export function clearOtherSidebarState(userID: number): void {
  const keep = positiveUserID(userID)
    ? [collapsedAccountsStorageKey(userID), hiddenSidebarStorageKey(userID)]
    : [];
  try {
    const stale: string[] = [];
    for (let index = 0; index < localStorage.length; index++) {
      const key = localStorage.key(index);
      const owned = key && (key.startsWith(collapsedAccountsPrefix) || key.startsWith(hiddenSidebarPrefix));
      if (owned && !keep.includes(key)) stale.push(key);
    }
    stale.forEach((key) => localStorage.removeItem(key));
  } catch {
    // Storage access failures leave the stale entries in place; they are inert.
  }
}
