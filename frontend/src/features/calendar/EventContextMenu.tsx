// File overview: The menu a right-click (or a long press, or the context-menu
// key) opens on an event in the calendar: the things worth doing to an event
// without opening it first. It offers exactly what the event's dialog would --
// the gates are the dialog's own (`eventDuplicable`, `eventRemoval`) -- and
// asks the dialog's question before deleting.

import { useEffect, useLayoutEffect, useRef, useState } from "react";
import type { KeyboardEvent as ReactKeyboardEvent } from "react";
import type { CalendarEvent, CalendarSummary } from "../../types";
import { Icon } from "../../components/Icon";
import { responseOptions } from "./EventDialog";
import { deleteQuestion, eventDuplicable, eventRemoval } from "./weekModel";

/** menuEdgeGap keeps the menu this many pixels inside the window. */
const menuEdgeGap = 8;

/** EventContextMenu is drawn at the point the reader asked for it, moved
 * inside the window when it would not fit there. */
export function EventContextMenu({
  event,
  x,
  y,
  calendars,
  writable,
  busy,
  onOpen,
  onDuplicate,
  onRespond,
  onDelete,
  onClose
}: {
  event: CalendarEvent;
  x: number;
  y: number;
  calendars: CalendarSummary[];
  /** writable is the calendars a new event may go into: listed and writable. */
  writable: CalendarSummary[];
  /** busy is a delete started from the menu that has not answered yet. */
  busy: boolean;
  onOpen: () => void;
  onDuplicate: () => void;
  onRespond: (response: string) => void;
  onDelete: () => void;
  onClose: () => void;
}) {
  const menuRef = useRef<HTMLDivElement | null>(null);
  const keepRef = useRef<HTMLButtonElement | null>(null);
  // What had focus when the menu opened, read while rendering: the effects
  // below move focus into the menu, so by the time any effect ran it would
  // read one of the menu's own items.
  const openerRef = useRef(document.activeElement instanceof HTMLElement ? document.activeElement : null);
  const [position, setPosition] = useState({ left: x, top: y });
  const [confirming, setConfirming] = useState(false);
  const calendar = calendars.find((item) => item.id === event.calendar_id);
  const removal = eventRemoval(event, calendar);
  const duplicable = eventDuplicable(event, writable);

  // Measured before paint, so a menu opened by the right edge or the bottom of
  // the window never flashes half outside it. Asking to delete changes the
  // menu's size, so that is measured again.
  useLayoutEffect(() => {
    const menu = menuRef.current;
    if (!menu) return;
    const width = menu.offsetWidth;
    const height = menu.offsetHeight;
    setPosition({
      left: Math.max(menuEdgeGap, Math.min(x, window.innerWidth - width - menuEdgeGap)),
      top: Math.max(menuEdgeGap, Math.min(y, window.innerHeight - height - menuEdgeGap))
    });
  }, [x, y, confirming]);

  // The question lands on Keep, never on Delete: Enter on "Delete…" repeats
  // while it is held, and a repeat landing on Delete would cancel the meeting
  // for every guest without the question ever having been read.
  useEffect(() => {
    if (confirming) keepRef.current?.focus();
    else focusItem(menuRef.current, 0);
  }, [confirming]);

  // Closing hands focus back to what had it, so the context-menu key leaves a
  // keyboard reader on the event rather than at the top of the page. Anything
  // that moved focus on purpose -- a dialog the menu opened -- keeps it.
  useEffect(() => {
    const opener = openerRef.current;
    const menu = menuRef.current;
    return () => {
      const focused = document.activeElement;
      const lost = !focused || focused === document.body || Boolean(menu?.contains(focused));
      if (lost && opener?.isConnected) opener.focus();
    };
  }, []);

  // Anything that moves what the menu points at closes it: a press anywhere
  // else, a scroll of the week under it, a resize, leaving the window.
  useEffect(() => {
    const onPointerDown = (pointerEvent: PointerEvent) => {
      if (menuRef.current?.contains(pointerEvent.target as Node)) return;
      onClose();
    };
    const onScroll = (scrollEvent: Event) => {
      if (menuRef.current?.contains(scrollEvent.target as Node)) return;
      onClose();
    };
    document.addEventListener("pointerdown", onPointerDown, true);
    document.addEventListener("scroll", onScroll, true);
    window.addEventListener("resize", onClose);
    window.addEventListener("blur", onClose);
    return () => {
      document.removeEventListener("pointerdown", onPointerDown, true);
      document.removeEventListener("scroll", onScroll, true);
      window.removeEventListener("resize", onClose);
      window.removeEventListener("blur", onClose);
    };
  }, [onClose]);

  const onKeyDown = (keyEvent: ReactKeyboardEvent) => {
    const items = menuItems(menuRef.current);
    const current = items.indexOf(document.activeElement as HTMLElement);
    switch (keyEvent.key) {
      case "Escape":
        keyEvent.preventDefault();
        keyEvent.stopPropagation();
        if (confirming && !busy) setConfirming(false);
        else onClose();
        return;
      case "ArrowDown":
        keyEvent.preventDefault();
        focusItem(menuRef.current, current + 1);
        return;
      case "ArrowUp":
        keyEvent.preventDefault();
        focusItem(menuRef.current, current <= 0 ? items.length - 1 : current - 1);
        return;
      case "Home":
        keyEvent.preventDefault();
        focusItem(menuRef.current, 0);
        return;
      case "End":
        keyEvent.preventDefault();
        focusItem(menuRef.current, items.length - 1);
        return;
      case "Tab":
        onClose();
        return;
    }
  };

  const title = event.summary || "(No title)";
  const providerLabel = calendar?.provider === "microsoft" ? "Open in Outlook" : "Open in Google Calendar";
  const meetingLabel = event.online_meeting_provider === "googleMeet" ? "Join Google Meet" : "Join Teams meeting";

  return (
    <div
      ref={menuRef}
      className="calendar-event-menu"
      role="menu"
      aria-label={`Actions for ${title}`}
      style={{ left: position.left, top: position.top }}
      onKeyDown={onKeyDown}
      onContextMenu={(mouseEvent) => mouseEvent.preventDefault()}
    >
      <div className="calendar-event-menu-title" title={title}>
        {title}
      </div>
      {confirming ? (
        <div className="calendar-event-menu-confirm">
          <p>{deleteQuestion(event, calendars)}</p>
          <div className="calendar-event-menu-confirm-actions">
            <button type="button" role="menuitem" className="danger" disabled={busy} onClick={onDelete}>
              {busy ? "Deleting…" : "Delete"}
            </button>
            <button
              ref={keepRef}
              type="button"
              role="menuitem"
              className="ghost"
              disabled={busy}
              onClick={() => setConfirming(false)}
            >
              Keep
            </button>
          </div>
        </div>
      ) : (
        <>
          <button type="button" role="menuitem" className="calendar-event-menu-row" onClick={onOpen}>
            <Icon name="edit" />
            Open
          </button>
          {duplicable ? (
            <button type="button" role="menuitem" className="calendar-event-menu-row" onClick={onDuplicate}>
              <Icon name="copy" />
              Duplicate
            </button>
          ) : null}
          {event.online_meeting_url ? (
            <a
              role="menuitem"
              className="calendar-event-menu-row"
              href={event.online_meeting_url}
              target="_blank"
              rel="noreferrer noopener"
              onClick={onClose}
            >
              <Icon name="video" />
              {meetingLabel}
            </a>
          ) : null}
          {event.my_response && !event.pending_offline ? (
            <div className="calendar-event-menu-rsvp" role="group" aria-label="Are you going?">
              <span>Going?</span>
              {responseOptions.map((option) => (
                <button
                  key={option.value}
                  type="button"
                  role="menuitemradio"
                  aria-checked={event.my_response === option.value}
                  className={`ghost ${event.my_response === option.value ? "active" : ""}`}
                  onClick={() => onRespond(option.value)}
                >
                  {option.label}
                </button>
              ))}
            </div>
          ) : null}
          {event.html_link ? (
            <a
              role="menuitem"
              className="calendar-event-menu-row"
              href={event.html_link}
              target="_blank"
              rel="noreferrer noopener"
              onClick={onClose}
            >
              <Icon name="link" />
              {providerLabel}
            </a>
          ) : null}
          {removal === "discard" ? (
            <button type="button" role="menuitem" className="calendar-event-menu-row danger" onClick={onDelete}>
              <Icon name="trash" />
              Discard
            </button>
          ) : removal === "delete" ? (
            <button type="button" role="menuitem" className="calendar-event-menu-row danger" onClick={() => setConfirming(true)}>
              <Icon name="trash" />
              Delete…
            </button>
          ) : null}
        </>
      )}
    </div>
  );
}

function menuItems(menu: HTMLElement | null): HTMLElement[] {
  if (!menu) return [];
  return Array.from(menu.querySelectorAll<HTMLElement>('[role="menuitem"], [role="menuitemradio"]')).filter(
    (item) => !(item as HTMLButtonElement).disabled
  );
}

function focusItem(menu: HTMLElement | null, index: number) {
  const items = menuItems(menu);
  if (items.length === 0) return;
  items[((index % items.length) + items.length) % items.length].focus();
}
