/**
 * The chrome event stream, as the rest of the app sees it.
 *
 * One EventSource in App.tsx owns the connection and applies each snapshot to
 * the bootstrap state. Anything else that is waiting for the server to have
 * done something — a queued move working through its messages, a sync finishing
 * a folder — needs to know that a snapshot arrived without opening a second
 * stream or having the subscription threaded down to it as a prop.
 *
 * The signal carries no payload on purpose, exactly like the server's own: it
 * says the server reported something, and the waiter re-reads whatever it
 * actually cares about.
 */

const listeners = new Set<() => void>();

/** notifyChromeEvent tells every waiter that a chrome snapshot arrived. */
export function notifyChromeEvent(): void {
  // Copied first: a listener that unsubscribes itself while it runs must not
  // change the set being iterated.
  for (const listener of Array.from(listeners)) {
    try {
      listener();
    } catch {
      // A waiter that throws is its own problem; the others still get told.
    }
  }
}

/** onChromeEvent subscribes to chrome snapshots and returns an unsubscribe. */
export function onChromeEvent(listener: () => void): () => void {
  listeners.add(listener);
  return () => {
    listeners.delete(listener);
  };
}

/**
 * waitForChromeEvent resolves on the next chrome snapshot, or when timeoutMS
 * has passed with none. It is the wait to use when polling the server for the
 * result of work the server will announce anyway: the announcement is what ends
 * the wait, and the timeout is only there for the announcement that never comes.
 */
export function waitForChromeEvent(timeoutMS: number): Promise<void> {
  return new Promise((resolve) => {
    let settled = false;
    const settle = () => {
      if (settled) return;
      settled = true;
      unsubscribe();
      window.clearTimeout(timer);
      resolve();
    };
    const unsubscribe = onChromeEvent(settle);
    const timer = window.setTimeout(settle, timeoutMS);
  });
}
