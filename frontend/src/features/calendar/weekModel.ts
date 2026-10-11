// File overview: The date and layout arithmetic behind the week view. It is
// kept apart from the component because two of its rules are easy to get wrong
// and expensive when they are: all-day events are dates rather than instants,
// and overlapping appointments have to share a day's width without hiding each
// other.

import type { CalendarEvent, CalendarSummary } from "../../types";

/** hourHeight is the pixel height of one hour in the time grid. */
export const hourHeight = 48;

/** dayHeight is the full height of one day column. */
export const dayHeight = hourHeight * 24;

/** minimumEventMinutes keeps a very short appointment readable. Google accepts
 * zero-length events, and a block of no height would be invisible rather than
 * merely small. */
const minimumEventMinutes = 20;

const msPerMinute = 60_000;
const msPerDay = 86_400_000;

/** startOfDay returns local midnight of the day a date falls in. */
export function startOfDay(date: Date): Date {
  return new Date(date.getFullYear(), date.getMonth(), date.getDate());
}

/**
 * startOfWeek returns local midnight of the Monday on or before a date.
 *
 * The week starts on Monday because that is what the calendars this mirrors
 * use; a configurable first day would be a preference with nowhere to live yet.
 */
export function startOfWeek(date: Date): Date {
  const day = startOfDay(date);
  // getDay is 0 for Sunday, which is six days into a Monday-first week.
  const offset = (day.getDay() + 6) % 7;
  day.setDate(day.getDate() - offset);
  return day;
}

/** addDays returns a new date shifted by whole days, honouring the local
 * calendar rather than adding a fixed number of milliseconds -- a DST change
 * makes those two disagree by an hour. */
export function addDays(date: Date, days: number): Date {
  const shifted = new Date(date);
  shifted.setDate(shifted.getDate() + days);
  return shifted;
}

/** weekDays returns the seven local days of the week a date falls in. */
export function weekDays(date: Date): Date[] {
  const start = startOfWeek(date);
  return Array.from({ length: 7 }, (_, index) => addDays(start, index));
}

/** startOfMonth returns local midnight of the first day of a date's month,
 * shifted by whole months. The day is pinned to the first before shifting, so
 * stepping on from the 31st never skips a short month. */
export function startOfMonth(date: Date, months = 0): Date {
  return new Date(date.getFullYear(), date.getMonth() + months, 1);
}

/** monthPickerWeekCount is how many weeks the week picker shows for every
 * month. A month spans four to six Monday-first weeks; a fixed six keeps the
 * picker the same height while the reader swipes through months. */
export const monthPickerWeekCount = 6;

/** monthPickerWeeks returns the Monday-first weeks the week picker shows for a
 * month: the week holding its first day and the ones after it, as seven days
 * each, padded with days of the neighbouring months. */
export function monthPickerWeeks(month: Date): Date[][] {
  const first = startOfWeek(startOfMonth(month));
  return Array.from({ length: monthPickerWeekCount }, (_, index) => weekDays(addDays(first, index * 7)));
}

/** isoWeekNumber returns the ISO 8601 week number of the week a date falls
 * in -- the "KW" printed in German calendars. A week belongs to the year its
 * Thursday falls in, so the last days of December can be week 1 of the next
 * year and the first days of January week 52 or 53 of the last. */
export function isoWeekNumber(date: Date): number {
  const thursday = addDays(startOfWeek(date), 3);
  const yearStart = new Date(thursday.getFullYear(), 0, 1);
  // Rounded, because a DST change makes the span between two local midnights
  // an hour short of or past a whole number of days.
  const days = Math.round((thursday.getTime() - yearStart.getTime()) / msPerDay);
  return Math.floor(days / 7) + 1;
}

function pad(value: number): string {
  return String(value).padStart(2, "0");
}

/** localDateKey renders a date as YYYY-MM-DD in the viewer's own zone. */
export function localDateKey(date: Date): string {
  return `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())}`;
}

