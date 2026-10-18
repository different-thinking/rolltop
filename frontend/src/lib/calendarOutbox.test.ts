import { afterEach, beforeEach, describe, expect, it } from "vitest";
import type { CalendarEventInput } from "../types";
import {
  calendarOutboxSnapshot,
  enqueueCalendarEvent,
  flushCalendarOutbox,
  loadCalendarOutbox,
  markQueuedCalendarEventError,
  maxQueuedCalendarEvents,
  pendingCalendarEvent,
  queuedLocalID,
  removeQueuedCalendarEvent,
  replaceQueuedCalendarEvent,
  resetCalendarOutboxForTests
} from "./calendarOutbox";
import { useMemoryOfflineStoreForTests } from "./offlineStore";

function input(summary: string): CalendarEventInput {
  return {
    calendar_id: 3,
    summary,
    description: "",
    location: "",
    start_at: "2026-10-20T09:00:00.000Z",
    end_at: "2026-10-20T10:00:00.000Z",
    all_day: false,
    time_zone: "Europe/Berlin",
    attendees: []
  };
}

const networkError = () => new TypeError("Failed to fetch");

describe("calendarOutbox", () => {
  let restore: () => void;
  beforeEach(() => {
    restore = useMemoryOfflineStoreForTests();
    resetCalendarOutboxForTests();
  });
  afterEach(() => {
    resetCalendarOutboxForTests();
    restore();
  });

  it("queues in order, survives a reload of the queue and is one user's own", async () => {
    await enqueueCalendarEvent(1, input("first"));
    await enqueueCalendarEvent(1, input("second"));
    expect(calendarOutboxSnapshot().map((item) => item.input.summary)).toEqual(["first", "second"]);

    resetCalendarOutboxForTests();
    expect((await loadCalendarOutbox(1)).map((item) => item.input.summary)).toEqual(["first", "second"]);
    expect(await loadCalendarOutbox(2)).toEqual([]);
  });

  it("refuses past the bound instead of dropping the oldest", async () => {
    for (let index = 0; index < maxQueuedCalendarEvents; index += 1) {
      expect(await enqueueCalendarEvent(1, input(`event ${index}`))).not.toBeNull();
    }
    expect(await enqueueCalendarEvent(1, input("one too many"))).toBeNull();
    expect(calendarOutboxSnapshot()[0].input.summary).toBe("event 0");
  });

  it("sends in order, removes what the server took and stops at a lost connection", async () => {
    await enqueueCalendarEvent(1, input("a"));
    await enqueueCalendarEvent(1, input("b"));
    await enqueueCalendarEvent(1, input("c"));
    const sent: string[] = [];
    const outcome = await flushCalendarOutbox(1, async (item) => {
      if (item.summary === "b") throw networkError();
      sent.push(item.summary);
    }, (err) => err instanceof TypeError, String);
    expect(sent).toEqual(["a"]);
    expect(outcome).toEqual({ sent: 1, refused: 0 });
    expect(calendarOutboxSnapshot().map((item) => item.input.summary)).toEqual(["b", "c"]);
  });

  it("keeps a refused event with its reason and does not send it again until it is re-saved", async () => {
    const refused = await enqueueCalendarEvent(1, input("refused"));
    await enqueueCalendarEvent(1, input("fine"));
    const attempts: string[] = [];
    const create = async (item: CalendarEventInput) => {
      attempts.push(item.summary);
      if (item.summary === "refused") throw new Error("Calendar is read-only");
    };
    expect(await flushCalendarOutbox(1, create, () => false, (err) => (err as Error).message)).toEqual({ sent: 1, refused: 1 });
    expect(calendarOutboxSnapshot()).toHaveLength(1);
    expect(calendarOutboxSnapshot()[0].error).toBe("Calendar is read-only");

    attempts.length = 0;
    expect(await flushCalendarOutbox(1, create, () => false, String)).toEqual({ sent: 0, refused: 0 });
    expect(attempts).toEqual([]);

    expect(await replaceQueuedCalendarEvent(1, refused!.local_id, input("fixed"))).toBe(true);
    expect(calendarOutboxSnapshot()[0].error).toBe("");
    expect(await flushCalendarOutbox(1, create, () => false, String)).toEqual({ sent: 1, refused: 0 });
    expect(calendarOutboxSnapshot()).toEqual([]);
  });

  it("a sign-in during another user's flush starts a flush of its own, and the first settles for its own user", async () => {
    await enqueueCalendarEvent(1, input("theirs"));
    let release: () => void = () => undefined;
    const blocked = new Promise<void>((resolve) => {
      release = resolve;
    });
    let started: () => void = () => undefined;
    const createStarted = new Promise<void>((resolve) => {
      started = resolve;
    });
    const first = flushCalendarOutbox(1, () => {
      started();
      return blocked;
    }, () => false, String);
    // The first user's request is on the wire when the second user signs in.
    await createStarted;
    await enqueueCalendarEvent(2, input("mine"));
    const sent: string[] = [];
    const second = flushCalendarOutbox(2, async (item) => {
      sent.push(item.summary);
    }, () => false, String);
    expect(second).not.toBe(first);
    expect(await second).toEqual({ sent: 1, refused: 0 });
    expect(sent).toEqual(["mine"]);

    release();
    expect(await first).toEqual({ sent: 1, refused: 0 });
    // The event the first flush sent is gone from its user's stored queue, and
    // the signed-in user has not been switched back.
    expect(calendarOutboxSnapshot()).toEqual([]);
    resetCalendarOutboxForTests();
    expect(await loadCalendarOutbox(1)).toEqual([]);
  });

  it("a load for the next user is not cut short by the previous user's load finishing", async () => {
    await enqueueCalendarEvent(2, input("two"));
    resetCalendarOutboxForTests();
    const firstLoad = loadCalendarOutbox(1);
    const secondLoad = loadCalendarOutbox(2);
    await firstLoad;
    expect((await loadCalendarOutbox(2)).map((item) => item.input.summary)).toEqual(["two"]);
    await secondLoad;
  });

  it("refuses a re-save while that event's create is on the wire", async () => {
    const item = await enqueueCalendarEvent(1, input("original"));
    let release: () => void = () => undefined;
    const blocked = new Promise<void>((resolve) => {
      release = resolve;
    });
    let started: () => void = () => undefined;
    const createStarted = new Promise<void>((resolve) => {
      started = resolve;
    });
    const sent: string[] = [];
    const flush = flushCalendarOutbox(1, async (sentInput) => {
      sent.push(sentInput.summary);
      started();
      await blocked;
    }, () => false, String);
    await createStarted;
    expect(await replaceQueuedCalendarEvent(1, item!.local_id, input("edited"))).toBe(false);
    release();
    expect(await flush).toEqual({ sent: 1, refused: 0 });
    expect(sent).toEqual(["original"]);
    expect(calendarOutboxSnapshot()).toEqual([]);
  });

  it("removes and marks by local id", async () => {
    const item = await enqueueCalendarEvent(1, input("x"));
    await markQueuedCalendarEventError(1, item!.local_id, "no");
    expect(calendarOutboxSnapshot()[0].error).toBe("no");
    await removeQueuedCalendarEvent(1, "unknown");
    expect(calendarOutboxSnapshot()).toHaveLength(1);
    await removeQueuedCalendarEvent(1, item!.local_id);
    expect(calendarOutboxSnapshot()).toEqual([]);
  });

  it("draws a waiting event with a negative id, its copies and its error", () => {
    const item = { local_id: "q1", created_at: 1, error: "nope", input: { ...input("draw"), copy: true, busy_copy: true, attendees: [{ email: "a@b.c", name: "A", optional: true }] } };
    const event = pendingCalendarEvent(item, 2, 7, 8);
    expect(event.id).toBe(-3);
    expect(event.calendar_id).toBe(3);
    expect(event.also_in.map((copy) => copy.calendar_id)).toEqual([7, 8]);
    expect(event.attendees[0]).toMatchObject({ email: "a@b.c", optional: true, self: false });
    expect(event.pending_offline).toBe(true);
    expect(event.pending_error).toBe("nope");
    expect(queuedLocalID(event)).toBe("q1");
    expect(queuedLocalID({ ...event, pending_offline: false })).toBeNull();
    expect(pendingCalendarEvent({ ...item, input: input("plain") }, 0).also_in).toEqual([]);
  });
});
