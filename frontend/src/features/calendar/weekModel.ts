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
  const dayKey = localDateKey(day);
  const startKey = utcDateKey(new Date(event.start_at));
  const endKey = utcDateKey(new Date(event.end_at));
  // Google's end date is exclusive. A degenerate pair where it is not after the
  // start still describes one day rather than none.
  if (endKey <= startKey) return dayKey === startKey;
  return dayKey >= startKey && dayKey < endKey;
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
};

type Span = {
  event: CalendarEvent;
  start: number;
  end: number;
  continuesBefore: boolean;
  continuesAfter: boolean;
};

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
    spans.push({
      event,
      start: Math.max(rawStart, dayStart),
      end: Math.min(end, dayEnd),
      continuesBefore: rawStart < dayStart,
      continuesAfter: rawEnd > dayEnd
    });
  }
  spans.sort((a, b) => a.start - b.start || a.end - b.end);

  const positioned: PositionedEvent[] = [];
  let cluster: Span[] = [];
  let clusterEnd = -Infinity;
  const flush = () => {
    if (cluster.length === 0) return;
    // columnEnds[i] is when column i becomes free again.
    const columnEnds: number[] = [];
    const assignment = new Map<Span, number>();
    for (const span of cluster) {
      let column = columnEnds.findIndex((end) => end <= span.start);
      if (column === -1) {
        column = columnEnds.length;
        columnEnds.push(span.end);
      } else {
        columnEnds[column] = span.end;
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
        continuesAfter: span.continuesAfter
      });
    }
    cluster = [];
    clusterEnd = -Infinity;
  };
  for (const span of spans) {
    if (cluster.length > 0 && span.start >= clusterEnd) flush();
    cluster.push(span);
    clusterEnd = Math.max(clusterEnd, span.end);
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
