import { act } from "react";
import { createRoot, type Root } from "react-dom/client";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { CalendarEvent, CalendarSummary } from "../../types";
import { MobileAgenda, MobileDayHead, MobileViewSwitch, MobileWeekList } from "./MobileCalendarViews";
import { mobileViewDays } from "./mobileModel";

(globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;

const monday = new Date(2026, 9, 19);
const tuesday = new Date(2026, 9, 20);

function at(date: Date, hour: number, minute = 0): string {
  const value = new Date(date);
  value.setHours(hour, minute, 0, 0);
  return value.toISOString();
}

function calendar(id: number, name: string, selected = true): CalendarSummary {
  return {
    id,
    provider: "google",
    connection_id: 1,
    connection_email: "reader@example.test",
    name,
    description: "",
    time_zone: "",
    color: "#1a73e8",
    access_role: "owner",
    can_write: true,
    is_primary: false,
    selected,
    copy_target: false,
    busy_target: false,
    busy_label: "",
    busy_title: "Busy",
    listed: true,
    online_meeting_providers: [],
    default_reminder_minutes: [],
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

const work = calendar(1, "Work");
const family = calendar(2, "Family", false);
const calendarsByID = new Map([work, family].map((item) => [item.id, item]));

let container: HTMLDivElement;
let root: Root;

beforeEach(() => {
  container = document.createElement("div");
  document.body.appendChild(container);
  root = createRoot(container);
});

afterEach(() => {
  act(() => root.unmount());
  container.remove();
});

function buttons(): HTMLButtonElement[] {
  return Array.from(container.querySelectorAll("button"));
}

function buttonNamed(text: string): HTMLButtonElement {
  const found = buttons().find((button) => button.textContent?.includes(text));
  if (!found) throw new Error(`no button "${text}" in ${container.innerHTML}`);
  return found;
}

describe("MobileViewSwitch", () => {
  it("marks the current view and reports a new choice", async () => {
    const onChange = vi.fn();
    await act(async () => root.render(<MobileViewSwitch view="three" onChange={onChange} />));
    expect(buttonNamed("3 days").getAttribute("aria-pressed")).toBe("true");
    expect(buttonNamed("Agenda").getAttribute("aria-pressed")).toBe("false");
    await act(async () => buttonNamed("Agenda").click());
    expect(onChange).toHaveBeenCalledWith("agenda");
  });
});

describe("MobileDayHead", () => {
  const events = [
    event(1, at(monday, 9), at(monday, 10), { summary: "Standup" }),
    // Midnight to midnight: drawn with the all-day events, not counted as timed.
    event(1, at(monday, 0), at(tuesday, 0), { summary: "Away" })
  ];

  it("offers a chip per calendar that switches it the way the menu does", async () => {
    const onToggle = vi.fn(async () => undefined);
    await act(async () =>
      root.render(
        <MobileDayHead
          day={monday}
          todayKey="2026-10-19"
          calendars={[work, family]}
          events={events}
          calendarsByID={calendarsByID}
          onToggleCalendar={onToggle}
          onOpenEvent={vi.fn()}
        />
      )
    );
    expect(buttonNamed("Work").getAttribute("aria-pressed")).toBe("true");
    expect(buttonNamed("Family").getAttribute("aria-pressed")).toBe("false");
    await act(async () => buttonNamed("Family").click());
    expect(onToggle).toHaveBeenCalledWith(family);
    expect(container.textContent).toContain("Today · 1 event · 1 all day");
  });

  it("folds the all-day events into one line that opens", async () => {
    const onOpen = vi.fn();
    await act(async () =>
      root.render(
        <MobileDayHead
          day={monday}
          todayKey="2026-10-20"
          calendars={[work]}
          events={events}
          calendarsByID={calendarsByID}
          onToggleCalendar={vi.fn(async () => undefined)}
          onOpenEvent={onOpen}
        />
      )
    );
    const toggle = buttonNamed("1 all day");
    expect(toggle.getAttribute("aria-expanded")).toBe("false");
    await act(async () => toggle.click());
    expect(toggle.getAttribute("aria-expanded")).toBe("true");
    const away = buttons().find((button) => button.textContent === "Away");
    expect(away).toBeDefined();
    await act(async () => away?.click());
    expect(onOpen).toHaveBeenCalledWith(events[1], monday);
  });
});

describe("MobileAgenda", () => {
  it("starts a new event in free time and keeps simultaneous appointments together", async () => {
    const onNew = vi.fn();
    const events = [
      event(1, at(monday, 8), at(monday, 9), { summary: "Call" }),
      event(1, at(monday, 11), at(monday, 12), { summary: "Review" }),
      event(2, at(monday, 11, 30), at(monday, 12, 30), { summary: "Dentist" })
    ];
    await act(async () =>
      root.render(
        <MobileAgenda
          days={[monday]}
          events={events}
          calendarsByID={calendarsByID}
          todayKey="2026-10-01"
          now={new Date(2026, 9, 1)}
          canCreate
          onOpenEvent={vi.fn()}
          onNewEvent={onNew}
        />
      )
    );
    expect(container.textContent).toContain("2 at the same time");
    await act(async () => buttonNamed("free").click());
    const [start, end] = onNew.mock.calls[0] as [Date, Date];
    expect([start.getHours(), end.getHours()]).toEqual([9, 10]);
  });

  it("says when a day has nothing", async () => {
    await act(async () =>
      root.render(
        <MobileAgenda
          days={[monday]}
          events={[]}
          calendarsByID={calendarsByID}
          todayKey="2026-10-01"
          now={new Date(2026, 9, 1)}
          canCreate={false}
          onOpenEvent={vi.fn()}
          onNewEvent={vi.fn()}
        />
      )
    );
    expect(container.textContent).toContain("Nothing scheduled");
  });
});

describe("MobileWeekList", () => {
  it("names the first three appointments, counts the rest, and opens the day", async () => {
    const onOpenDay = vi.fn();
    const events = [7, 9, 11, 14].map((hour) =>
      event(1, at(monday, hour), at(monday, hour + 1), { summary: `At ${hour}` })
    );
    await act(async () =>
      root.render(
        <MobileWeekList
          days={mobileViewDays("week", monday)}
          events={events}
          calendarsByID={calendarsByID}
          todayKey="2026-10-19"
          onOpenDay={onOpenDay}
        />
      )
    );
    const rows = container.querySelectorAll(".calendar-weeklist-row");
    expect(rows).toHaveLength(7);
    expect(rows[0].textContent).toContain("At 11");
    expect(rows[0].textContent).not.toContain("At 14");
    expect(rows[0].textContent).toContain("+1 more");
    await act(async () => (rows[1] as HTMLButtonElement).click());
    expect((onOpenDay.mock.calls[0][0] as Date).getDate()).toBe(20);
  });
});
