import { describe, expect, it } from "vitest";
import type { CalendarEvent, CalendarSummary } from "../../types";
import {
  allDayRangeCoversDay,
  allDayRangeFromLocalDays,
  busiestColumns,
  calendarAccountGroups,
  dayColumnWeights,
  drawnRange,
  eventEditable,
  isoWeekNumber,
  lensLaneCalendarIDs,
  localDateKey,
  monthPickerWeekCount,
  monthPickerWeeks,
  movedEventTimes,
  rangeSliceForDay,
  resizedEventTimes,
  shiftedAllDayTimes,
  startOfMonth,
  weekDays
} from "./weekModel";

const monday = new Date(2026, 9, 5);
const days = weekDays(monday);
const tuesday = days[1];

function at(date: Date, hour: number, minute = 0): string {
  const value = new Date(date);
  value.setHours(hour, minute, 0, 0);
  return value.toISOString();
}

function calendar(id: number, email: string, provider: CalendarSummary["provider"] = "google"): CalendarSummary {
  return {
    id,
    provider,
    connection_id: 1,
    connection_email: email,
    name: `Calendar ${id}`,
    description: "",
    time_zone: "",
    color: "#1a73e8",
    access_role: "owner",
    can_write: true,
    is_primary: false,
    selected: true,
    copy_target: false,
    busy_target: false,
    busy_label: "",
    busy_title: "Busy",
    listed: true,
    online_meeting_providers: [],
    synced_from: "",
    last_sync_at: "",
    status: "ok",
    status_detail: ""
  };
}

let nextID = 1;
function event(calendarID: number, start: string, end: string, extra: Partial<CalendarEvent> = {}): CalendarEvent {
  return {
    id: nextID++,
    calendar_id: calendarID,
    summary: "Event",
    description: "",
    location: "",
    status: "confirmed",
    start_at: start,
    end_at: end,
    all_day: false,
    time_zone: "",
    recurring_event_id: "",
    organizer_email: "",
    organizer_name: "",
    attendees: [],
    my_response: "",
    html_link: "",
    link_primary: false,
    link_masked: false,
    also_in: [],
    online_meeting: false,
    online_meeting_provider: "",
    online_meeting_url: "",
    ...extra
  };
}

// Tuesday has four appointments at once, one per calendar; Monday has one.
const events = [
  event(1, at(monday, 9), at(monday, 10)),
  event(3, at(tuesday, 10), at(tuesday, 11)),
  event(1, at(tuesday, 10), at(tuesday, 11)),
  event(4, at(tuesday, 10, 30), at(tuesday, 11, 30)),
  event(2, at(tuesday, 10), at(tuesday, 11, 30)),
  event(2, at(tuesday, 0), at(days[2], 0), { all_day: true })
];

describe("calendarAccountGroups", () => {
  it("groups by account in the order the accounts first appear", () => {
    const groups = calendarAccountGroups([
      calendar(1, "work@example.test", "microsoft"),
      calendar(3, "home@example.test"),
      calendar(2, "work@example.test", "microsoft"),
      calendar(4, "home@example.test")
    ]);
    expect(groups.map(([account, items]) => [account, items.map((item) => item.id)])).toEqual([
      ["work@example.test", [1, 2]],
      ["home@example.test", [3, 4]]
    ]);
  });
});

describe("lensLaneCalendarIDs", () => {
  const order = [calendar(1, "a"), calendar(2, "a"), calendar(3, "b"), calendar(4, "b")];

  it("gives a lane to each calendar with a timed event that day, in the given order", () => {
    expect(lensLaneCalendarIDs(events, order, tuesday)).toEqual([1, 2, 3, 4]);
    expect(lensLaneCalendarIDs(events, order, monday)).toEqual([1]);
    expect(lensLaneCalendarIDs(events, order, days[6])).toEqual([]);
  });

  it("still gives a lane to a calendar the order does not know", () => {
    expect(lensLaneCalendarIDs([event(9, at(monday, 9), at(monday, 10))], order, monday)).toEqual([9]);
  });
});

