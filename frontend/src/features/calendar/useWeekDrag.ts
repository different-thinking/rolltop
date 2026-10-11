// File overview: The pointer mechanics behind dragging in the week: moving and
// copying an appointment, pulling its end to change how long it lasts, drawing a
// new one, and the same for the all-day bar. It only measures and previews; what
// a finished drag means is decided by the view, which owns the writes. The
// arithmetic itself lives in weekModel, where it can be tested without a DOM.

import { useCallback, useEffect, useLayoutEffect, useRef, useState } from "react";
import type { PointerEvent as ReactPointerEvent, RefObject } from "react";
import type { CalendarEvent } from "../../types";
import {
  allDayRangeFromLocalDays,
  daysBetween,
  drawnRange,
  hourHeight,
  movedEventTimes,
  resizedEventTimes,
  shiftedAllDayTimes
} from "./weekModel";
import type { TimeRange } from "./weekModel";

/** dragThreshold is how far, in pixels, the pointer has to travel before a
 * press becomes a drag. Below it the press is a click and opens the event, as
 * it always has; a hand never holds a mouse perfectly still. */
const dragThreshold = 4;

/** autoScrollEdge is how close to the top or bottom of the time grid, in
 * pixels, the pointer has to be for the grid to scroll under it -- the only way
 * to drag an appointment to an hour that is not on screen. */
const autoScrollEdge = 36;

/** autoScrollStep is how far the grid scrolls per frame at the very edge. */
const autoScrollStep = 14;

/** WeekDrag is a drag in progress, as the view draws it. */
export type WeekDrag =
  | { kind: "move"; event: CalendarEvent; range: TimeRange; copy: boolean }
  | { kind: "resize"; event: CalendarEvent; range: TimeRange }
  | { kind: "create"; range: TimeRange; calendarID: number }
  | { kind: "allday-move"; event: CalendarEvent; range: TimeRange; copy: boolean }
  | { kind: "allday-create"; range: TimeRange };

type Session = {
  pointerID: number;
  originX: number;
  originY: number;
  /** clientX and clientY are where the pointer last was, which auto-scrolling
   * re-measures against while the hand holds still. */
  clientX: number;
  clientY: number;
  copyKey: boolean;
  started: boolean;
  /** abandoned marks a drag Escape called off while the button was still
   * held: nothing moves any more, and the release that ends it is swallowed
   * rather than opening the event it lands on. */
  abandoned: boolean;
  /** preview turns where the pointer is into the drag to draw, or null while
   * it is somewhere that means nothing (outside every column). */
  preview: (clientX: number, clientY: number, copy: boolean) => WeekDrag | null;
  timed: boolean;
};

/** copyModifier is the key that turns a move into a copy: Ctrl, or Option on a
 * Mac, where Ctrl-click is the context menu and Option-drag is how Finder
 * copies. */
function copyModifier(input: { ctrlKey: boolean; altKey: boolean }): boolean {
  return input.ctrlKey || input.altKey;
}

/** useWeekDrag tracks one drag at a time over the week's columns. days are the
 * week's days; the refs hold each day's column in the time grid and its cell in
 * the all-day bar, in the same order. onDrop receives the last preview of a drag
 * that travelled; a press that never did is left to the click it becomes. */
