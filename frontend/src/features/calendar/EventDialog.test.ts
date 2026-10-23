import { describe, expect, it } from "vitest";
import { withStartMoved } from "./EventDialog";

const timed = { allDay: false, startDate: "2026-10-23", startTime: "09:00", endDate: "2026-10-23", endTime: "10:30" };
const allDay = { allDay: true, startDate: "2026-10-23", startTime: "09:00", endDate: "2026-10-25", endTime: "10:00" };

describe("withStartMoved", () => {
  it("carries the end along when the start date moves", () => {
    expect(withStartMoved(timed, { startDate: "2026-10-27" })).toMatchObject({
      startDate: "2026-10-27",
      endDate: "2026-10-27",
      endTime: "10:30"
    });
  });

  it("carries the end along when the start time moves, across midnight too", () => {
    expect(withStartMoved(timed, { startTime: "23:00" })).toMatchObject({
      startTime: "23:00",
      endDate: "2026-10-24",
      endTime: "00:30"
    });
  });

  it("keeps the number of days of an all-day event", () => {
    expect(withStartMoved(allDay, { startDate: "2026-11-02" })).toMatchObject({
      startDate: "2026-11-02",
      endDate: "2026-11-04"
    });
  });

  it("leaves the end alone when there is no length to keep", () => {
    const backwards = { ...timed, endTime: "08:00" };
    expect(withStartMoved(backwards, { startDate: "2026-10-27" })).toMatchObject({
      startDate: "2026-10-27",
      endDate: "2026-10-23",
      endTime: "08:00"
    });
    expect(withStartMoved(timed, { startDate: "" })).toMatchObject({ startDate: "", endDate: "2026-10-23" });
  });

  it("keeps the length while a year is typed one digit at a time", () => {
    // A date input reports each keystroke of a typed year as a whole date.
    let timedDraft = timed;
    let allDayDraft = allDay;
    for (const startDate of ["0002-10-23", "0020-10-23", "0202-10-23", "2027-10-23"]) {
      timedDraft = withStartMoved(timedDraft, { startDate });
      allDayDraft = withStartMoved(allDayDraft, { startDate });
      expect(timedDraft.endDate).toBe(startDate);
      expect(allDayDraft.endDate).toBe(startDate.replace(/-23$/, "-25"));
    }
    expect(timedDraft).toMatchObject({ startDate: "2027-10-23", endDate: "2027-10-23", endTime: "10:30" });
  });
});
