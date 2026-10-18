import { afterEach, beforeEach, describe, expect, it } from "vitest";
import {
  clearOfflineUser,
  clearOtherOfflineUsers,
  deleteOfflineEntries,
  evictionKeys,
  listOfflineEntries,
  loadOfflineEntry,
  saveOfflineEntry,
  useMemoryOfflineStoreForTests,
  type OfflineEntryMeta
} from "./offlineStore";

function meta(key: string, savedAt: number, bytes: number): OfflineEntryMeta {
  return { key, user_id: 1, kind: "message", name: key, saved_at: savedAt, bytes };
}

describe("offlineStore", () => {
  let restore: () => void;
  beforeEach(() => {
    restore = useMemoryOfflineStoreForTests();
  });
  afterEach(() => restore());

  it("reads back only the user that stored an entry", async () => {
    expect(await saveOfflineEntry(1, "message", "42", { subject: "mine" })).toBe(true);
    expect((await loadOfflineEntry(1, "message", "42"))?.data).toEqual({ subject: "mine" });
    expect(await loadOfflineEntry(2, "message", "42")).toBeNull();
    expect(await loadOfflineEntry(1, "calendars", "42")).toBeNull();
  });

  it("refuses entries without a user or a name", async () => {
    expect(await saveOfflineEntry(0, "message", "1", {})).toBe(false);
    expect(await saveOfflineEntry(1, "message", "", {})).toBe(false);
    expect(await loadOfflineEntry(0, "message", "1")).toBeNull();
  });

  it("drops a user's entries on sign-out and the other users' on sign-in", async () => {
    await saveOfflineEntry(1, "message", "1", "a");
    await saveOfflineEntry(1, "calendars", "list", "b");
    await saveOfflineEntry(2, "message", "1", "c");
    await clearOfflineUser(1);
    expect(await loadOfflineEntry(1, "message", "1")).toBeNull();
    expect(await loadOfflineEntry(1, "calendars", "list")).toBeNull();
    expect((await loadOfflineEntry(2, "message", "1"))?.data).toBe("c");

    await saveOfflineEntry(3, "message", "7", "d");
    await clearOtherOfflineUsers(3);
    expect(await loadOfflineEntry(2, "message", "1")).toBeNull();
    expect((await loadOfflineEntry(3, "message", "7"))?.data).toBe("d");
  });

  it("deletes named entries and lists what is left", async () => {
    await saveOfflineEntry(1, "message", "1", "a");
    await saveOfflineEntry(1, "message", "2", "b");
    await deleteOfflineEntries(1, "message", ["1", "9"]);
    expect((await listOfflineEntries(1, "message")).map((entry) => entry.name)).toEqual(["2"]);
  });

  it("keeps only the newest calendar entry", async () => {
    await saveOfflineEntry(1, "calendars", "old", ["x"]);
    await saveOfflineEntry(1, "calendars", "new", ["y"]);
    expect(await loadOfflineEntry(1, "calendars", "old")).toBeNull();
    expect((await loadOfflineEntry(1, "calendars", "new"))?.data).toEqual(["y"]);
  });

  it("refuses a single entry larger than the whole budget would tolerate", async () => {
    const huge = "x".repeat(6 * 1024 * 1024 + 10);
    expect(await saveOfflineEntry(1, "message", "big", huge)).toBe(false);
    expect(await loadOfflineEntry(1, "message", "big")).toBeNull();
  });
});

describe("evictionKeys", () => {
  it("keeps the entry just written and the newest of the rest within the count", () => {
    const entries = [meta("old", 1, 10), meta("mid", 2, 10), meta("new", 3, 10), meta("written", 0, 10)];
    expect(evictionKeys(entries, { entries: 2, bytes: 1000 }, "written").sort()).toEqual(["mid", "old"]);
  });

  it("evicts by bytes once the retained total passes the budget", () => {
    const entries = [meta("a", 3, 60), meta("b", 2, 60), meta("c", 1, 60)];
    expect(evictionKeys(entries, { entries: 10, bytes: 100 }, "a")).toEqual(["b", "c"]);
  });

  it("never evicts the entry just written even when it alone exceeds the budget", () => {
    const entries = [meta("written", 0, 500), meta("other", 9, 10)];
    expect(evictionKeys(entries, { entries: 10, bytes: 100 }, "written")).toEqual(["other"]);
  });
});
