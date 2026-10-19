// File overview: The parts of the calendar that only a phone draws: the switch
// between its four views, the head of the day view with a chip per calendar,
// the agenda and the week list. The day and three-day views reuse the week's
// own time grid; CalendarView decides which of these it shows.

import { useEffect, useState } from "react";
import type { CSSProperties } from "react";
import type { CalendarEvent, CalendarSummary } from "../../types";
import { Icon } from "../../components/Icon";
import {
  agendaDayItems,
  compactAllDayEvents,
  compactCalendarQuery,
  compactTimedEvents,
  mobileCalendarViews,
  parseMobileCalendarView,
  weekListDays
} from "./mobileModel";
import type { AgendaItem, MobileCalendarView } from "./mobileModel";
import { addDays, calendarColor, localDateKey, readableTextColor, startOfDay } from "./weekModel";

/** useCompactCalendar follows whether the screen is narrow enough to draw the
 * phone views, and switches when a window is resized or a phone turned. */
export function useCompactCalendar(): boolean {
  const [compact, setCompact] = useState(
    () =>
      typeof window !== "undefined" &&
      typeof window.matchMedia === "function" &&
      window.matchMedia(compactCalendarQuery).matches
  );
  useEffect(() => {
    if (typeof window.matchMedia !== "function") return;
    const query = window.matchMedia(compactCalendarQuery);
    const onChange = () => setCompact(query.matches);
    onChange();
    query.addEventListener?.("change", onChange);
    return () => query.removeEventListener?.("change", onChange);
  }, []);
  return compact;
}

function mobileViewKey(userID: number) {
  return `rolltop.calendar.mobile-view.${userID}`;
}

/** useMobileCalendarView is the view a phone draws, remembered per user and
 * browser: someone who reads the agenda every morning should not have to pick
 * it every morning. Without storage the choice holds for the visit. */
export function useMobileCalendarView(userID: number): [MobileCalendarView, (view: MobileCalendarView) => void] {
  const [view, setView] = useState<MobileCalendarView>(() => {
    try {
      return parseMobileCalendarView(window.localStorage.getItem(mobileViewKey(userID)));
    } catch {
      return "day";
    }
  });
  const choose = (next: MobileCalendarView) => {
    setView(next);
    try {
      window.localStorage.setItem(mobileViewKey(userID), next);
    } catch {
      // Kept for this visit only.
    }
  };
  return [view, choose];
}

/** MobileViewSwitch picks one of the phone's four views. */
export function MobileViewSwitch({
  view,
  onChange
}: {
  view: MobileCalendarView;
  onChange: (view: MobileCalendarView) => void;
}) {
  return (
    <div className="calendar-view-switch" role="group" aria-label="Calendar view">
      {mobileCalendarViews.map((item) => (
        <button
          key={item.id}
          type="button"
          className={item.id === view ? "active" : ""}
          aria-pressed={item.id === view}
          onClick={() => onChange(item.id)}
        >
          {item.label}
        </button>
      ))}
    </div>
  );
}

/**
 * MobileDayHead is the top of the day view: the date, what the day holds, a
 * chip per calendar to show or hide it, and the all-day events folded into one
 * line. The chips are the Calendars menu's own switches laid out flat -- a
 * hidden calendar is switched off exactly as it is there, so the two never
 * disagree.
 */
