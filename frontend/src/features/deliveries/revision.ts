/**
 * File overview: The signal that says a parcel changed by hand.
 *
 * The header chip and the parcel list are on opposite sides of the shell -- one
 * lives in the topbar that survives every route, the other in the page under it
 * -- and they read the same rows. The list already refreshes on the mail
 * generation, which is what a sync bumps; a correction the reader makes is the
 * other way those rows change, and nothing was telling the chip about it. So
 * marking a parcel arrived left "Today: DHL" standing in the header until the
 * next sync or reload.
 *
 * This is deliberately a counter and not the parcels themselves. What the chip
 * needs is "ask again", and keeping one copy of the answer in the component
 * that fetched it is what stops the two views disagreeing about a third.
 */

import { useSyncExternalStore } from "react";

let revision = 0;
const listeners = new Set<() => void>();

/** notifyDeliveriesChanged is called after a correction is stored. */
export function notifyDeliveriesChanged() {
  revision += 1;
  listeners.forEach((listener) => listener());
}

/** subscribeToDeliveries is the store half of the hook below, exported because
 * that is the half worth testing: whether a correction reaches a listener that
 * is not the page which made it. */
export function subscribeToDeliveries(listener: () => void) {
  listeners.add(listener);
  return () => {
    listeners.delete(listener);
  };
}

export function deliveriesRevision() {
  return revision;
}

/** useDeliveriesRevision re-renders its caller whenever a parcel is corrected. */
export function useDeliveriesRevision(): number {
  return useSyncExternalStore(subscribeToDeliveries, deliveriesRevision, deliveriesRevision);
}
