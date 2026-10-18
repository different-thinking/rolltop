// File overview: The create/edit dialog for one calendar event, plus the
// invitation answer and, in a Microsoft 365 calendar, the Teams meeting.
// Everything it submits travels to the calendar's provider before the local
// copy changes, so a rejected save leaves the dialog open with the reason.

import { useEffect, useMemo, useRef, useState } from "react";
import type { FormEvent } from "react";
import { api } from "../../api";
import type { CalendarAttendee, CalendarEvent, CalendarEventInput, CalendarSummary, ContactAutocomplete } from "../../types";
import { Icon } from "../../components/Icon";
import { calendarColor, eventEditable, providerName } from "./weekModel";

/** responseOptions are the answers an invitee can give. */
export const responseOptions: { value: string; label: string }[] = [
  { value: "accepted", label: "Yes" },
  { value: "tentative", label: "Maybe" },
  { value: "declined", label: "No" }
];

type DraftState = {
  calendarID: number;
  summary: string;
  location: string;
  description: string;
  allDay: boolean;
  /** startDate and endDate are YYYY-MM-DD; endDate is inclusive in the form
   * because "17th to 17th" is how people describe a one-day event. The API
   * wants an exclusive bound and gets one on submit. */
  startDate: string;
  endDate: string;
  /** startTime and endTime are HH:mm in the viewer's own zone. */
  startTime: string;
  endTime: string;
  attendees: CalendarAttendee[];
  /** copy is the "also add to the second calendar" box. It is only sent when
   * the box is shown, so an edit that never offered it keeps the copies. */
  copy: boolean;
  /** busyCopy is the "block the time in the busy calendar" box, sent under
   * the same rule. */
  busyCopy: boolean;
  /** onlineMeeting is the "Teams meeting" box. */
  onlineMeeting: boolean;
  /** travelBeforeOn and travelAfterOn are the travel boxes; the minutes are
   * kept as typed, so unticking and ticking a box again keeps the number. */
  travelBeforeOn: boolean;
  travelBefore: string;
  travelAfterOn: boolean;
  travelAfter: string;
};

/** defaultTravelMinutes is what a travel box offers when it is ticked. */
const defaultTravelMinutes = 30;

/** maxTravelMinutes matches the server's bound on one journey. */
const maxTravelMinutes = 12 * 60;

/** travelDraft fills the travel fields from the minutes an event keeps. */
function travelDraft(before: number | undefined, after: number | undefined) {
  return {
    travelBeforeOn: (before || 0) > 0,
    travelBefore: String((before || 0) > 0 ? before : defaultTravelMinutes),
    travelAfterOn: (after || 0) > 0,
    travelAfter: String((after || 0) > 0 ? after : defaultTravelMinutes)
  };
}

/** travelValue reads one travel field, or null when it is ticked but holds no
 * usable number of minutes. */
function travelValue(on: boolean, minutes: string): number | null {
  if (!on) return 0;
  const value = Number(minutes.trim());
  if (!Number.isInteger(value) || value <= 0 || value > maxTravelMinutes) return null;
  return value;
}

function pad(value: number): string {
  return String(value).padStart(2, "0");
}

function localDateValue(date: Date): string {
  return `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())}`;
}

function localTimeValue(date: Date): string {
  return `${pad(date.getHours())}:${pad(date.getMinutes())}`;
}

function utcDateValue(date: Date): string {
  return `${date.getUTCFullYear()}-${pad(date.getUTCMonth() + 1)}-${pad(date.getUTCDate())}`;
}

/** draftFromEvent fills the form from an existing event, reading an all-day
 * event's bounds in UTC because that is the zone they were anchored in. */
function draftFromEvent(event: CalendarEvent, copyTargetID: number, busyTargetID: number): DraftState {
  const start = new Date(event.start_at);
  const end = new Date(event.end_at);
  const copy = (event.also_in || []).some((item) => item.calendar_id === copyTargetID);
  const busyCopy = (event.also_in || []).some((item) => item.calendar_id === busyTargetID);
  if (event.all_day) {
    const inclusiveEnd = new Date(end.getTime() - 86_400_000);
    return {
      calendarID: event.calendar_id,
      summary: event.summary,
      location: event.location,
      description: event.description,
      allDay: true,
      startDate: utcDateValue(start),
      endDate: utcDateValue(inclusiveEnd >= start ? inclusiveEnd : start),
      startTime: "09:00",
      endTime: "10:00",
      attendees: event.attendees || [],
      copy,
      busyCopy,
      onlineMeeting: Boolean(event.online_meeting),
      ...travelDraft(0, 0)
    };
  }
  return {
    calendarID: event.calendar_id,
    summary: event.summary,
    location: event.location,
    description: event.description,
    allDay: false,
    startDate: localDateValue(start),
    endDate: localDateValue(end),
    startTime: localTimeValue(start),
    endTime: localTimeValue(end),
    attendees: event.attendees || [],
    copy,
    busyCopy,
    onlineMeeting: Boolean(event.online_meeting),
    ...travelDraft(event.travel_before_minutes, event.travel_after_minutes)
  };
}

