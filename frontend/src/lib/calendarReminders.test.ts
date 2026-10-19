import { describe, expect, it } from "vitest";
import { reminderBody, reminderLabel, reminderLead, reminderSnoozeChoices } from "./calendarReminders";

describe("calendar reminder wording", () => {
  it("says how far off the event is from the reminder's own offset", () => {
    expect(reminderLead(0, false)).toBe("Starting now");
    expect(reminderLead(10, false)).toBe("Starts in 10 minutes");
    expect(reminderLead(60, false)).toBe("Starts in 1 hour");
    expect(reminderLead(1440, false)).toBe("Starts tomorrow");
    expect(reminderLead(900, true)).toBe("Tomorrow");
    expect(reminderLead(0, true)).toBe("Today");
    expect(reminderLead(-5, false)).toBe("Started 5 minutes ago");
  });

  it("offers only times still ahead when putting a reminder off", () => {
    const now = new Date("2026-10-20T07:00:00Z");
    const soon = reminderSnoozeChoices("2026-10-20T07:10:00Z", false, now);
    expect(soon.map((choice) => choice.label)).toEqual(["In 5 minutes", "5 minutes before it starts", "When it starts", "In 15 minutes", "In 1 hour"]);
    const started = reminderSnoozeChoices("2026-10-20T06:50:00Z", false, now);
    expect(started.map((choice) => choice.label)).toEqual(["In 5 minutes", "In 15 minutes", "In 1 hour"]);
    expect(reminderSnoozeChoices("2026-10-21T00:00:00Z", true, now)).toHaveLength(3);
  });

  it("names reminders for the dialog", () => {
    expect(reminderLabel([])).toBe("None");
    expect(reminderLabel([10, 1440])).toBe("10 minutes before, 1 day before");
    expect(reminderLabel([0, 120])).toBe("At start, 2 hours before");
  });

  it("builds the body from lead, time and place", () => {
    const body = reminderBody({ start_at: "2026-10-20T07:00:00Z", end_at: "2026-10-20T08:00:00Z", all_day: false, minutes_before: 10, location: "Room 1" });
    expect(body.startsWith("Starts in 10 minutes · ")).toBe(true);
    expect(body.endsWith(" · Room 1")).toBe(true);
    expect(reminderBody({ start_at: "2026-10-20T00:00:00Z", end_at: "2026-10-21T00:00:00Z", all_day: true, minutes_before: 900, location: "" }))
      .toBe("Tomorrow · All day");
  });
});
