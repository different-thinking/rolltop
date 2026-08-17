// File overview: Ctrl+Shift+<number> navigation for the sidebar's top-level
// lists, and the badge hint that appears while both modifiers are held.

import { useEffect, useRef, useState } from "react";
import { editableTarget } from "./keyboard";

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
 * is the only stable way to mean "the 1 key" while Shift is held. Both the
 * number row and the keypad count, because the badges advertise a number rather
 * than a particular key for it.
 */
function digitFromEvent(event: KeyboardEvent): number {
  const match = /^(?:Digit|Numpad)([1-9])$/.exec(event.code);
  return match ? Number(match[1]) : 0;
}

/** shortcutModifiersHeld reports the exact Ctrl+Shift chord, with no others along. */
function shortcutModifiersHeld(event: KeyboardEvent): boolean {
  return event.ctrlKey && event.shiftKey && !event.altKey && !event.metaKey;
}

/**
 * useSidebarShortcuts wires Ctrl+Shift+1..9 to the given destinations and
 * reports whether the number badges should currently be visible. The badges
 * appear while the chord is held so the numbering can be discovered by pressing
 * the modifiers rather than having to be memorized first.
 */
export function useSidebarShortcuts(shortcuts: SidebarShortcut[], open: (url: string) => void): boolean {
  const [hintsVisible, setHintsVisible] = useState(false);
  // The destinations and the navigate callback are both read through refs, so
  // the listeners are installed once and survive the chrome re-renders that
  // arrive while a chord is being held.
  const targets = useRef<string[]>([]);
  targets.current = shortcuts.slice(0, maxSidebarShortcuts).map((shortcut) => shortcut.url);
  const openRef = useRef(open);
  openRef.current = open;

  useEffect(() => {
    function onKeyDown(event: KeyboardEvent) {
      if (!shortcutModifiersHeld(event)) return;
      // A field that owns the keystroke also owns the chord: navigating away
      // mid-sentence would abandon whatever was being typed, so the badges stay
      // hidden there too rather than advertising a shortcut that will not fire.
      if (editableTarget(event.target)) return;
      setHintsVisible(true);
      if (event.defaultPrevented || event.repeat) return;
      const digit = digitFromEvent(event);
      const url = digit > 0 ? targets.current[digit - 1] : "";
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
  }, []);

  return hintsVisible;
}
