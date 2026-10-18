// File overview: Events entered while the server could not be reached. They
// wait here, per user, until a connection carries them to the calendar.
//
// An event is the one calendar write that can be made offline: it names no
// version to conflict with, so sending it later is the same request as sending
// it now. An edit or a delete is not - both act on the version the week was
// drawn from, and a copy held for hours would overwrite what Google has moved
// on to - so only creation queues. Four properties keep the queue honest.
//
//   - The queue is the reader's own, per user, and is kept through a sign-out:
//     an event entered on the train is not forfeited by signing out before the
//     train reaches a signal. Another user signing in on the browser drops it
//     with everything else (`clearOtherOfflineUsers`).
//   - Order is kept and the flush sends each event at most once per pass: it
//     walks the queue front to back, removes an event the moment the server
//     has answered for it, and stops at the first request the browser could
//     not deliver, so a connection that is still flickering is not asked about
//     the events behind it. The one case it cannot tell apart is a create the
//     server accepted whose answer was lost on the way back: that event is
//     still queued and the next pass sends it again, because a create carries
//     no key the server could recognise it by. Keeping that from making two
//     events needs the server to accept a client-chosen identity for a create.
//   - The flush is the only sender. Saving a waiting event in the dialog
//     restates it here (`replaceQueuedCalendarEvent`) rather than creating it,
//     or the dialog and the flush race each other the moment the connection
//     is back and the reader gets the event twice.
//   - An event the server refused stays, with the refusal on it, and is not
//     sent again until the reader has opened and saved it: a calendar that no
//     longer accepts writes would otherwise be asked about the same event on
//     every flush, and the reader would never see why it is not there.
//   - The set is bounded (`maxQueuedCalendarEvents`); past that the dialog
//     refuses rather than quietly dropping the oldest, because every item is
//     something the reader typed.

import { useEffect, useSyncExternalStore } from "react";
import type { CalendarEvent, CalendarEventInput } from "../types";
import { loadOfflineEntry, saveOfflineEntry } from "./offlineStore";

export type QueuedCalendarEvent = {
  local_id: string;
  created_at: number;
  input: CalendarEventInput;
  /** error is the server's refusal of the last attempt, empty while the event
   * is waiting for a connection. */
  error: string;
};

export const maxQueuedCalendarEvents = 50;
const queueName = "queue";

let activeUserID = 0;
let queue: QueuedCalendarEvent[] = [];
let loading: Promise<void> | null = null;
const listeners = new Set<() => void>();

function notify() {
  listeners.forEach((listener) => listener());
}

function validItem(value: unknown): value is QueuedCalendarEvent {
  if (!value || typeof value !== "object") return false;
  const item = value as Record<string, unknown>;
  return typeof item.local_id === "string" && item.local_id !== "" && typeof item.created_at === "number" &&
    typeof item.error === "string" && Boolean(item.input) && typeof item.input === "object";
}

/** loadCalendarOutbox makes one user's queue the one in memory, reading it from
 * the offline store when the user changed. */
export async function loadCalendarOutbox(userID: number): Promise<QueuedCalendarEvent[]> {
  if (userID <= 0) {
    if (activeUserID !== 0) {
      activeUserID = 0;
      queue = [];
      notify();
    }
    return queue;
  }
  if (activeUserID === userID) {
    if (loading) await loading;
    return queue;
  }
  activeUserID = userID;
  queue = [];
  notify();
  loading = (async () => {
    const stored = await loadOfflineEntry<unknown>(userID, "calendar-outbox", queueName);
    if (activeUserID !== userID) return;
    const items = Array.isArray(stored?.data) ? stored.data.filter(validItem) : [];
    if (items.length > 0) {
      queue = items;
      notify();
    }
  })();
  try {
    await loading;
  } finally {
    loading = null;
  }
  return queue;
}

async function commit(userID: number, next: QueuedCalendarEvent[]): Promise<void> {
  if (activeUserID !== userID) return;
  queue = next;
  notify();
  await saveOfflineEntry(userID, "calendar-outbox", queueName, next);
}

/** calendarOutboxSnapshot is the queue in memory, for the signed-in user. */
export function calendarOutboxSnapshot(): QueuedCalendarEvent[] {
  return queue;
}

export function subscribeCalendarOutbox(listener: () => void): () => void {
  listeners.add(listener);
  return () => {
    listeners.delete(listener);
  };
}

const emptyQueue: QueuedCalendarEvent[] = [];

/** useCalendarOutbox is the signed-in user's queue as React state. */
export function useCalendarOutbox(userID: number): QueuedCalendarEvent[] {
  useEffect(() => {
    void loadCalendarOutbox(userID);
  }, [userID]);
  const snapshot = useSyncExternalStore(subscribeCalendarOutbox, calendarOutboxSnapshot, () => emptyQueue);
  return activeUserID === userID ? snapshot : emptyQueue;
}

function newLocalID(): string {
  return `${Date.now().toString(36)}-${Math.random().toString(36).slice(2, 10)}`;
}

/**
 * enqueueCalendarEvent adds an event to the back of the queue. It answers null
 * when the queue is full, which the dialog reports rather than hiding.
 */
