// File overview: User-scoped persistence for which sidebar account groups are collapsed.
// Storage is best-effort; a missing or corrupt entry means every account stays expanded.

const collapsedAccountsPrefix = "rolltop.sidebar.collapsedAccounts.v1.";
const maxStoredKeys = 200;

function collapsedAccountsStorageKey(userID: number): string {
  return `${collapsedAccountsPrefix}${userID}`;
}

export function loadCollapsedAccounts(userID: number): Set<string> {
  if (!Number.isInteger(userID) || userID <= 0) return new Set();
  try {
    const parsed = JSON.parse(localStorage.getItem(collapsedAccountsStorageKey(userID)) || "null") as unknown;
    if (Array.isArray(parsed)) {
      return new Set(parsed.filter((key): key is string => typeof key === "string").slice(0, maxStoredKeys));
    }
  } catch {
    return new Set();
  }
  return new Set();
}

export function saveCollapsedAccounts(userID: number, collapsed: Set<string>): void {
  if (!Number.isInteger(userID) || userID <= 0) return;
  try {
    if (collapsed.size === 0) {
      localStorage.removeItem(collapsedAccountsStorageKey(userID));
      return;
    }
    localStorage.setItem(collapsedAccountsStorageKey(userID), JSON.stringify(Array.from(collapsed).slice(0, maxStoredKeys)));
  } catch {
    // Quota or privacy-mode failures leave the sidebar working without persistence.
  }
}
