import { describe, expect, it } from "vitest";

import { allMailRoute, mailRoute } from "./routes";

describe("allMailRoute", () => {
  it("names the unnarrowed list, on its first page and its later ones", () => {
    expect(allMailRoute(mailRoute("/mail"))).toBe(true);
    expect(allMailRoute(mailRoute("/mail/p4"))).toBe(true);
  });

  it("does not name a category, a fixed view, or a folder", () => {
    expect(allMailRoute(mailRoute("/mail/relevant"))).toBe(false);
    expect(allMailRoute(mailRoute("/mail/invoices/p2"))).toBe(false);
    expect(allMailRoute(mailRoute("/mail/inbox"))).toBe(false);
    expect(allMailRoute(mailRoute("/mail/unarchived"))).toBe(false);
    expect(allMailRoute(mailRoute("/mailbox/12"))).toBe(false);
  });

  // A folder id the router cannot use leaves no folder named, and the list that
  // renders is the whole-account one - which is what the rest of the view
  // already calls All Mail, down to the label on its delete confirmation. This
  // agrees with that rather than inventing a fourth answer.
  it("names All Mail for a folder route whose id is unusable", () => {
    expect(mailRoute("/mailbox/0").mailboxID).toBeNull();
    expect(allMailRoute(mailRoute("/mailbox/0"))).toBe(true);
  });
});
