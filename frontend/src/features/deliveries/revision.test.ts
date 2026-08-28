import { describe, expect, it } from "vitest";

import { deliveriesRevision, notifyDeliveriesChanged, subscribeToDeliveries } from "./revision";

// What broke, and what this holds: a correction made on the parcel page has to
// reach the header chip, which lives in the topbar and has no other way to hear
// about it. The chip reads its own count and rides this revision.
describe("the delivery revision", () => {
  it("wakes a listener on every correction and stops on unsubscribe", () => {
    let calls = 0;
    const unsubscribe = subscribeToDeliveries(() => {
      calls += 1;
    });
    const before = deliveriesRevision();

    notifyDeliveriesChanged();
    notifyDeliveriesChanged();
    expect(calls).toBe(2);
    expect(deliveriesRevision()).toBe(before + 2);

    unsubscribe();
    notifyDeliveriesChanged();
    expect(calls).toBe(2);
  });

  it("gives every listener the same number, so two readers cannot disagree", () => {
    const seen: number[] = [];
    const first = subscribeToDeliveries(() => seen.push(deliveriesRevision()));
    const second = subscribeToDeliveries(() => seen.push(deliveriesRevision()));
    notifyDeliveriesChanged();
    first();
    second();
    expect(seen).toHaveLength(2);
    expect(seen[0]).toBe(seen[1]);
  });
});
