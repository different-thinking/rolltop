import { describe, expect, it } from "vitest";

import { formatBytes, messageCountLabel } from "./format";

describe("formatBytes", () => {
  it("returns 0 B for zero, negatives, and non-numbers", () => {
    expect(formatBytes(0)).toBe("0 B");
    expect(formatBytes(-5)).toBe("0 B");
    expect(formatBytes(undefined)).toBe("0 B");
    expect(formatBytes(null)).toBe("0 B");
    expect(formatBytes(Number.NaN)).toBe("0 B");
    expect(formatBytes("nonsense")).toBe("0 B");
  });

  it("keeps whole bytes exact and scales up by 1024", () => {
    expect(formatBytes(512)).toBe("512 B");
    // In a scaled unit, a value below 10 always carries one decimal — even a
    // clean 1.0 — while bytes themselves never do.
    expect(formatBytes(1024)).toBe("1.0 KB");
    expect(formatBytes(1048576)).toBe("1.0 MB");
    expect(formatBytes(1073741824)).toBe("1.0 GB");
  });

  it("shows one decimal below 10 in a unit and none at or above it", () => {
    expect(formatBytes(1536)).toBe("1.5 KB");
    expect(formatBytes(10 * 1024)).toBe("10 KB");
    expect(formatBytes(Math.round(2.5 * 1024 * 1024))).toBe("2.5 MB");
  });

  it("accepts a numeric string", () => {
    expect(formatBytes("2048")).toBe("2.0 KB");
  });
});

describe("messageCountLabel", () => {
  it("uses the singular only for exactly one", () => {
    expect(messageCountLabel(0)).toBe("0 messages");
    expect(messageCountLabel(1)).toBe("1 message");
    expect(messageCountLabel(2)).toBe("2 messages");
  });
});
