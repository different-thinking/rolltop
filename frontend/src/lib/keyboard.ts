// Shared guards for app-level keyboard shortcuts.

/**
 * editableTarget reports a target that owns the keystroke: a form field or a
 * rich-text region. Every app-level shortcut checks this, so changing how the
 * app marks editable regions changes all of them at once.
 */
export function editableTarget(target: EventTarget | null): boolean {
  if (!(target instanceof Element)) return false;
  return Boolean(target.closest("input, textarea, select, [contenteditable='true']"));
}

// Mail shortcuts stay inactive while a native control or editable region owns
// the keyboard. This keeps typing and button activation predictable.
export function shouldIgnoreMailShortcut(event: KeyboardEvent): boolean {
  if (event.defaultPrevented || event.metaKey || event.ctrlKey || event.altKey) return true;
  const target = event.target;
  if (!(target instanceof Element)) return false;
  return Boolean(target.closest("input, textarea, select, button, a, summary, [contenteditable='true']"));
}
