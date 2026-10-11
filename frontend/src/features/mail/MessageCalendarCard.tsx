/**
 * File overview: The event a message carries -- an invitation's .ics, or the
 * calendar part Outlook sends in its place -- shown above the message: what,
 * when, where and who asked, and the way into the reader's own calendar.
 *
 * Adding opens the calendar's own event dialog, seeded with the event, rather
 * than writing anything on a click: the reader picks the calendar and can
 * correct the time before it is entered. The invitation's guests are not
 * carried over -- they are the organizer's guests, and an event entered with
 * them would invite every one of them a second time from the reader's account.
 */

import { useEffect, useState } from "react";
import { createPortal } from "react-dom";
import { api } from "../../api";
import type { AddToast, DatePrefs } from "../../appTypes";
import type { CalendarEventInput, CalendarSummary, MessageCalendar, MessageCalendarEvent, MessageCalendarPerson } from "../../types";
import { Icon } from "../../components/Icon";
import { messageFromError } from "../../lib/errors";
import { EventDialog, responseOptions } from "../calendar/EventDialog";
import type { EventPrefill } from "../calendar/EventDialog";
import { calendarURL, startOfDay } from "../calendar/weekModel";

/** shownEvents bounds the card: an invitation holds one event, a shared
 * calendar of holidays may hold dozens, and the file is still attached. */
const shownEvents = 5;

/** eventBounds reads an event's bounds as the dates the browser draws. */
export function eventBounds(event: MessageCalendarEvent): { start: Date; end: Date } {
  const start = new Date(event.start_at);
  let end = new Date(event.end_at);
  if (Number.isNaN(end.getTime()) || end <= start) {
    // An event without an end is an instant; the dialog needs a stretch.
    end = event.all_day ? new Date(start.getTime() + 86_400_000) : new Date(start.getTime() + 3_600_000);
  }
  return { start, end };
}

/** eventWhen spells out when an event is. An all-day event is read in UTC,
 * where its dates were anchored; a timed one in the viewer's own zone. */
export function eventWhen(event: MessageCalendarEvent, locale?: string): string {
  const { start, end } = eventBounds(event);
  if (Number.isNaN(start.getTime())) return "";
  const dateOptions: Intl.DateTimeFormatOptions = { weekday: "short", day: "numeric", month: "short", year: "numeric" };
  if (event.all_day) {
    const utc: Intl.DateTimeFormatOptions = { ...dateOptions, timeZone: "UTC" };
    const lastDay = new Date(end.getTime() - 86_400_000);
    const first = start.toLocaleDateString(locale, utc);
    if (lastDay <= start) return `${first} (all day)`;
    return `${first} – ${lastDay.toLocaleDateString(locale, utc)} (all day)`;
  }
  const time: Intl.DateTimeFormatOptions = { hour: "numeric", minute: "2-digit" };
  const day = start.toLocaleDateString(locale, dateOptions);
  const from = start.toLocaleTimeString(locale, time);
  const sameDay = startOfDay(start).getTime() === startOfDay(end).getTime();
  if (sameDay) {
    return end.getTime() === start.getTime() ? `${day}, ${from}` : `${day}, ${from} – ${end.toLocaleTimeString(locale, time)}`;
  }
  return `${day}, ${from} – ${end.toLocaleDateString(locale, dateOptions)}, ${end.toLocaleTimeString(locale, time)}`;
}

/** writtenZoneNote names the sender's zone when it is not the reader's, so a
 * converted time is not mistaken for the one the sender wrote. */
function writtenZoneNote(event: MessageCalendarEvent): string {
  if (event.all_day) return "";
  if (event.floating) return "local time";
  const own = Intl.DateTimeFormat().resolvedOptions().timeZone || "";
  if (!event.time_zone || event.time_zone === own) return "";
  return `your time; sent as ${event.time_zone}`;
}

function personLabel(person: MessageCalendarPerson): string {
  if (person.name && person.email) return `${person.name} <${person.email}>`;
  return person.name || person.email || "";
}

/** prefillFor seeds the event dialog with what the invitation says. */
export function prefillFor(event: MessageCalendarEvent): EventPrefill {
  const { start, end } = eventBounds(event);
  const notes = [event.description || "", event.url && !(event.description || "").includes(event.url) ? event.url : ""]
    .filter(Boolean)
    .join("\n\n");
  return {
    start,
    end,
    allDay: event.all_day,
    summary: event.summary || "",
    location: event.location || "",
    description: notes
  };
}

type Added = { calendarName: string; start: string };

/** answerWords says an attendee's answer as the card reads it. */
const answerWords: Record<string, string> = {
  accepted: "accepted",
  tentative: "said maybe",
  declined: "declined",
  needsAction: "has not answered"
};

