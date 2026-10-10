// File overview: Which calendars the calendar view lists at all. With Google
// and Microsoft 365 side by side a reader routinely has a dozen calendars, most
// of which they never want to see; hiding one here takes it out of the calendar menu,
// out of the week and out of the second-calendar choice in one step.

import { useCallback, useEffect, useMemo, useState } from "react";
import { api } from "../../api";
import type { Toast } from "../../appTypes";
import type { CalendarSummary } from "../../types";
import { messageFromError } from "../../lib/errors";
import { calendarAccountGroups, calendarColor } from "../calendar/weekModel";

/**
 * CalendarSettingsPanel switches calendars in and out of the calendar view.
 * Each switch is saved on its own, as the calendar menu's visibility switches are:
 * there is nothing to combine, and a form with a save button would let a
 * reader leave with the choice they made unsaved.
 */
export function CalendarSettingsPanel({
  csrf,
  addToast
}: {
  csrf: string;
  addToast: (message: string, kind?: Toast["kind"]) => number;
}) {
  const [calendars, setCalendars] = useState<CalendarSummary[]>([]);
  const [loading, setLoading] = useState(true);
  const [loadError, setLoadError] = useState("");
  const [busy, setBusy] = useState<Record<number, boolean>>({});

  const load = useCallback(async () => {
    setLoading(true);
    try {
      const data = await api.calendars();
      setCalendars(data.calendars || []);
      setLoadError("");
    } catch (err) {
      setLoadError(messageFromError(err));
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    void load();
  }, [load]);

  const groups = useMemo(() => calendarAccountGroups(calendars), [calendars]);

  const setListed = async (calendar: CalendarSummary, listed: boolean) => {
    if (!listed && calendar.copy_target) {
      const ok = window.confirm(
        `${calendar.name} is your second calendar.\n\nHiding it also stops new events from being copied into it. Copies that already exist stay where they are.`
      );
      if (!ok) return;
    }
    setBusy((current) => ({ ...current, [calendar.id]: true }));
    try {
      const data = await api.setCalendarListed(csrf, calendar.id, listed);
      setCalendars((current) => current.map((item) => (item.id === calendar.id ? data.calendar : item)));
    } catch (err) {
      addToast(messageFromError(err), "error");
    } finally {
      setBusy((current) => ({ ...current, [calendar.id]: false }));
    }
  };

  if (loading) {
    return <div className="panel calendar-settings" role="status" aria-label="Loading calendars">Loading calendars.</div>;
  }
  if (loadError) {
    return (
      <div className="panel calendar-settings">
        <p className="swipe-validation">{loadError}</p>
        <div className="actions"><button type="button" onClick={() => void load()}>Try again</button></div>
      </div>
    );
  }

  const listedCount = calendars.filter((calendar) => calendar.listed).length;
  return (
    <div className="panel calendar-settings">
      <p className="muted">
        Only the calendars ticked here appear in the calendar's Calendars menu, in the week and as a choice for the second
        calendar. A hidden calendar is not synced either; nothing is changed in Google or Microsoft 365.
      </p>
      {calendars.length === 0 ? (
        <small className="swipe-validation">
          No calendars yet. Connect a Google or Microsoft 365 account and allow calendar access.
        </small>
      ) : (
        <>
          <small className="muted">
            {listedCount} of {calendars.length} calendar{calendars.length === 1 ? "" : "s"} shown. A calendar shown again
            starts switched off; tick it in the calendar's Calendars menu to draw it.
          </small>
          {groups.map(([account, items]) => (
            <section key={account} className="calendar-settings-group">
              <h3>{account}</h3>
              {items.map((calendar) => (
                <label key={calendar.id} className="calendar-settings-row">
                  <input
                    type="checkbox"
                    checked={calendar.listed}
                    disabled={Boolean(busy[calendar.id])}
                    onChange={(changeEvent) => void setListed(calendar, changeEvent.target.checked)}
                  />
                  <span className="calendar-swatch" style={{ background: calendarColor(calendar) }} />
                  <span className="calendar-settings-name">{calendar.name}</span>
                  <span className="calendar-settings-tags">
                    {calendar.is_primary ? <span className="settings-badge">Primary</span> : null}
                    {calendar.copy_target ? <span className="settings-badge">Second calendar</span> : null}
                    {!calendar.can_write ? <span className="settings-badge">Read-only</span> : null}
                  </span>
                </label>
              ))}
            </section>
          ))}
        </>
      )}
    </div>
  );
}
