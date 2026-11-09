import { act } from "react";
import { createRoot, type Root } from "react-dom/client";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { longPressDelay, longPressSlop, useLongPress } from "./useLongPress";

(globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;

let container: HTMLDivElement;
let root: Root;

function Target({ onLongPress }: { onLongPress: (payload: string) => void }) {
  const longPress = useLongPress(onLongPress);
  return <div data-testid="target" onPointerDown={(event) => longPress.begin(event, "slot")} />;
}

function pointer(
  type: string,
  init: { pointerType?: string; pointerId?: number; clientX?: number; clientY?: number; isPrimary?: boolean }
) {
  const event = new PointerEvent(type, {
    bubbles: true,
    cancelable: true,
    isPrimary: init.isPrimary ?? true,
    pointerType: init.pointerType ?? "touch",
    pointerId: init.pointerId ?? 1,
    clientX: init.clientX ?? 100,
    clientY: init.clientY ?? 200
  });
  return event;
}

function target(): HTMLElement {
  return container.querySelector("[data-testid=target]") as HTMLElement;
}

beforeEach(() => {
  vi.useFakeTimers();
  container = document.createElement("div");
  document.body.appendChild(container);
  root = createRoot(container);
});

afterEach(() => {
  act(() => root.unmount());
  container.remove();
  vi.useRealTimers();
});

function mount(onLongPress: (payload: string) => void) {
  act(() => root.render(<Target onLongPress={onLongPress} />));
}

describe("useLongPress", () => {
  it("fires with the payload once a finger rests long enough", () => {
    const onLongPress = vi.fn();
    mount(onLongPress);
    act(() => {
      target().dispatchEvent(pointer("pointerdown", {}));
    });
    act(() => vi.advanceTimersByTime(longPressDelay - 1));
    expect(onLongPress).not.toHaveBeenCalled();
    act(() => vi.advanceTimersByTime(1));
    expect(onLongPress).toHaveBeenCalledWith("slot");
  });

  it("ignores a mouse, which has its own ways to make an event", () => {
    const onLongPress = vi.fn();
    mount(onLongPress);
    act(() => {
      target().dispatchEvent(pointer("pointerdown", { pointerType: "mouse" }));
    });
    act(() => vi.advanceTimersByTime(longPressDelay * 2));
    expect(onLongPress).not.toHaveBeenCalled();
  });

  it("is called off by lifting, scrolling or moving the finger", () => {
    const onLongPress = vi.fn();
    mount(onLongPress);
    for (const end of ["pointerup", "pointercancel"]) {
      act(() => {
        target().dispatchEvent(pointer("pointerdown", {}));
        window.dispatchEvent(pointer(end, {}));
      });
      act(() => vi.advanceTimersByTime(longPressDelay));
    }
    act(() => {
      target().dispatchEvent(pointer("pointerdown", {}));
      window.dispatchEvent(pointer("pointermove", { clientY: 200 + longPressSlop + 1 }));
    });
    act(() => vi.advanceTimersByTime(longPressDelay));
    expect(onLongPress).not.toHaveBeenCalled();
  });

  it("tolerates the small wobble of a finger held still", () => {
    const onLongPress = vi.fn();
    mount(onLongPress);
    act(() => {
      target().dispatchEvent(pointer("pointerdown", {}));
      window.dispatchEvent(pointer("pointermove", { clientX: 103, clientY: 204 }));
    });
    act(() => vi.advanceTimersByTime(longPressDelay));
    expect(onLongPress).toHaveBeenCalledTimes(1);
  });

  it("is not a long press when a second finger comes down", () => {
    const onLongPress = vi.fn();
    mount(onLongPress);
    act(() => {
      target().dispatchEvent(pointer("pointerdown", { pointerId: 1 }));
      target().dispatchEvent(pointer("pointerdown", { pointerId: 2, isPrimary: false }));
    });
    act(() => vi.advanceTimersByTime(longPressDelay));
    expect(onLongPress).not.toHaveBeenCalled();
  });

  it("is called off by a second finger that comes down anywhere else", () => {
    const onLongPress = vi.fn();
    mount(onLongPress);
    const elsewhere = document.createElement("div");
    document.body.appendChild(elsewhere);
    act(() => {
      target().dispatchEvent(pointer("pointerdown", { pointerId: 1 }));
      elsewhere.dispatchEvent(pointer("pointerdown", { pointerId: 2, isPrimary: false }));
    });
    act(() => vi.advanceTimersByTime(longPressDelay));
    expect(onLongPress).not.toHaveBeenCalled();
    elsewhere.remove();
  });

  it("does not arm a press for a finger that is not the primary one", () => {
    const onLongPress = vi.fn();
    mount(onLongPress);
    act(() => {
      target().dispatchEvent(pointer("pointerdown", { pointerId: 2, isPrimary: false }));
    });
    act(() => vi.advanceTimersByTime(longPressDelay));
    expect(onLongPress).not.toHaveBeenCalled();
  });

  it("stops swallowing clicks once a new touch shows the release was lost", () => {
    const onLongPress = vi.fn();
    const clicked = vi.fn();
    mount(onLongPress);
    document.body.addEventListener("click", clicked);
    act(() => {
      target().dispatchEvent(pointer("pointerdown", {}));
    });
    act(() => vi.advanceTimersByTime(longPressDelay));
    expect(onLongPress).toHaveBeenCalledTimes(1);
    // No pointerup ever reaches the window; the next tap must still work.
    act(() => {
      document.body.dispatchEvent(pointer("pointerdown", { pointerId: 3 }));
      document.body.dispatchEvent(new MouseEvent("click", { bubbles: true, cancelable: true }));
    });
    expect(clicked).toHaveBeenCalledTimes(1);
    document.body.removeEventListener("click", clicked);
  });

  it("takes over the browser's own long-press menu and swallows the release's click", () => {
    const onLongPress = vi.fn();
    const clicked = vi.fn();
    mount(onLongPress);
    document.body.addEventListener("click", clicked);
    act(() => {
      target().dispatchEvent(pointer("pointerdown", {}));
    });
    const menu = new MouseEvent("contextmenu", { bubbles: true, cancelable: true });
    act(() => {
      target().dispatchEvent(menu);
    });
    expect(onLongPress).toHaveBeenCalledTimes(1);
    expect(menu.defaultPrevented).toBe(true);
    act(() => {
      window.dispatchEvent(pointer("pointerup", {}));
      target().dispatchEvent(new MouseEvent("click", { bubbles: true, cancelable: true }));
    });
    expect(clicked).not.toHaveBeenCalled();
    // The next tap is a real one again.
    act(() => vi.advanceTimersByTime(400));
    act(() => {
      target().dispatchEvent(new MouseEvent("click", { bubbles: true, cancelable: true }));
    });
    expect(clicked).toHaveBeenCalledTimes(1);
    document.body.removeEventListener("click", clicked);
  });
});