/** utcDateKey renders a date as YYYY-MM-DD in UTC, which is how an all-day
 * event's stored bounds have to be read: they are midnights of the plain dates
 * Google named, not moments in the viewer's zone. */
export function utcDateKey(date: Date): string {
  return `${date.getUTCFullYear()}-${pad(date.getUTCMonth() + 1)}-${pad(date.getUTCDate())}`;
}

/** parseDateKey reads a YYYY-MM-DD path segment as a local date. Anything else
 * answers with null so the caller can fall back to today rather than rendering
 * a week around an invalid date. */
export function parseDateKey(value: string): Date | null {
  const match = /^(\d{4})-(\d{2})-(\d{2})$/.exec(value.trim());
  if (!match) return null;
  const [, year, month, day] = match;
  const date = new Date(Number(year), Number(month) - 1, Number(day));
  if (Number.isNaN(date.getTime())) return null;
  // Rejects 2026-02-31, which the Date constructor would silently roll over.
  if (localDateKey(date) !== `${year}-${month}-${day}`) return null;
  return date;
}

/** calendarURL builds the route showing the week a date falls in. */
export function calendarURL(date: Date): string {
  return `/calendar/${localDateKey(startOfWeek(date))}`;
}

/** calendarRouteDate reads the week anchor out of a /calendar path. */
export function calendarRouteDate(path: string): Date {
  const parts = path.split("/").filter(Boolean);
  const parsed = parts[0] === "calendar" && parts[1] ? parseDateKey(parts[1]) : null;
  return parsed || new Date();
}

/**
 * allDayCoversDay reports whether an all-day event is drawn on a local day.
 *
 * Both sides are compared as plain dates: the event's bounds in UTC, because
 * that is the zone they were anchored in, and the column's date in the
 * viewer's zone, because that is the day the user is looking at. Comparing
 * instants instead would move a public holiday to the previous day for
 * everyone west of wherever the event was created.
 */
export function allDayCoversDay(event: CalendarEvent, day: Date): boolean {
  return allDayRangeCoversDay({ start: new Date(event.start_at), end: new Date(event.end_at) }, day);
}

/** timedEventTouchesDay reports whether a timed event overlaps a local day. */
export function timedEventTouchesDay(event: CalendarEvent, day: Date): boolean {
  const dayStart = startOfDay(day).getTime();
  const dayEnd = addDays(startOfDay(day), 1).getTime();
  const start = new Date(event.start_at).getTime();
  const end = Math.max(new Date(event.end_at).getTime(), start);
  return start < dayEnd && end > dayStart;
}

/** PositionedEvent is one appointment placed in a day column. */
export type PositionedEvent = {
  event: CalendarEvent;
  /** top and height are pixels from the top of the day column. */
  top: number;
  height: number;
  /** column and columns share the width between appointments that overlap. */
  column: number;
  columns: number;
  /** continuesBefore and continuesAfter mark an event clipped by the day
   * boundary, so a meeting running past midnight does not look like it ends
   * there. */
  continuesBefore: boolean;
  continuesAfter: boolean;
  /** travelBefore and travelAfter are the pixel heights of the journey there
   * and back, drawn above and below the event within the same day. */
  travelBefore: number;
  travelAfter: number;
};

type Span = {
  event: CalendarEvent;
  start: number;
  end: number;
  /** occupiedStart and occupiedEnd include the travel around the event: the
   * reader is away for all of it, so an appointment during the journey
   * overlaps the event as much as one during the event does. */
  occupiedStart: number;
  occupiedEnd: number;
  continuesBefore: boolean;
  continuesAfter: boolean;
};

/** travelMinutes reads an event's journey there and back, never negative. */
export function travelMinutes(event: CalendarEvent): { before: number; after: number } {
  if (event.all_day) return { before: 0, after: 0 };
  return {
    before: Math.max(0, event.travel_before_minutes || 0),
    after: Math.max(0, event.travel_after_minutes || 0)
  };
}

