import { describe, expect, it } from "vitest";

import { appAreaForPath, appIconURL } from "./appIcon";

describe("appAreaForPath", () => {
  it("names the calendar for its page and the routes below it", () => {
    expect(appAreaForPath("/calendar")).toBe("calendar");
    expect(appAreaForPath("/calendar/2026-10-12")).toBe("calendar");
  });

  it("names the address book for its page", () => {
    expect(appAreaForPath("/contacts")).toBe("contacts");
  });

  it("treats everything else as mail", () => {
    expect(appAreaForPath("/mail/relevant")).toBe("mail");
    expect(appAreaForPath("/mailbox/4/p2")).toBe("mail");
    expect(appAreaForPath("/settings")).toBe("mail");
    expect(appAreaForPath("/calendarx")).toBe("mail");
  });
});

describe("appIconURL", () => {
  it("gives each area its own icon", () => {
    const urls = new Set([appIconURL("mail"), appIconURL("calendar"), appIconURL("contacts")]);
    expect(urls.size).toBe(3);
  });
});
