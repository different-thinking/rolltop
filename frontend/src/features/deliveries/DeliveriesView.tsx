/**
 * File overview: The parcel list. What the mail says is on its way, grouped by
 * the only question a reader actually has about a parcel: is it coming today.
 *
 * It is a view of shipments, not of mail, which is why it is not a mailbox. One
 * parcel is talked about by the shop that sent it, the carrier that carries it,
 * and the carrier again when it arrives; as a mail list that is four rows in
 * three folders, and as this list it is one row with the mail behind it. The
 * link back into the mail is what makes it a view of the mailbox all the same.
 */

import { useCallback, useEffect, useMemo, useState } from "react";
import { api } from "../../api";
import type { DatePrefs } from "../../appTypes";
import type { Shipment } from "../../types";
import { Icon } from "../../components/Icon";
import { messageURL } from "../../lib/routes";
import { messageFromError } from "../../lib/errors";
import { displayDateTime } from "../../lib/format";

/** localDay is the reader's own day, which is the day the list is answered in. */
export function localDay(now = new Date()): string {
  const month = `${now.getMonth() + 1}`.padStart(2, "0");
  const day = `${now.getDate()}`.padStart(2, "0");
  return `${now.getFullYear()}-${month}-${day}`;
}

/** shipmentsExpectedOn is the parcels due on one day that have not arrived. It
 * is what the header chip counts, so it is defined once and read from both. */
export function shipmentsExpectedOn(shipments: readonly Shipment[], day: string): Shipment[] {
  return shipments.filter((item) => item.expected_date === day && item.status !== "delivered");
}

/** A parcel whose day has passed without a delivery report. The carrier said a
 * day and the day is gone, which is the one state worth pointing at. */
function isOverdue(item: Shipment, today: string): boolean {
  return item.status !== "delivered" && item.expected_date !== "" && item.expected_date < today;
}

export type Group = { key: string; title: string; hint: string; items: Shipment[] };

/** groupShipments splits the list the way a reader reads it: today first,
 * because that is the question, then what is still coming, then what already
 * came. Everything else is detail inside a row. */
export function groupShipments(shipments: readonly Shipment[], today: string): Group[] {
  const today_ = shipmentsExpectedOn(shipments, today);
  const overdue = shipments.filter((item) => isOverdue(item, today));
  const upcoming = shipments.filter((item) => item.status !== "delivered"
    && item.expected_date !== "" && item.expected_date > today);
  const undated = shipments.filter((item) => item.status !== "delivered" && item.expected_date === "");
  const delivered = shipments.filter((item) => item.status === "delivered");
  return [
    { key: "today", title: "Kommt heute", hint: "Für heute angekündigt", items: today_ },
    { key: "overdue", title: "Überfällig", hint: "Der angekündigte Tag ist vorbei, eine Zustellung wurde nicht gemeldet", items: overdue },
    { key: "upcoming", title: "Unterwegs", hint: "Mit einem angekündigten Tag", items: upcoming },
    { key: "undated", title: "Ohne Termin", hint: "Versandt, aber noch ohne angekündigten Tag", items: undated },
    { key: "delivered", title: "Zugestellt", hint: "Die letzten zwei Wochen", items: delivered }
  ].filter((group) => group.items.length > 0);
}

const weekdayNames = ["Sonntag", "Montag", "Dienstag", "Mittwoch", "Donnerstag", "Freitag", "Samstag"];

/** dayLabel names a day the way a reader would say it: relative while that is
 * unambiguous, then by weekday inside the coming week, then by date. */
export function dayLabel(day: string, today: string): string {
  if (day === "") return "Kein Termin genannt";
  const date = new Date(`${day}T00:00:00`);
  const now = new Date(`${today}T00:00:00`);
  if (Number.isNaN(date.getTime()) || Number.isNaN(now.getTime())) return day;
  const distance = Math.round((date.getTime() - now.getTime()) / 86400000);
  if (distance === 0) return "Heute";
  if (distance === 1) return "Morgen";
  if (distance === -1) return "Gestern";
  const written = date.toLocaleDateString("de-DE", { day: "2-digit", month: "2-digit", year: "numeric" });
  if (distance > 1 && distance < 7) return `${weekdayNames[date.getDay()]}, ${written}`;
  return written;
}

const statusLabels: Record<Shipment["status"], string> = {
  announced: "Angekündigt",
  out_for_delivery: "In Zustellung",
  delivered: "Zugestellt"
};

