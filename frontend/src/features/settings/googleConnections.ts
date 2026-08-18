// File overview: One owner for the Google connection payload. The settings
// section and both server forms read the same endpoint, and three private copies
// of the shape would drift the moment a field is renamed.

import { getJSON } from "../../api";

/** GoogleContactsSync is the state of one connection's contact sync. It is
 * absent until the connection has granted access to contacts. */
export type GoogleContactsSync = {
  status: string;
  status_detail: string;
  last_sync_at: string;
  last_success_at: string;
  contact_count: number;
  ever_synced: boolean;
};

/** GoogleCalendarSync is the state of one connection's calendar-list sync. It
 * is absent until the connection has granted access to calendars. */
export type GoogleCalendarSync = {
  status: string;
  status_detail: string;
  last_sync_at: string;
  last_success_at: string;
  calendar_count: number;
  ever_synced: boolean;
};

/** GoogleConnection is one Google account this user has authorized. */
export type GoogleConnection = {
  id: number;
  email: string;
  scopes: string[];
  needs_reauth: boolean;
  has_contacts_scope: boolean;
  contacts_sync: GoogleContactsSync | null;
  has_calendar_scope: boolean;
  calendar_sync: GoogleCalendarSync | null;
};

export type GoogleConnectionsResponse = {
  configured: boolean;
  connections: GoogleConnection[];
};

const EMPTY: GoogleConnectionsResponse = { configured: false, connections: [] };

// Cached for the life of the page. The server forms ask for this every time one
// is opened, including on installs that never configured Google, and the answer
// changes only when the user connects or disconnects an account — both of which
// go through this module and clear the cache themselves.
let cached: GoogleConnectionsResponse | null = null;
let inFlight: Promise<GoogleConnectionsResponse> | null = null;

/** loadGoogleConnections fetches the connection list, reusing a cached answer. */
export async function loadGoogleConnections(force = false): Promise<GoogleConnectionsResponse> {
  if (force) {
    forgetGoogleConnections();
    // A request that started before the connection changed would answer with
    // the state from before it, so a forced reload waits it out rather than
    // adopting its result. The forms would otherwise keep offering an account
    // that was just disconnected.
    if (inFlight) {
      try {
        await inFlight;
      } catch {
        // Whatever it failed with, the fresh request below is what counts.
      }
      forgetGoogleConnections();
    }
  }
  if (cached) return cached;
  if (!inFlight) {
    inFlight = getJSON<GoogleConnectionsResponse>("/api/google/connections")
      .then((data) => {
        cached = { configured: Boolean(data.configured), connections: data.connections || [] };
        return cached;
      })
      .finally(() => {
        inFlight = null;
      });
  }
  return inFlight;
}

/** forgetGoogleConnections drops the cache after a connect or disconnect. */
export function forgetGoogleConnections() {
  cached = null;
}

/** emptyGoogleConnections is what a failed load leaves the forms with: the
 * password form still works, so a Google-specific error is not worth a toast on
 * an install that never configured it. */
export function emptyGoogleConnections(): GoogleConnectionsResponse {
  return EMPTY;
}
