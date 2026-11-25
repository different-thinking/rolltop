// File overview: Where a pushed route starts scrolled, and that returning to a
// list from the message opened in it restores the list's own position.

import { afterEach, describe, expect, it } from "vitest";
import { messageURL } from "./routes";
import { rememberScroll, rememberScrollLeaving, rememberedScroll, resetScrollMemoryForTest, scrollAfterNavigation } from "./scrollMemory";

function at(url: string) {
  const parsed = new URL(url, "http://rolltop.test");
  return { path: parsed.pathname, search: parsed.search };
}

afterEach(() => resetScrollMemoryForTest());

describe("scrollAfterNavigation", () => {
  it("returns to the list where it was left, not where the message was scrolled to", () => {
    const list = "/mail/inbox?page=2";
    rememberScroll(list, 1840);
    const message = at(messageURL(42, "", [], list));
    // Back, Delete and Archive all push the message's `back`.
    expect(scrollAfterNavigation(message, at(list))).toBe(1840);
  });

  it("remembers the list under the back the opened message carries", () => {
    // The address bar says /mail; the list spells its own return as the inbox.
    const message = at(messageURL(42, "", [], "/mail/inbox?page=1"));
    rememberScrollLeaving(at("/mail"), message, 620);
    expect(scrollAfterNavigation(message, at("/mail/inbox?page=1"))).toBe(620);
  });

  it("starts a list it has no memory of at the top", () => {
    const list = "/mail/inbox";
    expect(scrollAfterNavigation(at(messageURL(42, "", [], list)), at(list))).toBe(0);
  });

  it("starts an opened message at the top", () => {
    expect(scrollAfterNavigation(at("/mail/inbox"), at(messageURL(42, "", [], "/mail/inbox")))).toBe(0);
  });

  it("does not restore a remembered list reached some other way", () => {
    rememberScroll("/mail/archive", 900);
    expect(scrollAfterNavigation(at(messageURL(42, "", [], "/mail/inbox")), at("/mail/archive"))).toBe(0);
  });

  it("leaves the page alone when only the query of the same view changes", () => {
    expect(scrollAfterNavigation(at("/calendar"), at("/calendar?remind=7"))).toBeNull();
  });

  it("forgets the oldest routes beyond its bound", () => {
    for (let index = 0; index < 60; index += 1) rememberScroll(`/mail/f${index}`, index);
    expect(rememberedScroll("/mail/f0")).toBeUndefined();
    expect(rememberedScroll("/mail/f59")).toBe(59);
  });
});