/** draftForNewEvent starts a new event at a sensible hour of the chosen day. */
function draftForNewEvent(day: Date, calendarID: number): DraftState {
  const start = new Date(day);
  if (start.getHours() === 0 && start.getMinutes() === 0) start.setHours(9, 0, 0, 0);
  const end = new Date(start.getTime() + 3_600_000);
  return {
    calendarID,
    summary: "",
    location: "",
    description: "",
    allDay: false,
    startDate: localDateValue(start),
    endDate: localDateValue(end),
    startTime: localTimeValue(start),
    endTime: localTimeValue(end),
    attendees: [],
    copy: false,
    busyCopy: false,
    onlineMeeting: false,
    ...travelDraft(0, 0)
  };
}

/** EventPrefill seeds a new event with more than a day: the stretch the reader
 * drew in the week, or everything of an event they copied. start and end are
 * instants for a timed event; for an all-day one they are the UTC midnights an
 * all-day event is stored with, the end exclusive. */
export type EventPrefill = {
  start: Date;
  end: Date;
  allDay: boolean;
  /** calendarID is the calendar to offer first, when the reader may write to
   * it; the lane a new event was drawn in, or the copied event's own. */
  calendarID?: number;
  summary?: string;
  location?: string;
  description?: string;
  attendees?: CalendarAttendee[];
  copy?: boolean;
  busyCopy?: boolean;
  onlineMeeting?: boolean;
  /** travelBefore and travelAfter are the journey's minutes, 0 for none. */
  travelBefore?: number;
  travelAfter?: number;
};

/** draftFromPrefill fills the form for a new event from a prefill. */
function draftFromPrefill(prefill: EventPrefill, calendarID: number): DraftState {
  const base = draftForNewEvent(prefill.start, calendarID);
  const filled = {
    ...base,
    summary: prefill.summary || "",
    location: prefill.location || "",
    description: prefill.description || "",
    // Only who is invited carries over; their answers were to the other event.
    // The reader is the organizer of the new one, not a guest at it.
    attendees: (prefill.attendees || [])
      .filter((attendee) => !attendee.self)
      .map((attendee) => ({ ...attendee, response: "", organizer: false, self: false })),
    copy: Boolean(prefill.copy),
    busyCopy: Boolean(prefill.busyCopy),
    onlineMeeting: Boolean(prefill.onlineMeeting),
    ...travelDraft(prefill.travelBefore, prefill.travelAfter)
  };
  if (prefill.allDay) {
    const inclusiveEnd = new Date(prefill.end.getTime() - 86_400_000);
    return {
      ...filled,
      allDay: true,
      startDate: utcDateValue(prefill.start),
      endDate: utcDateValue(inclusiveEnd >= prefill.start ? inclusiveEnd : prefill.start),
      // The bounds are UTC midnights, which read in the viewer's zone are some
      // other hour; unticking All day starts from the same hours an existing
      // all-day event offers.
      startTime: "09:00",
      endTime: "10:00"
    };
  }
  return {
    ...filled,
    allDay: false,
    startDate: localDateValue(prefill.start),
    endDate: localDateValue(prefill.end),
    startTime: localTimeValue(prefill.start),
    endTime: localTimeValue(prefill.end)
  };
}

/** toInput renders the form as the API payload, converting the inclusive end
 * date back into the exclusive bound the backend and Google both use. */
