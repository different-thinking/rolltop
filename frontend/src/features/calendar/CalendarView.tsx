// File overview: The week view. Every calendar of every connected Google and
// Microsoft 365 account is drawn in one overlay, coloured the way its provider
// colours it, with a visibility switch per calendar. The provider is the
// leading system, so every write here goes through the API that reaches it
// first.

import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import type { CSSProperties } from "react";
import { ApiError, api } from "../../api";
import type { CalendarEvent, CalendarEventInput, CalendarSummary } from "../../types";
import type { AddToast, LocationState } from "../../appTypes";
import { Icon } from "../../components/Icon";
import { messageFromError } from "../../lib/errors";
import { EventDialog } from "./EventDialog";
import {
  addDays,
  allDayEventsForDay,
  busiestColumns,
  calendarAccountGroups,
  calendarColor,
  calendarRouteDate,
  calendarURL,
  dayColumnWeights,
  dayHeight,
  hourHeight,
  layoutDayEvents,
  lensLaneCalendarIDs,
  localDateKey,
  minutesIntoDay,
  providerName,
  readableTextColor,
  startOfDay,
  startOfWeek,
  timedEventTouchesDay,
  weekDays
} from "./weekModel";
import type { PositionedEvent } from "./weekModel";

/** nowLineInterval keeps the current-time marker roughly honest without
 * re-rendering the whole week every second. */
const nowLineInterval = 60_000;

/** calendarSettingsPath is where the reader chooses which calendars the view
 * lists. */
const calendarSettingsPath = "/settings/account/preferences/calendars";

/** dayLensKey remembers per browser whether the day lens is on. It is a way
 * of looking at the week, not a setting: a narrow laptop and a wide desk
 * screen want different answers, and losing it to cleared site data costs one
 * click. */
const dayLensKey = "rolltop.calendar.dayLens";

function storedDayLens(): boolean {
  try {
    return window.localStorage.getItem(dayLensKey) === "on";
  } catch {
    return false;
  }
}

/** initialScrollHour is where the grid opens. Starting at midnight would put
 * the working day below the fold on every visit. */
const initialScrollHour = 7;

