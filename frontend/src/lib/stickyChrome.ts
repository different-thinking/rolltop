// File overview: Height measurement for the sticky chrome the mail views park
// under the top bar - the list's selection toolbar, a conversation's header.

import { useCallback, useRef, useState } from "react";
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
 * measurement belongs to the node itself.
 *
 * The observer is kept in a ref and taken down by the next call rather than
 * returned as the callback's cleanup: React only honours a returned cleanup
 * from 19 on, and a version that ignored it would keep an observer alive on
 * every strip that ever left. Every call ends the previous observation first,
 * so the call React makes with null when the node goes is what both stops the
 * observer and sends the offset the rows keep for the strip back to zero.
 */
export function useStickyChromeHeight<T extends HTMLElement>(): [(node: T | null) => void, number] {
  const [height, setHeight] = useState(0);
  const observer = useRef<ResizeObserver | null>(null);
  const measure = useCallback((node: T | null) => {
    observer.current?.disconnect();
    observer.current = null;
    if (!node) {
      setHeight(0);
      return;
    }
    const apply = () => setHeight(node.getBoundingClientRect().height);
    apply();
    if (typeof ResizeObserver === "undefined") return;
    observer.current = new ResizeObserver(apply);
    observer.current.observe(node);
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
