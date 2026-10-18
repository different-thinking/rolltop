import { describe, expect, it } from "vitest";
import { rowSwipeAvailable } from "./swipeActions";

const media = (coarse: boolean) => (query: string) => ({ matches: coarse && query === "(pointer: coarse)" });

describe("rowSwipeAvailable", () => {
  it("keeps swipes in the native Android shell", () => {
    expect(rowSwipeAvailable(true, {})).toBe(true);
  });

  it("offers swipes to the installed web app and a phone browser", () => {
    expect(rowSwipeAvailable(false, { maxTouchPoints: 5, matchMedia: media(true) })).toBe(true);
    expect(rowSwipeAvailable(false, { maxTouchPoints: 0, matchMedia: media(true) })).toBe(true);
    expect(rowSwipeAvailable(false, { maxTouchPoints: 2, matchMedia: media(false) })).toBe(true);
  });

  it("leaves a mouse-only desktop without them", () => {
    expect(rowSwipeAvailable(false, { maxTouchPoints: 0, matchMedia: media(false) })).toBe(false);
    expect(rowSwipeAvailable(false, {})).toBe(false);
  });
});
