import { useEffect, useRef } from "react";
import { api } from "../../api";
import type { AddToast } from "../../appTypes";
import { flushCalendarOutbox, useCalendarOutbox } from "../../lib/calendarOutbox";
import { messageFromError } from "../../lib/errors";
import { isNetworkError, useServerReachable } from "../../lib/offline";

/**
 * useCalendarOutboxFlush sends the events entered offline as soon as the
 * server can be reached again. It lives in App rather than in the calendar
 * view, because the reader who entered an appointment on the train and then
 * opened their mail still expects it to arrive; the calendar view only has to
 * draw what is waiting and reload once it has gone.
 */
export function useCalendarOutboxFlush(userID: number, csrf: string, addToast: AddToast): void {
  const reachable = useServerReachable();
  const queued = useCalendarOutbox(userID);
  const waiting = queued.filter((item) => !item.error).length;
  const addToastRef = useRef(addToast);
  addToastRef.current = addToast;
  useEffect(() => {
    if (!userID || !csrf || !reachable || waiting === 0) return;
    void flushCalendarOutbox(
      userID,
      (input) => api.createCalendarEvent(csrf, input),
      isNetworkError,
      messageFromError
    ).then(({ sent, refused }) => {
      if (sent > 0) {
        addToastRef.current(sent === 1 ? "The event you entered offline is in your calendar now." : `${sent} events you entered offline are in your calendar now.`, "success");
      }
      if (refused > 0) {
        addToastRef.current(
          refused === 1
            ? "An event you entered offline was refused by the calendar. Open it in the week to see why."
            : `${refused} events you entered offline were refused by the calendar. Open them in the week to see why.`,
          "error"
        );
      }
    });
  }, [userID, csrf, reachable, waiting]);
}