/**
 * layoutDayEvents places one day's timed events, sharing the column width
 * between those that overlap.
 *
 * Events are grouped into clusters of transitively overlapping appointments and
 * each cluster is split into as many columns as it has simultaneous events.
 * Sizing every event to the day's busiest moment instead would make a single
 * double-booked half hour squeeze the whole day into slivers.
 */
export function layoutDayEvents(events: CalendarEvent[], day: Date): PositionedEvent[] {
  const dayStart = startOfDay(day).getTime();
  const dayEnd = addDays(startOfDay(day), 1).getTime();
  const spans: Span[] = [];
  for (const event of events) {
    if (event.all_day) continue;
    const rawStart = new Date(event.start_at).getTime();
    const rawEnd = new Date(event.end_at).getTime();
    if (Number.isNaN(rawStart) || Number.isNaN(rawEnd)) continue;
    const end = Math.max(rawEnd, rawStart + minimumEventMinutes * msPerMinute);
    if (rawStart >= dayEnd || end <= dayStart) continue;
    // The journey is drawn on the day of the event only: a block reaching
    // into the evening before is part of an appointment that is not there.
    const travel = travelMinutes(event);
    const start = Math.max(rawStart, dayStart);
    const clippedEnd = Math.min(end, dayEnd);
    spans.push({
      event,
      start,
      end: clippedEnd,
      occupiedStart: Math.max(start - travel.before * msPerMinute, dayStart),
      occupiedEnd: Math.min(clippedEnd + travel.after * msPerMinute, dayEnd),
      continuesBefore: rawStart < dayStart,
      continuesAfter: rawEnd > dayEnd
    });
  }
  spans.sort((a, b) => a.occupiedStart - b.occupiedStart || a.occupiedEnd - b.occupiedEnd);

  const positioned: PositionedEvent[] = [];
  let cluster: Span[] = [];
  let clusterEnd = -Infinity;
  const flush = () => {
    if (cluster.length === 0) return;
    // columnEnds[i] is when column i becomes free again.
    const columnEnds: number[] = [];
    const assignment = new Map<Span, number>();
    for (const span of cluster) {
      let column = columnEnds.findIndex((end) => end <= span.occupiedStart);
      if (column === -1) {
        column = columnEnds.length;
        columnEnds.push(span.occupiedEnd);
      } else {
        columnEnds[column] = span.occupiedEnd;
      }
      assignment.set(span, column);
    }
    const columns = columnEnds.length;
    for (const span of cluster) {
      const minutesFromMidnight = (span.start - dayStart) / msPerMinute;
      const minutes = (span.end - span.start) / msPerMinute;
      positioned.push({
        event: span.event,
        top: (minutesFromMidnight / 60) * hourHeight,
        height: Math.max((minutes / 60) * hourHeight, 12),
        column: assignment.get(span) ?? 0,
        columns,
        continuesBefore: span.continuesBefore,
        continuesAfter: span.continuesAfter,
        travelBefore: ((span.start - span.occupiedStart) / msPerMinute / 60) * hourHeight,
        travelAfter: ((span.occupiedEnd - span.end) / msPerMinute / 60) * hourHeight
      });
    }
    cluster = [];
    clusterEnd = -Infinity;
  };
  for (const span of spans) {
    if (cluster.length > 0 && span.occupiedStart >= clusterEnd) flush();
    cluster.push(span);
    clusterEnd = Math.max(clusterEnd, span.occupiedEnd);
  }
  flush();
  return positioned;
}

/** allDayEventsForDay returns the all-day events drawn above one day column,
 * longest first so a multi-day span sits above the single days it covers. */
export function allDayEventsForDay(events: CalendarEvent[], day: Date): CalendarEvent[] {
  return events
    .filter((event) => event.all_day && allDayCoversDay(event, day))
    .sort((a, b) => allDayLength(b) - allDayLength(a) || a.summary.localeCompare(b.summary));
}

function allDayLength(event: CalendarEvent): number {
  const start = new Date(event.start_at).getTime();
  const end = new Date(event.end_at).getTime();
  if (Number.isNaN(start) || Number.isNaN(end)) return 1;
  return Math.max(Math.round((end - start) / msPerDay), 1);
}