export function MobileDayHead({
  day,
  todayKey,
  calendars,
  events,
  calendarsByID,
  onToggleCalendar,
  onOpenEvent
}: {
  day: Date;
  todayKey: string;
  calendars: CalendarSummary[];
  events: CalendarEvent[];
  calendarsByID: Map<number, CalendarSummary>;
  onToggleCalendar: (calendar: CalendarSummary) => Promise<void>;
  onOpenEvent: (event: CalendarEvent, day: Date) => void;
}) {
  const [allDayOpen, setAllDayOpen] = useState(false);
  const allDay = compactAllDayEvents(events, day);
  const timedCount = compactTimedEvents(events, day).length;
  const today = localDateKey(day) === todayKey;
  const summary = [
    today ? "Today" : "",
    `${timedCount} ${timedCount === 1 ? "event" : "events"}`,
    allDay.length > 0 ? `${allDay.length} all day` : ""
  ]
    .filter(Boolean)
    .join(" · ");
  const allDayColors = distinctColors(allDay, calendarsByID).slice(0, 3);

  return (
    <div className="calendar-mobile-day-head">
      <div>
        <h2 className="calendar-mobile-day-title">
          {day.toLocaleDateString(undefined, { weekday: "long", day: "numeric", month: "long" })}
        </h2>
        <p className="calendar-mobile-day-summary">{summary}</p>
      </div>
      {calendars.length > 0 ? (
        <div className="calendar-mobile-chips" role="group" aria-label="Calendars shown">
          {calendars.map((calendar) => (
            <button
              key={calendar.id}
              type="button"
              className={`calendar-mobile-chip ${calendar.selected ? "on" : ""}`}
              aria-pressed={calendar.selected}
              title={calendar.connection_email ? `${calendar.name} — ${calendar.connection_email}` : calendar.name}
              style={{ "--chip-color": calendarColor(calendar) } as CSSProperties}
              onClick={() => void onToggleCalendar(calendar)}
            >
              <span className="calendar-mobile-chip-dot" aria-hidden="true" />
              {calendar.name}
            </button>
          ))}
        </div>
      ) : null}
      {allDay.length > 0 ? (
        <>
          <button
            type="button"
            className="calendar-mobile-allday-toggle"
            aria-expanded={allDayOpen}
            onClick={() => setAllDayOpen(!allDayOpen)}
          >
            <span className="calendar-mobile-allday-dots" aria-hidden="true">
              {allDayColors.map((color) => (
                <span key={color} style={{ background: color }} />
              ))}
            </span>
            <span className="calendar-mobile-allday-text">
              <strong>{allDay.length} all day</strong> ·{" "}
              {allDay.map((event) => event.summary || "(No title)").join(", ")}
            </span>
            <Icon name="expand_more" />
          </button>
          {allDayOpen ? (
            <div className="calendar-mobile-allday-list">
              {allDay.map((event) => (
                <EventChip
                  key={event.id}
                  event={event}
                  day={day}
                  calendarsByID={calendarsByID}
                  onOpenEvent={onOpenEvent}
                />
              ))}
            </div>
          ) : null}
        </>
      ) : null}
    </div>
  );
}

/**
 * MobileAgenda lists the coming days as a list rather than a grid: each day's
 * appointments in order, the free time between them, and appointments that
 * run at the same time kept together in one box -- three of them at once is a
 * clash to resolve, not three lines to read past.
 */
export function MobileAgenda({
  days,
  events,
  calendarsByID,
  todayKey,
  now,
  canCreate,
  onOpenEvent,
  onNewEvent
}: {
  days: Date[];
  events: CalendarEvent[];
  calendarsByID: Map<number, CalendarSummary>;
  todayKey: string;
  now: Date;
  canCreate: boolean;
  onOpenEvent: (event: CalendarEvent, day: Date) => void;
  onNewEvent: (start: Date, end: Date) => void;
}) {
  return (
    <div className="calendar-agenda">
      {days.map((day) => {
        const today = localDateKey(day) === todayKey;
        const allDay = compactAllDayEvents(events, day);
        const timed = compactTimedEvents(events, day);
        const items = agendaDayItems(timed, day);
        const freeMinutes = items.reduce(
          (total, item) => total + (item.kind === "free" ? (item.end.getTime() - item.start.getTime()) / 60_000 : 0),
          0
        );
        const summary = [
          `${timed.length} ${timed.length === 1 ? "event" : "events"}`,
          freeMinutes > 0 ? `${formatMinutes(freeMinutes)} free` : ""
        ]
          .filter(Boolean)
          .join(" · ");
        // The now line goes before the first entry that has not begun yet.
        const nowIndex = today ? items.findIndex((item) => itemStart(item).getTime() > now.getTime()) : -1;
        const dateLabel = day.toLocaleDateString(undefined, { weekday: "short", day: "numeric", month: "short" });
        return (
          <section
            key={localDateKey(day)}
            className={`calendar-agenda-day ${today ? "today" : ""}`}
            aria-label={dateLabel}
          >
            <header className="calendar-agenda-day-head">
              <h2>{today ? `Today · ${dateLabel}` : dateLabel}</h2>
              <span>{summary}</span>
            </header>
            {allDay.length > 0 ? (
              <div className="calendar-agenda-allday">
                {allDay.map((event) => (
                  <EventChip
                    key={event.id}
                    event={event}
                    day={day}
                    calendarsByID={calendarsByID}
                    onOpenEvent={onOpenEvent}
                  />
                ))}
              </div>
            ) : null}
            {items.length === 0 ? <p className="calendar-agenda-empty">Nothing scheduled</p> : null}
            {items.map((item, index) => (
              <AgendaEntry
                key={itemKey(item)}
                item={item}
                day={day}
                calendarsByID={calendarsByID}
                canCreate={canCreate}
                showNow={index === nowIndex}
                now={now}
                onOpenEvent={onOpenEvent}
                onNewEvent={onNewEvent}
              />
            ))}
            {today &&
            items.length > 0 &&
            nowIndex === -1 &&
            itemEnd(items[items.length - 1]).getTime() <= now.getTime() ? (
              <NowLine now={now} />
            ) : null}
          </section>
        );
      })}
    </div>
  );
}