describe("dayColumnWeights", () => {
  it("counts the most appointments at one moment", () => {
    expect(busiestColumns(events, tuesday)).toBe(4);
    expect(busiestColumns(events, monday)).toBe(1);
  });

  it("widens the lens day for its lanes and busy days a little, and narrows quiet ones", () => {
    const weights = dayColumnWeights(events, days, 1, 4);
    expect(weights[1]).toBeCloseTo(5);
    expect(weights[0]).toBe(0.7);
    expect(weights[6]).toBe(0.7);

    const closed = dayColumnWeights(events, days, -1, 0);
    expect(closed[1]).toBeCloseTo(1.45);
    expect(closed[1]).toBeGreaterThan(closed[0]);
  });

  it("keeps a lens day readable even with one lane", () => {
    expect(dayColumnWeights(events, days, 0, 1)[0]).toBe(3);
  });
});

describe("monthPickerWeeks", () => {
  it("starts on the Monday on or before the first and always shows six weeks", () => {
    // October 2026 begins on a Thursday.
    const weeks = monthPickerWeeks(new Date(2026, 9, 20));
    expect(weeks).toHaveLength(monthPickerWeekCount);
    expect(localDateKey(weeks[0][0])).toBe("2026-09-28");
    expect(localDateKey(weeks[0][3])).toBe("2026-10-01");
    expect(localDateKey(weeks[5][6])).toBe("2026-11-08");
    expect(weeks.every((week) => week.length === 7 && week[0].getDay() === 1)).toBe(true);
  });

  it("starts on the first itself when the month begins on a Monday", () => {
    // June 2026 begins on a Monday.
    expect(localDateKey(monthPickerWeeks(new Date(2026, 5, 30))[0][0])).toBe("2026-06-01");
  });
});

describe("startOfMonth", () => {
  it("steps from the end of a long month without skipping a short one", () => {
    expect(localDateKey(startOfMonth(new Date(2026, 0, 31), 1))).toBe("2026-02-01");
    expect(localDateKey(startOfMonth(new Date(2026, 0, 15), -1))).toBe("2025-12-01");
  });
});

describe("isoWeekNumber", () => {
  it("numbers the week by ISO 8601", () => {
    expect(isoWeekNumber(new Date(2026, 9, 5))).toBe(41);
    expect(isoWeekNumber(new Date(2026, 9, 11))).toBe(41);
    expect(isoWeekNumber(new Date(2026, 9, 12))).toBe(42);
  });

  it("gives a week to the year its Thursday falls in", () => {
    // 2026 begins on a Thursday, so 1 January is in week 1 and the year has 53.
    expect(isoWeekNumber(new Date(2026, 0, 1))).toBe(1);
    expect(isoWeekNumber(new Date(2027, 0, 1))).toBe(53);
    expect(isoWeekNumber(new Date(2027, 0, 4))).toBe(1);
    // 30 December 2024 is a Monday whose Thursday is in 2025.
    expect(isoWeekNumber(new Date(2024, 11, 30))).toBe(1);
  });

  it("is not thrown by a daylight saving change inside the year", () => {
    // Pinned to a zone with DST: in UTC, where CI runs, there is no change to
    // be thrown by. 2 April 2026 is day 91 of the year, a whole number of
    // weeks that the spring change leaves an hour short.
    const previousTZ = process.env.TZ;
    process.env.TZ = "Europe/Berlin";
    try {
      expect(isoWeekNumber(new Date(2026, 2, 30))).toBe(14);
      expect(isoWeekNumber(new Date(2026, 9, 26))).toBe(44);
    } finally {
      if (previousTZ === undefined) delete process.env.TZ;
      else process.env.TZ = previousTZ;
    }
  });
});

