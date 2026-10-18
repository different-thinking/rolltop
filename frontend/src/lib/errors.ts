// File overview: Error-message extraction helpers for user-visible toast and panel failures.

import { ApiError } from "../api";
import { isNetworkError, noConnectionMessage } from "./offline";

/** messageFromError extracts a useful user-facing message from thrown API or runtime errors. */
export function messageFromError(err: unknown): string {
  if (err instanceof ApiError) return err.message;
  // A request that never got an answer used to surface as the browser's own
  // "Failed to fetch", which says nothing a reader can act on. The connection
  // is the reason, so the connection is what is named: the browser's flag when
  // it knows, and the server when the flag claims a connection the request did
  // not find.
  if (isNetworkError(err)) {
    return typeof navigator !== "undefined" && navigator.onLine === false ? noConnectionMessage : "The server could not be reached.";
  }
  if (err instanceof Error) return err.message;
  return "Something went wrong.";
}
