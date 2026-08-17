// File overview: The create/edit dialog for one calendar event, plus the
// invitation answer. Everything it submits travels to Google before the local
// copy changes, so a rejected save leaves the dialog open with the reason.

import { useEffect, useMemo, useRef, useState } from "react";
import type { FormEvent } from "react";
import { api } from "../../api";
import type { CalendarAttendee, CalendarEvent, CalendarEventInput, CalendarSummary, ContactAutocomplete } from "../../types";
import { Icon } from "../../components/Icon";

/** responseOptions are the answers an invitee can give. */
const responseOptions: { value: string; label: string }[] = [
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
};

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
function draftFromEvent(event: CalendarEvent): DraftState {
  const start = new Date(event.start_at);
  const end = new Date(event.end_at);
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
      attendees: event.attendees || []
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
    attendees: event.attendees || []
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
    attendees: []
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
        time_zone: "",
        attendees
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
      attendees
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
  calendars: CalendarSummary[];
  saving: boolean;
  problem: string;
  onSave: (input: CalendarEventInput) => void;
  onDelete: () => void;
  onRespond: (response: string) => void;
  onClose: () => void;
}) {
  const writable = useMemo(() => calendars.filter((calendar) => calendar.can_write), [calendars]);
  const [draft, setDraft] = useState<DraftState>(() =>
    event ? draftFromEvent(event) : draftForNewEvent(day, writable[0]?.id || 0)
  );
  const [localProblem, setLocalProblem] = useState("");
  const [confirmingDelete, setConfirmingDelete] = useState(false);
  const titleRef = useRef<HTMLInputElement | null>(null);

  useEffect(() => {
    setDraft(event ? draftFromEvent(event) : draftForNewEvent(day, writable[0]?.id || 0));
    setLocalProblem("");
    setConfirmingDelete(false);
  }, [day, event, writable]);

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
  // An event in a calendar shared read-only can still be answered, but nothing
  // about it can be edited, so the form is shown as a read-only summary.
  const editable = !event || Boolean(calendar?.can_write);
  const update = (patch: Partial<DraftState>) => setDraft((current) => ({ ...current, ...patch }));

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
    setLocalProblem("");
    onSave(input);
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

          <div>
            <label htmlFor="calendar-event-location">Location</label>
            <input
              id="calendar-event-location"
              value={draft.location}
              disabled={!editable || saving}
              onChange={(changeEvent) => update({ location: changeEvent.target.value })}
              placeholder="Where?"
            />
          </div>

          <AttendeeEditor
            attendees={draft.attendees}
            disabled={!editable || saving}
            onChange={(attendees) => update({ attendees })}
          />

          <div>
            <label htmlFor="calendar-event-notes">Notes</label>
            <textarea
              id="calendar-event-notes"
              rows={4}
              value={draft.description}
              disabled={!editable || saving}
              onChange={(changeEvent) => update({ description: changeEvent.target.value })}
            />
          </div>

          {event && !editable ? (
            <p className="calendar-dialog-note">
              This calendar is shared read-only, so the event cannot be changed here. Answering an invitation still works.
            </p>
          ) : null}
          {event?.html_link ? (
            <p className="calendar-dialog-note">
              <a href={event.html_link} target="_blank" rel="noreferrer noopener">
                Open in Google Calendar
              </a>
            </p>
          ) : null}
          {message ? <p className="calendar-dialog-problem">{message}</p> : null}
        </div>

        <div className="calendar-dialog-footer">
          {event && editable ? (
            confirmingDelete ? (
              <div className="calendar-dialog-confirm">
                <span>Delete this event in Google too?</span>
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