function withZone(zone: string, run: () => void) {
  const previousTZ = process.env.TZ;
  process.env.TZ = zone;
  try {
    run();
  } finally {
    if (previousTZ === undefined) delete process.env.TZ;
    else process.env.TZ = previousTZ;
  }
}

function clock(date: Date): string {
  return `${localDateKey(date)} ${String(date.getHours()).padStart(2, "0")}:${String(date.getMinutes()).padStart(2, "0")}`;
}

describe("movedEventTimes", () => {
  it("moves by the distance travelled, snapped, and keeps the length", () => {
    const meeting = event(1, at(tuesday, 10, 10), at(tuesday, 11, 40));
    // Grabbed at 10:30, held at 12:37: two hours and seven minutes, which
    // snaps to two hours and leaves the odd ten minutes alone.
    const moved = movedEventTimes(meeting, tuesday, 10 * 60 + 30, tuesday, 12 * 60 + 37);
    expect(clock(moved.start)).toBe(`${localDateKey(tuesday)} 12:10`);
    expect(clock(moved.end)).toBe(`${localDateKey(tuesday)} 13:40`);
  });

  it("carries an event to another day at the same time", () => {
    const meeting = event(1, at(tuesday, 9), at(tuesday, 10));
    const moved = movedEventTimes(meeting, tuesday, 9 * 60 + 20, days[3], 9 * 60 + 20);
    expect(clock(moved.start)).toBe(`${localDateKey(days[3])} 09:00`);
    expect(clock(moved.end)).toBe(`${localDateKey(days[3])} 10:00`);
  });

  it("keeps a drop at the top of a column on that day", () => {
    const meeting = event(1, at(tuesday, 1), at(tuesday, 2));
    const moved = movedEventTimes(meeting, tuesday, 90, days[3], 0);
    expect(clock(moved.start)).toBe(`${localDateKey(days[3])} 00:00`);
  });

  it("keeps the wall-clock time across a daylight saving change", () => {
    withZone("Europe/Berlin", () => {
      // Friday 23 October 2026, carried to Monday 26th, after the clocks went
      // back on the Sunday.
      const friday = new Date(2026, 9, 23);
      const monday = new Date(2026, 9, 26);
      const start = new Date(2026, 9, 23, 10, 0);
      const meeting = event(1, start.toISOString(), new Date(2026, 9, 23, 11, 0).toISOString());
      const moved = movedEventTimes(meeting, friday, 10 * 60, monday, 10 * 60);
      expect(moved.start.getHours()).toBe(10);
      expect(moved.end.getTime() - moved.start.getTime()).toBe(3_600_000);
    });
  });
});

describe("resizedEventTimes", () => {
  it("puts the end on the grid and never before the start", () => {
    const meeting = event(1, at(tuesday, 10), at(tuesday, 11));
    expect(clock(resizedEventTimes(meeting, tuesday, 12 * 60 + 8).end)).toBe(`${localDateKey(tuesday)} 12:15`);
    const squeezed = resizedEventTimes(meeting, tuesday, 9 * 60);
    expect(clock(squeezed.end)).toBe(`${localDateKey(tuesday)} 10:15`);
    expect(squeezed.start.toISOString()).toBe(meeting.start_at);
  });

  it("can pull the end into the next day", () => {
    const late = event(1, at(tuesday, 22), at(tuesday, 23));
    expect(clock(resizedEventTimes(late, days[2], 60).end)).toBe(`${localDateKey(days[2])} 01:00`);
  });
});

