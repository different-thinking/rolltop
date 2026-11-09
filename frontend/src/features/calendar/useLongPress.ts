// File overview: A long press on empty time in the calendar's grid, for touch
// screens. A mouse draws a new event by pressing and pulling, and double-clicks
// one into place; on a phone both gestures belong to scrolling and zooming, so
// holding a finger still on a free slot is how a new event is made there.

import { useEffect, useRef } from "react";
import type { PointerEvent as ReactPointerEvent } from "react";

/** longPressDelay is how long a finger has to rest before the press counts. */
export const longPressDelay = 500;

/** longPressSlop is how far a finger may wander and still be holding still. A
 * press that travels further is the start of a scroll or a swipe. */
export const longPressSlop = 10;

type Press<T> = {
  pointerID: number;
  x: number;
  y: number;
  timer: number;
  payload: T;
};

/**
 * useLongPress arms a long press from a touch pointerdown and calls onLongPress
 * with the payload once the finger has rested for longPressDelay. Moving,
 * lifting, a second finger, or the browser taking the gesture over for a scroll
 * (pointercancel) all call it off. Mouse and pen presses are ignored: they have
 * their own ways to make an event.
 *
 * Once it has fired, the click and the context menu the release would produce
 * are swallowed: the dialog is already open under the finger, and a click
 * landing on its backdrop would close it again.
 */
export function useLongPress<T>(onLongPress: (payload: T) => void) {
  const pressRef = useRef<Press<T> | null>(null);
  const swallowRef = useRef(false);
  const swallowTimerRef = useRef(0);
  const onLongPressRef = useRef(onLongPress);
  useEffect(() => {
    onLongPressRef.current = onLongPress;
  });

  // Fires a pending press: the timer ran out, or the browser's own long-press
  // gesture (a context menu) came first and is taken over.
  const fireRef = useRef((press: Press<T>) => {
    pressRef.current = null;
    window.clearTimeout(press.timer);
    swallowRef.current = true;
    try {
      navigator.vibrate?.(15);
    } catch {
      // Feedback only; a device without it still gets the dialog.
    }
    onLongPressRef.current(press.payload);
  });

  useEffect(() => {
    const cancel = () => {
      const press = pressRef.current;
      if (!press) return;
      window.clearTimeout(press.timer);
      pressRef.current = null;
    };
    const onMove = (pointerEvent: PointerEvent) => {
      const press = pressRef.current;
      if (!press) return;
      if (pointerEvent.pointerId !== press.pointerID) {
        // A second finger is a pinch, not a press.
        cancel();
        return;
      }
      if (Math.hypot(pointerEvent.clientX - press.x, pointerEvent.clientY - press.y) > longPressSlop) cancel();
    };
    const onEnd = (pointerEvent: PointerEvent) => {
      if (pressRef.current && pointerEvent.pointerId === pressRef.current.pointerID) cancel();
      if (!swallowRef.current) return;
      // The click a release turns into follows it in the same turn of the
      // event loop; past that, the next click is a real one again.
      window.clearTimeout(swallowTimerRef.current);
      swallowTimerRef.current = window.setTimeout(() => {
        swallowRef.current = false;
      }, 350);
    };
    // Any pointer coming down anywhere is news: another finger while a press
    // is pending makes it a pinch, and a fresh touch after one fired means its
    // release has long gone, even if the window never saw it.
    const onDown = (pointerEvent: PointerEvent) => {
      if (pressRef.current && pointerEvent.pointerId !== pressRef.current.pointerID) cancel();
      if (swallowRef.current && !pressRef.current) {
        window.clearTimeout(swallowTimerRef.current);
        swallowRef.current = false;
      }
    };
    const swallow = (event: Event) => {
      // Android's WebView opens a context menu (or starts selecting) on a
      // long press of its own, which may beat the timer: the press is ours.
      if (event.type === "contextmenu" && pressRef.current) fireRef.current(pressRef.current);
      if (!swallowRef.current) return;
      event.preventDefault();
      event.stopPropagation();
      if (event.type === "click") swallowRef.current = false;
    };
    // Captured, so it has run before the pointerdown that arms a new press.
    window.addEventListener("pointerdown", onDown, true);
    window.addEventListener("pointermove", onMove);
    window.addEventListener("pointerup", onEnd);
    window.addEventListener("pointercancel", onEnd);
    window.addEventListener("click", swallow, true);
    window.addEventListener("contextmenu", swallow, true);
    return () => {
      cancel();
      window.clearTimeout(swallowTimerRef.current);
      window.removeEventListener("pointerdown", onDown, true);
      window.removeEventListener("pointermove", onMove);
      window.removeEventListener("pointerup", onEnd);
      window.removeEventListener("pointercancel", onEnd);
      window.removeEventListener("click", swallow, true);
      window.removeEventListener("contextmenu", swallow, true);
    };
  }, []);

  /** begin arms a long press; payload is what onLongPress receives. */
  const begin = (pointerEvent: ReactPointerEvent, payload: T) => {
    if (pointerEvent.pointerType !== "touch") return;
    // A finger that comes down while another is on the screen is not the
    // primary pointer: neither of them is a long press.
    if (pressRef.current || !pointerEvent.isPrimary) {
      if (pressRef.current) window.clearTimeout(pressRef.current.timer);
      pressRef.current = null;
      return;
    }
    const pointerID = pointerEvent.pointerId;
    const timer = window.setTimeout(() => {
      const press = pressRef.current;
      if (!press || press.pointerID !== pointerID) return;
      fireRef.current(press);
    }, longPressDelay);
    pressRef.current = { pointerID, x: pointerEvent.clientX, y: pointerEvent.clientY, timer, payload };
  };

  return { begin };
}
