// File overview: The week view. Every calendar of every connected Google
// account is drawn in one overlay, coloured the way Google colours it, with a
// visibility switch per calendar. Google is the leading system, so every write
// here goes through the API that reaches Google first.

import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { ApiError, api } from "../../api";
import type { CalendarEvent, CalendarEventInput, CalendarSummary } from "../../types";
import type { AddToast, LocationState } from "../../appTypes";
import { Icon } from "../../components/Icon";
import { messageFromError } from "../../lib/errors";
import { EventDialog } from "./EventDialog";
import {
  addDays,
  allDayEventsForDay,
  calendarColor,
  calendarRouteDate,
  calendarURL,
  dayHeight,
  hourHeight,
  layoutDayEvents,
  localDateKey,
  minutesIntoDay,
  readableTextColor,
  startOfDay,
  startOfWeek,
  timedEventTouchesDay,
  weekDays
} from "./weekModel";

/** nowLineInterval keeps the current-time marker roughly honest without
 * re-rendering the whole week every second. */
const nowLineInterval = 60_000;

/** initialScrollHour is where the grid opens. Starting at midnight would put
 * the working day below the fold on every visit. */
const initialScrollHour = 7;

type DialogState = { event: CalendarEvent | null; day: Date } | null;

/** CalendarView renders one week of every visible calendar. */
export function CalendarView({
  csrf,
  location,
  navigate,
  addToast
}: {
  csrf: string;
  location: LocationState;
  navigate: (url: string) => void;
  addToast: AddToast;
}) {
  const anchor = useMemo(() => startOfWeek(calendarRouteDate(location.path)), [location.path]);
  const days = useMemo(() => weekDays(anchor), [anchor]);
  const [calendars, setCalendars] = useState<CalendarSummary[]>([]);
  const [events, setEvents] = useState<CalendarEvent[]>([]);
  const [loading, setLoading] = useState(true);
  const [syncing, setSyncing] = useState(false);
  const [saving, setSaving] = useState(false);
  const [dialogProblem, setDialogProblem] = useState("");
  const [dialog, setDialog] = useState<DialogState>(null);
  const [now, setNow] = useState(() => new Date());
  const gridRef = useRef<HTMLDivElement | null>(null);
  const scrolledRef = useRef(false);

  const rangeStart = days[0];
  const rangeEnd = addDays(days[6], 1);

  const loadCalendars = useCallback(async () => {
    const data = await api.calendars();
    setCalendars(data.calendars || []);
  }, []);

  const loadEvents = useCallback(async () => {
    const data = await api.calendarEvents(rangeStart, rangeEnd);
    setEvents(data.events || []);
  }, [rangeEnd, rangeStart]);

  useEffect(() => {
    let cancelled = false;
    setLoading(true);
    void Promise.all([loadCalendars(), loadEvents()])
      .catch((err) => {
        if (!cancelled) addToast(messageFromError(err), "error");
      })
      .finally(() => {
        if (!cancelled) setLoading(false);
      });
    return () => {
      cancelled = true;
    };
  }, [addToast, loadCalendars, loadEvents]);

  useEffect(() => {
    const timer = window.setInterval(() => setNow(new Date()), nowLineInterval);
    return () => window.clearInterval(timer);
  }, []);

  // Only on the first render: scrolling back to the morning every time the week
  // reloads would fight a user who scrolled to an evening appointment.
  useEffect(() => {
    if (scrolledRef.current || loading || !gridRef.current) return;
    gridRef.current.scrollTop = initialScrollHour * hourHeight;
    scrolledRef.current = true;
  }, [loading]);

  const calendarsByID = useMemo(() => {
    const map = new Map<number, CalendarSummary>();
    calendars.forEach((calendar) => map.set(calendar.id, calendar));
    return map;
  }, [calendars]);

  const toggleCalendar = async (calendar: CalendarSummary) => {
    // Switched optimistically: the answer can take a first sync's worth of time
    // and a checkbox that ignores the click until then reads as broken.
    setCalendars((current) =>
      current.map((item) => (item.id === calendar.id ? { ...item, selected: !item.selected } : item))
    );
    try {
      const data = await api.setCalendarSelected(csrf, calendar.id, !calendar.selected);
      setCalendars((current) => current.map((item) => (item.id === calendar.id ? data.calendar : item)));
      await loadEvents();
    } catch (err) {
      setCalendars((current) =>
        current.map((item) => (item.id === calendar.id ? { ...item, selected: calendar.selected } : item))
      );
      addToast(messageFromError(err), "error");
    }
  };

  const syncNow = async () => {
    const connectionIDs = Array.from(new Set(calendars.map((calendar) => calendar.connection_id))).filter(Boolean);
    if (connectionIDs.length === 0) return;
    setSyncing(true);
    try {
      for (const connectionID of connectionIDs) {
        await api.syncGoogleCalendar(csrf, connectionID);
      }
      await Promise.all([loadCalendars(), loadEvents()]);
    } catch (err) {
      addToast(messageFromError(err), "error");
    } finally {
      setSyncing(false);
    }
  };

  const closeDialog = () => {
    setDialog(null);
    setDialogProblem("");
  };

  // A write that Google refused is reported in the dialog rather than as a
  // toast: the form is still open with the values that were rejected, and that
  // is where the reason has to be.
  const runWrite = async (write: () => Promise<void>) => {
    setSaving(true);
    setDialogProblem("");
    try {
      await write();
    } catch (err) {
      if (err instanceof ApiError && err.status === 409) {
        const winner = err.payload.event as CalendarEvent | undefined;
        if (winner) {
          setEvents((current) => current.map((item) => (item.id === winner.id ? winner : item)));
          setDialog({ event: winner, day: startOfDay(new Date(winner.start_at)) });
        }
      }
      setDialogProblem(messageFromError(err));
    } finally {
      setSaving(false);
    }
  };

  const saveEvent = (input: CalendarEventInput) =>
    runWrite(async () => {
      if (dialog?.event) await api.updateCalendarEvent(csrf, dialog.event.id, input);
      else await api.createCalendarEvent(csrf, input);
      await loadEvents();
      closeDialog();
    });

  const deleteEvent = () =>
    runWrite(async () => {
      if (!dialog?.event) return;
      await api.deleteCalendarEvent(csrf, dialog.event.id);
      await loadEvents();
      closeDialog();
      addToast("Event deleted in Google too.", "success");
    });

  const respond = (response: string) =>
    runWrite(async () => {
      if (!dialog?.event) return;
      const data = await api.respondToCalendarEvent(csrf, dialog.event.id, response);
      setEvents((current) => current.map((item) => (item.id === data.event.id ? data.event : item)));
      setDialog({ event: data.event, day: dialog.day });
    });

  const writableCalendars = calendars.filter((calendar) => calendar.can_write);
  const todayKey = localDateKey(now);
  const weekLabel = weekRangeLabel(days[0], days[6]);

  return (
    <div className="calendar-shell">
      <CalendarSidebar calendars={calendars} onToggle={toggleCalendar} />
      <section className="calendar-main">
        <header className="content-head calendar-head">
          <div className="calendar-nav">
            <button
              type="button"
              className="icon-button"
              title="Previous week"
              onClick={() => navigate(calendarURL(addDays(anchor, -7)))}
            >
              <Icon name="chevron_left" />
            </button>
            <button type="button" className="ghost" onClick={() => navigate(calendarURL(new Date()))}>
              Today
            </button>
            <button
              type="button"
              className="icon-button"
              title="Next week"
              onClick={() => navigate(calendarURL(addDays(anchor, 7)))}
            >
              <Icon name="chevron_right" />
            </button>
            <h1>{weekLabel}</h1>
          </div>
          <div className="calendar-head-actions">
            <button type="button" className="ghost" disabled={syncing || calendars.length === 0} onClick={() => void syncNow()}>
              <Icon name="sync" />
              {syncing ? "Syncing…" : "Sync now"}
            </button>
            <button
              type="button"
              disabled={writableCalendars.length === 0}
              title={writableCalendars.length === 0 ? "No calendar you can write to is connected" : undefined}
              onClick={() => {
                setDialogProblem("");
                setDialog({ event: null, day: days[0] });
              }}
            >
              <Icon name="add" />
              New event
            </button>
          </div>
        </header>

        {calendars.length === 0 && !loading ? (
          <p className="calendar-empty">
            No calendars yet. Connect a Google account under <a href="/settings/account/google">Google settings</a> and
            allow calendar access.
          </p>
        ) : null}

        <div className="calendar-daynames">
          <div className="calendar-gutter" />
          {days.map((day) => (
            <div key={day.toISOString()} className={`calendar-dayname ${localDateKey(day) === todayKey ? "today" : ""}`}>
              <span className="calendar-dayname-weekday">{weekdayLabel(day)}</span>
              <span className="calendar-dayname-number">{day.getDate()}</span>
            </div>
          ))}
        </div>

        <div className="calendar-allday">
          <div className="calendar-gutter">All day</div>
          {days.map((day) => (
            <div key={day.toISOString()} className="calendar-allday-cell">
              {allDayEventsForDay(events, day).map((event) => {
                const color = calendarColor(calendarsByID.get(event.calendar_id));
                return (
                  <button
                    key={`${event.id}-${localDateKey(day)}`}
                    type="button"
                    className="calendar-chip"
                    style={{ background: color, color: readableTextColor(color) }}
                    onClick={() => {
                      setDialogProblem("");
                      setDialog({ event, day });
                    }}
                  >
                    {event.summary || "(No title)"}
                  </button>
                );
              })}
            </div>
          ))}
        </div>

        <div className="calendar-grid" ref={gridRef}>
          <div className="calendar-gutter calendar-hours" style={{ height: dayHeight }}>
            {Array.from({ length: 24 }, (_, hour) => (
              <div key={hour} className="calendar-hour" style={{ top: hour * hourHeight }}>
                {hour === 0 ? "" : formatHour(hour)}
              </div>
            ))}
          </div>
          {days.map((day) => {
            const isToday = localDateKey(day) === todayKey;
            const dayEvents = events.filter((event) => !event.all_day && timedEventTouchesDay(event, day));
            return (
              <div
                key={day.toISOString()}
                className={`calendar-day ${isToday ? "today" : ""}`}
                style={{ height: dayHeight }}
                onDoubleClick={(mouseEvent) => {
                  if (writableCalendars.length === 0) return;
                  setDialogProblem("");
                  setDialog({ event: null, day: dayAtOffset(day, mouseEvent) });
                }}
              >
                {Array.from({ length: 24 }, (_, hour) => (
                  <div key={hour} className="calendar-slot" style={{ top: hour * hourHeight, height: hourHeight }} />
                ))}
                {layoutDayEvents(dayEvents, day).map((placed) => {
                  const calendar = calendarsByID.get(placed.event.calendar_id);
                  const color = calendarColor(calendar);
                  const width = 100 / placed.columns;
                  const declined = placed.event.my_response === "declined";
                  return (
                    <button
                      key={placed.event.id}
                      type="button"
                      className={`calendar-event ${declined ? "declined" : ""} ${
                        placed.continuesBefore ? "continues-before" : ""
                      } ${placed.continuesAfter ? "continues-after" : ""}`}
                      style={{
                        top: placed.top,
                        height: placed.height,
                        left: `${placed.column * width}%`,
                        width: `calc(${width}% - 3px)`,
                        background: declined ? "transparent" : color,
                        borderColor: color,
                        color: declined ? "var(--text)" : readableTextColor(color)
                      }}
                      title={eventTooltip(placed.event, calendar)}
                      onClick={() => {
                        setDialogProblem("");
                        setDialog({ event: placed.event, day });
                      }}
                    >
                      <span className="calendar-event-time">{formatTimeRange(placed.event)}</span>
                      <span className="calendar-event-title">{placed.event.summary || "(No title)"}</span>
                      {placed.event.location ? (
                        <span className="calendar-event-location">{placed.event.location}</span>
                      ) : null}
                    </button>
                  );
                })}
                {isToday ? <div className="calendar-now" style={{ top: (minutesIntoDay(now) / 60) * hourHeight }} /> : null}
              </div>
            );
          })}
        </div>
      </section>

      {dialog ? (
        <EventDialog
          event={dialog.event}
          day={dialog.day}
          calendars={calendars}
          saving={saving}
          problem={dialogProblem}
          onSave={(input) => void saveEvent(input)}
          onDelete={() => void deleteEvent()}
          onRespond={(response) => void respond(response)}
          onClose={closeDialog}
        />
      ) : null}
    </div>
  );
}

