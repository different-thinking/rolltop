import { act } from "react";
import { createRoot, type Root } from "react-dom/client";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { CalendarSummary } from "../../types";

const setCalendarCopyTarget = vi.fn();
const setCalendarBusyTarget = vi.fn();
vi.mock("../../api", () => ({
  api: {
    setCalendarCopyTarget: (...args: unknown[]) => setCalendarCopyTarget(...args),
    setCalendarBusyTarget: (...args: unknown[]) => setCalendarBusyTarget(...args)
  }
}));

import { CalendarRoleChoices, writeBusyLabel, writeBusyTarget, writeCopyTarget } from "./CalendarRoles";

(globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;

function roleCalendar(id: number, name: string, extra: Partial<CalendarSummary> = {}): CalendarSummary {
  return {
    id,
    provider: "google",
    connection_id: 1,
    connection_email: "reader@example.test",
    name,
    description: "",
    time_zone: "",
    color: "#1a73e8",
    access_role: "owner",
    can_write: true,
    is_primary: false,
    selected: true,
    copy_target: false,
    busy_target: false,
    busy_label: "",
    busy_title: "Busy",
    listed: true,
    online_meeting_providers: [],
    default_reminder_minutes: [],
    synced_from: "",
    last_sync_at: "",
    status: "ok",
    status_detail: "",
    ...extra
  };
}

beforeEach(() => {
  setCalendarCopyTarget.mockReset();
  setCalendarBusyTarget.mockReset();
});

describe("writeCopyTarget", () => {
  const work = roleCalendar(1, "Work", { copy_target: true });
  const family = roleCalendar(2, "Family");

  it("moves the mark to the chosen calendar and off the one that had it", async () => {
    setCalendarCopyTarget.mockResolvedValue({ calendar: { ...family, copy_target: true } });
    const update = await writeCopyTarget("csrf", [work, family], family.id);
    expect(setCalendarCopyTarget).toHaveBeenCalledWith("csrf", family.id, true);
    const next = update?.([work, family]) || [];
    expect(next.map((calendar) => calendar.copy_target)).toEqual([false, true]);
  });

  it("takes the role off when None is chosen", async () => {
    setCalendarCopyTarget.mockResolvedValue({ calendar: { ...work, copy_target: false } });
    const update = await writeCopyTarget("csrf", [work, family], 0);
    expect(setCalendarCopyTarget).toHaveBeenCalledWith("csrf", work.id, false);
    expect(update?.([work, family]).some((calendar) => calendar.copy_target)).toBe(false);
  });

  it("sends nothing when the choice is the one already stored", async () => {
    expect(await writeCopyTarget("csrf", [work, family], work.id)).toBeNull();
    expect(await writeCopyTarget("csrf", [family], 0)).toBeNull();
    expect(setCalendarCopyTarget).not.toHaveBeenCalled();
  });
});

describe("writeBusyTarget", () => {
  it("carries the placeholder title along when the role moves", async () => {
    const away = roleCalendar(1, "Away", { busy_target: true, busy_label: "Out" });
    const shared = roleCalendar(2, "Shared");
    setCalendarBusyTarget.mockResolvedValue({ calendar: { ...shared, busy_target: true, busy_label: "Out" } });
    const update = await writeBusyTarget("csrf", [away, shared], shared.id, "Out");
    expect(setCalendarBusyTarget).toHaveBeenCalledWith("csrf", shared.id, { busy_target: true, busy_label: "Out" });
    expect(update?.([away, shared]).map((calendar) => calendar.busy_target)).toEqual([false, true]);
  });

  it("leaves a first busy calendar's title to the server", async () => {
    const shared = roleCalendar(2, "Shared");
    setCalendarBusyTarget.mockResolvedValue({ calendar: { ...shared, busy_target: true } });
    await writeBusyTarget("csrf", [shared], shared.id, "");
    expect(setCalendarBusyTarget).toHaveBeenCalledWith("csrf", shared.id, { busy_target: true });
  });

  it("renames the placeholders in place", async () => {
    const away = roleCalendar(1, "Away", { busy_target: true });
    setCalendarBusyTarget.mockResolvedValue({ calendar: { ...away, busy_label: "Away" } });
    const update = await writeBusyLabel("csrf", away.id, "Away");
    expect(setCalendarBusyTarget).toHaveBeenCalledWith("csrf", away.id, { busy_label: "Away" });
    expect(update([away])[0].busy_label).toBe("Away");
  });
});

describe("CalendarRoleChoices", () => {
  let container: HTMLDivElement;
  let root: Root;

  beforeEach(() => {
    container = document.createElement("div");
    document.body.appendChild(container);
    root = createRoot(container);
  });

  afterEach(() => {
    act(() => root.unmount());
    container.remove();
  });

  const render = (calendars: CalendarSummary[], handlers: Partial<Parameters<typeof CalendarRoleChoices>[0]> = {}) =>
    act(async () =>
      root.render(
        <CalendarRoleChoices
          calendars={calendars}
          idPrefix="test"
          onChooseCopyTarget={vi.fn()}
          onChooseBusyTarget={vi.fn()}
          onSetBusyLabel={vi.fn()}
          {...handlers}
        />
      )
    );

  it("offers nothing until two calendars can be written to", async () => {
    await render([roleCalendar(1, "Work"), roleCalendar(2, "Holidays", { can_write: false })]);
    expect(container.querySelector("select")).toBeNull();
  });

  it("offers both roles among writable calendars and reports a choice", async () => {
    const onChooseCopyTarget = vi.fn();
    await render([roleCalendar(1, "Work"), roleCalendar(2, "Family"), roleCalendar(3, "Holidays", { can_write: false })], {
      onChooseCopyTarget
    });
    const copy = container.querySelector<HTMLSelectElement>("#test-copy-target");
    expect(copy).not.toBeNull();
    expect(container.querySelector("#test-busy-target")).not.toBeNull();
    expect(Array.from(copy?.options || []).map((option) => option.textContent)).toEqual([
      "None",
      "Work — reader@example.test",
      "Family — reader@example.test"
    ]);
    await act(async () => {
      if (!copy) return;
      copy.value = "2";
      copy.dispatchEvent(new Event("change", { bubbles: true }));
    });
    expect(onChooseCopyTarget).toHaveBeenCalledWith(2);
  });

  it("saves the placeholder title when the field is left, and only when it changed", async () => {
    const onSetBusyLabel = vi.fn();
    await render([roleCalendar(1, "Work"), roleCalendar(2, "Away", { busy_target: true, busy_label: "Busy" })], {
      onSetBusyLabel
    });
    const field = container.querySelector<HTMLInputElement>(".calendar-busy-label");
    expect(field?.value).toBe("Busy");
    await act(async () => field?.dispatchEvent(new FocusEvent("focusout", { bubbles: true })));
    expect(onSetBusyLabel).not.toHaveBeenCalled();
    await act(async () => {
      if (!field) return;
      Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, "value")?.set?.call(field, " Out of office ");
      field.dispatchEvent(new Event("input", { bubbles: true }));
    });
    await act(async () => field?.dispatchEvent(new FocusEvent("focusout", { bubbles: true })));
    expect(onSetBusyLabel).toHaveBeenCalledWith(2, "Out of office");
  });
});
