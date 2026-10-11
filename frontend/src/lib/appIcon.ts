// File overview: Picks the tab icon for the area the reader is in, so a window
// showing the calendar or the address book can be told apart from one showing
// mail without reading its title.

import { organizerRoute } from "./routes";

/** AppArea is one of the three areas that carry an icon of their own. */
export type AppArea = "mail" | "calendar" | "contacts";

const appIconURLs: Record<AppArea, string> = {
  mail: "/icons/mail.svg",
  calendar: "/icons/calendar.svg",
  contacts: "/icons/contacts.svg"
};

/**
 * appAreaForPath answers through organizerRoute, the one definition of which
 * paths belong to the calendar and the address book, so the icon cannot claim
 * an area the router would not render. Everything else - the lists, a message,
 * search, settings, parcels and invoices - is mail.
 */
export function appAreaForPath(path: string): AppArea {
  if (organizerRoute(path, "calendar")) return "calendar";
  if (organizerRoute(path, "contacts")) return "contacts";
  return "mail";
}

/** appIconURL is the icon for an area. */
export function appIconURL(area: AppArea): string {
  return appIconURLs[area];
}

/**
 * syncDocumentIcon points the page's icon link at the area's icon. It edits the
 * link the shell ships rather than adding one, because browsers disagree about
 * which of several icon links wins.
 */
export function syncDocumentIcon(path: string): void {
  const href = appIconURL(appAreaForPath(path));
  let link = document.querySelector<HTMLLinkElement>('link[rel="icon"]');
  if (!link) {
    link = document.createElement("link");
    link.rel = "icon";
    document.head.appendChild(link);
  }
  if (link.getAttribute("href") !== href) {
    link.type = "image/svg+xml";
    link.setAttribute("href", href);
  }
}
