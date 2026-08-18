// File overview: Date, time, count, and byte formatting helpers. They apply per-user localization
// preferences while keeping list and thread rendering consistent.

import type { DatePrefs } from "../appTypes";

/** formatBytes renders unknown byte counts as compact human-readable storage text. */
export function formatBytes(value: unknown): string {
  const bytes = typeof value === "number" ? value : Number(value || 0);
  if (!Number.isFinite(bytes) || bytes <= 0) return "0 B";
  const units = ["B", "KB", "MB", "GB", "TB"];
  let amount = bytes;
  let index = 0;
  while (amount >= 1024 && index < units.length - 1) {
    amount /= 1024;
    index++;
  }
  return `${amount >= 10 || index === 0 ? amount.toFixed(0) : amount.toFixed(1)} ${units[index]}`;
}

// Validating a locale tag means constructing an Intl formatter and throwing it
// away, which is the most expensive thing in this file. The answer only depends
// on the tag, so it is remembered: list rendering calls this once per row.
const validatedLocales = new Map<string, string | undefined>();

function dateLocale(prefs?: DatePrefs): string | undefined {
  const locale = prefs?.date_locale?.trim();
  if (!locale) return undefined;
  const cached = validatedLocales.get(locale);
  if (cached !== undefined || validatedLocales.has(locale)) return cached;
  let resolved: string | undefined;
  try {
    Intl.DateTimeFormat(locale);
    resolved = locale;
  } catch {
    resolved = undefined;
  }
  validatedLocales.set(locale, resolved);
  return resolved;
}

function isSameLocalDay(a: Date, b: Date): boolean {
  return a.getFullYear() === b.getFullYear() && a.getMonth() === b.getMonth() && a.getDate() === b.getDate();
}

/** startOfLocalDay truncates to midnight in the reader's own zone. */
function startOfLocalDay(value: Date): Date {
  const day = new Date(value);
  day.setHours(0, 0, 0, 0);
  return day;
}

/**
 * localDayKey identifies the reader's current calendar day. Anything that
 * caches text naming a day relative to today — "Today", "Yesterday" — has to
 * hold this alongside its own inputs, or a list left open past midnight keeps
 * yesterday's wording.
 */
export function localDayKey(now = new Date()): number {
  return startOfLocalDay(now).getTime();
}

function isOlderThanLastYear(date: Date, now = new Date()): boolean {
  const cutoff = startOfLocalDay(now);
  cutoff.setFullYear(cutoff.getFullYear() - 1);
  return date < cutoff;
}

function numericDate(date: Date, prefs?: DatePrefs): string {
  const pad = (value: number) => String(value).padStart(2, "0");
  const mm = pad(date.getMonth() + 1);
  const dd = pad(date.getDate());
  const yy = pad(date.getFullYear() % 100);
  switch (prefs?.date_format) {
    case "dmy":
      return `${dd}/${mm}/${yy}`;
    case "ymd":
      return `${yy}/${mm}/${dd}`;
    case "locale":
      return date.toLocaleDateString(dateLocale(prefs), { year: "2-digit", month: "numeric", day: "numeric" });
    case "mdy":
    default:
      return `${mm}/${dd}/${yy}`;
  }
}

/** displayTime formats list/thread dates according to user preferences and message age. */
export function displayTime(value: string, prefs?: DatePrefs): string {
  if (!value) return "";
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return value;
  const today = new Date();
  const locale = dateLocale(prefs);
  if (isSameLocalDay(date, today)) {
    return date.toLocaleTimeString(locale, { hour: "numeric", minute: "2-digit" });
  }
  if (isOlderThanLastYear(date, today)) {
    return numericDate(date, prefs);
  }
  return date.toLocaleDateString(locale, { month: "short", day: "numeric" });
}

/** displayDateTime renders a full localized date/time for details and settings history. */
export function displayDateTime(value: string, prefs?: DatePrefs): string {
  if (!value) return "";
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return value;
  const locale = dateLocale(prefs);
  if (isOlderThanLastYear(date)) {
    return `${numericDate(date, prefs)} ${date.toLocaleTimeString(locale, { hour: "numeric", minute: "2-digit" })}`;
  }
  return date.toLocaleString(locale, { month: "short", day: "numeric", hour: "numeric", minute: "2-digit" });
}

/** displayLogTimestamp keeps the seconds every other formatter here rounds
 * away. A log tail is read to put events in order, and a burst of lines inside
 * one minute is exactly the case that has to stay orderable. */
export function displayLogTimestamp(value: string, prefs?: DatePrefs): string {
  if (!value) return "";
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return value;
  return date.toLocaleString(dateLocale(prefs), {
    month: "short",
    day: "numeric",
    hour: "2-digit",
    minute: "2-digit",
    second: "2-digit"
  });
}

/** displaySnoozeUntil keeps same-day confirmations compact and adds a localized
 * calendar day when the reminder is later than today. */
export function displaySnoozeUntil(value: string | Date, prefs?: DatePrefs, now = new Date()): string {
  const date = value instanceof Date ? value : new Date(value);
  if (Number.isNaN(date.getTime())) return typeof value === "string" ? value : "";
  const locale = dateLocale(prefs);
  if (isSameLocalDay(date, now)) {
    return new Intl.DateTimeFormat(locale, { hour: "numeric", minute: "2-digit" }).format(date);
  }
  return new Intl.DateTimeFormat(locale, {
    weekday: "short",
    month: "short",
    day: "numeric",
    ...(date.getFullYear() !== now.getFullYear() ? { year: "numeric" as const } : {}),
    hour: "numeric",
    minute: "2-digit"
  }).format(date);
}

/**
 * dateGroupLabel names the date section a list row belongs to, the way Gmail
 * heads its list with "Today" and "Yesterday".
 *
 * The labels are deliberately in English rather than the reader's date locale:
 * they are interface chrome, like "All Mail" and "Snoozed" beside them, and a
 * heading column reading "Today, Yesterday, Oktober" mixes two languages in one
 * list. The row's own timestamp beside it stays fully localized.
 *
 * Every label covers one unbroken run of dates, which is what lets the caller
 * open a section wherever the label changes. That is why the current month is
 * split in two: days after tomorrow carry the month name while days before
 * yesterday read "Earlier this month", so a future-dated message — a skewed
 * Date header, or a reminder set for later this month — cannot make one heading
 * appear twice with Today wedged between the halves. An unparseable date
 * returns "" so the row joins the section above it instead of opening one that
 * cannot be named.
 */
export function dateGroupLabel(value: string, now = new Date()): string {
  if (!value) return "";
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return "";
  const dayDiff = Math.round((startOfLocalDay(date).getTime() - startOfLocalDay(now).getTime()) / 86_400_000);
  if (dayDiff === 0) return "Today";
  if (dayDiff === 1) return "Tomorrow";
  if (dayDiff === -1) return "Yesterday";
  const sameMonth = date.getFullYear() === now.getFullYear() && date.getMonth() === now.getMonth();
  if (sameMonth && dayDiff < -1) return "Earlier this month";
  const month = date.toLocaleDateString("en", { month: "long" });
  return date.getFullYear() === now.getFullYear() ? month : `${month} ${date.getFullYear()}`;
}

/** messageCountLabel renders "1 message" / "N messages" for move/copy/delete toasts. */
export function messageCountLabel(count: number): string {
  return `${count.toLocaleString()} ${count === 1 ? "message" : "messages"}`;
}
