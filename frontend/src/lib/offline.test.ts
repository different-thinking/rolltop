import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { isNetworkError, reportRequestOutcome, resetReachabilityForTests, serverReachable } from "./offline";

describe("serverReachable", () => {
  beforeEach(() => {
    resetReachabilityForTests();
    vi.spyOn(navigator, "onLine", "get").mockReturnValue(true);
  });
  afterEach(() => {
    vi.restoreAllMocks();
    resetReachabilityForTests();
  });

  it("follows the browser's flag until a request says otherwise", () => {
    expect(serverReachable()).toBe(true);
    vi.spyOn(navigator, "onLine", "get").mockReturnValue(false);
    expect(serverReachable()).toBe(false);
  });

  it("reads a request that never got an answer as unreachable, and the next answer as reachable again", () => {
    reportRequestOutcome(new TypeError("Failed to fetch"));
    expect(serverReachable()).toBe(false);
    reportRequestOutcome(null);
    expect(serverReachable()).toBe(true);
  });

  it("ignores failures that are answers", () => {
    reportRequestOutcome(new Error("500 Internal Server Error"));
    expect(serverReachable()).toBe(true);
    reportRequestOutcome(new TypeError("Failed to fetch"));
    reportRequestOutcome(new Error("409 Conflict"));
    expect(serverReachable()).toBe(false);
  });

  it("does not mistake a TypeError from the code for a lost connection", () => {
    const slip = new TypeError("Cannot read properties of undefined (reading 'id')");
    expect(isNetworkError(slip)).toBe(false);
    reportRequestOutcome(slip);
    expect(isNetworkError(slip)).toBe(true);
    expect(serverReachable()).toBe(false);
  });
  it("never reports reachable while the browser says offline", () => {
    vi.spyOn(navigator, "onLine", "get").mockReturnValue(false);
    reportRequestOutcome(null);
    expect(serverReachable()).toBe(false);
  });
});

describe("isNetworkError", () => {
  it("recognises fetch's rejection and nothing else", () => {
    expect(isNetworkError(new TypeError("Failed to fetch"))).toBe(true);
    expect(isNetworkError(new Error("Failed to fetch"))).toBe(false);
    expect(isNetworkError("offline")).toBe(false);
  });
});
