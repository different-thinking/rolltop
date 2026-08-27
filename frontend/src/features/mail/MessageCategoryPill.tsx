// File overview: the chip that names the category a message was filed into.
//
// It is shared rather than written twice because the two places that name a
// message's category - the thread card and the All Mail row - have to agree
// about what an empty category means and what an unknown one is called. The
// stored name is turned into display text in exactly one place already
// (lib/messageCategories); this is the one place that turns it into a chip.

import type { MailCategorySummary } from "../../types";
import { Icon } from "../../components/Icon";
import { messageCategoryDisplay } from "../../lib/messageCategories";

/**
 * MessageCategoryPlace is where the chip is being read, which only changes what
 * its tooltip can honestly claim. A thread card is one message, so the category
 * beside its sender was decided from that message. A list row stands for a whole
 * conversation and carries the newest message's category, so it explains the
 * rule instead of pointing at a message the reader cannot single out.
 */
export type MessageCategoryPlace = "thread" | "list";

// MessageCategoryPill reads as a label rather than a link, because there is no
// list it could honestly open: a category list is the mail still in play
// narrowed to one category, so an archived, snoozed, junked, or trashed message
// is not in the list its own category names, and a pill that opened one would be
// a link to somewhere the message the reader is looking at is not. The sidebar
// entry is one click away for anyone who wants the list itself.
export function MessageCategoryPill({
  category,
  categories,
  place = "thread"
}: {
  category?: string;
  categories: MailCategorySummary[];
  place?: MessageCategoryPlace;
}) {
  // With no category registry in the chrome payload - an older server, or a
  // bootstrap that arrived without one - this frontend knows nothing about
  // categories at all, and neither a bare stored name nor "Not sorted yet"
  // would be an honest thing to say about a message. It says nothing instead.
  if (categories.length === 0) return null;
  const display = messageCategoryDisplay(category, categories);
  // Classification runs after a message is stored, so a message with no
  // category yet says so rather than leaving the line silent: an empty slot
  // reads as "this message has no category", which is never true for long. In a
  // list this is the whole point of the chip - it is the row a reader scanning
  // for unfiled mail is looking for.
  if (!display) {
    return (
      <span className="message-category-pill pending" title="Not sorted into a category yet. Sorting reads each message's own headers and runs shortly after it arrives.">
        <Icon name="clock" />
        <span className="message-category-pill-label">Not sorted yet</span>
      </span>
    );
  }
  const tooltip = place === "list"
    ? `Filed under ${display.label}, decided from the message itself rather than from the folder holding it.`
    : `Filed under ${display.label}, decided from this message itself.`;
  return (
    <span className="message-category-pill" title={tooltip} aria-label={tooltip} role="note">
      <Icon name={display.icon} />
      <span className="message-category-pill-label">{display.label}</span>
    </span>
  );
}