/** minutesIntoDay is how far a moment sits into its local day, which is where
 * the "now" line is drawn. */
export function minutesIntoDay(date: Date): number {
  return date.getHours() * 60 + date.getMinutes();
}

/** calendarColor falls back to the accent colour when Google gave none, so an
 * event is never drawn with no colour at all. */
export function calendarColor(calendar: CalendarSummary | undefined): string {
  const color = calendar?.color?.trim();
  return color && /^#[0-9a-f]{3,8}$/i.test(color) ? color : "var(--accent)";
}

/** providerName names the service a calendar is mirrored from, for the lines
 * that say where a change was made. */
export function providerName(calendar: CalendarSummary | undefined): string {
  return calendar?.provider === "microsoft" ? "Microsoft 365" : "Google";
}

/** readableTextColor picks black or white text for a calendar colour. Google's
 * palette spans very light and very dark, and one fixed text colour is
 * unreadable on half of it. */
export function readableTextColor(color: string): string {
  const match = /^#([0-9a-f]{6})$/i.exec(color.trim());
  if (!match) return "#fff";
  const value = Number.parseInt(match[1], 16);
  const r = (value >> 16) & 0xff;
  const g = (value >> 8) & 0xff;
  const b = value & 0xff;
  // Rec. 601 luma, which is the cheap approximation of perceived brightness.
  return (r * 299 + g * 587 + b * 114) / 1000 > 150 ? "#1b1b1b" : "#fff";
}

/** calendarAccountGroups groups calendars by the account they came from, in
 * the order the accounts first appear, because two accounts routinely have a
 * calendar called the same thing. The Calendars menu, the day lens's lanes and the
 * calendar settings all read this one order, so a calendar sits in the same
 * place in each. */
export function calendarAccountGroups(calendars: CalendarSummary[]): [string, CalendarSummary[]][] {
  const byAccount = new Map<string, CalendarSummary[]>();
  for (const calendar of calendars) {
    const key = calendar.connection_email || providerName(calendar);
    const list = byAccount.get(key) || [];
    list.push(calendar);
    byAccount.set(key, list);
  }
  return Array.from(byAccount.entries());
}

/**
 * lensLaneCalendarIDs names the calendars that get a lane of their own in the
 * day lens: those with a timed event on that day, in the order given (the
 * Calendars menu's). A calendar the order does not know -- a linked copy's calendar
 * hidden since -- still gets a lane, after the known ones: drawing an event in
 * an unnamed lane is recoverable, dropping it is not.
 */
export function lensLaneCalendarIDs(events: CalendarEvent[], calendars: CalendarSummary[], day: Date): number[] {
  const present = new Set(
    events.filter((event) => !event.all_day && timedEventTouchesDay(event, day)).map((event) => event.calendar_id)
  );
  const ordered = calendars.map((calendar) => calendar.id).filter((id) => present.has(id));
  for (const id of present) if (!ordered.includes(id)) ordered.push(id);
  return ordered;
}

/** busiestColumns is the most appointments a day has at one moment. */
export function busiestColumns(events: CalendarEvent[], day: Date): number {
  const timed = events.filter((event) => !event.all_day && timedEventTouchesDay(event, day));
  return layoutDayEvents(timed, day).reduce((most, placed) => Math.max(most, placed.columns), 0);
}

/**
 * dayColumnWeights sizes the seven day columns of the week.
 *
 * The lens day is the one to be read in full, so it gets a share wide enough
 * for a lane per calendar. Every other day gets a little more width for each
 * appointment it has at once, up to four, and a day with nothing overlapping
 * gives some of its width away -- an empty Sunday does not need what a Tuesday
 * with four meetings at ten does. lensIndex is -1 when no day is open.
 */
export function dayColumnWeights(events: CalendarEvent[], days: Date[], lensIndex: number, lensLanes: number): number[] {
  return days.map((day, index) => {
    if (index === lensIndex) return Math.max(3, lensLanes * 1.25);
    const busiest = busiestColumns(events, day);
    return busiest <= 1 ? 0.7 : 0.85 + 0.15 * Math.min(busiest, 4);
  });
}

