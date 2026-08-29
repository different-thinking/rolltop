/**
 * File overview: The signal that says a bill changed by hand.
 *
 * The header chip and the invoice list are on opposite sides of the shell -- one
 * lives in the topbar that survives every route, the other in the page under it
 * -- and they read the same rows. The list already refreshes on the mail
 * generation, which is what a sync bumps; a correction the reader makes is the
 * other way those rows change, and nothing else tells the chip about it. So
 * ticking off a bill would otherwise leave "Due: today" standing in the
 * header until the next sync or reload.
 *
 * This is the parcel list's revision counter with a different name rather than a
 * shared one, and deliberately so: the two chips ask different questions of
 * different tables, and a correction to one must not make the other refetch.
 */

import { useSyncExternalStore } from "react";

let revision = 0;
const listeners = new Set<() => void>();

/** notifyInvoicesChanged is called after a correction is stored. */
export function notifyInvoicesChanged() {
  revision += 1;
  listeners.forEach((listener) => listener());
}

/** subscribeToInvoices is the store half of the hook below, exported because
 * that is the half worth testing: whether a correction reaches a listener that
 * is not the page which made it. */
export function subscribeToInvoices(listener: () => void) {
  listeners.add(listener);
  return () => {
    listeners.delete(listener);
  };
}

export function invoicesRevision() {
  return revision;
}

/** useInvoicesRevision re-renders its caller whenever a bill is corrected. */
export function useInvoicesRevision(): number {
  return useSyncExternalStore(subscribeToInvoices, invoicesRevision, invoicesRevision);
}