export function useWeekDrag({
  days,
  gridRef,
  dayColumnRefs,
  allDayCellRefs,
  onDrop
}: {
  days: Date[];
  gridRef: RefObject<HTMLDivElement | null>;
  dayColumnRefs: RefObject<(HTMLDivElement | null)[]>;
  allDayCellRefs: RefObject<(HTMLDivElement | null)[]>;
  onDrop: (drag: WeekDrag) => void;
}) {
  const [drag, setDrag] = useState<WeekDrag | null>(null);
  const sessionRef = useRef<Session | null>(null);
  const dragRef = useRef<WeekDrag | null>(null);
  const frameRef = useRef(0);
  // A drag ends with the pointer let go over something, and the browser turns
  // that release into a click. That click is the end of the drag, not a request
  // to open whatever it landed on.
  const suppressClickRef = useRef(false);
  const onDropRef = useRef(onDrop);
  useLayoutEffect(() => {
    onDropRef.current = onDrop;
  });

  const show = (next: WeekDrag | null) => {
    dragRef.current = next;
    setDrag(next);
  };

  /** columnAt finds the day under a horizontal position. A pointer left or
   * right of the week counts as the first or last day, so a drag that overshoots
   * the edge stays on the week rather than snapping back. */
  const columnAt = useCallback((cells: (HTMLElement | null)[], clientX: number) => {
    let nearest = -1;
    let nearestDistance = Infinity;
    cells.forEach((cell, index) => {
      if (!cell) return;
      const bounds = cell.getBoundingClientRect();
      if (clientX >= bounds.left && clientX < bounds.right) {
        nearest = index;
        nearestDistance = 0;
        return;
      }
      const distance = Math.min(Math.abs(clientX - bounds.left), Math.abs(clientX - bounds.right));
      if (distance < nearestDistance) {
        nearest = index;
        nearestDistance = distance;
      }
    });
    return nearest;
  }, []);

  /** pointInGrid is the day and minute under a pointer in the time grid. */
  const pointInGrid = useCallback(
    (clientX: number, clientY: number): { index: number; minutes: number } | null => {
      const cells = dayColumnRefs.current || [];
      const index = columnAt(cells, clientX);
      const cell = index >= 0 ? cells[index] : null;
      if (!cell) return null;
      const bounds = cell.getBoundingClientRect();
      return { index, minutes: ((clientY - bounds.top) / hourHeight) * 60 };
    },
    [columnAt, dayColumnRefs]
  );

  const stop = useCallback(() => {
    sessionRef.current = null;
    window.cancelAnimationFrame(frameRef.current);
    frameRef.current = 0;
    show(null);
  }, []);

  const refresh = useCallback(() => {
    const session = sessionRef.current;
    if (!session || !session.started || session.abandoned) return;
    show(session.preview(session.clientX, session.clientY, session.copyKey));
  }, []);

  // Scrolls the grid while the pointer rests near its top or bottom edge, and
  // keeps the preview under the pointer as the hours slide past it.
  const autoScroll = useCallback(() => {
    frameRef.current = 0;
    const session = sessionRef.current;
    const grid = gridRef.current;
    if (!session || !session.started || session.abandoned || !session.timed || !grid) return;
    const bounds = grid.getBoundingClientRect();
    let step = 0;
    if (session.clientY < bounds.top + autoScrollEdge) {
      step = -autoScrollStep * Math.min(1, (bounds.top + autoScrollEdge - session.clientY) / autoScrollEdge);
    } else if (session.clientY > bounds.bottom - autoScrollEdge) {
      step = autoScrollStep * Math.min(1, (session.clientY - (bounds.bottom - autoScrollEdge)) / autoScrollEdge);
    }
    if (step === 0) return;
    const before = grid.scrollTop;
    grid.scrollTop += step;
    if (grid.scrollTop === before) return;
    refresh();
    frameRef.current = window.requestAnimationFrame(autoScroll);
  }, [gridRef, refresh]);

  useEffect(() => {
    const onMove = (pointerEvent: PointerEvent) => {
      const session = sessionRef.current;
      if (!session || pointerEvent.pointerId !== session.pointerID) return;
      // A release the window never saw -- a Ctrl-click's context menu on a Mac
      // takes it -- would otherwise leave the drag following a pointer whose
      // button is up, to be dropped wherever the next click happens to land.
      if ((pointerEvent.buttons & 1) === 0) {
        stop();
        return;
      }
      if (session.abandoned) return;
      session.clientX = pointerEvent.clientX;
      session.clientY = pointerEvent.clientY;
      session.copyKey = copyModifier(pointerEvent);
      if (!session.started) {
        const travelled = Math.hypot(pointerEvent.clientX - session.originX, pointerEvent.clientY - session.originY);
        if (travelled < dragThreshold) return;
        session.started = true;
        // Text the press had begun to select would otherwise be dragged along.
        window.getSelection()?.removeAllRanges();
      }
      pointerEvent.preventDefault();
      refresh();
      if (!frameRef.current) frameRef.current = window.requestAnimationFrame(autoScroll);
    };
    const onUp = (pointerEvent: PointerEvent) => {
      const session = sessionRef.current;
      if (!session || pointerEvent.pointerId !== session.pointerID) return;
      const finished = session.started && !session.abandoned ? dragRef.current : null;
      const abandoned = session.abandoned;
      stop();
      if (!finished && !abandoned) return;
      suppressClickRef.current = true;
      // Released over nothing that clicks, there is no click to swallow, and
      // the flag must not wait around for the next real one.
      window.setTimeout(() => {
        suppressClickRef.current = false;
      }, 0);
      if (finished) onDropRef.current(finished);
    };
    const onCancel = (pointerEvent: PointerEvent) => {
      if (sessionRef.current && pointerEvent.pointerId === sessionRef.current.pointerID) stop();
    };
    // The copy key can be pressed or let go while the pointer holds still, and
    // the preview has to say which one a release would do. Escape abandons the
    // drag and leaves everything where it was.
    const onKey = (keyEvent: KeyboardEvent) => {
      const session = sessionRef.current;
      if (!session) return;
      if (session.abandoned) return;
      if (keyEvent.key === "Escape" && keyEvent.type === "keydown") {
        keyEvent.preventDefault();
        session.abandoned = true;
        window.cancelAnimationFrame(frameRef.current);
        frameRef.current = 0;
        show(null);
        return;
      }
      const copy = copyModifier(keyEvent);
      if (copy !== session.copyKey) {
        session.copyKey = copy;
        refresh();
      }
    };
    // Switching to another window mid-drag loses the release; the drag ends
    // with nothing changed rather than following the pointer when it returns.
    const onBlur = () => {
      if (sessionRef.current) stop();
    };
    const onContextMenu = () => {
      if (sessionRef.current) stop();
    };
    window.addEventListener("pointermove", onMove);
    window.addEventListener("pointerup", onUp);
    window.addEventListener("pointercancel", onCancel);
    window.addEventListener("keydown", onKey);
    window.addEventListener("keyup", onKey);
    window.addEventListener("blur", onBlur);
    window.addEventListener("contextmenu", onContextMenu);
    return () => {
      window.removeEventListener("pointermove", onMove);
      window.removeEventListener("pointerup", onUp);
      window.removeEventListener("pointercancel", onCancel);
      window.removeEventListener("keydown", onKey);
      window.removeEventListener("keyup", onKey);
      window.removeEventListener("blur", onBlur);
      window.removeEventListener("contextmenu", onContextMenu);
      window.cancelAnimationFrame(frameRef.current);
    };
  }, [autoScroll, refresh, stop]);

  // A drag belongs to the week it started in; turning the week mid-drag would
  // drop onto days the reader never saw.
  useEffect(() => stop, [days, stop]);

  /** begin arms a drag from a press. Only the primary button of a mouse or a
   * pen starts one: on a touch screen the same gesture scrolls the week, and
   * taking it over would leave a phone unable to reach the evening. */
  const begin = (
    pointerEvent: ReactPointerEvent,
    timed: boolean,
    preview: Session["preview"]
  ) => {
    if (pointerEvent.button !== 0 || pointerEvent.pointerType === "touch") return;
    pointerEvent.stopPropagation();
    sessionRef.current = {
      pointerID: pointerEvent.pointerId,
      originX: pointerEvent.clientX,
      originY: pointerEvent.clientY,
      clientX: pointerEvent.clientX,
      clientY: pointerEvent.clientY,
      copyKey: copyModifier(pointerEvent),
      started: false,
      abandoned: false,
      preview,
      timed
    };
  };

  /** beginMove arms moving (or, with the copy key, copying) a timed event
   * grabbed in the column of days[dayIndex]. */
  const beginMove = (pointerEvent: ReactPointerEvent, event: CalendarEvent, dayIndex: number, canCopy: boolean) => {
    const grab = pointInGrid(pointerEvent.clientX, pointerEvent.clientY);
    if (!grab) return;
    const grabDay = days[dayIndex];
    begin(pointerEvent, true, (clientX, clientY, copy) => {
      const point = pointInGrid(clientX, clientY);
      if (!point) return null;
      const range = movedEventTimes(event, grabDay, grab.minutes, days[point.index], point.minutes);
      return { kind: "move", event, range, copy: copy && canCopy };
    });
  };

  /** beginResize arms pulling a timed event's end. */
  const beginResize = (pointerEvent: ReactPointerEvent, event: CalendarEvent) => {
    begin(pointerEvent, true, (clientX, clientY) => {
      const point = pointInGrid(clientX, clientY);
      if (!point) return null;
      return { kind: "resize", event, range: resizedEventTimes(event, days[point.index], point.minutes) };
    });
  };

  /** beginCreate arms drawing a new event in the column of days[dayIndex].
   * calendarID is the lane pressed in under the day lens, 0 elsewhere. The new
   * event stays on the day it was started on, whichever column the pointer
   * wanders into. */
  const beginCreate = (pointerEvent: ReactPointerEvent, dayIndex: number, calendarID: number) => {
    const anchor = pointInGrid(pointerEvent.clientX, pointerEvent.clientY);
    if (!anchor) return;
    const day = days[dayIndex];
    const column = dayColumnRefs.current?.[dayIndex];
    begin(pointerEvent, true, (_clientX, clientY) => {
      if (!column) return null;
      const minutes = ((clientY - column.getBoundingClientRect().top) / hourHeight) * 60;
      return { kind: "create", range: drawnRange(day, anchor.minutes, minutes), calendarID };
    });
  };

  /** beginAllDayMove arms moving or copying an all-day event by whole days,
   * grabbed in the cell of days[dayIndex]. */
  const beginAllDayMove = (pointerEvent: ReactPointerEvent, event: CalendarEvent, dayIndex: number, canCopy: boolean) => {
    const grabDay = days[dayIndex];
    begin(pointerEvent, false, (clientX, _clientY, copy) => {
      const index = columnAt(allDayCellRefs.current || [], clientX);
      if (index < 0) return null;
      const range = shiftedAllDayTimes(event, daysBetween(grabDay, days[index]));
      return { kind: "allday-move", event, range, copy: copy && canCopy };
    });
  };

  /** beginAllDayCreate arms drawing a new all-day event across the days the
   * pointer passes. */
  const beginAllDayCreate = (pointerEvent: ReactPointerEvent, dayIndex: number) => {
    const first = days[dayIndex];
    begin(pointerEvent, false, (clientX) => {
      const index = columnAt(allDayCellRefs.current || [], clientX);
      if (index < 0) return null;
      return { kind: "allday-create", range: allDayRangeFromLocalDays(first, days[index]) };
    });
  };

  /** consumeClick reports whether a click is the tail of a drag and must be
   * ignored. */
  const consumeClick = () => {
    if (!suppressClickRef.current) return false;
    suppressClickRef.current = false;
    return true;
  };

  return { drag, beginMove, beginResize, beginCreate, beginAllDayMove, beginAllDayCreate, consumeClick };
}