/** dragSnapMinutes is the step a dragged appointment moves, stretches and is
 * drawn in. A quarter hour is the finest step people schedule in; finer, and a
 * drop lands on 10:07 because the hand wobbled. */
export const dragSnapMinutes = 15;

const minutesPerDay = 24 * 60;

/** snapMinutes rounds a number of minutes to the drag step. */
export function snapMinutes(minutes: number): number {
  return Math.round(minutes / dragSnapMinutes) * dragSnapMinutes;
}

/** atMinutes returns the local wall-clock time a number of minutes after a
 * day's midnight. It sets the minutes rather than adding milliseconds, so ten
 * o'clock is ten o'clock on the day the clocks change too; a count past the
 * day's end runs on into the next one. */
export function atMinutes(day: Date, minutes: number): Date {
  const value = startOfDay(day);
  value.setMinutes(minutes);
  return value;
}

/** minutesFromDayStart counts an instant's wall-clock time from a local day's
 * midnight: negative for an instant on an earlier day, past 1440 for a later
 * one. */
export function minutesFromDayStart(day: Date, instant: Date): number {
  const days = Math.round((startOfDay(instant).getTime() - startOfDay(day).getTime()) / msPerDay);
  return days * minutesPerDay + minutesIntoDay(instant);
}

/** daysBetween is how many local days separate two dates' midnights. */
export function daysBetween(from: Date, to: Date): number {
  return Math.round((startOfDay(to).getTime() - startOfDay(from).getTime()) / msPerDay);
}

/** eventEditable says whether the reader may change an event: its calendar
 * has to be writable, and a Microsoft 365 meeting somebody else organizes can
 * only be answered -- only its organizer can change it. The dialog and the
 * week's drag both ask this one question, so a block that can be dragged is
 * one whose dialog would have saved the same change. */
export function eventEditable(event: CalendarEvent, calendar: CalendarSummary | undefined): boolean {
  if (!calendar?.can_write) return false;
  return !(Boolean(event.my_response) && calendar.provider === "microsoft");
}

/** TimeRange is a dragged appointment's new bounds. */
export type TimeRange = { start: Date; end: Date };

/**
 * movedEventTimes is where a timed event lands when it was grabbed
 * grabMinutes into grabDay and is held dropMinutes into dropDay.
 *
 * What moves is the distance the pointer travelled, snapped to the drag step,
 * not the event's start snapped to the grid: an appointment at 10:10 moved an
 * hour lands at 11:10, the way it would if the reader typed it. The length is
 * kept in milliseconds, so a meeting across the night the clocks change keeps
 * its real length. The start is kept inside the day it was dropped on unless it
 * already began the day before -- a drop at the top of a column means "first
 * thing", not "late last night".
 */
export function movedEventTimes(
  event: CalendarEvent,
  grabDay: Date,
  grabMinutes: number,
  dropDay: Date,
  dropMinutes: number
): TimeRange {
  const start = new Date(event.start_at);
  const length = Math.max(new Date(event.end_at).getTime() - start.getTime(), 0);
  const startMinutes = minutesFromDayStart(grabDay, start);
  const shifted = startMinutes + snapMinutes(dropMinutes - grabMinutes);
  // Both bounds give way to where the event already starts, so a block that
  // begins off the grid near either end of the day is never moved by a drop
  // that did not travel that far.
  const earliest = Math.min(0, startMinutes);
  const latest = Math.max(minutesPerDay - dragSnapMinutes, startMinutes);
  const newStart = atMinutes(dropDay, Math.min(Math.max(shifted, earliest), latest));
  return { start: newStart, end: new Date(newStart.getTime() + length) };
}

/** resizedEventTimes is a timed event whose end was dragged to dropMinutes
 * into dropDay: the end lands on the grid, never closer to the start than one
 * drag step, and may be pulled on into a later day. */
