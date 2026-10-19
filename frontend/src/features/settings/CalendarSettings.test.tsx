import { act } from "react";
import { createRoot, type Root } from "react-dom/client";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { CalendarSummary } from "../../types";

const calendars = vi.fn();
const setCalendarCopyTarget = vi.fn();
vi.mock("../../api", () => ({
  api: {
    calendars: (...args: unknown[]) => calendars(...args),
    setCalendarCopyTarget: (...args: unknown[]) => setCalendarCopyTarget(...args),
    setCalendarBusyTarget: vi.fn(),
    setCalendarListed: vi.fn()
  }
}));

import { CalendarSettingsPanel } from "./CalendarSettings";

(globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;

function calendar(id: number, name: string, extra: Partial<CalendarSummary> = {}): CalendarSummary {
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

let container: HTMLDivElement;
let root: Root;

beforeEach(() => {
  calendars.mockReset();
  setCalendarCopyTarget.mockReset();
  container = document.createElement("div");
  document.body.appendChild(container);
  root = createRoot(container);
});

afterEach(() => {
  act(() => root.unmount());
  container.remove();
});

async function renderPanel() {
  await act(async () => root.render(<CalendarSettingsPanel csrf="csrf" addToast={vi.fn(() => 0)} />));
}

// The settings are the only place a phone chooses the second and the busy
// calendar: its calendar shows chips instead of the menu that also offers them.
describe("CalendarSettingsPanel roles", () => {
  it("offers the second and busy calendar among the listed calendars only", async () => {
    calendars.mockResolvedValue({
      calendars: [calendar(1, "Work"), calendar(2, "Family"), calendar(3, "Old project", { listed: false })]
    });
    await renderPanel();
    const copy = container.querySelector<HTMLSelectElement>("#calendar-settings-copy-target");
    expect(copy).not.toBeNull();
    expect(container.querySelector("#calendar-settings-busy-target")).not.toBeNull();
    expect(Array.from(copy?.options || []).map((option) => option.value)).toEqual(["0", "1", "2"]);
  });

  it("saves a choice and shows the moved mark", async () => {
    const work = calendar(1, "Work", { copy_target: true });
    const family = calendar(2, "Family");
    calendars.mockResolvedValue({ calendars: [work, family] });
    setCalendarCopyTarget.mockResolvedValue({ calendar: { ...family, copy_target: true } });
    await renderPanel();
    const copy = container.querySelector<HTMLSelectElement>("#calendar-settings-copy-target");
    expect(copy?.value).toBe("1");
    await act(async () => {
      if (!copy) return;
      copy.value = "2";
      copy.dispatchEvent(new Event("change", { bubbles: true }));
    });
    expect(setCalendarCopyTarget).toHaveBeenCalledWith("csrf", 2, true);
    expect(container.querySelector<HTMLSelectElement>("#calendar-settings-copy-target")?.value).toBe("2");
  });

  it("leaves the roles out while fewer than two listed calendars can be written to", async () => {
    calendars.mockResolvedValue({
      calendars: [calendar(1, "Work"), calendar(2, "Holidays", { can_write: false }), calendar(3, "Hidden", { listed: false })]
    });
    await renderPanel();
    expect(container.querySelector(".calendar-settings-roles")).toBeNull();
  });
});
