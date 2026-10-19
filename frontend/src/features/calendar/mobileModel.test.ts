import { describe, expect, it } from "vitest";
import type { CalendarEvent } from "../../types";
import {
  agendaDayItems,
  calendarDayURL,
  calendarLoadRange,
  compactAllDayEvents,
  compactTimedEvents,
  mobileViewDays,
  mobileViewStep,
  parseMobileCalendarView,
  threeDayColumns,
  threeDayFocusIndex,
  timedEventFillsDay,
  weekListDays
} from "./mobileModel";
import { addDays, localDateKey } from "./weekModel";

// Monday 19 October 2026 and the days after it.
const monday = new Date(2026, 9, 19);
const tuesday = addDays(monday, 1);
const wednesday = addDays(monday, 2);
const sunday = addDays(monday, 6);

function at(date: Date, hour: number, minute = 0): string {
  const value = new Date(date);
  value.setHours(hour, minute, 0, 0);
  return value.toISOString();
}

function utcDate(date: Date): string {
  return new Date(Date.UTC(date.getFullYear(), date.getMonth(), date.getDate())).toISOString();
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

const keys = (days: Date[]) => days.map(localDateKey);

describe("mobile views", () => {
  it("falls back to the day view for anything it does not know", () => {
    expect(parseMobileCalendarView("agenda")).toBe("agenda");
    expect(parseMobileCalendarView("month")).toBe("day");
    expect(parseMobileCalendarView(null)).toBe("day");
  });

  it("lists the days each view shows from the routed day", () => {
    const thursday = addDays(monday, 3);
    expect(keys(mobileViewDays("day", thursday))).toEqual(["2026-10-22"]);
    expect(keys(mobileViewDays("three", thursday))).toEqual(["2026-10-22", "2026-10-23", "2026-10-24"]);
    expect(mobileViewDays("agenda", thursday)).toHaveLength(7);
    expect(localDateKey(mobileViewDays("agenda", thursday)[0])).toBe("2026-10-22");
    // The week list is the week the day falls in, Monday first.
    expect(keys(mobileViewDays("week", thursday))).toEqual(keys(mobileViewDays("agenda", monday)));
  });

  it("steps by the span a view shows", () => {
    expect([mobileViewStep("day"), mobileViewStep("three"), mobileViewStep("agenda"), mobileViewStep("week")]).toEqual([
      1, 3, 7, 7
    ]);
  });

  it("loads whole weeks, also when three days run into the next one", () => {
    const range = calendarLoadRange(mobileViewDays("three", addDays(monday, 5)));
    expect(localDateKey(range.start)).toBe("2026-10-19");
    expect(localDateKey(range.end)).toBe("2026-11-02");
    // A week is loaded exactly as the desktop always loaded it.
    const week = calendarLoadRange(mobileViewDays("week", monday));
    expect(localDateKey(week.start)).toBe("2026-10-19");
    expect(localDateKey(week.end)).toBe("2026-10-26");
  });

  it("routes to the day itself, not the start of its week", () => {
    expect(calendarDayURL(wednesday)).toBe("/calendar/2026-10-21");
  });
});

describe("timed events that fill a day", () => {
  const absence = event(1, at(monday, 0), at(addDays(monday, 4), 0), { summary: "Away" });
  const workday = event(1, at(monday, 8), at(monday, 18), { summary: "Workshop" });
  const nightShift = event(1, at(monday, 20), at(tuesday, 8), { summary: "Night shift" });
  const holiday = event(2, utcDate(monday), utcDate(addDays(monday, 6)), { all_day: true, summary: "Holidays" });

  it("draws an absence from midnight to midnight as all day, on every day it covers", () => {
    expect(timedEventFillsDay(absence, monday)).toBe(true);
    expect(timedEventFillsDay(absence, addDays(monday, 3))).toBe(true);
    expect(timedEventFillsDay(absence, addDays(monday, 4))).toBe(false);
  });

  it("keeps a long working day and a night shift in the grid", () => {
    expect(timedEventFillsDay(workday, monday)).toBe(false);
    expect(timedEventFillsDay(nightShift, monday)).toBe(false);
    expect(timedEventFillsDay(nightShift, tuesday)).toBe(false);
  });

  it("puts the filling event after the real all-day events and takes it out of the grid", () => {
    const all = [absence, workday, nightShift, holiday];
    expect(compactAllDayEvents(all, monday).map((item) => item.summary)).toEqual(["Holidays", "Away"]);
    expect(compactTimedEvents(all, monday).map((item) => item.summary)).toEqual(["Workshop", "Night shift"]);
  });

  it("never treats a real all-day event as timed", () => {
    expect(timedEventFillsDay(holiday, monday)).toBe(false);
  });
});

describe("three days", () => {
  it("makes the tapped day wide while it is on screen", () => {
    const days = mobileViewDays("three", monday);
    expect(threeDayFocusIndex(days, "2026-10-21", "2026-10-19")).toBe(2);
    // Tapped elsewhere, the wide day falls back to today, then to the first.
    expect(threeDayFocusIndex(days, "2026-11-01", "2026-10-20")).toBe(1);
    expect(threeDayFocusIndex(days, null, "2026-12-24")).toBe(0);
  });

  it("sizes the wide day for the shared column property", () => {
    expect(threeDayColumns(1)).toBe("minmax(0, 1.00fr) minmax(0, 3.40fr) minmax(0, 1.00fr)");
  });
});

describe("agenda", () => {
  const review = event(1, at(monday, 7), at(monday, 7, 45), { summary: "Review" });
  const call = event(1, at(monday, 8), at(monday, 8, 30), { summary: "Call" });
  const zoo = event(2, at(monday, 10), at(monday, 12, 30), { summary: "Zoo" });
  const callback = event(1, at(monday, 15), at(monday, 16), { summary: "Callback" });
  const doctor = event(2, at(monday, 15), at(monday, 15, 30), { summary: "Doctor" });
  const bike = event(3, at(monday, 15, 15), at(monday, 16, 15), { summary: "Bike" });
  const run = event(3, at(monday, 18), at(monday, 19), { summary: "Run" });

  it("names free time between appointments and keeps simultaneous ones together", () => {
    const items = agendaDayItems([run, bike, doctor, callback, zoo, call, review], monday);
    expect(
      items.map((item) =>
        item.kind === "event"
          ? item.event.summary
          : item.kind === "free"
            ? `free ${localTime(item.start)}–${localTime(item.end)}`
            : `together ${item.events.map((grouped) => grouped.summary).join("+")}`
      )
    ).toEqual([
      "Review",
      "Call",
      "free 08:30–10:00",
      "Zoo",
      "free 12:30–15:00",
      "together Callback+Doctor+Bike",
      "free 16:15–18:00",
      "Run"
    ]);
  });

  it("does not call a short gap free", () => {
    // Review ends at 07:45 and the call starts at 08:00.
    expect(agendaDayItems([review, call], monday).map((item) => item.kind)).toEqual(["event", "event"]);
  });

  it("counts the journey as taken time", () => {
    const visit = event(1, at(monday, 10), at(monday, 11), { summary: "Visit", travel_before_minutes: 90 });
    // The journey starts at 08:30, right when the call ends: no free time.
    expect(agendaDayItems([call, visit], monday).map((item) => item.kind)).toEqual(["event", "event"]);
  });

  it("groups by busy time but names the appointments' own time", () => {
    // B's journey starts before A ends, so they are grouped; the group still
    // says 10:00–12:00, the appointments' time, not 09:30 for B's journey.
    const a = event(1, at(monday, 10), at(monday, 11), { summary: "A" });
    const b = event(2, at(monday, 11, 15), at(monday, 12), { summary: "B", travel_before_minutes: 30 });
    const visit = event(1, at(monday, 10), at(monday, 10, 30), { summary: "Visit", travel_before_minutes: 30 });
    const [group] = agendaDayItems([a, b, visit], monday);
    expect(group.kind).toBe("together");
    if (group.kind !== "together") return;
    expect(localTime(group.start)).toBe("10:00");
    expect(localTime(group.end)).toBe("12:00");
  });

  it("leaves out all-day events and other days", () => {
    const holiday = event(2, utcDate(monday), utcDate(tuesday), { all_day: true });
    const tomorrow = event(1, at(tuesday, 9), at(tuesday, 10));
    expect(agendaDayItems([holiday, tomorrow], monday)).toEqual([]);
  });
});

describe("week list", () => {
  it("names the first appointments by start and counts the rest", () => {
    const events = [
      event(1, at(monday, 15), at(monday, 16), { summary: "C" }),
      event(1, at(monday, 7), at(monday, 8), { summary: "A" }),
      event(1, at(monday, 18), at(monday, 19), { summary: "D" }),
      event(1, at(monday, 10), at(monday, 11), { summary: "B" }),
      event(2, utcDate(monday), utcDate(tuesday), { all_day: true, summary: "Holiday" })
    ];
    const [first, second] = weekListDays(events, mobileViewDays("week", monday));
    expect(first.timed.map((item) => item.summary)).toEqual(["A", "B", "C"]);
    expect(first.more).toBe(1);
    expect(first.allDay.map((item) => item.summary)).toEqual(["Holiday"]);
    expect(second.timed).toEqual([]);
    expect(second.more).toBe(0);
  });

  it("has a row for every day of the week, empty ones too", () => {
    const rows = weekListDays([], mobileViewDays("week", sunday));
    expect(rows.map((row) => localDateKey(row.day))).toEqual(keys(mobileViewDays("agenda", monday)));
  });
});

function localTime(date: Date): string {
  return `${String(date.getHours()).padStart(2, "0")}:${String(date.getMinutes()).padStart(2, "0")}`;
}