type DialogState = {
  event: CalendarEvent | null;
  day: Date;
  /** formKey identifies the form's contents. The dialog is remounted whenever
   * it changes, which is how a deliberate reset -- opening a different event,
   * adopting the version that won a conflict -- is expressed, and how a routine
   * reload of the calendars is kept from wiping what the user has typed. */
  formKey: string;
} | null;

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
  const [dayLens, setDayLens] = useState(storedDayLens);
  // The day the lens is open on, as a date key. null leaves the choice to
  // lensDefaultIndex; "" is the reader having closed it, which holds until
  // they open a day again.
  const [lensDayKey, setLensDayKey] = useState<string | null>(null);
  const gridRef = useRef<HTMLDivElement | null>(null);
  const scrolledRef = useRef(false);

  // Memoized together with the days, because a fresh Date on every render would
  // give every loader a new identity, and a loader in an effect's dependencies
  // that changes on every render is an unbounded refetch loop.
  const range = useMemo(() => ({ start: days[0], end: addDays(days[6], 1) }), [days]);

  // One monotonically increasing token for every load, so a slow answer for a
  // week the user has already navigated away from is discarded instead of
  // drawing the wrong week over the right one.
  const loadTokenRef = useRef(0);

  const reload = useCallback(
    async (what: "all" | "events") => {
      const token = ++loadTokenRef.current;
      const [calendarData, eventData] = await Promise.all([
        what === "all" ? api.calendars() : Promise.resolve(null),
        api.calendarEvents(range.start, range.end)
      ]);
      if (token !== loadTokenRef.current) return;
      if (calendarData) setCalendars(calendarData.calendars || []);
      setEvents(eventData.events || []);
    },
    [range]
  );

  useEffect(() => {
    let cancelled = false;
    setLoading(true);
    void reload("all")
      .catch((err) => {
        if (!cancelled) addToast(messageFromError(err), "error");
      })
      .finally(() => {
        if (!cancelled) setLoading(false);
      });
    return () => {
      cancelled = true;
    };
  }, [addToast, reload]);

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
      await reload("events");
    } catch (err) {
      setCalendars((current) =>
        current.map((item) => (item.id === calendar.id ? { ...item, selected: calendar.selected } : item))
      );
      addToast(messageFromError(err), "error");
    }
  };

  // Choosing a second calendar moves the mark off whichever calendar had it,
  // so the whole list is updated from the one answer rather than reloaded.
  const chooseCopyTarget = async (calendarID: number) => {
    const previous = calendars.find((calendar) => calendar.copy_target);
    if ((previous?.id || 0) === calendarID) return;
    try {
      const data = calendarID
        ? await api.setCalendarCopyTarget(csrf, calendarID, true)
        : previous
          ? await api.setCalendarCopyTarget(csrf, previous.id, false)
          : null;
      if (!data) return;
      setCalendars((current) =>
        current.map((item) => (item.id === data.calendar.id ? data.calendar : { ...item, copy_target: false }))
      );
    } catch (err) {
      addToast(messageFromError(err), "error");
    }
  };

  // A write whose copies did not all follow still saved the event itself, so
  // what went wrong with a copy is told after the dialog closes, not in it.
  const reportWarnings = (warnings: string[] | undefined) => {
    if (warnings && warnings.length > 0) addToast(warnings.join(" "), "error");
  };

  const syncNow = async () => {
    // A connection id is only unique within its provider, so accounts are
    // told apart by both.
    const accounts = new Map<string, { provider: CalendarSummary["provider"]; connectionID: number }>();
    for (const calendar of calendars) {
      if (!calendar.connection_id) continue;
      accounts.set(`${calendar.provider}:${calendar.connection_id}`, {
        provider: calendar.provider,
        connectionID: calendar.connection_id
      });
    }
    if (accounts.size === 0) return;
    setSyncing(true);
    // Each account is synced on its own: one revoked grant must not stop the
    // others, and it must not cost the refresh that shows what the accounts
    // that did sync came back with.
    const failures: string[] = [];
    for (const account of accounts.values()) {
      try {
        if (account.provider === "microsoft") await api.syncMicrosoftCalendar(csrf, account.connectionID);
        else await api.syncGoogleCalendar(csrf, account.connectionID);
      } catch (err) {
        failures.push(messageFromError(err));
      }
    }
    try {
      await reload("all");
    } catch (err) {
      failures.push(messageFromError(err));
    } finally {
      setSyncing(false);
    }
    // One toast however many accounts failed: a user with four connected
    // accounts and an expired grant does not need four copies of the same line.
    if (failures.length > 0) addToast(failures[0], "error");
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
          // A new form key: the submitted values lost, and the form has to show
          // the version that won rather than the one that was rejected.
          setDialog({
            event: winner,
            day: startOfDay(new Date(winner.start_at)),
            formKey: `adopted-${winner.id}-${winner.start_at}`
          });
        } else {
          // A conflict with no winning version -- a rejected delete -- leaves
          // the week showing state Google has moved on from.
          void reload("events").catch(() => {
            // The message below is what the user acts on; a failed refresh on
            // top of it is not worth a second one.
          });
        }
      }
      setDialogProblem(messageFromError(err));
    } finally {
      setSaving(false);
    }
  };

  const saveEvent = (input: CalendarEventInput) =>
    runWrite(async () => {
      const data = dialog?.event
        ? await api.updateCalendarEvent(csrf, dialog.event.id, input)
        : await api.createCalendarEvent(csrf, input);
      await reload("events");
      closeDialog();
      reportWarnings(data.warnings);
    });

  const deleteEvent = () =>
    runWrite(async () => {
      if (!dialog?.event) return;
      const provider = providerName(calendarsByID.get(dialog.event.calendar_id));
      const data = await api.deleteCalendarEvent(csrf, dialog.event.id);
      await reload("events");
      closeDialog();
      if (data.warnings && data.warnings.length > 0) reportWarnings(data.warnings);
      else addToast(`Event deleted in ${provider} too.`, "success");
    });

  const respond = (response: string) =>
    runWrite(async () => {
      if (!dialog?.event) return;
      const data = await api.respondToCalendarEvent(csrf, dialog.event.id, response);
      setEvents((current) => current.map((item) => (item.id === data.event.id ? data.event : item)));
      // The same form key: only the answer changed, and remounting the dialog
      // would throw away an edit the user had started alongside it.
      setDialog({ event: data.event, day: dialog.day, formKey: dialog.formKey });
    });

  // Only the calendars the reader chose to list take part in the view; the
  // rest are still loaded so a copy mark can name the calendar it stands for.
  const listedCalendars = calendars.filter((calendar) => calendar.listed);
  const writableCalendars = listedCalendars.filter((calendar) => calendar.can_write);
  const todayKey = localDateKey(now);
  const weekLabel = weekRangeLabel(days[0], days[6]);

  // The day lens: one day opened up with a lane per calendar, every other day
  // sized by how much it has at once. Lanes follow the Calendars menu's order.
  const lensIndex = !dayLens || lensDayKey === "" ? -1 : resolveLensIndex(days, events, lensDayKey, todayKey);
  const lensLanes =
    lensIndex >= 0
      ? lensLaneCalendarIDs(events, calendarAccountGroups(listedCalendars).flatMap(([, items]) => items), days[lensIndex])
      : [];
  const dayColumns = dayLens
    ? dayColumnWeights(events, days, lensIndex, lensLanes.length)
        .map((weight) => `minmax(0, ${weight.toFixed(2)}fr)`)
        .join(" ")
    : undefined;

  const toggleDayLens = () => {
    const next = !dayLens;
    setDayLens(next);
    setLensDayKey(null);
    try {
      window.localStorage.setItem(dayLensKey, next ? "on" : "off");
    } catch {
      // Remembering the choice is a convenience; the switch itself still works.
    }
  };

  const openEvent = (event: CalendarEvent, day: Date) => {
    setDialogProblem("");
    setDialog({ event, day, formKey: `event-${event.id}` });
  };

  // One timed entry, placed in a share of its day column: the whole column
  // normally, one calendar's lane of it under the day lens.
  const renderTimedEvent = (placed: PositionedEvent, day: Date, lane: number, lanes: number) => {
    const calendar = calendarsByID.get(placed.event.calendar_id);
    const color = calendarColor(calendar);
    const laneWidth = 100 / lanes;
    const width = laneWidth / placed.columns;
    const declined = placed.event.my_response === "declined" || placed.event.status === "cancelled";
    return (
      <button
        key={placed.event.id}
        type="button"
        className={`calendar-event ${declined ? "declined" : ""} ${
          placed.event.status === "cancelled" ? "cancelled" : ""
        } ${placed.continuesBefore ? "continues-before" : ""} ${placed.continuesAfter ? "continues-after" : ""}`}
        style={{
          top: placed.top,
          height: placed.height,
          left: `${lane * laneWidth + placed.column * width}%`,
          width: `calc(${width}% - 3px)`,
          background: declined ? "transparent" : color,
          borderColor: color,
          color: declined ? "var(--text)" : readableTextColor(color)
        }}
        title={eventTooltip(placed.event, calendar, calendarsByID)}
        onClick={() => openEvent(placed.event, day)}
      >
        <span className="calendar-event-time">{formatTimeRange(placed.event)}</span>
        <span className="calendar-event-title">
          <CopyMarks event={placed.event} calendarsByID={calendarsByID} />
          {placed.event.online_meeting ? (
            <span className="calendar-event-online" title="Online meeting">
              <Icon name="video" />
            </span>
          ) : null}
          {placed.event.summary || "(No title)"}
        </span>
        {placed.event.location ? <span className="calendar-event-location">{placed.event.location}</span> : null}
      </button>
    );
  };

  return (
    <div className="calendar-shell">
      <section
        className={`calendar-main ${dayLens ? "day-lens" : ""}`}
        style={dayColumns ? ({ "--calendar-day-columns": dayColumns } as CSSProperties) : undefined}
      >
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
            <CalendarPicker
              calendars={listedCalendars}
              hiddenCount={calendars.length - listedCalendars.length}
              onToggle={toggleCalendar}
              onChooseCopyTarget={(calendarID) => void chooseCopyTarget(calendarID)}
              navigate={navigate}
            />
            <button
              type="button"
              className="ghost calendar-lens-toggle"
              aria-pressed={dayLens}
              title={
                dayLens
                  ? "Turn the day lens off and give every day the same width"
                  : "Open one day with a column per calendar; click a weekday to choose which"
              }
              onClick={toggleDayLens}
            >
              <Icon name="search" />
              Day lens
            </button>
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
                setDialog({ event: null, day: days[0], formKey: `new-${days[0].getTime()}` });
              }}
            >
              <Icon name="add" />
              New event
            </button>
          </div>
        </header>

        {calendars.length === 0 && !loading ? (
          <p className="calendar-empty">
            No calendars yet. Connect a Google account under <a href="/settings/account/google">Google settings</a> or a
            Microsoft 365 account under <a href="/settings/account/microsoft">Microsoft 365 settings</a> and allow
            calendar access.
          </p>
        ) : null}

        <div className="calendar-daynames">
          <div className="calendar-gutter" />
          {days.map((day, index) => {
            const today = localDateKey(day) === todayKey ? "today" : "";
            const label = (
              <>
                <span className="calendar-dayname-weekday">{weekdayLabel(day)}</span>
                <span className="calendar-dayname-number">{day.getDate()}</span>
              </>
            );
            if (!dayLens) {
              return (
                <div key={day.toISOString()} className={`calendar-dayname ${today}`}>
                  {label}
                </div>
              );
            }
            const open = index === lensIndex;
            return (
              <div key={day.toISOString()} className={`calendar-dayname ${today} ${open ? "lens-open" : ""}`}>
                <button
                  type="button"
                  className="calendar-dayname-button"
                  aria-pressed={open}
                  title={open ? "Close this day" : "Open this day with a column per calendar"}
                  onClick={() => setLensDayKey(open ? "" : localDateKey(day))}
                >
                  {label}
                </button>
                {open && lensLanes.length > 0 ? (
                  <div className="calendar-lens-lanes">
                    {lensLanes.map((calendarID) => {
                      const calendar = calendarsByID.get(calendarID);
                      return (
                        <span
                          key={calendarID}
                          title={calendar?.name}
                          style={{ "--calendar-color": calendarColor(calendar) } as CSSProperties}
                        >
                          {calendar?.name || "Calendar"}
                        </span>
                      );
                    })}
                  </div>
                ) : null}
              </div>
            );
          })}
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
                    title={eventTooltip(event, calendarsByID.get(event.calendar_id), calendarsByID)}
                    onClick={() => {
                      setDialogProblem("");
                      setDialog({ event, day, formKey: `event-${event.id}` });
                    }}
                  >
                    <CopyMarks event={event} calendarsByID={calendarsByID} />
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
          {days.map((day, index) => {
            const isToday = localDateKey(day) === todayKey;
            const dayEvents = events.filter((event) => !event.all_day && timedEventTouchesDay(event, day));
            const lanes = index === lensIndex ? lensLanes : [];
            return (
              <div
                key={day.toISOString()}
                className={`calendar-day ${isToday ? "today" : ""}`}
                style={{ height: dayHeight }}
                onDoubleClick={(mouseEvent) => {
                  if (writableCalendars.length === 0) return;
                  setDialogProblem("");
                  const start = dayAtOffset(day, mouseEvent);
                  setDialog({ event: null, day: start, formKey: `new-${start.getTime()}` });
                }}
              >
                {Array.from({ length: 24 }, (_, hour) => (
                  <div key={hour} className="calendar-slot" style={{ top: hour * hourHeight, height: hourHeight }} />
                ))}
                {lanes.length > 1
                  ? lanes.flatMap((calendarID, lane) => [
                      lane > 0 ? (
                        <div
                          key={`divider-${calendarID}`}
                          className="calendar-lens-divider"
                          style={{ left: `${(lane * 100) / lanes.length}%` }}
                        />
                      ) : null,
                      ...layoutDayEvents(
                        dayEvents.filter((event) => event.calendar_id === calendarID),
                        day
                      ).map((placed) => renderTimedEvent(placed, day, lane, lanes.length))
                    ])
                  : layoutDayEvents(dayEvents, day).map((placed) => renderTimedEvent(placed, day, 0, 1))}
                {isToday ? <div className="calendar-now" style={{ top: (minutesIntoDay(now) / 60) * hourHeight }} /> : null}
              </div>
            );
          })}
        </div>
      </section>

      {dialog ? (
        <EventDialog
          key={dialog.formKey}
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

/** CalendarPicker is the menu in the calendar header that lists the calendars
 * the reader chose to list, grouped by the account they came from, because two
 * accounts routinely have a calendar called the same thing. It used to be a
 * column of its own beside the week, which with four calendars took a sixth of
 * the screen from the days that needed it; folded into the header it costs one
 * button, and the button's dots still say which calendars are drawn. The
 * second-calendar choice is made among the same calendars: one hidden under
 * settings is never offered there. */
function CalendarPicker({
  calendars,
  hiddenCount,
  onToggle,
  onChooseCopyTarget,
  navigate
}: {
  calendars: CalendarSummary[];
  hiddenCount: number;
  onToggle: (calendar: CalendarSummary) => Promise<void>;
  onChooseCopyTarget: (calendarID: number) => void;
  navigate: (url: string) => void;
}) {
  const writable = calendars.filter((calendar) => calendar.can_write);
  const copyTarget = calendars.find((calendar) => calendar.copy_target);
  // A chosen calendar that has since become read-only stays listed, or the
  // select would read "None" while the choice is still stored.
  const copyOptions = calendars.filter((calendar) => calendar.can_write || calendar.copy_target);
  const groups = useMemo(() => calendarAccountGroups(calendars), [calendars]);
  const shown = calendars.filter((calendar) => calendar.selected);
  const menuRef = useRef<HTMLDetailsElement | null>(null);
  const [open, setOpen] = useState(false);

  // A click anywhere else or Escape closes the menu. Toggling calendars inside
  // it does not: switching three of them on in a row is the ordinary use.
  useEffect(() => {
    if (!open) return;
    const close = () => {
      if (menuRef.current) menuRef.current.open = false;
    };
    const onPointerDown = (pointerEvent: PointerEvent) => {
      if (menuRef.current && !menuRef.current.contains(pointerEvent.target as Node)) close();
    };
    const onKeyDown = (keyEvent: KeyboardEvent) => {
      if (keyEvent.key === "Escape") close();
    };
    document.addEventListener("pointerdown", onPointerDown);
    document.addEventListener("keydown", onKeyDown);
    return () => {
      document.removeEventListener("pointerdown", onPointerDown);
      document.removeEventListener("keydown", onKeyDown);
    };
  }, [open]);

  return (
    <details
      className="calendar-picker"
      ref={menuRef}
      onToggle={(toggleEvent) => setOpen((toggleEvent.currentTarget as HTMLDetailsElement).open)}
    >
      <summary
        className="calendar-picker-summary"
        aria-label={`Calendars, ${shown.length} of ${calendars.length} shown`}
        title={shown.length > 0 ? `Shown: ${shown.map((calendar) => calendar.name).join(", ")}` : "No calendar shown"}
      >
        <Icon name="calendar" />
        Calendars
        <span className="calendar-picker-dots" aria-hidden="true">
          {shown.map((calendar) => (
            <span key={calendar.id} style={{ background: calendarColor(calendar) }} />
          ))}
        </span>
        <Icon name="expand_more" />
      </summary>
      <div className="calendar-picker-panel">
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
        {writable.length > 1 ? (
          <div className="calendar-sidebar-group calendar-copy-target">
            <label className="calendar-sidebar-account" htmlFor="calendar-copy-target">
              Second calendar
            </label>
            <select
              id="calendar-copy-target"
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
              An event in any other calendar can be added here as well. It is shown once, marked with the other
              calendar's colour.
            </p>
          </div>
        ) : null}
        {calendars.length > 0 || hiddenCount > 0 ? (
          <div className="calendar-sidebar-manage">
            {calendars.length === 0 ? (
              <p className="calendar-copy-target-hint">Every calendar is hidden.</p>
            ) : null}
            <a
              href={calendarSettingsPath}
              onClick={(clickEvent) => {
                clickEvent.preventDefault();
                if (menuRef.current) menuRef.current.open = false;
                navigate(calendarSettingsPath);
              }}
            >
              <Icon name="settings" />
              {hiddenCount > 0 ? `Manage calendars (${hiddenCount} hidden)` : "Manage calendars"}
            </a>
          </div>
        ) : null}
      </div>
    </details>
  );
}

/** CopyMarks shows, on an entry the week draws once, the colour of every other
 * calendar holding a copy of it. */
function CopyMarks({
  event,
  calendarsByID
}: {
  event: CalendarEvent;
  calendarsByID: Map<number, CalendarSummary>;
}) {
  const copies = event.also_in || [];
  if (copies.length === 0) return null;
  return (
    <span className="calendar-event-copies" aria-hidden="true">
      {copies.map((copy) => (
        <span
          key={copy.event_id}
          className="calendar-event-copy-mark"
          style={{ background: calendarColor(calendarsByID.get(copy.calendar_id)) }}
        />
      ))}
    </span>
  );
}

/** resolveLensIndex picks the day the lens opens on: the one the reader chose
 * when it is in this week, otherwise today when today has anything timed,
 * otherwise the day with the most at once -- the day the lens exists for. */
function resolveLensIndex(days: Date[], events: CalendarEvent[], chosenKey: string | null, todayKey: string): number {
  const chosen = chosenKey ? days.findIndex((day) => localDateKey(day) === chosenKey) : -1;
  if (chosen >= 0) return chosen;
  const today = days.findIndex((day) => localDateKey(day) === todayKey);
  if (today >= 0 && busiestColumns(events, days[today]) > 0) return today;
  let busiest = 0;
  let most = -1;
  days.forEach((day, index) => {
    const columns = busiestColumns(events, day);
    if (columns > most) {
      most = columns;
      busiest = index;
    }
  });
  return busiest;
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

function eventTooltip(
  event: CalendarEvent,
  calendar: CalendarSummary | undefined,
  calendarsByID: Map<number, CalendarSummary>
): string {
  const parts = [event.summary || "(No title)"];
  if (event.location) parts.push(event.location);
  if (calendar) parts.push(calendar.name);
  const copies = (event.also_in || [])
    .map((copy) => calendarsByID.get(copy.calendar_id)?.name)
    .filter((name): name is string => Boolean(name));
  if (copies.length > 0) parts.push(`also in ${copies.join(", ")}`);
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
