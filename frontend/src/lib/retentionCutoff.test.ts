// File overview: The "older than" cutoff, which every backlog action resolves
// through. What is tested here is the arithmetic three separate features would
// otherwise each get slightly wrong.

import { describe, expect, it } from "vitest";
import { cutoffInstant, dateInputValue, relativeCutoffDate, relativeCutoffLabel } from "./retentionCutoff";

describe("relativeCutoffDate", () => {
  it("counts days back on the calendar", () => {
    const from = new Date(2025, 5, 15, 13, 45);
    expect(dateInputValue(relativeCutoffDate({ count: 30, unit: "days" }, from))).toBe("2025-05-16");
  });

  // Stepping the month on a date the earlier month may not have overflows into
  // the next one: 31 May minus three months is 31 February, which JavaScript
  // reads as 3 March.
  it("clamps a month step into the month it lands in", () => {
    const from = new Date(2025, 4, 31);
    expect(dateInputValue(relativeCutoffDate({ count: 3, unit: "months" }, from))).toBe("2025-02-28");
  });

  it("counts years as twelve months", () => {
    const from = new Date(2024, 1, 29);
    expect(dateInputValue(relativeCutoffDate({ count: 1, unit: "years" }, from))).toBe("2023-02-28");
  });

  // A count of nothing must still name a day, or the dialog would offer a
  // cutoff the server reads as no cutoff at all — which selects everything.
  it("never resolves to no cutoff", () => {
    const from = new Date(2025, 5, 15);
    expect(dateInputValue(relativeCutoffDate({ count: 0, unit: "days" }, from))).toBe("2025-06-14");
  });
});

describe("cutoffInstant", () => {
  it("names the moment the chosen day begins in the reader's own timezone", () => {
    const instant = cutoffInstant("2024-03-01");
    expect(new Date(instant).getTime()).toBe(new Date(2024, 2, 1, 0, 0, 0, 0).getTime());
  });

  it("leaves an unreadable day alone rather than inventing one", () => {
    expect(cutoffInstant("")).toBe("");
    expect(cutoffInstant("someday")).toBe("someday");
  });
});

describe("relativeCutoffLabel", () => {
  it("says the unit the way the reader chose it", () => {
    expect(relativeCutoffLabel({ count: 1, unit: "days" })).toBe("older than 1 day");
    expect(relativeCutoffLabel({ count: 30, unit: "days" })).toBe("older than 30 days");
    expect(relativeCutoffLabel({ count: 1, unit: "years" })).toBe("older than 1 year");
  });
});
