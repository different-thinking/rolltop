// File overview: Turning an address line into the two things the UI derives from
// it — the letter an avatar shows and a stable hash for that avatar's colour.
// Both live here because the list, the thread header and the contacts pane must
// agree: the same correspondent has to keep one letter and one colour wherever
// they appear.

/**
 * displayLabel reduces an address line to the text a human reads, mirroring the
 * server's senderDisplayName (backend/web/conversations.go): the display name
 * wins, the bare address stands in when there is none. A participant list is
 * reduced to its first entry, which is the one the row names first.
 */
function displayLabel(value: string): string {
  const first = (value.split(",")[0] || "").trim();
  if (!first) return "";
  const angled = first.match(/^(.*)<([^>]*)>\s*$/);
  if (angled) {
    const name = angled[1].trim().replace(/^"(.*)"$/, "$1").trim();
    return name || angled[2].trim();
  }
  return first.replace(/^"(.*)"$/, "$1").trim();
}

/**
 * displayInitial is the avatar letter for an address line. It follows the
 * server's senderInitial rather than slicing the raw string, so a sender written
 * as `"Bob" <bob@example.test>` reads as B in every view instead of as a quote
 * mark in one and a B in another.
 */
export function displayInitial(value: string): string {
  const match = displayLabel(value).match(/[\p{L}\p{N}]/u);
  return match ? match[0].toUpperCase() : "?";
}

/**
 * stableHash is FNV-1a over a string. It is the app's one string hash: callers
 * that need a value to stay put across reloads and releases (an avatar colour, a
 * per-context storage key) depend on it never changing, so there is deliberately
 * only one implementation to keep stable.
 */
export function stableHash(value: string): number {
  let hash = 2166136261;
  for (let index = 0; index < value.length; index += 1) {
    hash ^= value.charCodeAt(index);
    hash = Math.imul(hash, 16777619);
  }
  return hash >>> 0;
}
