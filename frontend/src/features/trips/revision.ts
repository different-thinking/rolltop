/**
 * File overview: The signal that says a trip changed by hand -- a journey added
 * to the calendar, a trip dismissed -- so the header chip asks again. It is the
 * parcel list's revision counter, for the same reason: the chip and the views
 * that change its answer live on opposite sides of the shell.
 */

import { useSyncExternalStore } from "react";

let revision = 0;
const listeners = new Set<() => void>();

export function notifyTripsChanged() {
  revision += 1;
  listeners.forEach((listener) => listener());
}

export function subscribeToTrips(listener: () => void) {
  listeners.add(listener);
  return () => {
    listeners.delete(listener);
  };
}

function tripsRevision() {
  return revision;
}

export function useTripsRevision(): number {
  return useSyncExternalStore(subscribeToTrips, tripsRevision, tripsRevision);
}
