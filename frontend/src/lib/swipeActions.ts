// Shared action metadata and recurring snooze calculations for touch row swipes.

import type { SwipeAction, SwipePreferences, SwipeSnoozePreset } from "../types";

export const swipeActionChoices: Array<{ value: SwipeAction; label: string }> = [
  { value: "trash", label: "Move to trash" },
  { value: "archive", label: "Archive" },
  { value: "snooze", label: "Snooze" },
  { value: "mark_read", label: "Mark read" },
  { value: "mark_unread", label: "Mark unread" }
];

export const swipeSnoozeChoices: Array<{ value: SwipeSnoozePreset; label: string }> = [
  { value: "later_today", label: "Later today" },
  { value: "tomorrow", label: "Tomorrow morning" },
  { value: "next_week", label: "Next Monday" }
];

/**
 * rowSwipeAvailable says whether the message list should answer a horizontal
 * touch with a row swipe. It used to be the native Android shell alone, which
 * left the installed web app and a phone browser with neither swipes nor the
 * hover toolbar a narrow row hides - no way to act on a row without opening
 * it. Any screen with touch input gets them now; the native shell keeps its
 * own long-press drag, which stays tied to the bridge.
 */
export function rowSwipeAvailable(
  nativeShell: boolean,
  env: { maxTouchPoints?: number; matchMedia?: (query: string) => { matches: boolean } } = typeof window === "undefined"
    ? {}
    : { maxTouchPoints: window.navigator.maxTouchPoints, matchMedia: window.matchMedia?.bind(window) }
): boolean {
  if (nativeShell) return true;
  if ((env.maxTouchPoints || 0) > 0) return true;
  return Boolean(env.matchMedia?.("(pointer: coarse)").matches);
}

export function defaultSwipePreferences(): SwipePreferences {
  return {
    left_action: "snooze",
    left_snooze_preset: "tomorrow",
    right_action: "mark_read",
    right_snooze_preset: "tomorrow",
    archive_mailboxes: []
  };
}

export function swipeSnoozeUntil(preset: SwipeSnoozePreset, now = new Date()): Date {
  switch (preset) {
    case "later_today":
      return now.getHours() < 17 ? atLocalTime(now, 0, 18) : atLocalTime(now, 1, 9);
    case "next_week": {
      const daysUntilMonday = ((8 - now.getDay()) % 7) || 7;
      return atLocalTime(now, daysUntilMonday, 9);
    }
    default:
      return atLocalTime(now, 1, 9);
  }
}

export function swipeActionPresentation(action: SwipeAction): { label: string; icon: string; className: string } {
  switch (action) {
    case "trash":
      return { label: "Trash", icon: "delete", className: "trash" };
    case "archive":
      return { label: "Archive", icon: "archive", className: "archive" };
    case "snooze":
      return { label: "Snooze", icon: "clock", className: "snooze" };
    case "mark_unread":
      return { label: "Unread", icon: "mail", className: "unread" };
    default:
      return { label: "Read", icon: "mail_open", className: "read" };
  }
}

function atLocalTime(base: Date, dayOffset: number, hour: number): Date {
  const value = new Date(base);
  value.setDate(value.getDate() + dayOffset);
  value.setHours(hour, 0, 0, 0);
  return value;
}
