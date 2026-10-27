import { act } from "react";
import { createRoot, type Root } from "react-dom/client";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { CalendarEvent, CalendarSummary } from "../../types";
import { copyPrefill, EventDialog, withoutMeetingJoinDetails, withStartMoved } from "./EventDialog";
import type { EventPrefill } from "./EventDialog";

(globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;

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

const teamsBlock = [
  "________________________________________________________________________________",
  "Microsoft Teams meeting",
  "Join on your computer, mobile app or room device",
  "Click here to join the meeting<https://teams.microsoft.com/l/meetup-join/abc>",
  "Meeting ID: 123 456 789",
  "________________________________________________________________________________"
].join("\r\n");

describe("withoutMeetingJoinDetails", () => {
  it("takes the Teams join block out and keeps the agenda around it", () => {
    const notes = `Agenda:\r\n1. Budget\r\n\r\n${teamsBlock}\r\n\r\nBring the figures.`;
    expect(withoutMeetingJoinDetails(notes, "")).toBe("Agenda:\n1. Budget\n\nBring the figures.");
  });

  it("takes a block left open at the end", () => {
    const open = teamsBlock.split("\r\n").slice(0, -1).join("\n");
    expect(withoutMeetingJoinDetails(`Agenda\n${open}`, "")).toBe("Agenda");
  });

  it("recognises the event's own join link when the host is not a known one", () => {
    const notes = "Hi\n__________\nJoin: https://meet.example/xyz\n__________";
    expect(withoutMeetingJoinDetails(notes, "https://meet.example/xyz")).toBe("Hi");
  });

  it("leaves a fenced block without a meeting link alone", () => {
    const notes = "Intro\n__________\nSigned, the team\n__________\nEnd";
    expect(withoutMeetingJoinDetails(notes, "")).toBe(notes);
  });
});

function summary(id: number, extra: Partial<CalendarSummary> = {}): CalendarSummary {
  return {
    id,
    provider: "microsoft",
    connection_id: 1,
    connection_email: "reader@example.com",
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
    online_meeting_providers: ["teamsForBusiness"],
    default_reminder_minutes: [],
    synced_from: "",
    last_sync_at: "",
    status: "ok",
    status_detail: "",
    ...extra
  };
}

function meeting(extra: Partial<CalendarEvent> = {}): CalendarEvent {
  return {
    id: 7,
    calendar_id: 1,
    summary: "Weekly sync",
    description: `Agenda\n${teamsBlock}`,
    location: "Room 4",
    status: "confirmed",
    start_at: "2026-10-27T09:00:00.000Z",
    end_at: "2026-10-27T10:00:00.000Z",
    all_day: false,
    time_zone: "",
    recurring_event_id: "",
    organizer_email: "boss@example.com",
    organizer_name: "Boss",
    attendees: [
      { email: "boss@example.com", name: "Boss", response: "accepted", optional: false, organizer: true, self: false, resource: false },
      { email: "reader@example.com", name: "Reader", response: "accepted", optional: false, organizer: false, self: true, resource: false }
    ],
    my_response: "accepted",
    html_link: "",
    link_primary: true,
    link_masked: false,
    also_in: [{ event_id: 8, calendar_id: 2 }],
    online_meeting: true,
    online_meeting_provider: "teamsForBusiness",
    online_meeting_url: "https://teams.microsoft.com/l/meetup-join/abc",
    travel_before_minutes: 15,
    ...extra
  } as CalendarEvent;
}

describe("copyPrefill", () => {
  const range = { start: new Date("2026-10-27T09:00:00.000Z"), end: new Date("2026-10-27T10:00:00.000Z") };

  it("carries everything of the event, a new meeting and the second calendar included", () => {
    const prefill = copyPrefill(meeting(), range, [summary(1), summary(2, { copy_target: true })], true);
    expect(prefill).toMatchObject({
      start: range.start,
      end: range.end,
      allDay: false,
      calendarID: 1,
      summary: "Weekly sync",
      location: "Room 4",
      description: "Agenda",
      onlineMeeting: true,
      copy: true,
      busyCopy: false,
      travelBefore: 15,
      travelAfter: 0,
      duplicate: true
    });
    expect(prefill.attendees?.map((attendee) => attendee.email)).toEqual(["boss@example.com", "reader@example.com"]);
  });

  it("takes the join block out of an event that is not an online meeting itself", () => {
    // An Outlook invitation kept in a Google calendar, or the copy of a Teams
    // meeting in the second calendar: not a meeting, but the block is there.
    const prefill = copyPrefill(meeting({ online_meeting: false, online_meeting_url: "" }), range, [summary(1)]);
    expect(prefill.description).toBe("Agenda");
    expect(prefill.onlineMeeting).toBe(false);
    expect(prefill.duplicate).toBeUndefined();
  });

  it("keeps notes without a join block as they are", () => {
    const notes = "Agenda\n__________\nSigned, the team\n__________";
    const prefill = copyPrefill(meeting({ description: notes, online_meeting: false, online_meeting_url: "" }), range, [summary(1)]);
    expect(prefill.description).toBe(notes);
  });
});

describe("EventDialog duplicate", () => {
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

  const render = (event: CalendarEvent | null, calendars: CalendarSummary[], extra: { prefill?: EventPrefill; onDuplicate?: () => void } = {}) =>
    act(() =>
      root.render(
        <EventDialog
          event={event}
          day={new Date(2026, 9, 27)}
          prefill={extra.prefill}
          calendars={calendars}
          saving={false}
          problem=""
          onSave={() => undefined}
          onDelete={() => undefined}
          onRespond={() => undefined}
          onDuplicate={extra.onDuplicate}
          onClose={() => undefined}
        />
      )
    );
  const duplicateButton = () =>
    Array.from(container.querySelectorAll("button")).find((button) => button.textContent?.includes("Duplicate"));

  it("offers Duplicate on a meeting somebody else organizes, which the reader cannot edit", () => {
    const onDuplicate = vi.fn();
    render(meeting(), [summary(1), summary(2, { copy_target: true })], { onDuplicate });
    expect(container.querySelector<HTMLInputElement>("#calendar-event-title")?.disabled).toBe(true);
    act(() => duplicateButton()?.click());
    expect(onDuplicate).toHaveBeenCalledTimes(1);
  });

  it("offers no Duplicate on a placeholder or when no calendar can take one", () => {
    render(meeting({ link_masked: true }), [summary(1)], { onDuplicate: () => undefined });
    expect(duplicateButton()).toBeUndefined();
    render(meeting(), [summary(1, { can_write: false })], { onDuplicate: () => undefined });
    expect(duplicateButton()).toBeUndefined();
  });

  it("opens the duplicate as a new event with the guests, the meeting and the date focused", () => {
    const calendars = [summary(1), summary(2, { copy_target: true })];
    const range = { start: new Date("2026-10-27T09:00:00.000Z"), end: new Date("2026-10-27T10:00:00.000Z") };
    render(null, calendars, { prefill: copyPrefill(meeting(), range, calendars, true) });
    expect(container.querySelector("h2")?.textContent).toBe("Duplicate event");
    expect(container.querySelector<HTMLInputElement>("#calendar-event-title")?.value).toBe("Weekly sync");
    expect(container.querySelector<HTMLTextAreaElement>("#calendar-event-notes")?.value).toBe("Agenda");
    expect(document.activeElement?.id).toBe("calendar-event-start-date");
    const guests = Array.from(container.querySelectorAll(".calendar-attendee-email, .calendar-attendee-name")).map((node) => node.textContent);
    // The organizer becomes a guest; the reader organizes the new event.
    expect(guests).toContain("Boss");
    expect(guests).not.toContain("Reader");
    const meetingBox = Array.from(container.querySelectorAll("label")).find((label) => label.textContent?.includes("Teams meeting"));
    expect(meetingBox?.querySelector("input")?.checked).toBe(true);
    expect(duplicateButton()).toBeUndefined();
  });
});