function AgendaEntry({
  item,
  day,
  calendarsByID,
  canCreate,
  showNow,
  now,
  onOpenEvent,
  onNewEvent
}: {
  item: AgendaItem;
  day: Date;
  calendarsByID: Map<number, CalendarSummary>;
  canCreate: boolean;
  showNow: boolean;
  now: Date;
  onOpenEvent: (event: CalendarEvent, day: Date) => void;
  onNewEvent: (start: Date, end: Date) => void;
}) {
  const nowLine = showNow ? <NowLine now={now} /> : null;
  if (item.kind === "event") {
    return (
      <>
        {nowLine}
        <AgendaRow event={item.event} day={day} calendarsByID={calendarsByID} onOpenEvent={onOpenEvent} />
      </>
    );
  }
  if (item.kind === "free") {
    const label = `${formatTime(item.start)}–${formatTime(item.end)} · ${formatMinutes(
      (item.end.getTime() - item.start.getTime()) / 60_000
    )} free`;
    return (
      <>
        {nowLine}
        {canCreate ? (
          <button
            type="button"
            className="calendar-agenda-free"
            // A new event starts when the gap does and lasts an hour, or less
            // when the gap is shorter.
            onClick={() =>
              onNewEvent(item.start, new Date(Math.min(item.end.getTime(), item.start.getTime() + 3_600_000)))
            }
          >
            <span>{label}</span>
            <span className="calendar-agenda-free-add">
              <Icon name="add" />
              Event
            </span>
          </button>
        ) : (
          <div className="calendar-agenda-free">
            <span>{label}</span>
          </div>
        )}
      </>
    );
  }
  return (
    <>
      {nowLine}
      <div className="calendar-agenda-together" role="group" aria-label={`${item.events.length} at the same time`}>
        <div className="calendar-agenda-together-head">
          <Icon name="copy" />
          {formatTime(item.start)}–{formatTime(item.end)} · {item.events.length} at the same time
        </div>
        {item.events.map((event) => (
          <AgendaRow key={event.id} event={event} day={day} calendarsByID={calendarsByID} onOpenEvent={onOpenEvent} />
        ))}
      </div>
    </>
  );
}

function AgendaRow({
  event,
  day,
  calendarsByID,
  onOpenEvent
}: {
  event: CalendarEvent;
  day: Date;
  calendarsByID: Map<number, CalendarSummary>;
  onOpenEvent: (event: CalendarEvent, day: Date) => void;
}) {
  const calendar = calendarsByID.get(event.calendar_id);
  const declined = event.my_response === "declined" || event.status === "cancelled";
  const meta = [calendar?.name || "", event.location].filter(Boolean).join(" · ");
  return (
    <button
      type="button"
      className={`calendar-agenda-row ${declined ? "declined" : ""} ${event.pending_offline ? "pending-offline" : ""}`}
      onClick={() => onOpenEvent(event, day)}
    >
      <AgendaTime event={event} day={day} />
      <span className="calendar-agenda-dot" style={{ background: calendarColor(calendar) }} aria-hidden="true" />
      <span className="calendar-agenda-text">
        <span className="calendar-agenda-title">
          {event.online_meeting ? <Icon name="video" /> : null}
          {event.summary || "(No title)"}
        </span>
        {meta ? <span className="calendar-agenda-meta">{meta}</span> : null}
      </span>
    </button>
  );
}

function NowLine({ now }: { now: Date }) {
  return (
    <div className="calendar-agenda-now" aria-label={`Now, ${formatTime(now)}`}>
      <span>{formatTime(now)}</span>
    </div>
  );
}

/**
 * MobileWeekList is the week as seven rows: what is on all day, the first few
 * appointments and how many more. A row is a way into its day rather than a
 * place to work, so the whole row opens the day view.
 */
