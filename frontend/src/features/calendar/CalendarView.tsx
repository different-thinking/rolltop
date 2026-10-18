// File overview: The week view. Every calendar of every connected Google and
// Microsoft 365 account is drawn in one overlay, coloured the way its provider
// colours it, with a visibility switch per calendar. The provider is the
// leading system, so every write here goes through the API that reaches it
// first.

import { Fragment, useCallback, useEffect, useLayoutEffect, useMemo, useRef, useState } from "react";
import type { CSSProperties, PointerEvent as ReactPointerEvent } from "react";
import { ApiError, api } from "../../api";
import type { CalendarEvent, CalendarEventInput, CalendarSummary } from "../../types";
import type { AddToast, LocationState } from "../../appTypes";
import { Icon } from "../../components/Icon";
import { messageFromError } from "../../lib/errors";
import { displayDateTime } from "../../lib/format";
import { isNetworkError, serverReachable } from "../../lib/offline";
import {
  enqueueCalendarEvent,
  pendingCalendarEvent,
  pendingReason,
  queuedLocalID,
  removeQueuedCalendarEvent,
  replaceQueuedCalendarEvent,
  useCalendarOutbox
} from "../../lib/calendarOutbox";
import { EventDialog } from "./EventDialog";
import type { EventPrefill } from "./EventDialog";
import { useWeekDrag } from "./useWeekDrag";
import type { WeekDrag } from "./useWeekDrag";
import {
  addDays,
  allDayEventsForDay,
  allDayRangeCoversDay,
  busiestColumns,
  calendarAccountGroups,
  calendarColor,
  calendarRouteDate,
  calendarURL,
  dayColumnWeights,
  dayHeight,
  eventEditable,
  hourHeight,
  isoWeekNumber,
  layoutDayEvents,
  lensLaneCalendarIDs,
  localDateKey,
  minutesIntoDay,
  monthPickerWeeks,
  providerName,
  rangeSliceForDay,
  readableTextColor,
  startOfDay,
  startOfMonth,
  startOfWeek,
  timedEventTouchesDay,
  travelMinutes,
  weekDays
} from "./weekModel";
import type { PositionedEvent, TimeRange } from "./weekModel";

/** nowLineInterval keeps the current-time marker roughly honest without
 * re-rendering the whole week every second. */
const nowLineInterval = 60_000;

/** calendarSettingsPath is where the reader chooses which calendars the view
 * lists. */
const calendarSettingsPath = "/settings/account/preferences/calendars";

/** initialScrollHour is where the grid opens. Starting at midnight would put
 * the working day below the fold on every visit. */
const initialScrollHour = 7;

type DialogState = {
  event: CalendarEvent | null;
  day: Date;
  /** prefill seeds a new event with a drawn stretch of time or a copied
   * event's contents. */
  prefill?: EventPrefill | null;
  /** formKey identifies the form's contents. The dialog is remounted whenever
   * it changes, which is how a deliberate reset -- opening a different event,
   * adopting the version that won a conflict -- is expressed, and how a routine
   * reload of the calendars is kept from wiping what the user has typed. */
  formKey: string;
} | null;

