import { describe, expect, it } from "vitest";

import { allMailRoute, contactsIntent, contactsURL, mailRoute, mailRouteView, organizerRoute, organizerURL } from "./routes";

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

describe("organizerRoute", () => {
  it("names each sidebar destination by the path that opens it", () => {
    expect(organizerRoute("/calendar", "calendar")).toBe(true);
    expect(organizerRoute("/contacts", "contacts")).toBe(true);
    expect(organizerRoute("/deliveries", "deliveries")).toBe(true);
    expect(organizerRoute("/invoices", "invoices")).toBe(true);
    expect(organizerURL("invoices")).toBe("/invoices");
  });

  it("does not answer for a neighbour's path", () => {
    expect(organizerRoute("/contacts", "calendar")).toBe(false);
    expect(organizerRoute("/mail/invoices", "invoices")).toBe(false);
  });

  // The calendar owns the days and events below it; the three lists do not
  // serve anything below theirs, so a path there renders the mail list and the
  // sidebar has to agree rather than highlight a view nobody is looking at.
  it("takes the paths below a destination only where the router serves them", () => {
    expect(organizerRoute("/calendar/2026-08-29", "calendar")).toBe(true);
    expect(organizerRoute("/deliveries/7", "deliveries")).toBe(false);
    expect(organizerRoute("/invoices/7", "invoices")).toBe(false);
    expect(organizerRoute("/contacts/7", "contacts")).toBe(false);
    expect(mailRouteView("/calendar/2026-08-29", false)).toBe(false);
    expect(mailRouteView("/deliveries/7", false)).toBe(true);
  });
});

describe("contactsURL", () => {
  it("opens a saved contact by id", () => {
    const url = contactsURL({ contactID: 12, email: "Ann@example.test" });
    expect(url).toBe("/contacts?contact=12");
    expect(contactsIntent(new URL(url, "http://x").search)).toEqual({ contactID: 12, newContact: false, name: "", email: "" });
  });

  it("opens the editor on a new contact carrying the header's name and address", () => {
    const url = contactsURL({ name: "Ann Lee", email: "ann@example.test" });
    expect(url).toBe("/contacts?new=1&name=Ann+Lee&email=ann%40example.test");
    expect(contactsIntent(new URL(url, "http://x").search)).toEqual({ contactID: 0, newContact: true, name: "Ann Lee", email: "ann@example.test" });
  });

  it("reads a plain visit as no intent and a nonsense contact id as none", () => {
    expect(contactsIntent("")).toEqual({ contactID: 0, newContact: false, name: "", email: "" });
    expect(contactsIntent("?contact=abc").contactID).toBe(0);
    expect(contactsIntent("?contact=0x10").contactID).toBe(0);
    expect(organizerRoute("/contacts", "contacts")).toBe(true);
  });
});
