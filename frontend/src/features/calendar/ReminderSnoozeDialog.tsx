// File overview: "Remind me later" for one calendar reminder. A notification
// offers at most a couple of fixed buttons, and the reader asked for a time of
// their choosing, so the button on the notification opens the calendar with
// this up (CalendarView reads ?remind=) and the time is picked here. The wait
// is the server's: it fires through the same notifications as the provider's
// reminders, tab open or not.

import { useMemo, useState } from "react";
import { createPortal } from "react-dom";
import { Icon } from "../../components/Icon";
import type { CalendarEvent } from "../../types";
import { reminderSnoozeChoices, reminderTimeRange } from "../../lib/calendarReminders";

function localDateTimeInputValue(value: Date) {
  const local = new Date(value.getTime() - value.getTimezoneOffset() * 60_000);
  return local.toISOString().slice(0, 16);
}

/**
 * ReminderSnoozeDialog puts the reminder of one event off. The event may be
 * unknown when the week on screen does not hold it any more (deleted since the
 * reminder, or another week drawn); the picker still offers the relative
 * choices, because the request names the event by id and the server answers
 * for whether it exists.
 */
export function ReminderSnoozeDialog({
  eventID,
  event,
  onSnooze,
  onClose
}: {
  eventID: number;
  event: CalendarEvent | null;
  onSnooze: (eventID: number, at: Date) => Promise<void>;
  onClose: () => void;
}) {
  const [busy, setBusy] = useState(false);
  const choices = useMemo(
    () => reminderSnoozeChoices(event?.start_at || "", Boolean(event?.all_day)),
    [event?.start_at, event?.all_day]
  );
  const [custom, setCustom] = useState(() => localDateTimeInputValue(new Date(Date.now() + 30 * 60_000)));

  async function choose(at: Date) {
    if (busy || Number.isNaN(at.getTime()) || at.getTime() <= Date.now()) return;
    setBusy(true);
    try {
      await onSnooze(eventID, at);
    } finally {
      setBusy(false);
    }
  }

  if (typeof document === "undefined") return null;
  const title = event?.summary?.trim() || "this event";
  const when = event ? reminderTimeRange(event.start_at, event.end_at, event.all_day) : "";
  return createPortal(
    <div className="snooze-backdrop" role="presentation" onClick={() => !busy && onClose()}>
      <section
        className="snooze-dialog"
        role="dialog"
        aria-modal="true"
        aria-labelledby="reminder-snooze-title"
        onClick={(clickEvent) => clickEvent.stopPropagation()}
      >
        <header>
          <h2 id="reminder-snooze-title">Remind me again</h2>
          <button className="ghost" type="button" disabled={busy} onClick={onClose} title="Dismiss reminder" aria-label="Dismiss reminder">
            <Icon name="close" />
          </button>
        </header>
        <p className="calendar-dialog-note">
          {event ? <>{title}{when ? ` · ${when}` : ""}</> : "The event is not in the week shown; the reminder is still put off for it."}
        </p>
        <div className="snooze-presets">
          {choices.map((choice) => (
            <button type="button" disabled={busy} key={choice.label} onClick={() => void choose(choice.at)}>
              <span>{choice.label}</span>
              <small>{choice.at.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" })}</small>
            </button>
          ))}
        </div>
        <div className="snooze-custom">
          <input
            type="datetime-local"
            value={custom}
            min={localDateTimeInputValue(new Date(Date.now() + 60_000))}
            onChange={(changeEvent) => setCustom(changeEvent.target.value)}
            aria-label="Remind me at"
          />
          <button type="button" disabled={busy || !custom || new Date(custom).getTime() <= Date.now()} onClick={() => void choose(new Date(custom))}>
            <Icon name="clock" />
            Set
          </button>
        </div>
        <div className="calendar-dialog-actions">
          <button type="button" className="ghost" disabled={busy} onClick={onClose}>
            Dismiss
          </button>
        </div>
      </section>
    </div>,
    document.body
  );
}
