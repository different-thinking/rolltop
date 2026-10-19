// File overview: The second calendar and the busy calendar -- the two roles a
// reader gives one calendar each -- chosen in the desktop calendar's Calendars
// menu and in the calendar settings, which is where a phone chooses them: its
// calendar has chips instead of the menu. Both places use these controls and
// these writes, so the two cannot answer the same choice differently.

import { useEffect, useState } from "react";
import { api } from "../../api";
import type { CalendarSummary } from "../../types";

type CalendarsUpdate = (current: CalendarSummary[]) => CalendarSummary[];

/**
 * writeCopyTarget makes calendarID the second calendar, or takes the role off
 * whichever calendar has it when calendarID is 0. Choosing one moves the mark
 * off the calendar that had it, so the answer is returned as an update of the
 * whole list rather than reloaded. Null means there was nothing to change.
 */
export async function writeCopyTarget(
  csrf: string,
  calendars: CalendarSummary[],
  calendarID: number
): Promise<CalendarsUpdate | null> {
  const previous = calendars.find((calendar) => calendar.copy_target);
  if ((previous?.id || 0) === calendarID) return null;
  const data = calendarID
    ? await api.setCalendarCopyTarget(csrf, calendarID, true)
    : previous
      ? await api.setCalendarCopyTarget(csrf, previous.id, false)
      : null;
  if (!data) return null;
  return (current) =>
    current.map((item) => (item.id === data.calendar.id ? data.calendar : { ...item, copy_target: false }));
}

/**
 * writeBusyTarget moves the busy calendar the same way, and takes its
 * placeholder title along: the title is stored on the calendar, and moving the
 * role should not quietly reset what the reader chose to call their blocked
 * time. Becoming the busy calendar also stops a calendar being the second one.
 * label is the title as the controls hold it, which may be a draft whose save
 * has not answered yet; the stored one would carry the title from before it.
 */
export async function writeBusyTarget(
  csrf: string,
  calendars: CalendarSummary[],
  calendarID: number,
  label: string
): Promise<CalendarsUpdate | null> {
  const previous = calendars.find((calendar) => calendar.busy_target);
  if ((previous?.id || 0) === calendarID) return null;
  const data = calendarID
    ? await api.setCalendarBusyTarget(csrf, calendarID, {
        busy_target: true,
        ...(previous ? { busy_label: label } : {})
      })
    : previous
      ? await api.setCalendarBusyTarget(csrf, previous.id, { busy_target: false })
      : null;
  if (!data) return null;
  return (current) =>
    current.map((item) => (item.id === data.calendar.id ? data.calendar : { ...item, busy_target: false }));
}

/** writeBusyLabel renames the placeholders the busy calendar holds. */
export async function writeBusyLabel(csrf: string, calendarID: number, label: string): Promise<CalendarsUpdate> {
  const data = await api.setCalendarBusyTarget(csrf, calendarID, { busy_label: label });
  return (current) => current.map((item) => (item.id === data.calendar.id ? data.calendar : item));
}

/**
 * CalendarRoleChoices offers the second and the busy calendar among the
 * calendars it is given, which are the listed ones: a calendar hidden under
 * settings is never offered. Neither choice appears while fewer than two
 * calendars can be written to, because a role needs another calendar to copy
 * from.
 */
export function CalendarRoleChoices({
  calendars,
  idPrefix,
  onChooseCopyTarget,
  onChooseBusyTarget,
  onSetBusyLabel
}: {
  calendars: CalendarSummary[];
  /** idPrefix keeps the labels' ids apart when both places are on one page. */
  idPrefix: string;
  onChooseCopyTarget: (calendarID: number) => void;
  onChooseBusyTarget: (calendarID: number, label: string) => void;
  onSetBusyLabel: (calendarID: number, label: string) => void;
}) {
  const writable = calendars.filter((calendar) => calendar.can_write);
  const copyTarget = calendars.find((calendar) => calendar.copy_target);
  // A chosen calendar that has since become read-only stays listed, or the
  // select would read "None" while the choice is still stored.
  const copyOptions = calendars.filter((calendar) => calendar.can_write || calendar.copy_target);
  const busyTarget = calendars.find((calendar) => calendar.busy_target);
  const busyOptions = calendars.filter((calendar) => calendar.can_write || calendar.busy_target);
  // The title is edited in place and saved when the field is left, so typing
  // does not send a request per key. It is reseeded whenever the stored title
  // or the busy calendar changes underneath it.
  const [busyLabel, setBusyLabelDraft] = useState(busyTarget?.busy_label || "");
  useEffect(() => {
    setBusyLabelDraft(busyTarget?.busy_label || "");
  }, [busyTarget?.id, busyTarget?.busy_label]);
  const saveBusyLabel = () => {
    if (!busyTarget || busyLabel.trim() === busyTarget.busy_label) return;
    onSetBusyLabel(busyTarget.id, busyLabel.trim());
  };
  if (writable.length < 2) return null;

  return (
    <>
      <div className="calendar-sidebar-group calendar-copy-target">
        <label className="calendar-sidebar-account" htmlFor={`${idPrefix}-copy-target`}>
          Second calendar
        </label>
        <select
          id={`${idPrefix}-copy-target`}
          value={copyTarget?.id || 0}
          onChange={(changeEvent) => onChooseCopyTarget(Number(changeEvent.target.value))}
        >
          <option value={0}>None</option>
          {copyOptions.map((calendar) => (
            <option key={calendar.id} value={calendar.id}>
              {calendar.name}
              {calendar.connection_email ? ` — ${calendar.connection_email}` : ""}
            </option>
          ))}
        </select>
        <p className="calendar-copy-target-hint">
          An event in any other calendar can be added here as well. It is shown once, marked with the other calendar's
          colour.
        </p>
      </div>
      <div className="calendar-sidebar-group calendar-copy-target">
        <label className="calendar-sidebar-account" htmlFor={`${idPrefix}-busy-target`}>
          Busy calendar
        </label>
        <select
          id={`${idPrefix}-busy-target`}
          value={busyTarget?.id || 0}
          onChange={(changeEvent) => onChooseBusyTarget(Number(changeEvent.target.value), busyLabel.trim())}
        >
          <option value={0}>None</option>
          {busyOptions.map((calendar) => (
            <option key={calendar.id} value={calendar.id}>
              {calendar.name}
              {calendar.connection_email ? ` — ${calendar.connection_email}` : ""}
              {calendar.copy_target ? " (second calendar)" : ""}
            </option>
          ))}
        </select>
        {busyTarget ? (
          <input
            aria-label="Placeholder title"
            className="calendar-busy-label"
            value={busyLabel}
            maxLength={60}
            placeholder="Busy"
            onChange={(changeEvent) => setBusyLabelDraft(changeEvent.target.value)}
            onBlur={saveBusyLabel}
            onKeyDown={(keyEvent) => {
              if (keyEvent.key === "Enter") {
                // Leaving the field saves it; saving here as well would
                // send the same title twice before the first answer.
                keyEvent.preventDefault();
                keyEvent.currentTarget.blur();
              }
            }}
          />
        ) : null}
        <p className="calendar-copy-target-hint">
          {busyTarget
            ? `An event in any other calendar can block its time here as “${busyTarget.busy_title}” — no title, notes, place or guests.`
            : "An event in any other calendar can block its time here without any of its details, titled “Busy” or “Away”."}
        </p>
      </div>
    </>
  );
}
