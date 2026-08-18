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