function toInput(draft: DraftState): { input: CalendarEventInput; problem: string } {
  const summary = draft.summary.trim();
  if (!summary) return { input: blankInput(), problem: "Give the event a title." };
  const attendees = draft.attendees
    .filter((attendee) => attendee.email.trim())
    .map((attendee) => ({ email: attendee.email.trim(), name: attendee.name.trim(), optional: attendee.optional }));

  if (draft.allDay) {
    const start = parseUTCDate(draft.startDate);
    const inclusiveEnd = parseUTCDate(draft.endDate || draft.startDate);
    if (!start || !inclusiveEnd) return { input: blankInput(), problem: "Pick a start and an end date." };
    if (inclusiveEnd < start) return { input: blankInput(), problem: "The event has to end on or after it starts." };
    const end = new Date(inclusiveEnd.getTime() + 86_400_000);
    return {
      input: {
        calendar_id: draft.calendarID,
        summary,
        description: draft.description,
        location: draft.location.trim(),
        start_at: start.toISOString(),
        end_at: end.toISOString(),
        all_day: true,
        // The dates are what count; the zone only tells Microsoft 365 which
        // midnight to keep the day at. Google ignores it for an all-day event.
        time_zone: Intl.DateTimeFormat().resolvedOptions().timeZone || "",
        attendees,
        online_meeting: draft.onlineMeeting
      },
      problem: ""
    };
  }

  const start = parseLocalDateTime(draft.startDate, draft.startTime);
  const end = parseLocalDateTime(draft.endDate || draft.startDate, draft.endTime);
  if (!start || !end) return { input: blankInput(), problem: "Pick a start and an end time." };
  if (end <= start) return { input: blankInput(), problem: "The event has to end after it starts." };
  return {
    input: {
      calendar_id: draft.calendarID,
      summary,
      description: draft.description,
      location: draft.location.trim(),
      start_at: start.toISOString(),
      end_at: end.toISOString(),
      all_day: false,
      // The instant is already absolute; the zone only tells Google which one
      // to show the event in.
      time_zone: Intl.DateTimeFormat().resolvedOptions().timeZone || "",
      attendees,
      online_meeting: draft.onlineMeeting
    },
    problem: ""
  };
}

function blankInput(): CalendarEventInput {
  return {
    calendar_id: 0,
    summary: "",
    description: "",
    location: "",
    start_at: "",
    end_at: "",
    all_day: false,
    time_zone: "",
    attendees: []
  };
}

function parseUTCDate(value: string): Date | null {
  const match = /^(\d{4})-(\d{2})-(\d{2})$/.exec(value);
  if (!match) return null;
  return new Date(Date.UTC(Number(match[1]), Number(match[2]) - 1, Number(match[3])));
}

function parseLocalDateTime(date: string, time: string): Date | null {
  const dateMatch = /^(\d{4})-(\d{2})-(\d{2})$/.exec(date);
  const timeMatch = /^(\d{2}):(\d{2})$/.exec(time);
  if (!dateMatch || !timeMatch) return null;
  const parsed = new Date(
    Number(dateMatch[1]),
    Number(dateMatch[2]) - 1,
    Number(dateMatch[3]),
    Number(timeMatch[1]),
    Number(timeMatch[2])
  );
  return Number.isNaN(parsed.getTime()) ? null : parsed;
}

/**
 * EventDialog edits one event. It is deliberately a controlled dialog rather
 * than a route: the week behind it stays on screen, which is the context that
 * makes a time worth choosing.
 */
