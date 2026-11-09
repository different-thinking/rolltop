// File overview: The way the shell's floating action button asks the calendar
// for a new event. The button lives in AppShell, the dialog and everything it
// needs to know -- the day in view, which calendars can be written to -- live in
// CalendarView, so the button only says "a new event, please" and the view
// answers exactly as its own New event button does.

const listeners = new Set<() => void>();

/** requestNewCalendarEvent asks the calendar on screen to open a new event. */
export function requestNewCalendarEvent(): void {
  // Copied first: a listener that unsubscribes itself while it runs must not
  // change the set being iterated.
  for (const listener of Array.from(listeners)) listener();
}

/** onNewCalendarEventRequest subscribes to those requests and returns an
 * unsubscribe. */
export function onNewCalendarEventRequest(listener: () => void): () => void {
  listeners.add(listener);
  return () => {
    listeners.delete(listener);
  };
}