export function resizedEventTimes(event: CalendarEvent, dropDay: Date, dropMinutes: number): TimeRange {
  const start = new Date(event.start_at);
  const end = atMinutes(dropDay, snapMinutes(Math.min(Math.max(dropMinutes, 0), minutesPerDay)));
  const shortest = start.getTime() + dragSnapMinutes * msPerMinute;
  return { start, end: end.getTime() < shortest ? new Date(shortest) : end };
}

/** drawnRange is the stretch of one day a reader drew from anchorMinutes to
 * pointerMinutes, in either direction: whole drag steps, never less than one,
 * inside the day. The first step is the one pressed on, so a press at 10:05
 * starts the event at 10:00 rather than 10:15. */
export function drawnRange(day: Date, anchorMinutes: number, pointerMinutes: number): TimeRange {
  const floor = (minutes: number) => Math.floor(minutes / dragSnapMinutes) * dragSnapMinutes;
  const clamp = (minutes: number) => Math.min(Math.max(minutes, 0), minutesPerDay);
  const anchor = clamp(floor(Math.min(anchorMinutes, minutesPerDay - 1)));
  const pointer = clamp(snapMinutes(pointerMinutes));
  const first = pointer >= anchor ? anchor : pointer;
  const last = pointer >= anchor ? Math.max(pointer, anchor + dragSnapMinutes) : anchor + dragSnapMinutes;
  return { start: atMinutes(day, first), end: atMinutes(day, Math.min(last, minutesPerDay)) };
}

/** shiftedAllDayTimes moves an all-day event by whole days. Its bounds are UTC
 * midnights of plain dates, so the shift is made in UTC dates as well: adding
 * local days would carry a DST change into a value that has no zone. */
export function shiftedAllDayTimes(event: CalendarEvent, days: number): TimeRange {
  const shift = (value: string) => {
    const date = new Date(value);
    date.setUTCDate(date.getUTCDate() + days);
    return date;
  };
  return { start: shift(event.start_at), end: shift(event.end_at) };
}

/** allDayRangeFromLocalDays renders the local days first..last (inclusive) as
 * the UTC midnights an all-day event is stored with, the end exclusive. */
export function allDayRangeFromLocalDays(first: Date, last: Date): TimeRange {
  const utcMidnight = (day: Date) => new Date(Date.UTC(day.getFullYear(), day.getMonth(), day.getDate()));
  const [from, to] = first <= last ? [first, last] : [last, first];
  const end = utcMidnight(to);
  end.setUTCDate(end.getUTCDate() + 1);
  return { start: utcMidnight(from), end };
}

/** allDayRangeCoversDay reports whether an all-day range covers a local day.
 * allDayCoversDay asks it of a stored event; the week's drag asks it of the
 * preview of one being dragged, which is not an event yet. */
export function allDayRangeCoversDay(range: TimeRange, day: Date): boolean {
  const dayKey = localDateKey(day);
  const startKey = utcDateKey(range.start);
  const endKey = utcDateKey(range.end);
  // Google's end date is exclusive. A degenerate pair where it is not after the
  // start still describes one day rather than none.
  if (endKey <= startKey) return dayKey === startKey;
  return dayKey >= startKey && dayKey < endKey;
}

/** rangeSliceForDay is the part of a timed range drawn in one day column, in
 * pixels, or null when the range does not reach that day. */
export function rangeSliceForDay(range: TimeRange, day: Date): { top: number; height: number } | null {
  const dayStart = startOfDay(day).getTime();
  const dayEnd = addDays(startOfDay(day), 1).getTime();
  const start = Math.max(range.start.getTime(), dayStart);
  const end = Math.min(Math.max(range.end.getTime(), range.start.getTime() + minimumEventMinutes * msPerMinute), dayEnd);
  if (end <= start) return null;
  return {
    top: ((start - dayStart) / msPerMinute / 60) * hourHeight,
    height: Math.max(((end - start) / msPerMinute / 60) * hourHeight, 12)
  };
}