export function DeliveriesView({
  datePrefs,
  mailGeneration,
  navigate
}: {
  datePrefs: DatePrefs;
  /** Bumps when a sync stores messages, which is when a parcel can change. */
  mailGeneration: number;
  navigate: (url: string) => void;
}) {
  const [shipments, setShipments] = useState<Shipment[] | null>(null);
  const [error, setError] = useState("");
  const [openRows, setOpenRows] = useState<Set<number>>(() => new Set());
  // The day is read once per load rather than per render: a tab left open over
  // midnight should not silently re-group under the reader while they scroll.
  const [today] = useState(() => localDay());

  const refresh = useCallback(async () => {
    try {
      const next = await api.deliveries(today);
      setShipments(next.shipments || []);
      setError("");
    } catch (err) {
      setError(messageFromError(err));
    }
  }, [today]);

  useEffect(() => {
    void refresh();
  }, [refresh, mailGeneration]);

  const groups = useMemo(() => groupShipments(shipments || [], today), [shipments, today]);
  const openCount = (shipments || []).filter((item) => item.status !== "delivered").length;

  function toggleRow(id: number) {
    setOpenRows((current) => {
      const next = new Set(current);
      if (next.has(id)) next.delete(id);
      else next.add(id);
      return next;
    });
  }

  return (
    <>
      <div className="content-head">
        <div>
          <h1>Pakete</h1>
          <span className="label-pill">
            {openCount.toLocaleString()} {openCount === 1 ? "unterwegs" : "unterwegs"}
          </span>
        </div>
      </div>

      {error ? <div className="error">{error}</div> : null}

      {shipments !== null && groups.length === 0 && !error ? (
        <section className="panel deliveries-idle">
          <Icon name="package" />
          <div>
            <strong>Keine Pakete angekündigt.</strong>
            <p>
              Rolltop liest Sendungsnummern und Liefertermine aus der Post mit — aus den Mails der
              Paketdienste und aus Versandbestätigungen von Shops. Sobald etwas unterwegs ist, steht es hier.
            </p>
          </div>
        </section>
      ) : null}

      {groups.map((group) => (
        <section className="panel deliveries-section" key={group.key}>
          <h2>
            {group.title}
            <span className="deliveries-section-count">{group.items.length.toLocaleString()}</span>
          </h2>
          <p className="deliveries-section-hint">{group.hint}</p>
          <div className="deliveries-rows">
            {group.items.map((item) => (
              <DeliveryRow
                key={item.id}
                shipment={item}
                today={today}
                datePrefs={datePrefs}
                expanded={openRows.has(item.id)}
                onToggle={() => toggleRow(item.id)}
                navigate={navigate}
              />
            ))}
          </div>
        </section>
      ))}
    </>
  );
}

function DeliveryRow({
  shipment,
  today,
  datePrefs,
  expanded,
  onToggle,
  navigate
}: {
  shipment: Shipment;
  today: string;
  datePrefs: DatePrefs;
  expanded: boolean;
  onToggle: () => void;
  navigate: (url: string) => void;
}) {
  // The newest mail is the one that said what the row now claims, so it is the
  // one the row opens; the rest are shown only when the row is expanded.
  const latest = shipment.messages[0];
  const others = shipment.messages.slice(1);
  const window = shipment.window_start && shipment.window_end
    ? `${shipment.window_start}–${shipment.window_end} Uhr`
    : "";

  function open(event: { preventDefault: () => void }, url: string) {
    event.preventDefault();
    navigate(url);
  }

  return (
    <div className={`deliveries-row status-${shipment.status}`}>
      <div className="deliveries-row-main">
        <span className="deliveries-carrier" title={statusLabels[shipment.status]}>
          <Icon name="package" weight={shipment.status === "delivered" ? "regular" : "fill"} />
          {shipment.carrier_label}
        </span>
        <div className="deliveries-row-text">
          {latest ? (
            <a href={messageURL(latest.id)} className="deliveries-subject" onClick={(event) => open(event, messageURL(latest.id))}>
              {latest.subject || "(kein Betreff)"}
            </a>
          ) : null}
          <div className="deliveries-meta">
            <span className="deliveries-day">{dayLabel(shipment.expected_date, today)}</span>
            {window ? <span className="deliveries-window">{window}</span> : null}
            <span className={`deliveries-status status-${shipment.status}`}>{statusLabels[shipment.status]}</span>
          </div>
        </div>
        <div className="deliveries-row-actions">
          {shipment.tracking_url ? (
            <a
              className="secondary deliveries-track"
              href={shipment.tracking_url}
              target="_blank"
              rel="noreferrer noopener"
              title={`Sendung ${shipment.tracking_number} bei ${shipment.carrier_label} verfolgen`}
            >
              <Icon name="link" />
              Verfolgen
            </a>
          ) : null}
          <button
            className={expanded ? "ghost deliveries-expand expanded" : "ghost deliveries-expand"}
            type="button"
            aria-expanded={expanded}
            title={expanded ? "Weniger anzeigen" : "Sendungsnummer und alle Mails anzeigen"}
            onClick={onToggle}
          >
            <Icon name="expand_more" />
          </button>
        </div>
      </div>
      {expanded ? (
        <div className="deliveries-row-details">
          <dl>
            <dt>Sendungsnummer</dt>
            <dd><code>{shipment.tracking_number}</code></dd>
          </dl>
          {others.length > 0 ? (
            <div className="deliveries-history">
              <span className="deliveries-history-title">Weitere Mails zu dieser Sendung</span>
              {others.map((message) => (
                <a
                  key={message.id}
                  href={messageURL(message.id)}
                  className="deliveries-history-row"
                  onClick={(event) => open(event, messageURL(message.id))}
                >
                  <span className="deliveries-history-subject">{message.subject || "(kein Betreff)"}</span>
                  <span className="deliveries-history-date">{displayDateTime(message.date, datePrefs)}</span>
                </a>
              ))}
            </div>
          ) : null}
        </div>
      ) : null}
    </div>
  );
}
