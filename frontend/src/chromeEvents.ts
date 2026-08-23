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
 *
 * floorMS is what keeps that from becoming its own problem. The work being
 * waited on reports while it runs, not only when it finishes — a move run says
 * so every few hundred milliseconds — so a waiter that woke on every snapshot
 * would poll several times a second for as long as the work lasts, against the
 * same database the work is competing with. Snapshots below the floor do not
 * end the wait; the first moment past it does.
 */
export function waitForChromeEvent(timeoutMS: number, floorMS = 0): Promise<void> {
  return new Promise((resolve) => {
    let settled = false;
    let pastFloor = floorMS <= 0;
    let announced = false;
    const settle = () => {
      if (settled) return;
      settled = true;
      unsubscribe();
      window.clearTimeout(floor);
      window.clearTimeout(timer);
      resolve();
    };
    const unsubscribe = onChromeEvent(() => {
      announced = true;
      if (pastFloor) settle();
    });
    // A snapshot that arrived under the floor still counts: the work may have
    // finished during it, and waiting out the whole timeout for the next one
    // would be the delay the floor exists to avoid, not to add.
    const floor = window.setTimeout(() => {
      pastFloor = true;
      if (announced) settle();
    }, Math.max(floorMS, 0));
    const timer = window.setTimeout(settle, Math.max(timeoutMS, floorMS));
  });
}
