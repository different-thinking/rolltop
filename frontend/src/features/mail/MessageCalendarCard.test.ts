import { describe, expect, it } from "vitest";
import type { MessageCalendarEvent } from "../../types";
import { eventBounds, eventWhen, prefillFor } from "./MessageCalendarCard";

function event(patch: Partial<MessageCalendarEvent>): MessageCalendarEvent {
  return {
    summary: "Review",
    start_at: "2026-11-24T13:00:00Z",
    end_at: "2026-11-24T14:00:00Z",
    all_day: false,
    attendees: [{ name: "Bob", email: "bob@example.test" }],
    in_calendars: [],
    ...patch
  };
}

describe("message calendar card", () => {
  it("gives an event without an end a stretch the dialog accepts", () => {
    const timed = eventBounds(event({ end_at: "2026-11-24T13:00:00Z" }));
    expect(timed.end.getTime() - timed.start.getTime()).toBe(3_600_000);
    const allDay = eventBounds(event({ all_day: true, start_at: "2026-12-24T00:00:00Z", end_at: "" }));
    expect(allDay.end.toISOString()).toBe("2026-12-25T00:00:00.000Z");
  });

  it("reads an all-day event's dates in UTC, where they were anchored", () => {
    const single = eventWhen(
      event({ all_day: true, start_at: "2026-12-24T00:00:00Z", end_at: "2026-12-25T00:00:00Z" }),
      "en-GB"
    );
    expect(single).toContain("24 Dec 2026");
    expect(single).not.toContain("–");
    const span = eventWhen(
      event({ all_day: true, start_at: "2026-12-24T00:00:00Z", end_at: "2026-12-27T00:00:00Z" }),
      "en-GB"
    );
    expect(span).toContain("24 Dec 2026");
    expect(span).toContain("26 Dec 2026");
  });

  it("reads a floating time as the viewer's own wall clock", () => {
    const floating = eventBounds(event({ floating: true, start_at: "2026-11-01T08:30:00", end_at: "2026-11-01T09:30:00" }));
    expect(floating.start.getHours()).toBe(8);
    expect(floating.start.getMinutes()).toBe(30);
  });

  it("seeds a new event without the invitation's guests", () => {
    const prefill = prefillFor(
      event({ location: "Room 2", description: "Agenda", url: "https://meet.example.test/x", all_day: false })
    );
    expect(prefill.summary).toBe("Review");
    expect(prefill.location).toBe("Room 2");
    expect(prefill.description).toBe("Agenda\n\nhttps://meet.example.test/x");
    expect(prefill.attendees).toBeUndefined();
    expect(prefill.start.toISOString()).toBe("2026-11-24T13:00:00.000Z");
  });
});