export async function enqueueCalendarEvent(userID: number, input: CalendarEventInput): Promise<QueuedCalendarEvent | null> {
  await loadCalendarOutbox(userID);
  if (activeUserID !== userID || queue.length >= maxQueuedCalendarEvents) return null;
  const item: QueuedCalendarEvent = { local_id: newLocalID(), created_at: Date.now(), input, error: "" };
  await commit(userID, [...queue, item]);
  return item;
}

/** replaceQueuedCalendarEvent restates a waiting event with what the reader
 * saved, and clears a refusal: the reader has answered it. */
export async function replaceQueuedCalendarEvent(userID: number, localID: string, input: CalendarEventInput): Promise<boolean> {
  await loadCalendarOutbox(userID);
  if (activeUserID !== userID || !queue.some((item) => item.local_id === localID)) return false;
  await commit(userID, queue.map((item) => (item.local_id === localID ? { ...item, input, error: "" } : item)));
  return true;
}

export async function removeQueuedCalendarEvent(userID: number, localID: string): Promise<void> {
  await loadCalendarOutbox(userID);
  if (activeUserID !== userID) return;
  const next = queue.filter((item) => item.local_id !== localID);
  if (next.length !== queue.length) await commit(userID, next);
}

export async function markQueuedCalendarEventError(userID: number, localID: string, error: string): Promise<void> {
  await loadCalendarOutbox(userID);
  if (activeUserID !== userID) return;
  await commit(userID, queue.map((item) => (item.local_id === localID ? { ...item, error } : item)));
}

/**
 * flushCalendarOutbox sends the waiting events, in order, through `create`. It
 * stops at the first request that never got an answer and leaves a refused
 * event in place with its reason; see the rules at the top of the file. One
 * flush runs at a time.
 */
let flushing: Promise<{ sent: number; refused: number }> | null = null;

export function flushCalendarOutbox(
  userID: number,
  create: (input: CalendarEventInput) => Promise<unknown>,
  isNetworkFailure: (err: unknown) => boolean,
  describe: (err: unknown) => string
): Promise<{ sent: number; refused: number }> {
  if (flushing) return flushing;
  flushing = (async () => {
    const outcome = { sent: 0, refused: 0 };
    await loadCalendarOutbox(userID);
    const waiting = queue.filter((item) => !item.error);
    for (const item of waiting) {
      // Read again from the queue as it is now: the reader may have opened and
      // re-saved the event since this pass began, and what is sent has to be
      // what they saved last, not what the pass started with.
      const current = activeUserID === userID ? queue.find((queued) => queued.local_id === item.local_id) : undefined;
      if (!current || current.error) continue;
      try {
        await create(current.input);
        await removeQueuedCalendarEvent(userID, item.local_id);
        outcome.sent += 1;
      } catch (err) {
        if (isNetworkFailure(err)) break;
        await markQueuedCalendarEventError(userID, item.local_id, describe(err));
        outcome.refused += 1;
      }
    }
    return outcome;
  })().finally(() => {
    flushing = null;
  });
  return flushing;
}

/**
 * pendingCalendarEvent draws a waiting event the way the week draws any other:
 * an event with a negative id that nothing on the server knows. The copy flags
 * are restated as copies in the second and busy calendars so the dialog's
 * boxes come back ticked the way they were saved.
 */
export function pendingCalendarEvent(item: QueuedCalendarEvent, index: number, copyTargetID = 0, busyTargetID = 0): CalendarEvent {
  const input = item.input;
  const alsoIn: CalendarEvent["also_in"] = [];
  if (input.copy && copyTargetID > 0) alsoIn.push({ calendar_id: copyTargetID, event_id: 0 });
  if (input.busy_copy && busyTargetID > 0) alsoIn.push({ calendar_id: busyTargetID, event_id: 0 });
  return {
    id: -(index + 1),
    calendar_id: input.calendar_id,
    summary: input.summary,
    description: input.description,
    location: input.location,
    status: "confirmed",
    start_at: input.start_at,
    end_at: input.end_at,
    all_day: input.all_day,
    time_zone: input.time_zone,
    recurring_event_id: "",
    organizer_email: "",
    organizer_name: "",
    attendees: (input.attendees || []).map((attendee) => ({
      email: attendee.email,
      name: attendee.name,
      response: "",
      optional: attendee.optional,
      organizer: false,
      self: false,
      resource: false
    })),
    my_response: "",
    html_link: "",
    link_primary: true,
    link_masked: false,
    also_in: alsoIn,
    online_meeting: Boolean(input.online_meeting),
    online_meeting_provider: "",
    online_meeting_url: "",
    travel_before_minutes: input.travel_before_minutes || 0,
    travel_after_minutes: input.travel_after_minutes || 0,
    pending_offline: true,
    pending_local_id: item.local_id,
    pending_error: item.error
  };
}

/** pendingReason is a refusal as it reads inside a sentence: without the full
 * stop the server may have ended it with, so the sentence can add its own. */
export function pendingReason(error: string): string {
  return error.trim().replace(/[.\s]+$/, "");
}

/** queuedLocalID names the waiting event a drawn event stands for, or null for
 * an event the server holds. */
export function queuedLocalID(event: CalendarEvent | null | undefined): string | null {
  return event?.pending_offline && event.pending_local_id ? event.pending_local_id : null;
}

/** resetCalendarOutboxForTests forgets the queue in memory. Tests only. */
export function resetCalendarOutboxForTests(): void {
  activeUserID = 0;
  queue = [];
  loading = null;
  flushing = null;
}
