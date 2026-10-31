import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { clearFiledMessages, filedMessageDestination, filedMessageTTLMS, recordFiledMessages, releaseFiledMessages, setFiledMessagesUser } from "./filedMessages";

describe("filedMessageDestination", () => {
  beforeEach(() => {
    setFiledMessagesUser(7);
  });
  afterEach(() => {
    vi.useRealTimers();
    clearFiledMessages(7);
    setFiledMessagesUser(0);
  });

  it("names where the reader filed a message until the filing is released", () => {
    expect(filedMessageDestination(41)).toBe(0);
    recordFiledMessages([{ id: 41, toMailboxID: 9 }]);
    expect(filedMessageDestination(41)).toBe(9);
    releaseFiledMessages([41]);
    expect(filedMessageDestination(41)).toBe(0);
  });

  it("names nothing once the filing has aged out", () => {
    vi.useFakeTimers();
    recordFiledMessages([{ id: 42, fromMailboxID: 3, toMailboxID: 9 }]);
    vi.advanceTimersByTime(filedMessageTTLMS + 1);
    expect(filedMessageDestination(42)).toBe(0);
  });
});