describe("drawnRange", () => {
  it("draws downwards from the step pressed on", () => {
    const range = drawnRange(tuesday, 10 * 60 + 5, 11 * 60 + 20);
    expect(clock(range.start)).toBe(`${localDateKey(tuesday)} 10:00`);
    expect(clock(range.end)).toBe(`${localDateKey(tuesday)} 11:15`);
  });

  it("draws upwards and is never empty", () => {
    const up = drawnRange(tuesday, 10 * 60 + 5, 8 * 60 + 50);
    expect(clock(up.start)).toBe(`${localDateKey(tuesday)} 08:45`);
    expect(clock(up.end)).toBe(`${localDateKey(tuesday)} 10:15`);
    const still = drawnRange(tuesday, 10 * 60 + 5, 10 * 60 + 2);
    expect(clock(still.end)).toBe(`${localDateKey(tuesday)} 10:15`);
  });

  it("stays inside the day", () => {
    const range = drawnRange(tuesday, 23 * 60 + 40, 30 * 60);
    expect(clock(range.start)).toBe(`${localDateKey(tuesday)} 23:30`);
    expect(clock(range.end)).toBe(`${localDateKey(days[2])} 00:00`);
  });
});

describe("all-day drags", () => {
  it("shifts the UTC dates and keeps the length", () => {
    const holiday = event(1, "2026-10-06T00:00:00Z", "2026-10-08T00:00:00Z", { all_day: true });
    const shifted = shiftedAllDayTimes(holiday, 3);
    expect(shifted.start.toISOString()).toBe("2026-10-09T00:00:00.000Z");
    expect(shifted.end.toISOString()).toBe("2026-10-11T00:00:00.000Z");
  });

  it("renders the local days drawn as an exclusive UTC range in either direction", () => {
    const range = allDayRangeFromLocalDays(days[3], days[1]);
    expect(range.start.toISOString()).toBe(`${localDateKey(days[1])}T00:00:00.000Z`);
    expect(range.end.toISOString()).toBe(`${localDateKey(days[4])}T00:00:00.000Z`);
    expect(allDayRangeCoversDay(range, days[1])).toBe(true);
    expect(allDayRangeCoversDay(range, days[3])).toBe(true);
    expect(allDayRangeCoversDay(range, days[4])).toBe(false);
  });

  it("is read on the plain dates west of UTC as well", () => {
    withZone("America/Los_Angeles", () => {
      const thursday = new Date(2026, 9, 8);
      const range = allDayRangeFromLocalDays(thursday, thursday);
      expect(range.start.toISOString()).toBe("2026-10-08T00:00:00.000Z");
      expect(allDayRangeCoversDay(range, thursday)).toBe(true);
      expect(allDayRangeCoversDay(range, new Date(2026, 9, 7))).toBe(false);
    });
  });
});

describe("rangeSliceForDay", () => {
  it("clips a range to each day it reaches", () => {
    const range = { start: new Date(at(tuesday, 23)), end: new Date(at(days[2], 1)) };
    expect(rangeSliceForDay(range, tuesday)).toEqual({ top: 23 * 48, height: 48 });
    expect(rangeSliceForDay(range, days[2])).toEqual({ top: 0, height: 48 });
    expect(rangeSliceForDay(range, days[3])).toBeNull();
  });
});

describe("eventEditable", () => {
  it("needs a writable calendar and is never a Microsoft meeting somebody else organizes", () => {
    const google = calendar(1, "a@example.com");
    const microsoft = calendar(2, "b@example.com", "microsoft");
    const readOnly = { ...calendar(3, "c@example.com"), can_write: false };
    expect(eventEditable(event(1, at(tuesday, 9), at(tuesday, 10)), google)).toBe(true);
    expect(eventEditable(event(1, at(tuesday, 9), at(tuesday, 10), { my_response: "accepted" }), google)).toBe(true);
    expect(eventEditable(event(2, at(tuesday, 9), at(tuesday, 10), { my_response: "accepted" }), microsoft)).toBe(false);
    expect(eventEditable(event(2, at(tuesday, 9), at(tuesday, 10)), microsoft)).toBe(true);
    expect(eventEditable(event(3, at(tuesday, 9), at(tuesday, 10)), readOnly)).toBe(false);
    expect(eventEditable(event(3, at(tuesday, 9), at(tuesday, 10)), undefined)).toBe(false);
  });
});
