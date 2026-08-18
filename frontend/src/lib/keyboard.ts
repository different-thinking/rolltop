// Shared guards for app-level keyboard shortcuts.

/**
 * editableTarget reports a target that owns the keystroke: a form field or an
 * editable region. isContentEditable is the DOM's own answer, so it covers
 * plaintext-only regions and inherited editability, and correctly says no
 * inside a contenteditable="false" island — none of which a selector matching
 * one attribute value can do. Every app-level shortcut checks this, so how the
 * app marks editable regions is decided in one place.
 */
export function editableTarget(target: EventTarget | null): boolean {
  if (!(target instanceof Element)) return false;
  if (target instanceof HTMLElement && target.isContentEditable) return true;
  return Boolean(target.closest("input, textarea, select"));
}

/**
 * isSendChord reports the composer's send shortcut: Ctrl+Enter, or Cmd+Enter on
 * a Mac. Alt and Shift are excluded so the chord stays one keystroke rather than
 * a family of them.
 *
 * It lives here because two components have to agree on it exactly: the form
 * sends on it, and the recipient field has to recognise it to hand its pending
 * address over first. Two spellings of "belongs to the composer" left chords
 * that one of them claimed and the other did not, which is a keystroke that
 * does nothing at all.
 *
 * The parameter is structural so a React synthetic event satisfies it as well
 * as a DOM one.
 */
export function isSendChord(event: Pick<KeyboardEvent, "key" | "ctrlKey" | "metaKey" | "altKey" | "shiftKey">): boolean {
  return event.key === "Enter" && (event.ctrlKey || event.metaKey) && !event.altKey && !event.shiftKey;
}

// Mail shortcuts stay inactive while a native control or editable region owns
// the keyboard. This keeps typing and button activation predictable. Single-key
// shortcuts additionally stay off anything that activates on a keystroke of its
// own, which the chorded shortcuts have no reason to avoid.
export function shouldIgnoreMailShortcut(event: KeyboardEvent): boolean {
  if (event.defaultPrevented || event.metaKey || event.ctrlKey || event.altKey) return true;
  if (editableTarget(event.target)) return true;
  const target = event.target;
  if (!(target instanceof Element)) return false;
  return Boolean(target.closest("button, a, summary"));
}
