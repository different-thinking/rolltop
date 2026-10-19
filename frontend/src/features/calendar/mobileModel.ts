// File overview: The calendar on a phone. Seven day columns do not fit a
// phone's width -- a lens day with a lane per calendar left the other six days
// a sliver each -- so a narrow screen draws one of four views instead: a day,
// three days with the tapped one wide, an agenda of the coming week, and the
// week as a list. This file is the arithmetic behind them; CalendarView draws
// them. Everything here is pure, so it is tested without a browser.

import type { CalendarEvent } from "../../types";
import {
  addDays,
  allDayEventsForDay,
  localDateKey,
  startOfDay,
  startOfWeek,
  timedEventTouchesDay,
  travelMinutes,
  weekDays
} from "./weekModel";

/** compactCalendarQuery is the width at which the calendar stops drawing the
 * week as seven columns. It is the app's own phone breakpoint (the one the
 * folder sidebar collapses at), so the calendar turns compact exactly when the
 * rest of the app does. */
export const compactCalendarQuery = "(max-width: 760px)";

/** MobileCalendarView names the four ways a phone draws the calendar. */
export type MobileCalendarView = "day" | "three" | "agenda" | "week";

export const mobileCalendarViews: { id: MobileCalendarView; label: string }[] = [
  { id: "day", label: "Day" },
  { id: "three", label: "3 days" },
  { id: "agenda", label: "Agenda" },
  { id: "week", label: "Week" }
];

/** parseMobileCalendarView reads a stored choice; anything unknown is the day,
 * the view a phone opens on. */
export function parseMobileCalendarView(value: string | null | undefined): MobileCalendarView {
  return mobileCalendarViews.some((view) => view.id === value) ? (value as MobileCalendarView) : "day";
}

/** agendaDays is how far ahead the agenda reads: a week, the span the prev and
 * next buttons step by. */
export const agendaDays = 7;

/** mobileViewDays lists the days a view shows for the day in the route. The
 * day, three-day and agenda views start at that day; the week list shows the
 * week it falls in, Monday first like the desktop week. */
export function mobileViewDays(view: MobileCalendarView, routeDay: Date): Date[] {
  const first = startOfDay(routeDay);
  switch (view) {
    case "day":
      return [first];
    case "three":
      return [0, 1, 2].map((offset) => addDays(first, offset));
    case "agenda":
      return Array.from({ length: agendaDays }, (_, offset) => addDays(first, offset));
    case "week":
      return weekDays(startOfWeek(first));
  }
}

/** mobileViewStep is how many days the previous and next buttons move. */
export function mobileViewStep(view: MobileCalendarView): number {
  switch (view) {
    case "day":
      return 1;
    case "three":
      return 3;
    case "agenda":
    case "week":
      return 7;
  }
}

/** calendarLoadRange is the stretch of time loaded for a run of days: whole
 * weeks, Monday to Monday, however few days are drawn. The offline store keeps
 * whole weeks too, and a range that starts mid-week would be answered from it
 * only when a stored window happened to cover the odd edge; on the desktop,
 * where the days are a week, it is that week unchanged. */
export function calendarLoadRange(days: Date[]): { start: Date; end: Date } {
  const start = startOfWeek(days[0]);
  const end = addDays(startOfWeek(days[days.length - 1]), 7);
  return { start, end };
}

/** calendarDayURL builds the route showing one day. The desktop reads the week
 * the day falls in, so the same address opens that week there; a phone opens
 * the day itself. */
export function calendarDayURL(date: Date): string {
  return `/calendar/${localDateKey(date)}`;
}

/** wholeDayTimedMinutes is how much of a day a timed event has to fill before
 * the phone draws it with the all-day events. An absence entered as "00:00 to
 * 00:00" is a timed event to the provider, and drawn in the grid it is a
 * column-high bar that takes half the day's width from the appointments beside
 * it. Twelve hours keeps a long working day in the grid. */
export const wholeDayTimedMinutes = 12 * 60;

/** timedEventFillsDay reports whether a timed event covers so much of a local
 * day that a phone draws it on that day as an all-day event. It is measured
 * per day: a night shift from eight to eight fills neither of its days and
 * stays in the grid on both. */
export function timedEventFillsDay(event: CalendarEvent, day: Date): boolean {
  if (event.all_day) return false;
  const dayStart = startOfDay(day).getTime();
  const dayEnd = addDays(startOfDay(day), 1).getTime();
  const start = new Date(event.start_at).getTime();
  const end = new Date(event.end_at).getTime();
  if (Number.isNaN(start) || Number.isNaN(end)) return false;
  const covered = Math.min(end, dayEnd) - Math.max(start, dayStart);
  // The day's own length, not 24 hours: the day the clocks change has 23 or 25.
  const dayMinutes = (dayEnd - dayStart) / 60_000;
  return covered / 60_000 >= Math.min(wholeDayTimedMinutes, dayMinutes);
}

/** byStart orders events by when they begin, then by title. The instants are
 * compared, not the strings: two RFC 3339 times in different offsets do not
 * sort as text. */
function byStart(a: CalendarEvent, b: CalendarEvent): number {
  return new Date(a.start_at).getTime() - new Date(b.start_at).getTime() || a.summary.localeCompare(b.summary);
}

/** compactAllDayEvents is what a phone draws above a day: its all-day events,
 * longest first as on the desktop, then the timed events that fill the day. */
