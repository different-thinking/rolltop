// File overview: What the app knows about its connection. `navigator.onLine`
// alone is not it: a laptop on a captive portal or a VPN that dropped reads as
// online while every request fails, and a request that reached the server is
// proof of a connection whatever the flag says. So reachability is both - the
// browser's flag, corrected by the last request that failed or succeeded.

import { useSyncExternalStore } from "react";

const listeners = new Set<() => void>();
// The last request's verdict, where one has been given since the flag last
// changed: null defers to navigator.onLine.
let lastRequestReachable: boolean | null = null;
let watching = false;

function notify() {
  listeners.forEach((listener) => listener());
}

function browserOnline(): boolean {
  return typeof navigator === "undefined" || navigator.onLine !== false;
}

function watchBrowser() {
  if (watching || typeof window === "undefined") return;
  watching = true;
  // A flag change is fresher evidence than the request that came before it.
  window.addEventListener("online", () => {
    lastRequestReachable = null;
    notify();
  });
  window.addEventListener("offline", () => {
    lastRequestReachable = null;
    notify();
  });
}

/** serverReachable reports whether the next request can be expected to reach
 * the server. */
export function serverReachable(): boolean {
  if (!browserOnline()) return false;
  return lastRequestReachable !== false;
}

// The rejections fetch itself produced, as `reportRequestOutcome` saw them. A
// fetch that never got an answer rejects with a TypeError, but so does any
// slip in the code that handles the answer, and the two must not read alike:
// one is a reason to show a saved copy or queue an event, the other is a bug
// that would then be reported to the reader as a lost connection. So the one
// place that knows a rejection came out of fetch marks it, and the wording
// browsers give that rejection is the fallback for an error nothing marked.
const fetchRejections = new WeakSet<object>();
const fetchRejectionMessage = /fetch|network|load failed/i;

/**
 * isNetworkError reports a request that never got an answer (an HTTP failure
 * of any status resolves and is not one). The retry inside `fetchGET` rethrows
 * the original, so the verdict holds for both attempts.
 */
export function isNetworkError(err: unknown): boolean {
  if (!(err instanceof TypeError)) return false;
  return fetchRejections.has(err) || fetchRejectionMessage.test(err.message);
}

/** reportRequestOutcome feeds the verdict of one request back into what the app
 * shows: null for an answer, fetch's rejection otherwise. A failure that was
 * not a network failure says nothing about the connection and is ignored. */
export function reportRequestOutcome(err: unknown): void {
  if (err instanceof TypeError) fetchRejections.add(err);
  const reachable = err === null ? true : isNetworkError(err) ? false : null;
  if (reachable === null || reachable === lastRequestReachable) return;
  lastRequestReachable = reachable;
  notify();
}

/** noConnectionMessage is what a failed request says to the reader when the
 * reason is the connection rather than the server. */
export const noConnectionMessage = "You are offline.";

function subscribe(listener: () => void): () => void {
  watchBrowser();
  listeners.add(listener);
  return () => {
    listeners.delete(listener);
  };
}

/** useServerReachable is `serverReachable` as React state. */
export function useServerReachable(): boolean {
  return useSyncExternalStore(subscribe, serverReachable, () => true);
}

/** resetReachabilityForTests forgets the last request's verdict. Tests only. */
export function resetReachabilityForTests(): void {
  lastRequestReachable = null;
}

const offlineClearMessage = "rolltop:offline-clear";

/**
 * forgetOfflineShell asks the service worker to drop the app shell it keeps
 * for starting offline. The shell embeds the signed-in user's startup data, so
 * a sign-out has to take it with it - the next reader on this browser must not
 * be able to start the app as the previous one by going offline.
 */
export function forgetOfflineShell(): void {
  if (typeof navigator === "undefined" || !("serviceWorker" in navigator)) return;
  const message = { type: offlineClearMessage };
  const controller = navigator.serviceWorker.controller;
  if (controller) {
    controller.postMessage(message);
    return;
  }
  navigator.serviceWorker.ready
    .then((registration) => registration.active?.postMessage(message))
    .catch(() => {
      // No worker, no shell to forget.
    });
}