export function MessageCalendarCard({
  messageID,
  csrf,
  datePrefs,
  navigate,
  addToast,
  onDialogOpenChange
}: {
  messageID: number;
  csrf: string;
  datePrefs?: DatePrefs;
  navigate: (url: string) => void;
  addToast: AddToast;
  /** onDialogOpenChange tells the thread the event dialog is open, so its
   * single-key shortcuts -- Escape back to the list, r to reply -- leave the
   * dialog's keys alone. */
  onDialogOpenChange?: (open: boolean) => void;
}) {
  const [calendar, setCalendar] = useState<MessageCalendar | null>(null);
  const [failed, setFailed] = useState("");
  const [calendars, setCalendars] = useState<CalendarSummary[] | null>(null);
  const [dialogFor, setDialogFor] = useState<number | null>(null);
  const [saving, setSaving] = useState(false);
  const [problem, setProblem] = useState("");
  const [added, setAdded] = useState<Record<number, Added>>({});
  /** answers holds what the reader answered here, by event; an answer given
   * through the calendar also updates the copy that calendar holds. */
  const [answers, setAnswers] = useState<Record<number, string>>({});
  const [answering, setAnswering] = useState<number | null>(null);
  const locale = datePrefs?.date_locale?.trim() || undefined;
  const dialogOpen = dialogFor !== null;

  useEffect(() => {
    if (!dialogOpen) return;
    onDialogOpenChange?.(true);
    return () => onDialogOpenChange?.(false);
  }, [dialogOpen, onDialogOpenChange]);

  useEffect(() => {
    let cancelled = false;
    setCalendar(null);
    setFailed("");
    setAdded({});
    setAnswers({});
    api
      .messageCalendar(messageID)
      .then((data) => {
        if (!cancelled) setCalendar(data);
      })
      .catch((err) => {
        if (!cancelled) setFailed(messageFromError(err));
      });
    return () => {
      cancelled = true;
    };
  }, [messageID]);

  if (failed) {
    return (
      <div className="message-calendar message-calendar-failed">
        <Icon name="calendar" />
        <span>The calendar attached to this message could not be read: {failed}</span>
      </div>
    );
  }
  if (!calendar || calendar.events.length === 0) return null;

  const cancellation = calendar.method === "CANCEL";
  const invitation = calendar.method === "REQUEST";
  const reply = calendar.method === "REPLY";

  // An event a connected calendar holds with the reader as a guest is answered
  // there: the provider tells the organizer and the calendar shows the answer.
  // Anything else is answered by mail to the organizer, from the account the
  // invitation arrived through.
  const respond = async (index: number, event: MessageCalendarEvent, response: string) => {
    const held = event.in_calendars[0];
    setAnswering(index);
    try {
      if (held?.my_response) {
        await api.respondToCalendarEvent(csrf, held.event_id, response);
        addToast(`Answer sent. ${held.calendar_name || "Your calendar"} shows it too.`, "success");
      } else {
        const data = await api.messageCalendarReply(csrf, messageID, {
          event_index: index,
          uid: event.uid || "",
          response
        });
        addToast(`Answer sent to ${data.sent_to}.`, "success");
        if (data.warning) addToast(data.warning, "error");
      }
      setAnswers((current) => ({ ...current, [index]: response }));
    } catch (err) {
      addToast(messageFromError(err), "error");
    } finally {
      setAnswering(null);
    }
  };

  const openDialog = async (index: number) => {
    setProblem("");
    try {
      const loaded = calendars || (await api.calendars()).calendars;
      setCalendars(loaded);
      if (!loaded.some((item) => item.can_write && item.listed)) {
        addToast("There is no calendar to add this event to. Connect one under Settings first.", "error");
        return;
      }
      setDialogFor(index);
    } catch (err) {
      addToast(messageFromError(err), "error");
    }
  };

  const save = async (input: CalendarEventInput) => {
    if (dialogFor === null) return;
    setSaving(true);
    setProblem("");
    try {
      const data = await api.createCalendarEvent(csrf, input);
      const target = (calendars || []).find((item) => item.id === input.calendar_id);
      const calendarName = target?.name || "your calendar";
      setAdded((current) => ({ ...current, [dialogFor]: { calendarName, start: data.event.start_at } }));
      setDialogFor(null);
      if (data.warnings && data.warnings.length > 0) {
        for (const warning of data.warnings) addToast(warning, "error");
      } else {
        addToast(`Event added to ${calendarName}.`, "success");
      }
    } catch (err) {
      setProblem(messageFromError(err));
    } finally {
      setSaving(false);
    }
  };

  const visible = calendar.events.slice(0, shownEvents);
  const hidden = calendar.events.length - visible.length;
  const dialogEvent = dialogFor === null ? null : calendar.events[dialogFor];

  return (
    <div className={`message-calendar ${cancellation ? "cancelled" : ""}`}>
      {visible.map((event, index) => {
        const cancelled = cancellation || event.status === "CANCELLED";
        const zoneNote = writtenZoneNote(event);
        const done = added[index];
        const held = event.in_calendars[0];
        const guests = event.attendees.length;
        const answerable = !cancelled && !reply && (Boolean(held?.my_response) || Boolean(event.can_reply));
        const answer = answers[index] || held?.my_response || "";
        return (
          <div className="message-calendar-event" key={`${event.uid || ""}-${event.start_at}-${index}`}>
            <div className="message-calendar-head">
              <Icon name="calendar" weight="fill" />
              <span className="message-calendar-kind">
                {cancelled ? "Cancelled" : reply ? "Answer" : invitation ? "Invitation" : "Event"}
              </span>
              <span className={`message-calendar-title ${cancelled ? "struck" : ""}`}>{event.summary || "(no title)"}</span>
            </div>
            <dl>
              <dt>When</dt>
              <dd>
                {eventWhen(event, locale)}
                {zoneNote ? <span className="message-calendar-note"> ({zoneNote})</span> : null}
                {event.recurring ? <span className="message-calendar-note"> · repeats</span> : null}
              </dd>
              {event.location ? (
                <>
                  <dt>Where</dt>
                  <dd>{event.location}</dd>
                </>
              ) : null}
              {event.organizer ? (
                <>
                  <dt>Organizer</dt>
                  <dd>{personLabel(event.organizer)}</dd>
                </>
              ) : null}
              {reply && guests > 0 ? (
                <>
                  <dt>Answer</dt>
                  <dd>
                    {event.attendees
                      .map((person) => `${person.name || person.email || "A guest"} ${answerWords[person.status || ""] || "answered"}`)
                      .join(", ")}
                  </dd>
                </>
              ) : null}
              {!reply && guests > 0 ? (
                <>
                  <dt>Guests</dt>
                  <dd title={event.attendees.map(personLabel).join("\n")}>
                    {guests === 1 ? personLabel(event.attendees[0]) : `${guests} people`}
                  </dd>
                </>
              ) : null}
            </dl>
            {event.recurring && !cancelled && !reply && !held && !done && calendar.can_add ? (
              <p className="message-calendar-hint">This event repeats. Adding it enters the first date only.</p>
            ) : null}
            {answerable ? (
              <div className="calendar-rsvp message-calendar-rsvp">
                <span className="calendar-rsvp-label">Are you going?</span>
                <div className="calendar-rsvp-actions">
                  {responseOptions.map((option) => (
                    <button
                      key={option.value}
                      type="button"
                      className={`ghost ${answer === option.value ? "active" : ""}`}
                      disabled={answering !== null}
                      aria-pressed={answer === option.value}
                      onClick={() => void respond(index, event, option.value)}
                    >
                      {option.label}
                    </button>
                  ))}
                </div>
              </div>
            ) : null}
            <div className="message-calendar-actions">
              {held || done ? (
                <>
                  <span className="message-calendar-held">
                    <Icon name="check" />
                    {done ? `Added to ${done.calendarName}` : `Already in ${held?.calendar_name || "your calendar"}`}
                  </span>
                  <button
                    className="secondary"
                    type="button"
                    onClick={() => navigate(calendarURL(new Date(done ? done.start : held?.start_at || event.start_at)))}
                  >
                    <Icon name="calendar" />
                    Show in calendar
                  </button>
                </>
              ) : null}
              {!cancelled && !reply && !held && !done && calendar.can_add ? (
                <button className="secondary" type="button" onClick={() => void openDialog(index)}>
                  <Icon name="add" />
                  Add to calendar
                </button>
              ) : null}
            </div>
          </div>
        );
      })}
      {hidden > 0 ? (
        <p className="message-calendar-hint">
          {hidden === 1 ? "One more event" : `${hidden} more events`} in the attached calendar file.
        </p>
      ) : null}
      {dialogEvent && calendars
        ? createPortal(
            <EventDialog
              key={`message-${messageID}-${dialogFor}`}
              event={null}
              day={startOfDay(eventBounds(dialogEvent).start)}
              prefill={prefillFor(dialogEvent)}
              calendars={calendars}
              saving={saving}
              problem={problem}
              onSave={(input) => void save(input)}
              onDelete={() => undefined}
              onRespond={() => undefined}
              onClose={() => {
                setDialogFor(null);
                setProblem("");
              }}
            />,
            document.body
          )
        : null}
    </div>
  );
}
