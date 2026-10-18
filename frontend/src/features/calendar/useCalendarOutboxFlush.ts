import { useEffect, useRef } from "react";
import { ApiError, api } from "../../api";
import type { AddToast } from "../../appTypes";
import { flushCalendarOutbox, useCalendarOutbox } from "../../lib/calendarOutbox";
import { messageFromError } from "../../lib/errors";
import { isNetworkError, useServerReachable } from "../../lib/offline";

// A refusal is the calendar's word about the event; a gateway answering for a
// backend that is restarting, a session that has expired, a request that timed
// out or was rate-limited is not. Those stop the flush the way a lost
// connection does and are tried again on the next turn, because one such pass
// after a server restart otherwise marked every waiting event as refused and
// left the reader to re-save each one.
function transientFailure(err: unknown): boolean {
  if (isNetworkError(err)) return true;
  return err instanceof ApiError && (err.status >= 500 || err.status === 401 || err.status === 408 || err.status === 429);
}

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
  // The flush being reported on. Every event the running flush sends shrinks
  // `waiting`, which runs this effect again and hands it the same flush; a
  // report attached on each of those turns told the reader about three events
  // three times over. One flush, one report.
  const reportedRef = useRef<Promise<unknown> | null>(null);
  useEffect(() => {
    if (!userID || !csrf || !reachable || waiting === 0) return;
    const flush = flushCalendarOutbox(
      userID,
      (input) => api.createCalendarEvent(csrf, input),
      transientFailure,
      messageFromError
    );
    if (reportedRef.current === flush) return;
    reportedRef.current = flush;
    void flush.then(({ sent, refused }) => {
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