/** CalendarSidebar lists every calendar grouped by the account it came from,
 * because two accounts routinely have a calendar called the same thing. */
function CalendarSidebar({
  calendars,
  onToggle
}: {
  calendars: CalendarSummary[];
  onToggle: (calendar: CalendarSummary) => Promise<void>;
}) {
  const groups = useMemo(() => {
    const byAccount = new Map<string, CalendarSummary[]>();
    for (const calendar of calendars) {
      const key = calendar.connection_email || "Google";
      const list = byAccount.get(key) || [];
      list.push(calendar);
      byAccount.set(key, list);
    }
    return Array.from(byAccount.entries());
  }, [calendars]);

  return (
    <aside className="calendar-sidebar">
      {groups.map(([account, items]) => (
        <div key={account} className="calendar-sidebar-group">
          <div className="calendar-sidebar-account">{account}</div>
          {items.map((calendar) => {
            const color = calendarColor(calendar);
            return (
              <label key={calendar.id} className="calendar-toggle">
                <input
                  type="checkbox"
                  checked={calendar.selected}
                  onChange={() => void onToggle(calendar)}
                />
                <span className="calendar-swatch" style={{ background: color }} />
                <span className="calendar-toggle-name">{calendar.name}</span>
                {calendar.status === "error" ? (
                  <span className="calendar-toggle-problem" title={calendar.status_detail}>
                    <Icon name="report" />
                  </span>
                ) : null}
                {!calendar.can_write ? <span className="calendar-toggle-tag">read-only</span> : null}
              </label>
            );
          })}
        </div>
      ))}
    </aside>
  );
}