/** CalendarView renders one week of every visible calendar. */
export function CalendarView({
  csrf,
  userID,
  location,
  navigate,
  addToast
}: {
  csrf: string;
  userID: number;
  location: LocationState;
  navigate: (url: string) => void;
  addToast: AddToast;
}) {
  const anchor = useMemo(() => startOfWeek(calendarRouteDate(location.path)), [location.path]);
  const days = useMemo(() => weekDays(anchor), [anchor]);
  const [calendars, setCalendars] = useState<CalendarSummary[]>([]);
  const [events, setEvents] = useState<CalendarEvent[]>([]);
  // Events entered while the server could not be reached. They are drawn with
  // the week until the flush in App has sent them; the week reloads when the
  // queue shrinks, which is when the server has them.
  const queued = useCalendarOutbox(userID);
  const pendingEvents = useMemo(() => {
    const copyTargetID = calendars.find((calendar) => calendar.copy_target)?.id || 0;
    const busyTargetID = calendars.find((calendar) => calendar.busy_target)?.id || 0;
    return queued.map((item, index) => pendingCalendarEvent(item, index, copyTargetID, busyTargetID));
  }, [queued, calendars]);
  const shownEvents = useMemo(() => (pendingEvents.length === 0 ? events : [...events, ...pendingEvents]), [events, pendingEvents]);
  const [loading, setLoading] = useState(true);
  // When the week on screen came out of the offline store: the time the copy
  // was saved, 0 while the week is the server's answer.
  const [savedCopyAt, setSavedCopyAt] = useState(0);
  const [syncing, setSyncing] = useState(false);
  const [saving, setSaving] = useState(false);
  const [dialogProblem, setDialogProblem] = useState("");
  const [dialog, setDialog] = useState<DialogState>(null);
  const [now, setNow] = useState(() => new Date());
  // The day the lens is open on, as a date key. null leaves the choice to
  // resolveLensIndex; "" is the reader having closed it, which holds until
  // they open a day again.
  const [lensDayKey, setLensDayKey] = useState<string | null>(null);
  const gridRef = useRef<HTMLDivElement | null>(null);
  const scrolledRef = useRef(false);
  // Each day's column in the time grid and its cell in the all-day bar, in the
  // order of days, which is what a drag measures the pointer against.
  const dayColumnRefs = useRef<(HTMLDivElement | null)[]>([]);
  const allDayCellRefs = useRef<(HTMLDivElement | null)[]>([]);

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
      try {
        const [calendarData, eventData] = await Promise.all([
          what === "all" ? api.calendars() : Promise.resolve(null),
          api.calendarEvents(range.start, range.end)
        ]);
        if (token !== loadTokenRef.current) return;
        if (calendarData) setCalendars(calendarData.calendars || []);
        setEvents(eventData.events || []);
        setSavedCopyAt(0);
        // The weeks around this one, so turning the page offline still answers.
        void api.warmOfflineCalendar(range.start);
      } catch (err) {
        // Only a request the browser could not deliver is answered from the
        // offline store; an answer from the server, whatever it says, is not
        // contradicted by a saved copy. A week the store never saw is the
        // error it always was.
        if (!isNetworkError(err)) throw err;
        const saved = await api.cachedCalendar(range.start, range.end);
        if (token !== loadTokenRef.current) return;
        if (!saved) throw err;
        if (saved.calendars.length > 0 || what === "all") setCalendars(saved.calendars);
        setEvents(saved.events);
        setSavedCopyAt(saved.saved_at);
      }
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

  const previousQueuedRef = useRef(queued.length);
  useEffect(() => {
    if (queued.length < previousQueuedRef.current) {
      void reload("events").catch(() => {
        // The queue shrinking was the news; the next load draws the event.
      });
    }
    previousQueuedRef.current = queued.length;
  }, [queued.length, reload]);

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

  // The busy calendar moves the same way, and takes its placeholder title
  // along: the title is stored on the calendar, and moving the role should not
  // quietly reset what the reader chose to call their blocked time. Becoming
  // the busy calendar also stops a calendar being the second one.
  // label is the title as the picker holds it, which may be a draft whose save
  // has not answered yet; the stored one would carry the title from before it.
  const chooseBusyTarget = async (calendarID: number, label: string) => {
    const previous = calendars.find((calendar) => calendar.busy_target);
    if ((previous?.id || 0) === calendarID) return;
    try {
      const data = calendarID
        ? await api.setCalendarBusyTarget(csrf, calendarID, {
            busy_target: true,
            ...(previous ? { busy_label: label } : {})
          })
        : previous
          ? await api.setCalendarBusyTarget(csrf, previous.id, { busy_target: false })
          : null;
      if (!data) return;
      setCalendars((current) =>
        current.map((item) => (item.id === data.calendar.id ? data.calendar : { ...item, busy_target: false }))
      );
    } catch (err) {
      addToast(messageFromError(err), "error");
    }
  };

  const setBusyLabel = async (calendarID: number, label: string) => {
    try {
      const data = await api.setCalendarBusyTarget(csrf, calendarID, { busy_label: label });
      setCalendars((current) => current.map((item) => (item.id === data.calendar.id ? data.calendar : item)));
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

  // Creating an event is the one write that can be made offline: it names no
  // version to conflict with. A create the browser could not deliver is queued
  // (`lib/calendarOutbox.ts`) and drawn with the week until a connection
  // carries it; opening the waiting event opens this same dialog, and saving it
  // restates the queued one. An edit of an event the server holds is not
  // queued - it would overwrite whatever the calendar has moved on to - and
  // fails with the connection named.
  const saveEvent = (input: CalendarEventInput) =>
    runWrite(async () => {
      const pendingID = queuedLocalID(dialog?.event);
      if (dialog?.event && !pendingID) {
        const data = await api.updateCalendarEvent(csrf, dialog.event.id, input);
        await reload("events");
        closeDialog();
        reportWarnings(data.warnings);
        return;
      }
      if (pendingID) {
        // A waiting event has one sender, the flush in App, and saving it
        // restates what that sender will send. Creating it from here as well
        // raced the flush the moment the connection came back: the dialog and
        // the flush each created it, and the reader got the event twice.
        const restated = await replaceQueuedCalendarEvent(userID, pendingID, input);
        closeDialog();
        if (!restated) {
          // The flush took it while the dialog was open; the reload that the
          // queue shrinking triggers draws it as the server has it.
          addToast("This event has just been added to your calendar. Open it there to change it.", "success");
          return;
        }
        addToast(
          serverReachable()
            ? "Saved. The event is being added to your calendar."
            : "You are offline. The event is kept here and will be added to your calendar when you are back online.",
          "success"
        );
        return;
      }
      try {
        const data = await api.createCalendarEvent(csrf, input);
        await reload("events");
        closeDialog();
        reportWarnings(data.warnings);
      } catch (err) {
        if (!isNetworkError(err)) throw err;
        if ((await enqueueCalendarEvent(userID, input)) === null) {
          throw new Error("You are offline, and too many events are already waiting to be added. Try again once you are connected.");
        }
        closeDialog();
        addToast("You are offline. The event is kept here and will be added to your calendar when you are back online.", "success");
      }
    });

  const deleteEvent = () =>
    runWrite(async () => {
      if (!dialog?.event) return;
      const pendingID = queuedLocalID(dialog.event);
      if (pendingID) {
        await removeQueuedCalendarEvent(userID, pendingID);
        closeDialog();
        addToast("The event was discarded. It had not reached your calendar.", "success");
        return;
      }
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

  // A drag writes as soon as it is let go, the way the dialog's Save does, and
  // the event is drawn at its new place before the provider answers so the
  // block does not jump back under the pointer. Whatever the answer, the week
  // is then reloaded from what the provider holds: a refusal puts the event
  // back where it was, and the copies a linked event keeps in other calendars
  // are only known to have followed once they are read back.
  const moveEvent = async (event: CalendarEvent, range: TimeRange, verb: "moved" | "resized" | "undo") => {
    const original = { start: new Date(event.start_at), end: new Date(event.end_at) };
    if (sameRange(original, range)) return;
    const provider = providerName(calendarsByID.get(event.calendar_id));
    setEvents((current) =>
      current.map((item) =>
        item.id === event.id ? { ...item, start_at: range.start.toISOString(), end_at: range.end.toISOString() } : item
      )
    );
    try {
      const data = await api.updateCalendarEvent(csrf, event.id, inputForEvent(event, range));
      await reload("events").catch(() => {
        // The move landed; a refresh that failed after it only delays the
        // copies being drawn, which the next load does.
      });
      if (data.warnings && data.warnings.length > 0) {
        reportWarnings(data.warnings);
      } else if (verb === "undo") {
        addToast(`Change undone in ${provider} too.`, "success");
      } else {
        addToast(verb === "resized" ? `Event length changed in ${provider} too.` : `Event moved in ${provider} too.`, "success", {
          label: "Undo",
          onUndo: () => void moveEvent(data.event, original, "undo"),
          onCommit: () => undefined
        });
      }
    } catch (err) {
      // Put the block back where it was before asking the provider again: a
      // reload that fails as well must not leave it drawn where it never went.
      setEvents((current) =>
        current.map((item) =>
          item.id === event.id
            ? { ...item, start_at: original.start.toISOString(), end_at: original.end.toISOString() }
            : item
        )
      );
      if (err instanceof ApiError && err.status === 409) {
        const winner = err.payload.event as CalendarEvent | undefined;
        if (winner) setEvents((current) => current.map((item) => (item.id === winner.id ? winner : item)));
      }
      void reload("events").catch(() => {
        // The message below is what the reader acts on.
      });
      addToast(messageFromError(err), "error");
    }
  };

  // A copy is a new event: the same title, place and notes at the new time,
  // in the same calendar, and in the second and busy calendars when the
  // original is kept there too. An event with guests or an online meeting is
  // not created on the spot -- that would send invitations, or book a meeting,
  // on a drop -- but opened as a new event for the reader to look over first.
  const copyEvent = async (event: CalendarEvent, range: TimeRange) => {
    const copyTarget = calendars.find((calendar) => calendar.copy_target && calendar.can_write);
    const busyTarget = calendars.find((calendar) => calendar.busy_target && calendar.can_write);
    const keptIn = (target: CalendarSummary | undefined) =>
      Boolean(target) &&
      target?.id !== event.calendar_id &&
      (event.also_in || []).some((copy) => copy.calendar_id === target?.id);
    const copy = keptIn(copyTarget);
    const busyCopy = keptIn(busyTarget);
    if ((event.attendees || []).length > 0 || event.online_meeting) {
      setDialogProblem("");
      setDialog({
        event: null,
        day: startOfDay(range.start),
        prefill: {
          start: range.start,
          end: range.end,
          allDay: event.all_day,
          calendarID: event.calendar_id,
          summary: event.summary,
          location: event.location,
          description: event.description,
          attendees: event.attendees,
          copy,
          busyCopy,
          onlineMeeting: event.online_meeting,
          travelBefore: travelMinutes(event).before,
          travelAfter: travelMinutes(event).after
        },
        formKey: `copy-${event.id}-${range.start.getTime()}`
      });
      return;
    }
    const provider = providerName(calendarsByID.get(event.calendar_id));
    try {
      const data = await api.createCalendarEvent(csrf, {
        ...inputForEvent(event, range),
        ...travelInputForEvent(event),
        ...(copy ? { copy: true } : {}),
        ...(busyCopy ? { busy_copy: true } : {})
      });
      await reload("events").catch(() => {
        // The copy exists; the next load draws it.
      });
      if (data.warnings && data.warnings.length > 0) {
        reportWarnings(data.warnings);
        return;
      }
      addToast(`Event copied in ${provider} too.`, "success", {
        label: "Undo",
        onUndo: () =>
          void api
            .deleteCalendarEvent(csrf, data.event.id)
            .then((deleted) => {
              reportWarnings(deleted.warnings);
              return reload("events");
            })
            .catch((err) => addToast(messageFromError(err), "error")),
        onCommit: () => undefined
      });
    } catch (err) {
      addToast(messageFromError(err), "error");
    }
  };

  const openNewEvent = (prefill: EventPrefill) => {
    setDialogProblem("");
    setDialog({
      event: null,
      day: startOfDay(prefill.start),
      prefill,
      formKey: `drawn-${prefill.start.getTime()}-${prefill.end.getTime()}`
    });
  };

  const dropDrag = (finished: WeekDrag) => {
    switch (finished.kind) {
      case "create":
        openNewEvent({ ...finished.range, allDay: false, calendarID: finished.calendarID || undefined });
        return;
      case "allday-create":
        openNewEvent({ ...finished.range, allDay: true });
        return;
      case "resize":
        void moveEvent(finished.event, finished.range, "resized");
        return;
      case "move":
      case "allday-move": {
        const original = { start: new Date(finished.event.start_at), end: new Date(finished.event.end_at) };
        // Let go where it started is no change, copy key or not: a copy on top
        // of the original is a double booking nobody meant.
        if (sameRange(original, finished.range)) return;
        if (finished.copy) void copyEvent(finished.event, finished.range);
        else void moveEvent(finished.event, finished.range, "moved");
        return;
      }
    }
  };

  const { drag, beginMove, beginResize, beginCreate, beginAllDayMove, beginAllDayCreate, consumeClick } = useWeekDrag({
    days,
    gridRef,
    dayColumnRefs,
    allDayCellRefs,
    onDrop: dropDrag
  });

  // Only the calendars the reader chose to list take part in the view; the
  // rest are still loaded so a copy mark can name the calendar it stands for.
  const listedCalendars = useMemo(() => calendars.filter((calendar) => calendar.listed), [calendars]);
  const writableCalendars = listedCalendars.filter((calendar) => calendar.can_write);
  const todayKey = localDateKey(now);
  const weekLabel = weekRangeLabel(days[0], days[6]);

  // The day lens, which is how the week is always drawn: one day opened up
  // with a lane per calendar, every other day sized by how much it has at
  // once. Lanes follow the Calendars menu's order. Every day is laid out to
  // size it, so the result is kept until the week, its events or the open day
  // change rather than redone on every render (the now-line ticks once a
  // minute).
  const { lensIndex, lensLanes, dayColumns } = useMemo(() => {
    const index = lensDayKey === "" ? -1 : resolveLensIndex(days, shownEvents, lensDayKey, todayKey);
    const lanes =
      index >= 0
        ? lensLaneCalendarIDs(shownEvents, calendarAccountGroups(listedCalendars).flatMap(([, items]) => items), days[index])
        : [];
    const columns = dayColumnWeights(shownEvents, days, index, lanes.length)
      .map((weight) => `minmax(0, ${weight.toFixed(2)}fr)`)
      .join(" ");
    return { lensIndex: index, lensLanes: lanes, dayColumns: columns };
  }, [lensDayKey, days, shownEvents, todayKey, listedCalendars]);

  const openEvent = (event: CalendarEvent, day: Date) => {
    // The release that ends a drag arrives as a click on the block it ended on.
    if (consumeClick()) return;
    setDialogProblem("");
    setDialog({ event, day, formKey: `event-${event.id}` });
  };

  // One timed entry, placed in a share of its day column: the whole column
  // normally, one calendar's lane of it under the day lens.
  const renderTimedEvent = (placed: PositionedEvent, day: Date, dayIndex: number, lane: number, lanes: number) => {
    const calendar = calendarsByID.get(placed.event.calendar_id);
    const color = calendarColor(calendar);
    const laneWidth = 100 / lanes;
    const width = laneWidth / placed.columns;
    const declined = placed.event.my_response === "declined" || placed.event.status === "cancelled";
    const draggable = eventDraggable(placed.event, calendar);
    const left = `${lane * laneWidth + placed.column * width}%`;
    const blockWidth = `calc(${width}% - 3px)`;
    const source = dragSource(drag, placed.event);
    const tooltip = pendingTooltip(placed.event) || eventTooltip(placed.event, calendar, calendarsByID);
    // Pressing an event never starts drawing a new one underneath it, and
    // its journey is part of it: grabbing the travel moves the event.
    const press = (pointerEvent: ReactPointerEvent) => {
      pointerEvent.stopPropagation();
      if (draggable) beginMove(pointerEvent, placed.event, dayIndex, eventCopyable(placed.event, calendar));
    };
    const travel = (side: "before" | "after", height: number) =>
      height > 0 ? (
        <div
          key={`${placed.event.id}-travel-${side}`}
          className={`calendar-event-travel ${side} ${draggable ? "draggable" : ""} ${source ? "drag-source" : ""}`}
          style={
            {
              top: side === "before" ? placed.top - height : placed.top + placed.height,
              height,
              left,
              width: blockWidth,
              "--travel-color": color
            } as CSSProperties
          }
          title={tooltip}
          aria-hidden="true"
          onPointerDown={press}
          onClick={() => openEvent(placed.event, day)}
        >
          {height >= 14 ? (
            <span className="calendar-event-travel-label">
              <Icon name="car" />
              {formatTravel(side === "before" ? placed.event.travel_before_minutes : placed.event.travel_after_minutes)}
            </span>
          ) : null}
        </div>
      ) : null;
    // One keyed fragment per entry, so React keeps matching the block and its
    // journey by event rather than by position when the day reorders.
    return (
      <Fragment key={placed.event.id}>
        {travel("before", placed.travelBefore)}
        <button
          type="button"
          className={`calendar-event ${declined ? "declined" : ""} ${placed.event.pending_offline ? "pending-offline" : ""} ${
            placed.event.status === "cancelled" ? "cancelled" : ""
          } ${placed.continuesBefore ? "continues-before" : ""} ${placed.continuesAfter ? "continues-after" : ""} ${
            draggable ? "draggable" : ""
          } ${source ? "drag-source" : ""} ${placed.travelBefore > 0 ? "travel-before" : ""} ${
            placed.travelAfter > 0 ? "travel-after" : ""
          } ${placed.event.travel ? "travel-block" : ""}`}
          style={
            {
              top: placed.top,
              height: placed.height,
              left,
              width: blockWidth,
              // A travel block standing alone is drawn hatched, as the journey
              // it is, by its class rather than in its calendar's solid colour.
              background: declined ? "transparent" : placed.event.travel ? undefined : color,
              borderColor: color,
              color: declined || placed.event.travel ? "var(--text)" : readableTextColor(color),
              "--travel-color": color
            } as CSSProperties
          }
          title={tooltip}
          onPointerDown={press}
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
          {draggable && !placed.continuesAfter ? (
            // The end of an event is pulled to change how long it lasts. Only
            // where the end is drawn: a block cut off by midnight ends elsewhere.
            <span
              className="calendar-event-resize"
              aria-hidden="true"
              onPointerDown={(pointerEvent) => {
                pointerEvent.stopPropagation();
                beginResize(pointerEvent, placed.event);
              }}
            />
          ) : null}
        </button>
        {travel("after", placed.travelAfter)}
      </Fragment>
    );
  };

  // Where a drag would leave an appointment, drawn over the day it reaches:
  // in its calendar's lane under the day lens, across the day otherwise.
  const renderTimedGhost = (day: Date, dayIndex: number, lanes: number[]) => {
    if (!drag || (drag.kind !== "move" && drag.kind !== "resize" && drag.kind !== "create")) return null;
    const slice = rangeSliceForDay(drag.range, day);
    if (!slice) return null;
    const calendarID = drag.kind === "create" ? drag.calendarID : drag.event.calendar_id;
    const calendar = calendarID ? calendarsByID.get(calendarID) : undefined;
    // calendarColor falls back to the accent for a drawing with no lane.
    const color = calendarColor(calendar);
    const lane = dayIndex === lensIndex && lanes.length > 1 ? lanes.indexOf(calendarID) : -1;
    const left = lane >= 0 ? (lane * 100) / lanes.length : 0;
    const width = lane >= 0 ? 100 / lanes.length : 100;
    const copying = drag.kind === "move" && drag.copy;
    // The journey moves with the event, so the ghost shows where it lands too.
    const journey = drag.kind === "create" ? { before: 0, after: 0 } : travelMinutes(drag.event);
    const dayStart = startOfDay(day).getTime();
    const dayEnd = addDays(startOfDay(day), 1).getTime();
    const travelBefore =
      drag.range.start.getTime() >= dayStart ? Math.min((journey.before / 60) * hourHeight, slice.top) : 0;
    const travelAfter =
      drag.range.end.getTime() <= dayEnd
        ? Math.min((journey.after / 60) * hourHeight, dayHeight - slice.top - slice.height)
        : 0;
    const ghostTravel = (side: "before" | "after", height: number) =>
      height > 0 ? (
        <div
          key={`ghost-travel-${side}`}
          className={`calendar-event-travel ghost ${side}`}
          style={
            {
              top: side === "before" ? slice.top - height : slice.top + slice.height,
              height,
              left: `${left}%`,
              width: `calc(${width}% - 3px)`,
              "--travel-color": color
            } as CSSProperties
          }
          aria-hidden="true"
        />
      ) : null;
    return [
      ghostTravel("before", travelBefore),
      <div
        key="ghost"
        className={`calendar-drag-ghost ${drag.kind === "create" ? "drawing" : ""} ${
          travelBefore > 0 ? "travel-before" : ""
        } ${travelAfter > 0 ? "travel-after" : ""}`}
        style={{
          top: slice.top,
          height: slice.height,
          left: `${left}%`,
          width: `calc(${width}% - 3px)`,
          background: color,
          borderColor: color,
          color: readableTextColor(color)
        }}
        aria-hidden="true"
      >
        <span className="calendar-event-time">
          {copying ? <Icon name="copy" /> : null}
          {formatRange(drag.range)}
        </span>
        {drag.kind === "create" ? null : (
          <span className="calendar-event-title">{drag.event.summary || "(No title)"}</span>
        )}
      </div>,
      ghostTravel("after", travelAfter)
    ];
  };

  // The lane a press landed in under the day lens: a new event drawn there is
  // offered that lane's calendar first, when the reader may write to it.
  const laneCalendarAt = (dayIndex: number, clientX: number, column: HTMLElement): number => {
    if (dayIndex !== lensIndex || lensLanes.length < 2) return 0;
    const bounds = column.getBoundingClientRect();
    const lane = Math.min(lensLanes.length - 1, Math.max(0, Math.floor(((clientX - bounds.left) / bounds.width) * lensLanes.length)));
    const calendarID = lensLanes[lane];
    return writableCalendars.some((calendar) => calendar.id === calendarID) ? calendarID : 0;
  };

  return (
    <div className="calendar-shell">
      <section
        className={`calendar-main day-lens ${drag ? `dragging drag-${drag.kind}` : ""} ${
          drag && "copy" in drag && drag.copy ? "drag-copy" : ""
        }`}
        style={{ "--calendar-day-columns": dayColumns } as CSSProperties}
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
            <WeekPicker anchor={anchor} label={weekLabel} todayKey={todayKey} navigate={navigate} />
          </div>
          <div className="calendar-head-actions">
            <CalendarPicker
              calendars={listedCalendars}
              hiddenCount={calendars.length - listedCalendars.length}
              onToggle={toggleCalendar}
              onChooseCopyTarget={(calendarID) => void chooseCopyTarget(calendarID)}
              onChooseBusyTarget={(calendarID, label) => void chooseBusyTarget(calendarID, label)}
              onSetBusyLabel={(calendarID, label) => void setBusyLabel(calendarID, label)}
              navigate={navigate}
            />
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

        {savedCopyAt > 0 && !loading ? (
          <p className="calendar-offline-notice" role="status">
            You are offline. Showing the calendar as it was saved on {displayDateTime(new Date(savedCopyAt).toISOString())}; changes need a connection.
          </p>
        ) : null}
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
                          style={{ borderBottomColor: calendarColor(calendar) }}
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
          {days.map((day, dayIndex) => (
            <div
              key={day.toISOString()}
              className="calendar-allday-cell"
              ref={(element) => {
                allDayCellRefs.current[dayIndex] = element;
              }}
              onPointerDown={(pointerEvent) => {
                if (writableCalendars.length === 0 || pointerEvent.target !== pointerEvent.currentTarget) return;
                beginAllDayCreate(pointerEvent, dayIndex);
              }}
            >
              {allDayEventsForDay(shownEvents, day).map((event) => {
                const calendar = calendarsByID.get(event.calendar_id);
                const color = calendarColor(calendar);
                const draggable = eventDraggable(event, calendar);
                return (
                  <button
                    key={`${event.id}-${localDateKey(day)}`}
                    type="button"
                    className={`calendar-chip ${draggable ? "draggable" : ""} ${dragSource(drag, event) ? "drag-source" : ""} ${event.pending_offline ? "pending-offline" : ""}`}
                    style={{ background: color, color: readableTextColor(color) }}
                    title={pendingTooltip(event) || eventTooltip(event, calendar, calendarsByID)}
                    onPointerDown={(pointerEvent) => {
                      pointerEvent.stopPropagation();
                      if (draggable) beginAllDayMove(pointerEvent, event, dayIndex, eventCopyable(event, calendar));
                    }}
                    onClick={() => openEvent(event, day)}
                  >
                    <CopyMarks event={event} calendarsByID={calendarsByID} />
                    {event.summary || "(No title)"}
                  </button>
                );
              })}
              {drag && (drag.kind === "allday-move" || drag.kind === "allday-create") && allDayRangeCoversDay(drag.range, day) ? (
                <div
                  className="calendar-chip calendar-drag-ghost-chip"
                  style={
                    drag.kind === "allday-move"
                      ? {
                          background: calendarColor(calendarsByID.get(drag.event.calendar_id)),
                          color: readableTextColor(calendarColor(calendarsByID.get(drag.event.calendar_id)))
                        }
                      : undefined
                  }
                  aria-hidden="true"
                >
                  {drag.kind === "allday-move" && drag.copy ? <Icon name="copy" /> : null}
                  {drag.kind === "allday-move" ? drag.event.summary || "(No title)" : "New event"}
                </div>
              ) : null}
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
            const dayEvents = shownEvents.filter((event) => !event.all_day && timedEventTouchesDay(event, day));
            const lanes = index === lensIndex ? lensLanes : [];
            return (
              <div
                key={day.toISOString()}
                className={`calendar-day ${isToday ? "today" : ""}`}
                style={{ height: dayHeight }}
                ref={(element) => {
                  dayColumnRefs.current[index] = element;
                }}
                onPointerDown={(pointerEvent) => {
                  // Pressing empty time and pulling draws a new event; the
                  // hour lines and the now line let the press through to here.
                  if (writableCalendars.length === 0 || pointerEvent.target !== pointerEvent.currentTarget) return;
                  beginCreate(
                    pointerEvent,
                    index,
                    laneCalendarAt(index, pointerEvent.clientX, pointerEvent.currentTarget)
                  );
                }}
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
                      ).map((placed) => renderTimedEvent(placed, day, index, lane, lanes.length))
                    ])
                  : layoutDayEvents(dayEvents, day).map((placed) => renderTimedEvent(placed, day, index, 0, 1))}
                {renderTimedGhost(day, index, lanes)}
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
          prefill={dialog.prefill}
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

/** swipeThreshold is how far, in pixels, a horizontal drag across the week
 * picker has to travel before it turns the month. Shorter, and a tap that
 * wobbles turns it; a day cell is about this wide. */
const swipeThreshold = 40;

/** wheelThreshold is how much scrolling over the week picker turns the month
 * once. A trackpad reports a gesture as dozens of small deltas, so they are
 * added up rather than each one turning a month. */
const wheelThreshold = 60;

/** wheelGestureGap is how long, in milliseconds, the wheel has to be quiet
 * before scrolling counts as a new gesture. A trackpad flick keeps reporting
 * deltas through its momentum for hundreds of pixels; without a gap that whole
 * flick would turn a month per threshold rather than once. */
const wheelGestureGap = 200;

/**
 * WeekPicker is the week's date range in the header, and the way to any other
 * week: pressing it opens a month with its weeks, which the reader turns with
 * the arrows, a swipe, the scroll wheel or Page Up/Down, and picking a day
 * opens the week it falls in. The arrows beside it still step one week.
 */
function WeekPicker({
  anchor,
  label,
  todayKey,
  navigate
}: {
  anchor: Date;
  label: string;
  todayKey: string;
  navigate: (url: string) => void;
}) {
  const [open, setOpen] = useState(false);
  const [month, setMonth] = useState(() => startOfMonth(anchor));
  // Which way the last turn went, so the new month slides in from that side.
  const [direction, setDirection] = useState<"next" | "previous" | null>(null);
  const rootRef = useRef<HTMLDivElement | null>(null);
  const buttonRef = useRef<HTMLButtonElement | null>(null);
  const panelRef = useRef<HTMLDivElement | null>(null);
  const swipeRef = useRef<{ x: number; y: number; pointer: number } | null>(null);
  // A swipe ends on a day button more often than not; the click that follows
  // it is the end of the gesture, not a choice of week.
  const swipedRef = useRef(false);
  // The scroll gathered towards a turn, when the wheel last moved, and whether
  // the current gesture has already turned the month.
  const wheelRef = useRef({ total: 0, last: 0, turned: false });

  const weeks = useMemo(() => monthPickerWeeks(month), [month]);
  const anchorKey = localDateKey(anchor);

  // Turning the month remounts the grid, and a day button that had the focus
  // goes with it; the panel takes the focus back so the keyboard stays here.
  useLayoutEffect(() => {
    if (open && document.activeElement === document.body) panelRef.current?.focus();
  }, [month, open]);

  const turn = useCallback((months: number) => {
    setDirection(months > 0 ? "next" : "previous");
    setMonth((current) => startOfMonth(current, months));
  }, []);

  const toggle = () => {
    if (!open) {
      // The picker opens on the month of the week being shown, not wherever
      // it was last left.
      setMonth(startOfMonth(anchor));
      setDirection(null);
    }
    setOpen(!open);
  };

  // A click anywhere else or Escape closes the picker; Page Up and Page Down
  // turn the month while the focus is in the picker, and leave the page's own
  // scrolling alone otherwise. The panel takes focus itself when it is
  // clicked or swiped, so those keys keep working after a swipe.
  useEffect(() => {
    if (!open) return;
    const onPointerDown = (pointerEvent: PointerEvent) => {
      if (rootRef.current && !rootRef.current.contains(pointerEvent.target as Node)) setOpen(false);
    };
    const onKeyDown = (keyEvent: KeyboardEvent) => {
      if (keyEvent.key === "Escape") {
        setOpen(false);
        // Focus inside the panel would otherwise fall back to the page.
        if (rootRef.current?.contains(document.activeElement)) buttonRef.current?.focus();
      }
      if (
        (keyEvent.key === "PageUp" || keyEvent.key === "PageDown") &&
        rootRef.current?.contains(document.activeElement)
      ) {
        keyEvent.preventDefault();
        turn(keyEvent.key === "PageDown" ? 1 : -1);
      }
    };
    document.addEventListener("pointerdown", onPointerDown);
    document.addEventListener("keydown", onKeyDown);
    return () => {
      document.removeEventListener("pointerdown", onPointerDown);
      document.removeEventListener("keydown", onKeyDown);
    };
  }, [open, turn]);

  // The wheel listener is attached by hand because React's is passive, and a
  // scroll that turns the month must not also scroll the page behind it.
  useEffect(() => {
    const panel = panelRef.current;
    if (!open || !panel) return;
    wheelRef.current = { total: 0, last: 0, turned: false };
    const onWheel = (wheelEvent: WheelEvent) => {
      wheelEvent.preventDefault();
      const wheel = wheelRef.current;
      // A pause ends the gesture: what was gathered before it is dropped, so
      // stray deltas a minute apart never add up to a turn.
      if (wheelEvent.timeStamp - wheel.last > wheelGestureGap) {
        wheel.total = 0;
        wheel.turned = false;
      }
      wheel.last = wheelEvent.timeStamp;
      // One gesture turns one month; the rest of it is momentum.
      if (wheel.turned) return;
      const delta = Math.abs(wheelEvent.deltaX) > Math.abs(wheelEvent.deltaY) ? wheelEvent.deltaX : wheelEvent.deltaY;
      // A mouse wheel in line mode reports a few lines, not pixels.
      wheel.total += wheelEvent.deltaMode === 0 ? delta : delta * 20;
      if (Math.abs(wheel.total) < wheelThreshold) return;
      turn(wheel.total > 0 ? 1 : -1);
      wheel.total = 0;
      wheel.turned = true;
    };
    panel.addEventListener("wheel", onWheel, { passive: false });
    return () => panel.removeEventListener("wheel", onWheel);
  }, [open, turn]);

  const choose = (day: Date, clickDetail: number) => {
    // The flag is spent by the first click after a swipe whatever it was. A
    // touch swipe or a drag across days usually produces no click at all, and
    // a flag left standing would swallow the next choice made from the
    // keyboard (a click with no pointer behind it, detail 0).
    const swiped = swipedRef.current;
    swipedRef.current = false;
    if (swiped && clickDetail !== 0) return;
    setOpen(false);
    navigate(calendarURL(day));
  };

  const monthLabel = month.toLocaleDateString(undefined, { month: "long", year: "numeric" });

  return (
    <div className="calendar-week-picker" ref={rootRef}>
      <h1>
        <button
          type="button"
          ref={buttonRef}
          className="calendar-week-picker-button"
          aria-expanded={open}
          aria-haspopup="dialog"
          title="Choose a week"
          onClick={toggle}
        >
          <span>{label}</span>
          <Icon name="expand_more" />
        </button>
      </h1>
      {open ? (
        <div
          className="calendar-week-picker-panel"
          ref={panelRef}
          tabIndex={-1}
          role="dialog"
          aria-label="Choose a week"
          onPointerDown={(pointerEvent) => {
            swipedRef.current = false;
            swipeRef.current = { x: pointerEvent.clientX, y: pointerEvent.clientY, pointer: pointerEvent.pointerId };
          }}
          onPointerUp={(pointerEvent) => {
            const start = swipeRef.current;
            swipeRef.current = null;
            if (!start || start.pointer !== pointerEvent.pointerId) return;
            const dx = pointerEvent.clientX - start.x;
            const dy = pointerEvent.clientY - start.y;
            if (Math.abs(dx) < swipeThreshold || Math.abs(dx) < Math.abs(dy)) return;
            // Dragging the page to the left brings the next month in from the
            // right, the way a phone's pages turn.
            swipedRef.current = true;
            turn(dx < 0 ? 1 : -1);
          }}
          onPointerCancel={() => {
            swipeRef.current = null;
          }}
        >
          <div className="calendar-week-picker-head">
            <button type="button" className="icon-button" title="Previous month" onClick={() => turn(-1)}>
              <Icon name="chevron_left" />
            </button>
            <span className="calendar-week-picker-month" aria-live="polite">
              {monthLabel}
            </span>
            <button type="button" className="icon-button" title="Next month" onClick={() => turn(1)}>
              <Icon name="chevron_right" />
            </button>
          </div>
          <div
            key={localDateKey(month)}
            className={`calendar-week-picker-grid ${direction ? `slide-${direction}` : ""}`}
          >
            <div className="calendar-week-picker-week calendar-week-picker-weekdays" aria-hidden="true">
              <span />
              {weeks[0].map((day) => (
                <span key={day.getDay()}>{day.toLocaleDateString(undefined, { weekday: "narrow" })}</span>
              ))}
            </div>
            {weeks.map((week) => {
              const shown = localDateKey(week[0]) === anchorKey;
              const weekNumber = isoWeekNumber(week[0]);
              return (
                <div key={localDateKey(week[0])} className={`calendar-week-picker-week ${shown ? "shown" : ""}`}>
                  {/* The calendar week (ISO 8601), which opens that week like any of its days. */}
                  <button
                    type="button"
                    className="calendar-week-picker-number"
                    aria-label={`Week ${weekNumber}${shown ? ", shown week" : ""}`}
                    title={`Week ${weekNumber}`}
                    onClick={(clickEvent) => choose(week[0], clickEvent.detail)}
                  >
                    {weekNumber}
                  </button>
                  {week.map((day) => {
                    const key = localDateKey(day);
                    const outside = day.getMonth() !== month.getMonth();
                    return (
                      <button
                        key={key}
                        type="button"
                        className={`calendar-week-picker-day ${outside ? "outside" : ""} ${
                          key === todayKey ? "today" : ""
                        }`}
                        aria-current={key === todayKey ? "date" : undefined}
                        aria-label={`${day.toLocaleDateString(undefined, { dateStyle: "full" })}${
                          shown ? ", shown week" : ""
                        }`}
                        onClick={(clickEvent) => choose(day, clickEvent.detail)}
                      >
                        {day.getDate()}
                      </button>
                    );
                  })}
                </div>
              );
            })}
          </div>
        </div>
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
  onChooseBusyTarget,
  onSetBusyLabel,
  navigate
}: {
  calendars: CalendarSummary[];
  hiddenCount: number;
  onToggle: (calendar: CalendarSummary) => Promise<void>;
  onChooseCopyTarget: (calendarID: number) => void;
  onChooseBusyTarget: (calendarID: number, label: string) => void;
  onSetBusyLabel: (calendarID: number, label: string) => void;
  navigate: (url: string) => void;
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

  // A link inside the menu closes it first, so the menu is not left open over
  // the page it led to when the reader comes back.
  const menuLink = (path: string, icon: "settings" | "add", label: string) => (
    <a
      href={path}
      onClick={(clickEvent) => {
        clickEvent.preventDefault();
        if (menuRef.current) menuRef.current.open = false;
        navigate(path);
      }}
    >
      <Icon name={icon} />
      {label}
    </a>
  );

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
        {writable.length > 1 ? (
          <div className="calendar-sidebar-group calendar-copy-target">
            <label className="calendar-sidebar-account" htmlFor="calendar-busy-target">
              Busy calendar
            </label>
            <select
              id="calendar-busy-target"
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
        ) : null}
        {calendars.length > 0 || hiddenCount > 0 ? (
          <div className="calendar-sidebar-manage">
            {calendars.length === 0 ? (
              <p className="calendar-copy-target-hint">Every calendar is hidden.</p>
            ) : null}
            {menuLink(
              calendarSettingsPath,
              "settings",
              hiddenCount > 0 ? `Manage calendars (${hiddenCount} hidden)` : "Manage calendars"
            )}
          </div>
        ) : (
          // Nothing connected yet: an empty box would say nothing, so the menu
          // says where calendars come from.
          <div className="calendar-picker-empty">
            <p>No calendars yet. Connect an account and allow calendar access.</p>
            {menuLink("/settings/account/google", "add", "Connect a Google account")}
            {menuLink("/settings/account/microsoft", "add", "Connect a Microsoft 365 account")}
          </div>
        )}
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
 * otherwise the day with the most at once -- the day the lens exists for. A
 * week with nothing timed in it has no such day and opens none (-1), rather
 * than widening Monday around an empty column -- which is also what the week
 * looks like while its events are still loading. */
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
  return most > 0 ? busiest : -1;
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
  const journey = travelMinutes(event);
  if (journey.before > 0) parts.push(`${formatTravel(journey.before)} travel there`);
  if (journey.after > 0) parts.push(`${formatTravel(journey.after)} travel back`);
  return parts.join(" — ");
}

/** formatTravel names a journey's length the way a person would say it. */
function formatTravel(minutes: number | undefined): string {
  const total = Math.max(0, Math.round(minutes || 0));
  const hours = Math.floor(total / 60);
  const rest = total % 60;
  if (hours === 0) return `${rest} min`;
  return rest === 0 ? `${hours} h` : `${hours} h ${rest} min`;
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

/** eventDraggable says whether a block in the week can be dragged: the reader
 * may change the event, the way the dialog would let them, and the organizer
 * has not cancelled it -- moving a cancelled meeting reschedules nothing. An
 * event without a title is one the dialog refuses to save and the server
 * refuses to write, so a drag of it could only bounce back. */
function eventDraggable(event: CalendarEvent, calendar: CalendarSummary | undefined): boolean {
  // A waiting event has nothing on the server to move; it is changed through
  // its dialog, which restates the queued one.
  if (event.pending_offline) return false;
  return event.status !== "cancelled" && (event.summary || "").trim() !== "" && eventEditable(event, calendar);
}

/** pendingTooltip is what a waiting event says on hover, or "" for any other. */
function pendingTooltip(event: CalendarEvent): string {
  if (!event.pending_offline) return "";
  return event.pending_error
    ? `Not in your calendar yet: ${pendingReason(event.pending_error)}. Open it to change it or discard it.`
    : "Entered offline. It will be added to your calendar when you are back online.";
}

/** eventCopyable says whether the copy key may copy a dragged event. A
 * placeholder is only the time of an event entered elsewhere, and a copy of it
 * would be a second "Busy" standing for nothing. */
function eventCopyable(event: CalendarEvent, calendar: CalendarSummary | undefined): boolean {
  return !event.link_masked && eventDraggable(event, calendar);
}

/** dragSource says whether an event is the one a drag is taking away from its
 * place, which is drawn faded; a copy leaves the original as it is. */
function dragSource(drag: WeekDrag | null, event: CalendarEvent): boolean {
  if (!drag) return false;
  if (drag.kind === "resize") return drag.event.id === event.id;
  if (drag.kind === "move" || drag.kind === "allday-move") return !drag.copy && drag.event.id === event.id;
  return false;
}

function sameRange(a: TimeRange, b: TimeRange): boolean {
  return a.start.getTime() === b.start.getTime() && a.end.getTime() === b.end.getTime();
}

/** inputForEvent restates an event as the dialog would submit it, at new
 * bounds. Everything else is sent as it stands, so a drag changes the time and
 * nothing more: the backend compares the guest list with the one it holds and
 * leaves it alone when it is the same, and no copy field is sent, so the copies
 * an event keeps in other calendars follow it. */
function inputForEvent(event: CalendarEvent, range: TimeRange): CalendarEventInput {
  return {
    calendar_id: event.calendar_id,
    summary: event.summary,
    description: event.description,
    location: event.location,
    start_at: range.start.toISOString(),
    end_at: range.end.toISOString(),
    all_day: event.all_day,
    time_zone: Intl.DateTimeFormat().resolvedOptions().timeZone || "",
    attendees: (event.attendees || []).map((attendee) => ({
      email: attendee.email,
      name: attendee.name,
      optional: attendee.optional
    })),
    online_meeting: event.online_meeting
  };
}

/** travelInputForEvent restates an event's journey for a copy of it: the copy
 * is the same appointment at another time, and the reader travels to it the
 * same way. A move sends none, so the journey the event has follows it. */
function travelInputForEvent(event: CalendarEvent): Partial<CalendarEventInput> {
  const journey = travelMinutes(event);
  if (journey.before === 0 && journey.after === 0) return {};
  return { travel_before_minutes: journey.before, travel_after_minutes: journey.after };
}

function formatRange(range: TimeRange): string {
  const options: Intl.DateTimeFormatOptions = { hour: "numeric", minute: "2-digit" };
  return `${range.start.toLocaleTimeString(undefined, options)} – ${range.end.toLocaleTimeString(undefined, options)}`;
}