export function EventDialog({
  event,
  day,
  prefill,
  calendars,
  saving,
  problem,
  onSave,
  onDelete,
  onRespond,
  onClose
}: {
  /** event is null when creating. */
  event: CalendarEvent | null;
  /** day seeds a new event's date. */
  day: Date;
  /** prefill seeds a new event with more than its date; it wins over day. */
  prefill?: EventPrefill | null;
  calendars: CalendarSummary[];
  saving: boolean;
  problem: string;
  onSave: (input: CalendarEventInput) => void;
  onDelete: () => void;
  onRespond: (response: string) => void;
  onClose: () => void;
}) {
  // A new event goes only into a calendar the calendar view lists: one hidden
  // under settings is one the reader has said they do not use here.
  const writable = useMemo(() => calendars.filter((calendar) => calendar.can_write && calendar.listed), [calendars]);
  // Seeded once, on mount. The dialog is remounted by its caller whenever the
  // form should start over, so there is deliberately no effect resetting the
  // draft here: the calendar list is reloaded on a poll, after a sync and after
  // every write, and any reset keyed on it would wipe what the user is typing.
  const copyTarget = calendars.find((item) => item.copy_target && item.can_write);
  const busyTarget = calendars.find((item) => item.busy_target && item.can_write);
  const [draft, setDraft] = useState<DraftState>(() => {
    if (event) return draftFromEvent(event, copyTarget?.id || 0, busyTarget?.id || 0);
    if (prefill) {
      const preferred = writable.find((item) => item.id === prefill.calendarID);
      return draftFromPrefill(prefill, (preferred || writable[0])?.id || 0);
    }
    return draftForNewEvent(day, writable[0]?.id || 0);
  });
  const [localProblem, setLocalProblem] = useState("");
  const [confirmingDelete, setConfirmingDelete] = useState(false);
  const titleRef = useRef<HTMLInputElement | null>(null);

  useEffect(() => {
    titleRef.current?.focus();
  }, []);

  useEffect(() => {
    const onKey = (keyEvent: KeyboardEvent) => {
      if (keyEvent.key === "Escape") onClose();
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [onClose]);

  const calendar = calendars.find((item) => item.id === draft.calendarID);
  const provider = providerName(calendar);
  // An event in a calendar shared read-only can still be answered, but nothing
  // about it can be edited, so the form is shown as a read-only summary. The
  // same goes for a Microsoft 365 meeting somebody else organizes: only its
  // organizer can change it, and the reader's part is the answer.
  const invitedToMicrosoftMeeting = Boolean(event?.my_response) && calendar?.provider === "microsoft";
  const editable = !event || eventEditable(event, calendar);
  const update = (patch: Partial<DraftState>) => setDraft((current) => ({ ...current, ...patch }));
  // The box is offered whenever the event lives anywhere but the second
  // calendar itself; an event already in it has nothing to copy.
  // A placeholder is the time of an event entered elsewhere and nothing more,
  // so nothing can be copied from it; it moves when that event does.
  const placeholder = Boolean(event?.link_masked);
  // A placeholder cannot be deleted while its event exists, just as the copy
  // in the second calendar is the event's and not a thing of its own: it goes
  // when the box on the event is unticked or the event is deleted. One whose
  // event is gone is an ordinary event and can be deleted like one.
  const placeholderOfEvent = placeholder && (event?.also_in || []).length > 0;
  // A travel block standing alone is the reader's journey, never an
  // appointment to keep in another calendar: the server refuses to copy it.
  const travelBlock = Boolean(event?.travel);
  const offerCopy =
    editable && !placeholder && !travelBlock && Boolean(copyTarget) && copyTarget?.id !== draft.calendarID;
  const offerBusyCopy =
    editable && !placeholder && !travelBlock && Boolean(busyTarget) && busyTarget?.id !== draft.calendarID;
  const alsoIn = (event?.also_in || [])
    .map((item) => calendars.find((option) => option.id === item.calendar_id))
    .filter((item): item is CalendarSummary => Boolean(item));
  // While the box is shown it already says whether the second calendar holds
  // a copy, so the line only names the calendars it does not cover.
  const alsoInOthers = alsoIn.filter(
    (item) => !(offerCopy && item.id === copyTarget?.id) && !(offerBusyCopy && item.id === busyTarget?.id)
  );
  // A copy carries no guests -- they were invited by the event that was
  // entered -- so a guest list edited here would invite them all a second time.
  const guestsElsewhere = (Boolean(event) && alsoIn.length > 0 && !event?.link_primary) || placeholder;
  // A Teams meeting is offered where the calendar can hold one, and never on
  // the copy of an event: the meeting belongs to the event that was entered.
  // Once a meeting is online it stays so -- Outlook keeps it whatever a later
  // write says -- so the box cannot be unticked again.
  const offerOnlineMeeting =
    editable && (calendar?.online_meeting_providers || []).length > 0 && !guestsElsewhere;
  const meetingLocked = Boolean(event?.online_meeting);
  // Travel is the reader's own time around an appointment with a time of day.
  // A placeholder has no journey of its own -- it moves the event's -- and a
  // travel block standing alone is a journey, not something to travel to.
  const offerTravel = editable && !placeholder && !travelBlock && !draft.allDay;
  const travelTimes = useMemo(() => {
    if (draft.allDay) return { leave: "", back: "" };
    const start = parseLocalDateTime(draft.startDate, draft.startTime);
    const end = parseLocalDateTime(draft.endDate || draft.startDate, draft.endTime);
    const before = travelValue(draft.travelBeforeOn, draft.travelBefore);
    const after = travelValue(draft.travelAfterOn, draft.travelAfter);
    const clock = (value: Date) => value.toLocaleTimeString(undefined, { hour: "numeric", minute: "2-digit" });
    return {
      leave: start && before ? `leave at ${clock(new Date(start.getTime() - before * 60_000))}` : "",
      back: end && after ? `back at ${clock(new Date(end.getTime() + after * 60_000))}` : ""
    };
  }, [draft]);

  const submit = (formEvent: FormEvent) => {
    formEvent.preventDefault();
    const { input, problem: issue } = toInput(draft);
    if (issue) {
      setLocalProblem(issue);
      return;
    }
    if (input.calendar_id <= 0) {
      setLocalProblem("Pick a calendar to save this to.");
      return;
    }
    const travelBefore = travelValue(draft.travelBeforeOn, draft.travelBefore);
    const travelAfter = travelValue(draft.travelAfterOn, draft.travelAfter);
    if (offerTravel && (travelBefore === null || travelAfter === null)) {
      setLocalProblem("Enter the travel time in whole minutes, up to 12 hours.");
      return;
    }
    setLocalProblem("");
    const withMeeting = { ...input, online_meeting: offerOnlineMeeting && draft.onlineMeeting };
    onSave({
      ...withMeeting,
      // Only sent while the boxes are shown: an event whose dialog never
      // offered them keeps the travel it has.
      ...(offerTravel ? { travel_before_minutes: travelBefore || 0, travel_after_minutes: travelAfter || 0 } : {}),
      ...(offerCopy ? { copy: draft.copy } : {}),
      ...(offerBusyCopy ? { busy_copy: draft.busyCopy } : {})
    });
  };

  const message = localProblem || problem;

  return (
    <div
      className="calendar-dialog-backdrop"
      role="presentation"
      onMouseDown={(mouseEvent) => {
        if (mouseEvent.target === mouseEvent.currentTarget) onClose();
      }}
    >
      <form className="calendar-dialog" onSubmit={submit} aria-label={event ? "Edit event" : "New event"}>
        <div className="calendar-dialog-header">
          <h2>{event ? "Event" : "New event"}</h2>
          <button type="button" className="icon-button" onClick={onClose} title="Close">
            <Icon name="close" />
          </button>
        </div>

        <div className="calendar-dialog-body">
          {event?.pending_offline ? (
            <p className="calendar-dialog-note calendar-dialog-pending">
              {event.pending_error
                ? `Your calendar refused this event: ${event.pending_error} Change it and save again, or discard it.`
                : "Entered offline. It is kept on this device and will be added to your calendar when you are back online."}
            </p>
          ) : null}
          {event && event.my_response ? (
            <div className="calendar-rsvp">
              <span className="calendar-rsvp-label">Are you going?</span>
              <div className="calendar-rsvp-actions">
                {responseOptions.map((option) => (
                  <button
                    key={option.value}
                    type="button"
                    className={`ghost ${event.my_response === option.value ? "active" : ""}`}
                    disabled={saving}
                    onClick={() => onRespond(option.value)}
                  >
                    {option.label}
                  </button>
                ))}
              </div>
            </div>
          ) : null}

          <div>
            <label htmlFor="calendar-event-title">Title</label>
            <input
              id="calendar-event-title"
              ref={titleRef}
              value={draft.summary}
              disabled={!editable || saving}
              onChange={(changeEvent) => update({ summary: changeEvent.target.value })}
              placeholder="What is it?"
            />
          </div>

          <label className="calendar-checkbox">
            <input
              type="checkbox"
              checked={draft.allDay}
              disabled={!editable || saving}
              onChange={(changeEvent) => update({ allDay: changeEvent.target.checked })}
            />
            All day
          </label>

          <div className="calendar-dialog-times">
            <div>
              <label htmlFor="calendar-event-start-date">Starts</label>
              <div className="calendar-dialog-when">
                <input
                  id="calendar-event-start-date"
                  type="date"
                  value={draft.startDate}
                  disabled={!editable || saving}
                  onChange={(changeEvent) => update({ startDate: changeEvent.target.value })}
                />
                {draft.allDay ? null : (
                  <input
                    type="time"
                    aria-label="Start time"
                    value={draft.startTime}
                    disabled={!editable || saving}
                    onChange={(changeEvent) => update({ startTime: changeEvent.target.value })}
                  />
                )}
              </div>
            </div>
            <div>
              <label htmlFor="calendar-event-end-date">Ends</label>
              <div className="calendar-dialog-when">
                <input
                  id="calendar-event-end-date"
                  type="date"
                  value={draft.endDate}
                  disabled={!editable || saving}
                  onChange={(changeEvent) => update({ endDate: changeEvent.target.value })}
                />
                {draft.allDay ? null : (
                  <input
                    type="time"
                    aria-label="End time"
                    value={draft.endTime}
                    disabled={!editable || saving}
                    onChange={(changeEvent) => update({ endTime: changeEvent.target.value })}
                  />
                )}
              </div>
            </div>
          </div>

          <div>
            <label htmlFor="calendar-event-calendar">Calendar</label>
            <select
              id="calendar-event-calendar"
              value={draft.calendarID}
              // Moving an event between calendars is a Google operation of its
              // own, so the picker only chooses where a new event goes.
              disabled={Boolean(event) || saving}
              onChange={(changeEvent) => update({ calendarID: Number(changeEvent.target.value) })}
            >
              {(event ? calendars : writable).map((option) => (
                <option key={option.id} value={option.id}>
                  {option.name}
                  {option.connection_email ? ` — ${option.connection_email}` : ""}
                </option>
              ))}
            </select>
          </div>

          {offerCopy && copyTarget ? (
            <label className="calendar-checkbox">
              <input
                type="checkbox"
                checked={draft.copy}
                disabled={saving}
                onChange={(changeEvent) => update({ copy: changeEvent.target.checked })}
              />
              <span className="calendar-swatch" style={{ background: calendarColor(copyTarget) }} />
              Also add to {copyTarget.name}
            </label>
          ) : null}

          {offerBusyCopy && busyTarget ? (
            <label className="calendar-checkbox">
              <input
                type="checkbox"
                checked={draft.busyCopy}
                disabled={saving}
                onChange={(changeEvent) => update({ busyCopy: changeEvent.target.checked })}
              />
              <span className="calendar-swatch" style={{ background: calendarColor(busyTarget) }} />
              Block the time in {busyTarget.name} as “{busyTarget.busy_title}”
            </label>
          ) : null}

          {placeholder ? (
            <p className="calendar-dialog-note">
              This is a placeholder: it shows only the time of an event in another calendar. Moving it moves that
              event too; its title, notes and guests stay there.
              {placeholderOfEvent
                ? " To remove it, untick “Block the time” on that event, or delete the event itself."
                : ""}
            </p>
          ) : null}

          {alsoInOthers.length > 0 ? (
            <div className="calendar-also-in">
              <span className="calendar-also-in-label">
                <Icon name="copy" />
                Also in
              </span>
              {alsoInOthers.map((item) => (
                <span key={item.id} className="calendar-also-in-item" title={item.connection_email || undefined}>
                  <span className="calendar-swatch" style={{ background: calendarColor(item) }} />
                  {item.name}
                </span>
              ))}
            </div>
          ) : null}

          {offerOnlineMeeting ? (
            <label className="calendar-checkbox">
              <input
                type="checkbox"
                checked={draft.onlineMeeting}
                disabled={saving || meetingLocked}
                onChange={(changeEvent) => update({ onlineMeeting: changeEvent.target.checked })}
              />
              <Icon name="video" />
              Teams meeting
            </label>
          ) : null}

          {event?.online_meeting_url ? (
            <p className="calendar-dialog-note calendar-join">
              <a href={event.online_meeting_url} target="_blank" rel="noreferrer noopener">
                <Icon name="video" />
                {event.online_meeting_provider === "googleMeet" ? "Join Google Meet" : "Join Teams meeting"}
              </a>
            </p>
          ) : event?.online_meeting ? (
            <p className="calendar-dialog-note">The meeting link appears once {provider} has created it.</p>
          ) : null}

          <div>
            <label htmlFor="calendar-event-location">Location</label>
            <input
              id="calendar-event-location"
              value={draft.location}
              disabled={!editable || saving || placeholder}
              onChange={(changeEvent) => update({ location: changeEvent.target.value })}
              placeholder="Where?"
            />
          </div>

          {offerTravel ? (
            <div className="calendar-travel" role="group" aria-label="Travel time">
              <TravelField
                id="calendar-event-travel-before"
                label="Travel there"
                on={draft.travelBeforeOn}
                minutes={draft.travelBefore}
                hint={travelTimes.leave}
                disabled={saving}
                onToggle={(on) => update({ travelBeforeOn: on })}
                onMinutes={(minutes) => update({ travelBefore: minutes })}
              />
              <TravelField
                id="calendar-event-travel-after"
                label="Travel back"
                on={draft.travelAfterOn}
                minutes={draft.travelAfter}
                hint={travelTimes.back}
                disabled={saving}
                onToggle={(on) => update({ travelAfterOn: on })}
                onMinutes={(minutes) => update({ travelAfter: minutes })}
              />
              {draft.travelBeforeOn || draft.travelAfterOn ? (
                <p className="calendar-dialog-note">
                  Blocked as its own time in {provider} and in every calendar this event is copied to, so others see
                  you as busy. It moves and is deleted with this event.
                </p>
              ) : null}
            </div>
          ) : null}
          {event?.travel ? (
            <p className="calendar-dialog-note">
              This is travel time Rolltop blocked for an event that is not shown next to it — the event is in another
              week, in a calendar that is switched off, or was moved or deleted somewhere else. Delete it only if you no
              longer need it.
            </p>
          ) : null}

          <AttendeeEditor
            attendees={draft.attendees}
            disabled={!editable || saving || guestsElsewhere}
            onChange={(attendees) => update({ attendees })}
          />
          {guestsElsewhere && !placeholder ? (
            <p className="calendar-dialog-note">
              This is the copy. Guests are invited from the original event, so they cannot be changed here.
            </p>
          ) : null}

          <div>
            <label htmlFor="calendar-event-notes">Notes</label>
            <textarea
              id="calendar-event-notes"
              rows={4}
              value={draft.description}
              disabled={!editable || saving || placeholder}
              onChange={(changeEvent) => update({ description: changeEvent.target.value })}
            />
          </div>

          {event && !editable ? (
            <p className="calendar-dialog-note">
              {invitedToMicrosoftMeeting
                ? "Only the organizer can change this meeting. Answering the invitation still works."
                : "This calendar is shared read-only, so the event cannot be changed here. Answering an invitation still works."}
            </p>
          ) : null}
          {event?.status === "cancelled" ? (
            <p className="calendar-dialog-note">The organizer cancelled this meeting.</p>
          ) : null}
          {event?.html_link ? (
            <p className="calendar-dialog-note">
              <a href={event.html_link} target="_blank" rel="noreferrer noopener">
                {calendar?.provider === "microsoft" ? "Open in Outlook" : "Open in Google Calendar"}
              </a>
            </p>
          ) : null}
          {message ? <p className="calendar-dialog-problem">{message}</p> : null}
        </div>

        <div className="calendar-dialog-footer">
          {event?.pending_offline ? (
            <button type="button" className="ghost" disabled={saving} onClick={onDelete}>
              Discard
            </button>
          ) : event && editable && !placeholderOfEvent ? (
            confirmingDelete ? (
              <div className="calendar-dialog-confirm">
                <span>
                  {alsoIn.length > 0
                      ? `Delete this event in ${provider} too, including the copy in ${alsoIn.map((item) => item.name).join(", ")}?`
                      : event.attendees.length > 0 && !event.my_response
                        ? `Delete this event in ${provider} too? The guests are told it is cancelled.`
                        : `Delete this event in ${provider} too?`}
                  {(event.travel_before_minutes || 0) > 0 || (event.travel_after_minutes || 0) > 0
                    ? " Its travel time goes with it."
                    : ""}
                </span>
                <button type="button" className="danger" disabled={saving} onClick={onDelete}>
                  Delete
                </button>
                <button type="button" className="ghost" disabled={saving} onClick={() => setConfirmingDelete(false)}>
                  Keep
                </button>
              </div>
            ) : (
              <button type="button" className="ghost" disabled={saving} onClick={() => setConfirmingDelete(true)}>
                Delete
              </button>
            )
          ) : (
            <span />
          )}
          <div className="calendar-dialog-actions">
            <button type="button" className="ghost" disabled={saving} onClick={onClose}>
              Close
            </button>
            {editable ? (
              <button type="submit" disabled={saving}>
                {saving ? "Saving…" : "Save"}
              </button>
            ) : null}
          </div>
        </div>
      </form>
    </div>
  );
}

/** TravelField is one side of the journey: a box to ask for it and the
 * minutes it takes. */
function TravelField({
  id,
  label,
  on,
  minutes,
  hint,
  disabled,
  onToggle,
  onMinutes
}: {
  id: string;
  label: string;
  on: boolean;
  minutes: string;
  hint: string;
  disabled: boolean;
  onToggle: (on: boolean) => void;
  onMinutes: (minutes: string) => void;
}) {
  return (
    <div className="calendar-travel-row">
      <label className="calendar-checkbox">
        <input type="checkbox" checked={on} disabled={disabled} onChange={(changeEvent) => onToggle(changeEvent.target.checked)} />
        <Icon name="car" />
        {label}
      </label>
      {on ? (
        <>
          <input
            id={id}
            className="calendar-travel-minutes"
            type="number"
            inputMode="numeric"
            min={1}
            max={maxTravelMinutes}
            // Any whole minute is a journey. A coarser step makes the browser
            // count its valid values from min -- 1, 6, 11 ... -- and refuse
            // the round numbers people actually type, 30 among them.
            step={1}
            value={minutes}
            disabled={disabled}
            aria-label={`${label}, minutes`}
            onChange={(changeEvent) => onMinutes(changeEvent.target.value)}
          />
          <span className="calendar-travel-unit">min</span>
          {hint ? <span className="calendar-travel-hint">{hint}</span> : null}
        </>
      ) : null}
    </div>
  );
}

/** AttendeeEditor adds and removes guests, suggesting from the address book
 * that Phase 3 fills. */
function AttendeeEditor({
  attendees,
  disabled,
  onChange
}: {
  attendees: CalendarAttendee[];
  disabled: boolean;
  onChange: (attendees: CalendarAttendee[]) => void;
}) {
  const [query, setQuery] = useState("");
  const [suggestions, setSuggestions] = useState<ContactAutocomplete[]>([]);

  useEffect(() => {
    const term = query.trim();
    if (term.length < 2) {
      setSuggestions([]);
      return;
    }
    let cancelled = false;
    const timer = window.setTimeout(() => {
      void api
        .contactAutocomplete(term)
        .then((data) => {
          if (!cancelled) setSuggestions(data.contacts || []);
        })
        .catch(() => {
          // Suggestions are a convenience; typing the address still works.
          if (!cancelled) setSuggestions([]);
        });
    }, 150);
    return () => {
      cancelled = true;
      window.clearTimeout(timer);
    };
  }, [query]);

  const add = (email: string, name: string) => {
    const address = email.trim();
    if (!address) return;
    if (attendees.some((attendee) => attendee.email.toLowerCase() === address.toLowerCase())) {
      setQuery("");
      return;
    }
    onChange([
      ...attendees,
      { email: address, name: name.trim(), response: "", optional: false, organizer: false, self: false, resource: false }
    ]);
    setQuery("");
    setSuggestions([]);
  };

  return (
    <div className="calendar-attendees">
      <label htmlFor="calendar-event-guest">Guests</label>
      {attendees.length > 0 ? (
        <ul className="calendar-attendee-list">
          {attendees.map((attendee) => (
            <li key={attendee.email} className={`calendar-attendee response-${attendee.response || "none"}`}>
              <span className="calendar-attendee-name">{attendee.name || attendee.email}</span>
              {attendee.name ? <span className="calendar-attendee-email">{attendee.email}</span> : null}
              {attendee.organizer ? <span className="calendar-attendee-tag">Organizer</span> : null}
              {disabled ? null : (
                <button
                  type="button"
                  className="icon-button"
                  title={`Remove ${attendee.email}`}
                  onClick={() => onChange(attendees.filter((item) => item.email !== attendee.email))}
                >
                  <Icon name="close" />
                </button>
              )}
            </li>
          ))}
        </ul>
      ) : null}
      {disabled ? null : (
        <>
          <input
            id="calendar-event-guest"
            value={query}
            placeholder="Add a guest by name or address"
            onChange={(changeEvent) => setQuery(changeEvent.target.value)}
            onKeyDown={(keyEvent) => {
              if (keyEvent.key !== "Enter" && keyEvent.key !== ",") return;
              keyEvent.preventDefault();
              add(query, "");
            }}
          />
          {suggestions.length > 0 ? (
            <ul className="calendar-attendee-suggestions">
              {suggestions.slice(0, 6).map((suggestion) => (
                <li key={`${suggestion.contact_id}-${suggestion.email}`}>
                  <button type="button" onClick={() => add(suggestion.email, suggestion.name)}>
                    <span>{suggestion.name || suggestion.email}</span>
                    {suggestion.name ? <small>{suggestion.email}</small> : null}
                  </button>
                </li>
              ))}
            </ul>
          ) : null}
        </>
      )}
    </div>
  );
}
