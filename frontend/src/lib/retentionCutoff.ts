// File overview: The "older than" cutoff, in the two shapes the app lets a reader
// name one. Every place that acts on a backlog — archiving older mail, deleting
// older mail, and the stored retention rules — resolves its cutoff here, so the
// day one of them keeps is the day the others keep.

import type { RetentionUnit } from "../types";

/**
 * RelativeCutoff is a cutoff that moves with the calendar: "older than 30 days"
 * means something different tomorrow, which is the point of it. A fixed cutoff
 * is a plain yyyy-mm-dd day instead and stays where it was put.
 */
export type RelativeCutoff = { count: number; unit: RetentionUnit };

/** dateInputValue renders a Date as the yyyy-mm-dd a date input expects, in local time. */
export function dateInputValue(value: Date): string {
  const local = new Date(value.getTime() - value.getTimezoneOffset() * 60_000);
  return local.toISOString().slice(0, 10);
}

/**
 * stepBack moves a date back by whole months or years without letting the day of
 * the month overflow. Stepping the month on a date the earlier month may not
 * have overflows into the next one: 31 May minus three months is 31 February,
 * which JavaScript reads as 3 March. Build the month first, then clamp the day
 * into it.
 */
function stepBack(from: Date, months: number): Date {
  const value = new Date(from.getFullYear(), from.getMonth() - months, 1);
  const lastDayOfMonth = new Date(value.getFullYear(), value.getMonth() + 1, 0).getDate();
  value.setDate(Math.min(from.getDate(), lastDayOfMonth));
  return value;
}

export function monthsAgo(months: number, from: Date = new Date()): Date {
  return stepBack(from, months);
}

export function daysAgo(days: number, from: Date = new Date()): Date {
  // Built from the local calendar day rather than by subtracting milliseconds,
  // so a cutoff spanning a daylight-saving change still lands on midnight of
  // the day it names instead of an hour either side of it.
  return new Date(from.getFullYear(), from.getMonth(), from.getDate() - days);
}

/** relativeCutoffDate resolves a relative cutoff against a moment. */
export function relativeCutoffDate(cutoff: RelativeCutoff, from: Date = new Date()): Date {
  const count = Math.max(1, Math.round(cutoff.count || 0));
  switch (cutoff.unit) {
    case "months":
      return monthsAgo(count, from);
    case "years":
      return monthsAgo(count * 12, from);
    default:
      return daysAgo(count, from);
  }
}

/** relativeCutoffDay resolves a relative cutoff to the yyyy-mm-dd a date input holds. */
export function relativeCutoffDay(cutoff: RelativeCutoff, from: Date = new Date()): string {
  return dateInputValue(relativeCutoffDate(cutoff, from));
}

/**
 * cutoffInstant turns a chosen calendar day into the exact moment the action
 * starts from: local midnight, sent as a timestamp. The reader picks a day in
 * their own calendar, so resolving it as UTC midnight would act on the first
 * hours of the day the dialog promises to keep for anyone east of UTC.
 */
export function cutoffInstant(day: string): string {
  const [year, month, date] = day.split("-").map(Number);
  if (!year || !month || !date) return day;
  return new Date(year, month - 1, date, 0, 0, 0, 0).toISOString();
}

/** displayCutoff renders a chosen day the way the dialogs talk about it. */
export function displayCutoff(value: string): string {
  const parsed = new Date(`${value}T00:00:00`);
  if (Number.isNaN(parsed.getTime())) return value;
  return parsed.toLocaleDateString(undefined, { year: "numeric", month: "long", day: "numeric" });
}

/** relativeCutoffLabel names a relative cutoff in the words the reader chose it by. */
export function relativeCutoffLabel(cutoff: RelativeCutoff): string {
  const count = Math.max(1, Math.round(cutoff.count || 0));
  const unit = cutoff.unit === "months" ? "month" : cutoff.unit === "years" ? "year" : "day";
  return `older than ${count} ${unit}${count === 1 ? "" : "s"}`;
}

/** retentionUnitChoices are the steps a relative cutoff may count in. */
export const retentionUnitChoices: { value: RetentionUnit; label: string }[] = [
  { value: "days", label: "days" },
  { value: "months", label: "months" },
  { value: "years", label: "years" }
];
