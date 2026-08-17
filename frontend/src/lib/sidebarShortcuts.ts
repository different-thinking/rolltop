// File overview: Ctrl+Shift+<number> navigation for the sidebar's top-level
// lists, and the badge hint that appears while both modifiers are held.

import { useEffect, useRef, useState } from "react";

/** SidebarShortcut is one numbered destination, in the order the sidebar shows it. */
export type SidebarShortcut = {
  url: string;
};

/**
 * maxSidebarShortcuts is how many entries can be reached by number. Digit0 is
 * left alone rather than pressed into service as a tenth slot, because "0" for
 * the tenth item is a guess nobody makes on the first try.
 */
export const maxSidebarShortcuts = 9;

/**
 * digitFromEvent reads the number key by physical position rather than by the
 * character it produces. Shift+1 is "!" on a US layout and "!" again on a
 * German one, and on several layouts the digits need AltGr — the key's position
 * is the only stable way to mean "the 1 key" while Shift is held.
 */
function digitFromEvent(event: KeyboardEvent): number {
  const match = /^Digit([1-9])$/.exec(event.code);
  return match ? Number(match[1]) : 0;
}

/** shortcutModifiersHeld reports the exact Ctrl+Shift chord, with no others along. */
function shortcutModifiersHeld(event: KeyboardEvent): boolean {
  return event.ctrlKey && event.shiftKey && !event.altKey && !event.metaKey;
}

/**
 * editingRichText reports a target that owns its own Ctrl+Shift chords. The
 * message body is the one place in the app where these combinations already
 * mean formatting, so navigation stays out of it.
 */
function editingRichText(target: EventTarget | null): boolean {
  return target instanceof Element && Boolean(target.closest("[contenteditable='true']"));
}

/**
 * useSidebarShortcuts wires Ctrl+Shift+1..9 to the given destinations and
 * reports whether the number badges should currently be visible. The badges
 * appear while the chord is held so the numbering can be discovered by pressing
 * the modifiers rather than having to be memorized first.
 */
export function useSidebarShortcuts(shortcuts: SidebarShortcut[], open: (url: string) => void): boolean {
  const [hintsVisible, setHintsVisible] = useState(false);
  const urls = shortcuts.slice(0, maxSidebarShortcuts).map((shortcut) => shortcut.url).join("\n");
  // The navigate callback is read through a ref so the listeners survive the
  // chrome re-renders that arrive while a chord is being held.
  const openRef = useRef(open);
  openRef.current = open;

  useEffect(() => {
    const targets = urls ? urls.split("\n") : [];
    function onKeyDown(event: KeyboardEvent) {
      if (!shortcutModifiersHeld(event)) return;
      setHintsVisible(true);
      if (event.defaultPrevented || event.repeat || editingRichText(event.target)) return;
      const digit = digitFromEvent(event);
      const url = digit > 0 ? targets[digit - 1] : "";
      if (!url) return;
      event.preventDefault();
      setHintsVisible(false);
      openRef.current(url);
    }
    function onKeyUp(event: KeyboardEvent) {
      if (!event.ctrlKey || !event.shiftKey) setHintsVisible(false);
    }
    // A chord that ends while the window is in the background never delivers
    // its keyup, so the badges would stay up until the next keystroke.
    function hide() {
      setHintsVisible(false);
    }
    window.addEventListener("keydown", onKeyDown);
    window.addEventListener("keyup", onKeyUp);
    window.addEventListener("blur", hide);
    return () => {
      window.removeEventListener("keydown", onKeyDown);
      window.removeEventListener("keyup", onKeyUp);
      window.removeEventListener("blur", hide);
    };
  }, [urls]);

  return hintsVisible;
}