export function compactAllDayEvents(events: CalendarEvent[], day: Date): CalendarEvent[] {
  const filling = events
    .filter((event) => timedEventTouchesDay(event, day) && timedEventFillsDay(event, day))
    .sort(byStart);
  return [...allDayEventsForDay(events, day), ...filling];
}

/** compactTimedEvents is what a phone draws in a day's time grid or agenda:
 * the timed events touching it, less the ones drawn as all-day. */
export function compactTimedEvents(events: CalendarEvent[], day: Date): CalendarEvent[] {
  return events.filter(
    (event) => !event.all_day && timedEventTouchesDay(event, day) && !timedEventFillsDay(event, day)
  );
}

/** threeDayFocusWeight is how much wider the tapped day is than the two beside
 * it: wide enough to read a title, while the other two keep a start time. */
export const threeDayFocusWeight = 3.4;

/** threeDayColumns sizes the three columns of the three-day view for the
 * shared grid property, the tapped day wide and the others narrow. */
export function threeDayColumns(focusIndex: number, count = 3): string {
  return Array.from(
    { length: count },
    (_, index) => `minmax(0, ${index === focusIndex ? threeDayFocusWeight.toFixed(2) : "1.00"}fr)`
  ).join(" ");
}

/** threeDayFocusIndex picks the wide day: the one the reader tapped while it is
 * on screen, otherwise today when it is, otherwise the first. */
export function threeDayFocusIndex(days: Date[], chosenKey: string | null, todayKey: string): number {
  const chosen = chosenKey ? days.findIndex((day) => localDateKey(day) === chosenKey) : -1;
  if (chosen >= 0) return chosen;
  const today = days.findIndex((day) => localDateKey(day) === todayKey);
  return today >= 0 ? today : 0;
}

/** agendaFreeMinutes is the shortest gap between two appointments the agenda
 * names as free time. Shorter gaps are the walk between rooms. */
export const agendaFreeMinutes = 60;

/** AgendaItem is one line of a day in the agenda: an appointment, a stretch of
 * free time between two, or appointments that run at the same time, which the
 * agenda keeps together so they read as one decision rather than a list. */
export type AgendaItem =
  | { kind: "event"; event: CalendarEvent }
  | { kind: "free"; start: Date; end: Date }
  | { kind: "together"; start: Date; end: Date; events: CalendarEvent[] };

/**
 * agendaDayItems orders a day's timed events for the agenda.
 *
 * Events whose time overlaps -- counting the journey around them, which the
 * week counts as taken too -- form one group; a gap between groups of at least
 * agendaFreeMinutes becomes a free line. Nothing is said about the time before
 * the first appointment or after the last: the agenda does not know the
 * reader's working hours, and "free from midnight" would be noise.
 */
export function agendaDayItems(events: CalendarEvent[], day: Date): AgendaItem[] {
  const dayStart = startOfDay(day).getTime();
  const dayEnd = addDays(startOfDay(day), 1).getTime();
  const spans = events
    .filter((event) => !event.all_day)
    .map((event) => {
      const start = new Date(event.start_at).getTime();
      const end = Math.max(new Date(event.end_at).getTime(), start);
      const travel = travelMinutes(event);
      return {
        event,
        start,
        end,
        busyStart: Math.max(start - travel.before * 60_000, dayStart),
        busyEnd: Math.min(end + travel.after * 60_000, dayEnd)
      };
    })
    .filter(
      (span) => !Number.isNaN(span.start) && !Number.isNaN(span.end) && span.start < dayEnd && span.end > dayStart
    )
    .sort(
      (a, b) => a.busyStart - b.busyStart || b.busyEnd - a.busyEnd || a.event.summary.localeCompare(b.event.summary)
    );

  const groups: { start: number; end: number; events: CalendarEvent[] }[] = [];
  for (const span of spans) {
    const last = groups[groups.length - 1];
    if (last && span.busyStart < last.end) {
      last.events.push(span.event);
      last.end = Math.max(last.end, span.busyEnd);
      continue;
    }
    groups.push({ start: span.busyStart, end: span.busyEnd, events: [span.event] });
  }

  const items: AgendaItem[] = [];
  groups.forEach((group, index) => {
    const previous = groups[index - 1];
    if (previous && (group.start - previous.end) / 60_000 >= agendaFreeMinutes) {
      items.push({ kind: "free", start: new Date(previous.end), end: new Date(group.start) });
    }
    if (group.events.length === 1) {
      items.push({ kind: "event", event: group.events[0] });
    } else {
      items.push({ kind: "together", start: new Date(group.start), end: new Date(group.end), events: group.events });
    }
  });
  return items;
}

/** weekListLimit is how many timed appointments a day's row of the week list
 * names before it counts the rest. */
export const weekListLimit = 3;

/** WeekListDay is one row of the week list. */
export type WeekListDay = {
  day: Date;
  allDay: CalendarEvent[];
  timed: CalendarEvent[];
  /** more is how many timed appointments the row does not name. */
  more: number;
};

/** weekListDays summarises each day for the week list: what is on all day, the
 * first few appointments by start, and how many more there are. */
export function weekListDays(events: CalendarEvent[], days: Date[], limit = weekListLimit): WeekListDay[] {
  return days.map((day) => {
    const timed = compactTimedEvents(events, day).sort(byStart);
    return {
      day,
      allDay: compactAllDayEvents(events, day),
      timed: timed.slice(0, limit),
      more: Math.max(0, timed.length - limit)
    };
  });
}
