// File overview: how a message's stored category is named on screen.
//
// The category a message carries is a stored name ("newsletters"), while the
// sidebar entry that holds it carries the display text ("Newsletters") and the
// icon. The server publishes that registry once, in the chrome payload, so the
// display text is looked up there rather than kept in a second list here that
// could fall behind the classifier.

import type { MailCategorySummary } from "../types";

/** MessageCategoryDisplay is one category as the reader sees it named. */
export type MessageCategoryDisplay = {
  /** The stored name, which is also the view name in /mail/<name>. */
  name: string;
  label: string;
  icon: string;
};

/**
 * messageCategoryDisplay names the category a message was filed into, or null
 * while it has none - classification runs after a message is stored, so an
 * empty category means "not sorted yet" rather than "sorted into nothing".
 *
 * A stored name the chrome payload does not list is still shown, under its own
 * name: an older tab whose chrome predates a new category should say where the
 * message went, not go silent about it.
 */
export function messageCategoryDisplay(
  category: string | undefined,
  categories: readonly MailCategorySummary[]
): MessageCategoryDisplay | null {
  const name = (category || "").trim();
  if (!name) return null;
  const summary = categories.find((entry) => entry.name === name);
  if (!summary) return { name, label: name, icon: "label" };
  return { name, label: summary.label || name, icon: summary.icon || "label" };
}
