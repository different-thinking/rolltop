import { act } from "react";
import { createRoot, type Root } from "react-dom/client";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { CalendarSummary, MessageTripSummary, Trip } from "../../types";

const calls = vi.hoisted(() => ({
  created: [] as unknown[],
  linked: [] as unknown[]
}));

vi.mock("../../api", () => ({
  api: {
    calendars: async () => ({ calendars: [calendar] }),
    createCalendarEvent: async (_csrf: string, input: unknown) => {
      calls.created.push(input);
      return { event: { id: 77, start_at: "2026-11-14T06:59:00.000Z" } };
    },
    linkTripToCalendar: async (_csrf: string, id: number, segmentIDs: number[], eventID: number) => {
      calls.linked.push({ id, segmentIDs, eventID });
      return { trip: { ...ticket, segments: ticket.segments.map((leg) => ({ ...leg, calendar_event_id: eventID, calendar_start: "2026-11-14T06:59:00.000Z" })) } };
    }
  }
}));

import { TripDetails } from "./TripCard";

(globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;

const calendar: CalendarSummary = {
  id: 1,
  provider: "google",
  connection_id: 1,
  connection_email: "reader@example.com",
  name: "Personal",
  description: "",
  time_zone: "",
  color: "#1a73e8",
  access_role: "owner",
  can_write: true,
  is_primary: true,
  selected: true,
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

const ticket: Trip = {
  id: 5,
  kind: "train",
  reference: "482910375512",
  provider: "Deutsche Bahn",
  messages: [],
  segments: [
    {
      id: 11, kind: "train", number: "ICE 597", title: "ICE 597", provider: "Deutsche Bahn",
      from: "Berlin Hbf", from_code: "", to: "München Hbf", to_code: "", venue: "", address: "",
      start: "2099-11-14T08:29", start_at: "2099-11-14T07:29:00Z", end: "2099-11-14T12:32", end_at: "2099-11-14T11:32:00Z",
      all_day: false, seat: "Coach 25, seat 45", seat_only: false, cancelled: false,
      from_calendar_object: false, calendar_event_id: 0, calendar_start: "", calendar_all_day: false
    }
  ]
};

describe("TripDetails", () => {
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

  it("opens the event dialog seeded with the journey and records the event on the legs", async () => {
    const toasts: string[] = [];
    const summary: MessageTripSummary = { ...ticket, count: 1 };
    act(() =>
      root.render(
        <TripDetails trip={summary} csrf="t" navigate={() => undefined} addToast={(message) => { toasts.push(message); return 0; }} />
      )
    );
    expect(container.textContent).toContain("Train ticket");
    expect(container.textContent).toContain("Coach 25, seat 45");
    const add = Array.from(container.querySelectorAll("button")).find((button) => button.textContent?.includes("Add to calendar"));
    expect(add).toBeDefined();
    await act(async () => {
      add?.click();
    });

    const title = document.body.querySelector<HTMLInputElement>("#calendar-event-title");
    expect(title?.value).toBe("Berlin Hbf → München Hbf (ICE 597)");
    expect(document.body.querySelector<HTMLInputElement>("#calendar-event-travel-before")?.value).toBe("30");
    const form = document.body.querySelector<HTMLFormElement>("form");
    await act(async () => {
      form?.requestSubmit();
    });

    expect(calls.created).toHaveLength(1);
    expect(calls.linked).toEqual([{ id: 5, segmentIDs: [11], eventID: 77 }]);
    expect(toasts).toContain("Added to Personal.");
    expect(container.textContent).toContain("In your calendar");
  });
});
