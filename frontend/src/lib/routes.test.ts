import { describe, expect, it } from "vitest";

import { allMailRoute, mailRoute, mailRouteView, organizerRoute, organizerURL, pluginAppRoute } from "./routes";

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

describe("pluginAppRoute", () => {
  const routes = [{ path: "/files", nested: true }, { path: "/flat" }];

  it("claims a declared page and, when it is nested, the paths below it", () => {
    expect(pluginAppRoute("/files", routes)).toBe(true);
    expect(pluginAppRoute("/files/2026/05", routes)).toBe(true);
    expect(pluginAppRoute("/flat", routes)).toBe(true);
  });

  it("claims nothing else, including a neighbour that starts the same way", () => {
    expect(pluginAppRoute("/flat/child", routes)).toBe(false);
    expect(pluginAppRoute("/filesx", routes)).toBe(false);
    expect(pluginAppRoute("/mail", routes)).toBe(false);
    expect(pluginAppRoute("/files", [])).toBe(false);
    expect(pluginAppRoute("/files")).toBe(false);
  });
});

describe("mailRouteView with plugin pages", () => {
  // The router has to answer this before the plugin module has loaded, from
  // the paths the server declared -- otherwise a deep link paints the mail
  // list first and replaces it a moment later.
  it("stops calling a plugin page mail once the install declares one", () => {
    expect(mailRouteView("/files", false)).toBe(true);
    expect(mailRouteView("/files", false, [{ path: "/files", nested: true }])).toBe(false);
    expect(mailRouteView("/files/2026", false, [{ path: "/files", nested: true }])).toBe(false);
  });

  it("leaves everything else alone", () => {
    const routes = [{ path: "/files", nested: true }];
    expect(mailRouteView("/mail/relevant", false, routes)).toBe(true);
    expect(mailRouteView("/mailbox/12", false, routes)).toBe(true);
    expect(mailRouteView("/deliveries", false, routes)).toBe(false);
  });
});
