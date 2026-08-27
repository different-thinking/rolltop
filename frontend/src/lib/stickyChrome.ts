// File overview: Height measurement for the sticky chrome the mail views park
// under the top bar - the list's selection toolbar, a conversation's header.

import { useCallback, useState } from "react";
import type { CSSProperties } from "react";

/**
 * useStickyChromeHeight keeps the height of a sticky strip in state and hands
 * back the ref to attach to it. What reads the height is `scroll-margin-top` on
 * whatever gets scrolled into view underneath: `scrollIntoView` with "nearest"
 * counts a row hidden behind sticky chrome as already visible, so without the
 * margin the keyboard lands on rows the strip is covering. The height is
 * measured rather than assumed because the strips wrap onto two or three rows
 * at narrow widths and a subject line wraps at any width.
 *
 * It is a ref callback rather than an effect because the strips come and go -
 * the toolbar with the selection, the header with the conversation - so the
 * measurement belongs to the node itself. React 19 calls the cleanup a ref
 * callback returns when that node leaves, which is when the offset the rows
 * keep for it has to go back to zero.
 */
export function useStickyChromeHeight<T extends HTMLElement>(): [(node: T | null) => (() => void) | void, number] {
  const [height, setHeight] = useState(0);
  const measure = useCallback((node: T | null) => {
    if (!node) {
      setHeight(0);
      return;
    }
    const apply = () => setHeight(node.getBoundingClientRect().height);
    apply();
    if (typeof ResizeObserver === "undefined") return;
    const observer = new ResizeObserver(apply);
    observer.observe(node);
    return () => {
      observer.disconnect();
      setHeight(0);
    };
  }, []);
  return [measure, height];
}

/**
 * stickyChromeStyle publishes a measured height as a custom property for the
 * rules that keep clear of it, and nothing at all while there is no strip up -
 * the fallback in the stylesheet is what answers then.
 */
export function stickyChromeStyle(name: string, height: number): CSSProperties | undefined {
  return height > 0 ? { [name]: `${Math.round(height)}px` } as CSSProperties : undefined;
}
