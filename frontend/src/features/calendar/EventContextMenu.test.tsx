import { act } from "react";
import { createRoot, type Root } from "react-dom/client";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { CalendarEvent, CalendarSummary } from "../../types";
import { EventContextMenu } from "./EventContextMenu";

(globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;

function calendar(id: number, extra: Partial<CalendarSummary> = {}): CalendarSummary {
  return {
    id,
    provider: "google",
    connection_id: 1,
    connection_email: "reader@example.test",
    name: `Calendar ${id}`,
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

function event(extra: Partial<CalendarEvent> = {}): CalendarEvent {
  return {
    id: 5,
    calendar_id: 1,
    summary: "Planning",
    description: "",
    location: "",
    status: "confirmed",
    start_at: "2026-10-27T09:00:00.000Z",
    end_at: "2026-10-27T10:00:00.000Z",
    all_day: false,
    time_zone: "",
    recurring_event_id: "",
    organizer_email: "",
    organizer_name: "",
    attendees: [],
    my_response: "",
    html_link: "",
    link_primary: false,
    link_masked: false,
    also_in: [],
    online_meeting: false,
    online_meeting_provider: "",
    online_meeting_url: "",
    ...extra
  };
}

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

type Handlers = {
  onOpen: () => void;
  onDuplicate: () => void;
  onRespond: (response: string) => void;
  onDelete: () => void;
  onClose: () => void;
};

function render(item: CalendarEvent, calendars: CalendarSummary[], handlers: Partial<Handlers> = {}) {
  const all: Handlers = {
    onOpen: vi.fn(),
    onDuplicate: vi.fn(),
    onRespond: vi.fn(),
    onDelete: vi.fn(),
    onClose: vi.fn(),
    ...handlers
  };
  act(() =>
    root.render(
      <EventContextMenu
        event={item}
        x={40}
        y={60}
        calendars={calendars}
        writable={calendars.filter((entry) => entry.can_write && entry.listed)}
        busy={false}
        {...all}
      />
    )
  );
  return all;
}

function labels(): string[] {
  return Array.from(container.querySelectorAll('[role="menuitem"], [role="menuitemradio"]')).map(
    (item) => item.textContent?.trim() || ""
  );
}

function item(text: string): HTMLElement {
  const found = Array.from(container.querySelectorAll<HTMLElement>('[role="menuitem"], [role="menuitemradio"]')).find(
    (entry) => entry.textContent?.trim() === text
  );
  if (!found) throw new Error(`no item "${text}" in ${labels().join(", ")}`);
  return found;
}

describe("EventContextMenu", () => {
  it("offers open, duplicate and delete on an event the reader owns, and focuses the first", () => {
    render(event(), [calendar(1)]);
    expect(labels()).toEqual(["Open", "Duplicate", "Delete…"]);
    expect(document.activeElement?.textContent?.trim()).toBe("Open");
  });

  it("asks before deleting, with the dialog's question, and Keep goes back", () => {
    const handlers = render(event({ attendees: [{ email: "a@example.test", name: "", response: "", optional: false, organizer: false, self: false, resource: false }] }), [calendar(1)]);
    act(() => item("Delete…").click());
    expect(handlers.onDelete).not.toHaveBeenCalled();
    expect(container.textContent).toContain("Delete this event in Google too? The guests are told it is cancelled.");
    act(() => item("Keep").click());
    expect(labels()).toContain("Delete…");
    act(() => item("Delete…").click());
    act(() => item("Delete").click());
    expect(handlers.onDelete).toHaveBeenCalledTimes(1);
  });

  it("offers the answers, the meeting and the provider link on a meeting somebody else organizes, and no delete", () => {
    const handlers = render(
      event({
        my_response: "needsAction",
        online_meeting: true,
        online_meeting_provider: "teamsForBusiness",
        online_meeting_url: "https://teams.microsoft.com/l/meetup-join/abc",
        html_link: "https://outlook.office.com/calendar/item/abc"
      }),
      [calendar(1, { provider: "microsoft" })]
    );
    expect(labels()).toEqual(["Open", "Duplicate", "Join Teams meeting", "Yes", "Maybe", "No", "Open in Outlook"]);
    act(() => item("Maybe").click());
    expect(handlers.onRespond).toHaveBeenCalledWith("tentative");
  });

  it("offers neither duplicate nor delete on a placeholder whose event exists", () => {
    render(event({ link_masked: true, also_in: [{ calendar_id: 2, event_id: 9 }] }), [calendar(1), calendar(2)]);
    expect(labels()).toEqual(["Open"]);
  });

  it("discards a waiting event at once, since it never reached the calendar", () => {
    const handlers = render(event({ id: -1, pending_offline: true }), [calendar(1)]);
    expect(labels()).toEqual(["Open", "Discard"]);
    act(() => item("Discard").click());
    expect(handlers.onDelete).toHaveBeenCalledTimes(1);
  });

  it("closes on Escape and on a press outside it", () => {
    const handlers = render(event(), [calendar(1)]);
    act(() => {
      document.activeElement?.dispatchEvent(new KeyboardEvent("keydown", { key: "Escape", bubbles: true }));
    });
    expect(handlers.onClose).toHaveBeenCalledTimes(1);
    act(() => {
      document.body.dispatchEvent(new Event("pointerdown", { bubbles: true }));
    });
    expect(handlers.onClose).toHaveBeenCalledTimes(2);
  });

  it("moves through its items with the arrow keys", () => {
    render(event(), [calendar(1)]);
    act(() => {
      document.activeElement?.dispatchEvent(new KeyboardEvent("keydown", { key: "ArrowDown", bubbles: true }));
    });
    expect(document.activeElement?.textContent?.trim()).toBe("Duplicate");
    act(() => {
      document.activeElement?.dispatchEvent(new KeyboardEvent("keydown", { key: "ArrowUp", bubbles: true }));
      document.activeElement?.dispatchEvent(new KeyboardEvent("keydown", { key: "ArrowUp", bubbles: true }));
    });
    expect(document.activeElement?.textContent?.trim()).toBe("Delete…");
  });
});
