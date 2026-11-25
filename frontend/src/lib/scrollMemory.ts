// File overview: Where the page was scrolled to on each route, so returning to
// a list puts the reader back where they left it.

import type { LocationState } from "../appTypes";
import { messageBackURL, routeWithSearch } from "./routes";

/**
 * The page itself is what scrolls - the list and an open message share the one
 * document scroll position - and every in-app "back" is a push of the list's URL
 * rather than a history step, so the browser's own scroll restoration never gets
 * a say. Without this the list remounted at whatever offset the reader had
 * scrolled the *message* to: open a mail, read down to its end, press Delete,
 * and the list behind it sat somewhere else entirely.
 *
 * The memory is per URL, in memory only, and bounded: it is a convenience for
 * the session, not state anybody else reads.
 */
const scrollPositions = new Map<string, number>();
const scrollMemoryLimit = 50;

/**
 * restoreBudgetMS bounds how long a restore keeps trying. A list remounts from
 * its cached page most of the time and is tall enough at once, but a page that
 * has to load first is short until it arrives, and scrolling it early only
 * reaches as far as its loading state.
 */
const restoreBudgetMS = 3000;

let activeRestore: (() => void) | null = null;

export function locationKey(location: LocationState): string {
  return routeWithSearch(location.path, location.search);
}

/** rememberScroll records where a route was scrolled to as the reader leaves it. */
export function rememberScroll(key: string, y: number) {
  scrollPositions.delete(key);
  scrollPositions.set(key, Math.max(0, Math.round(y)));
  while (scrollPositions.size > scrollMemoryLimit) {
    const oldest = scrollPositions.keys().next().value;
    if (oldest === undefined) break;
    scrollPositions.delete(oldest);
  }
}

/**
 * rememberScrollLeaving records the position of the route being left, under
 * its own URL and under the `back` the route being opened carries. The list
 * spells that `back` itself and it need not be the address bar's spelling of
 * the same list (a default folder or page made explicit), and it is the `back`
 * that the return will push.
 */
export function rememberScrollLeaving(from: LocationState, to: LocationState, y: number) {
  rememberScroll(locationKey(from), y);
  if (from.path !== to.path && new URLSearchParams(to.search).has("back")) {
    rememberScroll(messageBackURL(to), y);
  }
}

export function rememberedScroll(key: string): number | undefined {
  return scrollPositions.get(key);
}

/**
 * scrollAfterNavigation decides where a pushed route starts.
 *
 * - The same view with a different query (a filter, a page of the list, the
 *   calendar's `?remind=`) keeps the page where it is: that is the view
 *   changing, not the reader going somewhere.
 * - Going back to the view this one was opened from - the `back` a message or
 *   the composer carries, which is where Back, Delete, Archive and Send all
 *   return to - restores the position that view was left at.
 * - Anywhere else starts at the top, so a message opened from far down a list
 *   is read from its beginning.
 *
 * The answer is `null` for "leave it", otherwise the offset to scroll to.
 */
export function scrollAfterNavigation(from: LocationState, to: LocationState): number | null {
  if (from.path === to.path) return null;
  const toKey = locationKey(to);
  if (new URLSearchParams(from.search).has("back") && messageBackURL(from) === toKey) {
    return rememberedScroll(toKey) ?? 0;
  }
  return 0;
}

/**
 * restoreScroll scrolls the page to `y`, retrying for a short while until the
 * page is tall enough to get there. It stops the moment the reader scrolls on
 * their own, and a newer restore cancels an older one: the reader's own
 * movement always wins over a remembered one.
 */
export function restoreScroll(y: number) {
  cancelScrollRestore();
  if (y <= 0) {
    window.scrollTo(0, 0);
    return;
  }
  const started = Date.now();
  let frame = 0;
  const stop = () => {
    window.cancelAnimationFrame(frame);
    for (const type of interruptEvents) window.removeEventListener(type, stop, true);
    if (activeRestore === stop) activeRestore = null;
  };
  activeRestore = stop;
  for (const type of interruptEvents) window.addEventListener(type, stop, { capture: true, passive: true });
  const attempt = () => {
    window.scrollTo(0, y);
    if (Math.abs(window.scrollY - y) <= 1 || Date.now() - started > restoreBudgetMS) {
      stop();
      return;
    }
    frame = window.requestAnimationFrame(attempt);
  };
  // The new route has not rendered yet when the navigation asks for this; the
  // first attempt waits for the frame that draws it.
  frame = window.requestAnimationFrame(attempt);
}

export function cancelScrollRestore() {
  activeRestore?.();
}

const interruptEvents = ["wheel", "touchstart", "keydown", "mousedown"] as const;

/** resetScrollMemoryForTest empties the memory between tests. */
export function resetScrollMemoryForTest() {
  cancelScrollRestore();
  scrollPositions.clear();
}