export function MobileWeekList({
  days,
  events,
  calendarsByID,
  todayKey,
  onOpenDay
}: {
  days: Date[];
  events: CalendarEvent[];
  calendarsByID: Map<number, CalendarSummary>;
  todayKey: string;
  onOpenDay: (day: Date) => void;
}) {
  return (
    <div className="calendar-weeklist">
      {weekListDays(events, days).map((row) => {
        const today = localDateKey(row.day) === todayKey;
        const label = row.day.toLocaleDateString(undefined, { weekday: "long", day: "numeric", month: "long" });
        const empty = row.allDay.length === 0 && row.timed.length === 0;
        return (
          <button
            key={localDateKey(row.day)}
            type="button"
            className={`calendar-weeklist-row ${today ? "today" : ""}`}
            aria-label={`${label}${today ? ", today" : ""}: open the day`}
            onClick={() => onOpenDay(row.day)}
          >
            <span className="calendar-weeklist-date" aria-hidden="true">
              <span className="calendar-weeklist-weekday">
                {row.day.toLocaleDateString(undefined, { weekday: "short" })}
              </span>
              <span className="calendar-weeklist-number">{row.day.getDate()}</span>
            </span>
            <span className="calendar-weeklist-body">
              {row.allDay.length > 0 ? (
                <span className="calendar-weeklist-allday">
                  {distinctColors(row.allDay, calendarsByID)
                    .slice(0, 3)
                    .map((color) => (
                      <span key={color} className="calendar-weeklist-dot" style={{ background: color }} />
                    ))}
                  <span className="calendar-weeklist-allday-text">
                    {row.allDay.map((event) => event.summary || "(No title)").join(" · ")}
                  </span>
                </span>
              ) : null}
              {row.timed.map((event) => (
                <span key={event.id} className="calendar-weeklist-line">
                  <span
                    className="calendar-weeklist-dot"
                    style={{ background: calendarColor(calendarsByID.get(event.calendar_id)) }}
                  />
                  <span className="calendar-weeklist-time">{startTimeOnDay(event, row.day)}</span>
                  <span className="calendar-weeklist-title">{event.summary || "(No title)"}</span>
                </span>
              ))}
              {row.more > 0 ? <span className="calendar-weeklist-more">+{row.more} more</span> : null}
              {empty ? <span className="calendar-weeklist-empty">Nothing scheduled</span> : null}
            </span>
          </button>
        );
      })}
    </div>
  );
}

function EventChip({
  event,
  day,
  calendarsByID,
  onOpenEvent
}: {
  event: CalendarEvent;
  day: Date;
  calendarsByID: Map<number, CalendarSummary>;
  onOpenEvent: (event: CalendarEvent, day: Date) => void;
}) {
  const color = calendarColor(calendarsByID.get(event.calendar_id));
  return (
    <button
      type="button"
      className={`calendar-chip ${event.pending_offline ? "pending-offline" : ""}`}
      style={{ background: color, color: readableTextColor(color) }}
      onClick={() => onOpenEvent(event, day)}
    >
      {event.summary || "(No title)"}
    </button>
  );
}

/** distinctColors is the colour of each calendar among some events, once. */
function distinctColors(events: CalendarEvent[], calendarsByID: Map<number, CalendarSummary>): string[] {
  const colors: string[] = [];
  for (const event of events) {
    const color = calendarColor(calendarsByID.get(event.calendar_id));
    if (!colors.includes(color)) colors.push(color);
  }
  return colors;
}

function itemStart(item: AgendaItem): Date {
  return item.kind === "event" ? new Date(item.event.start_at) : item.start;
}

function itemEnd(item: AgendaItem): Date {
  return item.kind === "event" ? new Date(item.event.end_at) : item.end;
}

function itemKey(item: AgendaItem): string {
  if (item.kind === "event") return `event-${item.event.id}`;
  if (item.kind === "free") return `free-${item.start.getTime()}`;
  return `together-${item.events.map((event) => event.id).join("-")}`;
}

// The week's own time format, so a time reads the same in every view.
function formatTime(date: Date): string {
  return date.toLocaleTimeString(undefined, { hour: "numeric", minute: "2-digit" });
}

/** AgendaTime is an event's time as its day sees it, the start above the end:
 * an appointment that began the day before runs "until", one that goes on past
 * midnight runs "from". */
function AgendaTime({ event, day }: { event: CalendarEvent; day: Date }) {
  const start = new Date(event.start_at);
  const end = new Date(event.end_at);
  const dayStart = startOfDay(day);
  const dayEnd = addDays(dayStart, 1);
  let first = formatTime(start);
  let second = `until ${formatTime(end)}`;
  if (start < dayStart && end > dayEnd) {
    first = "All day";
    second = "";
  } else if (start < dayStart) {
    first = `Until ${formatTime(end)}`;
    second = "";
  } else if (end > dayEnd) {
    second = "past midnight";
  }
  return (
    <span className="calendar-agenda-time">
      <span>{first}</span>
      {second ? <span className="calendar-agenda-time-end">{second}</span> : null}
    </span>
  );
}

function startTimeOnDay(event: CalendarEvent, day: Date): string {
  const start = new Date(event.start_at);
  return start < startOfDay(day) ? "…" : formatTime(start);
}

/** formatMinutes names a stretch of time the way a person would say it. */
function formatMinutes(minutes: number): string {
  const total = Math.round(minutes);
  const hours = Math.floor(total / 60);
  const rest = total % 60;
  if (hours === 0) return `${rest} min`;
  return rest === 0 ? `${hours} h` : `${hours} h ${rest} min`;
}
