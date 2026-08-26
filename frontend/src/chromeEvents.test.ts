import { afterEach, describe, expect, it, vi } from "vitest";

import { notifyChromeEvent, onChromeEvent, waitForChromeEvent } from "./chromeEvents";

describe("onChromeEvent / notifyChromeEvent", () => {
  it("notifies subscribers until they unsubscribe", () => {
    let count = 0;
    const off = onChromeEvent(() => {
      count += 1;
    });
    notifyChromeEvent();
    notifyChromeEvent();
    off();
    notifyChromeEvent();
    expect(count).toBe(2);
  });

  it("isolates a throwing listener from the others", () => {
    let reached = false;
    const offThrower = onChromeEvent(() => {
      throw new Error("boom");
    });
    const offReader = onChromeEvent(() => {
      reached = true;
    });
    expect(() => notifyChromeEvent()).not.toThrow();
    expect(reached).toBe(true);
    offThrower();
    offReader();
  });
});

describe("waitForChromeEvent", () => {
  afterEach(() => {
    vi.useRealTimers();
  });

  it("resolves at the timeout when no snapshot arrives", async () => {
    vi.useFakeTimers();
    let resolved = false;
    const done = waitForChromeEvent(1000).then(() => {
      resolved = true;
    });
    await vi.advanceTimersByTimeAsync(999);
    expect(resolved).toBe(false);
    await vi.advanceTimersByTimeAsync(1);
    await done;
    expect(resolved).toBe(true);
  });

  it("resolves immediately on a snapshot when there is no floor", async () => {
    vi.useFakeTimers();
    let resolved = false;
    const done = waitForChromeEvent(10000).then(() => {
      resolved = true;
    });
    notifyChromeEvent();
    await vi.advanceTimersByTimeAsync(0);
    await done;
    expect(resolved).toBe(true);
  });

  it("does not resolve on a snapshot below the floor, but settles when the floor passes", async () => {
    vi.useFakeTimers();
    let resolved = false;
    const done = waitForChromeEvent(10000, 500).then(() => {
      resolved = true;
    });
    await vi.advanceTimersByTimeAsync(100);
    notifyChromeEvent();
    await vi.advanceTimersByTimeAsync(0);
    // A snapshot under the floor is remembered but does not end the wait yet.
    expect(resolved).toBe(false);
    // Reaching the floor with a snapshot already seen settles it, without
    // waiting out the whole timeout for a second one.
    await vi.advanceTimersByTimeAsync(400);
    await done;
    expect(resolved).toBe(true);
  });

  it("resolves on the first snapshot after the floor has passed", async () => {
    vi.useFakeTimers();
    let resolved = false;
    const done = waitForChromeEvent(10000, 500).then(() => {
      resolved = true;
    });
    // Floor passes with no snapshot: still waiting.
    await vi.advanceTimersByTimeAsync(500);
    expect(resolved).toBe(false);
    // The next snapshot ends the wait at once.
    notifyChromeEvent();
    await vi.advanceTimersByTimeAsync(0);
    await done;
    expect(resolved).toBe(true);
  });
});