/** dayAtOffset turns a double-click in a day column into the hour it landed on,
 * so a new event starts where the user pointed. */
function dayAtOffset(day: Date, mouseEvent: { clientY: number; currentTarget: HTMLElement }): Date {
  const bounds = mouseEvent.currentTarget.getBoundingClientRect();
  const minutes = Math.max(0, Math.min(((mouseEvent.clientY - bounds.top) / hourHeight) * 60, 23 * 60));
  const start = startOfDay(day);
  // Rounded to the half hour, which is how appointments are actually made.
  start.setMinutes(Math.round(minutes / 30) * 30);
  return start;
}

function weekdayLabel(day: Date): string {
  return day.toLocaleDateString(undefined, { weekday: "short" });
}

function formatHour(hour: number): string {
  const date = new Date();
  date.setHours(hour, 0, 0, 0);
  return date.toLocaleTimeString(undefined, { hour: "numeric" });
}

function formatTimeRange(event: CalendarEvent): string {
  const start = new Date(event.start_at);
  return start.toLocaleTimeString(undefined, { hour: "numeric", minute: "2-digit" });
}

function eventTooltip(event: CalendarEvent, calendar: CalendarSummary | undefined): string {
  const parts = [event.summary || "(No title)"];
  if (event.location) parts.push(event.location);
  if (calendar) parts.push(calendar.name);
  return parts.join(" — ");
}

function weekRangeLabel(first: Date, last: Date): string {
  const sameMonth = first.getMonth() === last.getMonth() && first.getFullYear() === last.getFullYear();
  const firstLabel = first.toLocaleDateString(undefined, { day: "numeric", month: sameMonth ? undefined : "short" });
  const lastLabel = last.toLocaleDateString(undefined, {
    day: "numeric",
    month: "short",
    year: first.getFullYear() === last.getFullYear() ? "numeric" : undefined
  });
  if (first.getFullYear() !== last.getFullYear()) {
    return `${first.toLocaleDateString(undefined, { day: "numeric", month: "short", year: "numeric" })} – ${lastLabel}`;
  }
  return `${firstLabel} – ${lastLabel}`;
}
