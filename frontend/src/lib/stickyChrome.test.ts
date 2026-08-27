// File overview: The measurement behind the sticky strips' scroll offsets, and
// what happens to it when a strip leaves.

import { describe, expect, it } from "vitest";
import { act, createElement, useState } from "react";
import type { ReactNode } from "react";
import { createRoot } from "react-dom/client";
import { stickyChromeStyle, useStickyChromeHeight } from "./stickyChrome";

type ObserverRecord = { observed: number; disconnected: number };

/**
 * withMeasuredStrip mounts a component that shows a strip of the given height
 * on demand, and hands the test the switch and what the hook published. happy-dom
 * lays nothing out, so the height comes from a stubbed rect, and ResizeObserver
 * is stubbed as well: what matters here is that it is taken down again.
 */
function withMeasuredStrip(stripHeight: number, run: (control: {
  show: (visible: boolean) => void;
  height: () => number;
  observer: ObserverRecord;
}) => void) {
  const record: ObserverRecord = { observed: 0, disconnected: 0 };
  const originalObserver = globalThis.ResizeObserver;
  const originalRect = Element.prototype.getBoundingClientRect;
  globalThis.ResizeObserver = class {
    observe() { record.observed += 1; }
    unobserve() {}
    disconnect() { record.disconnected += 1; }
  } as unknown as typeof ResizeObserver;
  Element.prototype.getBoundingClientRect = function () {
    return { height: stripHeight, width: 0, top: 0, left: 0, right: 0, bottom: stripHeight, x: 0, y: 0, toJSON: () => ({}) };
  };
  (globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;

  let published = 0;
  let setVisible: ((visible: boolean) => void) | null = null;
  function Strip(): ReactNode {
    const [visible, setState] = useState(true);
    const [measure, height] = useStickyChromeHeight<HTMLDivElement>();
    setVisible = setState;
    published = height;
    return createElement("div", null, visible ? createElement("div", { ref: measure }) : null);
  }

  const host = document.createElement("div");
  document.body.appendChild(host);
  const root = createRoot(host);
  try {
    act(() => { root.render(createElement(Strip)); });
    run({
      show: (visible) => act(() => { setVisible?.(visible); }),
      height: () => published,
      observer: record
    });
  } finally {
    act(() => { root.unmount(); });
    host.remove();
    globalThis.ResizeObserver = originalObserver;
    Element.prototype.getBoundingClientRect = originalRect;
  }
}

describe("useStickyChromeHeight", () => {
  it("publishes the strip's height while it is up", () => {
    withMeasuredStrip(48, ({ height, observer }) => {
      expect(height()).toBe(48);
      expect(observer.observed).toBe(1);
    });
  });

  it("stops observing and drops the offset when the strip leaves", () => {
    withMeasuredStrip(48, ({ show, height, observer }) => {
      show(false);
      expect(height()).toBe(0);
      expect(observer.disconnected).toBe(1);
    });
  });
});

describe("stickyChromeStyle", () => {
  it("publishes a measured height as the named custom property", () => {
    expect(stickyChromeStyle("--selection-bar-height", 48.4)).toEqual({ "--selection-bar-height": "48px" });
  });

  it("says nothing while no strip is up, so the stylesheet's fallback answers", () => {
    expect(stickyChromeStyle("--selection-bar-height", 0)).toBeUndefined();
  });
});
