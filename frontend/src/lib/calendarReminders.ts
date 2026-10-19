// File overview: Wording for a calendar reminder shown as a browser
// notification in this tab, when the browser has no Web Push and the reminder
// arrives over the feed instead. The service worker renders the push payload
// with the same rules in `public/sw.js`, which is plain JavaScript and cannot
// import this; keep the two in step.

import type { CalendarReminderNotification } from "../types";

/** reminderLead says how far off the event is, from the reminder's own offset. */
export function reminderLead(minutesBefore: number, allDay: boolean): string {
  if (allDay) {
    if (minutesBefore <= 0) return "Today";
    if (minutesBefore <= 24 * 60) return "Tomorrow";
    return `In ${Math.ceil(minutesBefore / (24 * 60))} days`;
  }
  if (minutesBefore < 0) return `Started ${-minutesBefore} minutes ago`;
  if (minutesBefore === 0) return "Starting now";
  if (minutesBefore < 60) return `Starts in ${minutesBefore} minutes`;
  if (minutesBefore % (24 * 60) === 0) {
    const days = minutesBefore / (24 * 60);
    return days === 1 ? "Starts tomorrow" : `Starts in ${days} days`;
  }
  if (minutesBefore % 60 === 0) {
    const hours = minutesBefore / 60;
    return hours === 1 ? "Starts in 1 hour" : `Starts in ${hours} hours`;
  }
  return `Starts in ${minutesBefore} minutes`;
}

/** reminderTimeRange is the event's time in the reader's own zone and locale. */
export function reminderTimeRange(startAt: string, endAt: string, allDay: boolean): string {
  if (allDay) return "All day";
  const start = new Date(startAt);
  const end = new Date(endAt);
  if (Number.isNaN(start.getTime())) return "";
  const options: Intl.DateTimeFormatOptions = { hour: "2-digit", minute: "2-digit" };
  const from = start.toLocaleTimeString([], options);
  if (Number.isNaN(end.getTime()) || end.getTime() <= start.getTime()) return from;
  return `${from}–${end.toLocaleTimeString([], options)}`;
}

/** reminderBody is the notification's second line: when, then where. */
export function reminderBody(reminder: Pick<CalendarReminderNotification, "start_at" | "end_at" | "all_day" | "minutes_before" | "location">): string {
  const parts = [reminderLead(reminder.minutes_before, reminder.all_day), reminderTimeRange(reminder.start_at, reminder.end_at, reminder.all_day)];
  if (reminder.location.trim()) parts.push(reminder.location.trim());
  return parts.filter(Boolean).join(" · ");
}

/** reminderLabel names an event's reminders for the dialog: "10 minutes before". */
export function reminderLabel(minutes: number[]): string {
  if (minutes.length === 0) return "None";
  return minutes.map((value) => {
    if (value === 0) return "At start";
    if (value % (24 * 60) === 0) {
      const days = value / (24 * 60);
      return days === 1 ? "1 day before" : `${days} days before`;
    }
    if (value % 60 === 0) {
      const hours = value / 60;
      return hours === 1 ? "1 hour before" : `${hours} hours before`;
    }
    return `${value} minutes before`;
  }).join(", ");
}

/** ReminderSnoozeChoice is one time a reminder can be put off to. */
export type ReminderSnoozeChoice = { label: string; at: Date };

/**
 * reminderSnoozeChoices offers the times a reminder can be put off to: a few
 * minutes from now, and the moments of the event itself that are still ahead.
 * Only times ahead of now are offered; the picker's own time field covers the
 * rest.
 */
export function reminderSnoozeChoices(startAt: string, allDay: boolean, now = new Date()): ReminderSnoozeChoice[] {
  const choices: ReminderSnoozeChoice[] = [
    { label: "In 5 minutes", at: new Date(now.getTime() + 5 * 60_000) },
    { label: "In 15 minutes", at: new Date(now.getTime() + 15 * 60_000) },
    { label: "In 1 hour", at: new Date(now.getTime() + 60 * 60_000) }
  ];
  const start = new Date(startAt);
  if (!allDay && !Number.isNaN(start.getTime())) {
    choices.push({ label: "5 minutes before it starts", at: new Date(start.getTime() - 5 * 60_000) });
    choices.push({ label: "When it starts", at: start });
  }
  return choices
    .filter((choice) => choice.at.getTime() > now.getTime() + 30_000)
    .sort((left, right) => left.at.getTime() - right.at.getTime());
}
