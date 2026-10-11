import { describe, expect, it } from "vitest";
import type { CalendarEvent, CalendarSummary } from "../../types";
import {
  busiestColumns,
  calendarAccountGroups,
  dayColumnWeights,
  isoWeekNumber,
  lensLaneCalendarIDs,
  localDateKey,
  monthPickerWeekCount,
  monthPickerWeeks,
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
    expect(isoWeekNumber(new Date(2026, 2, 30))).toBe(14);
    expect(isoWeekNumber(new Date(2026, 9, 26))).toBe(44);
  });
});
