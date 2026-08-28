/**
 * File overview: The parcel marker on a message -- a chip on the list row and
 * the same fact spelled out in an open thread.
 *
 * It reads the shipment the server attached to the message rather than
 * anything the message itself says, so a row and the parcel list are always the
 * same answer: the row cannot claim a day the list has already moved on from.
 */

import type { MessageShipmentSummary } from "../../types";
import { Icon } from "../../components/Icon";
import { dayLabel, localDay } from "./DeliveriesView";

const statusLabels: Record<MessageShipmentSummary["status"], string> = {
  announced: "angekündigt",
  out_for_delivery: "in Zustellung",
  delivered: "zugestellt"
};

/** shipmentChipText is what the marker says: the day when there is one, because
 * that is the fact a reader is scanning for, and the status when there is not. */
export function shipmentChipText(shipment: MessageShipmentSummary, today: string): string {
  const parcels = shipment.count > 1 ? `${shipment.count} Pakete` : "Paket";
  if (shipment.status === "delivered") return `${parcels} zugestellt`;
  if (shipment.expected_date === "") return `${parcels} ${statusLabels[shipment.status]}`;
  return `${parcels}: ${dayLabel(shipment.expected_date, today).toLowerCase()}`;
}

/**
 * ShipmentChip marks a message in a list. A delivered parcel is left unmarked:
 * the list is scanned for what is still coming, and a chip on every arrived
 * parcel would be a column of noise the reader has to look past.
 */
export function ShipmentChip({ shipment }: { shipment: MessageShipmentSummary }) {
  if (shipment.status === "delivered") return null;
  const today = localDay();
  const dueToday = shipment.expected_date === today;
  return (
    <span
      className={`shipment-chip ${dueToday ? "due-today" : ""}`}
      title={`Sendung ${shipment.tracking_number} (${shipment.carrier_label}), ${statusLabels[shipment.status]}`}
    >
      <Icon name="package" weight={dueToday ? "fill" : "regular"} />
      <span className="shipment-chip-label">{shipmentChipText(shipment, today)}</span>
    </span>
  );
}

/**
 * ShipmentDetails is the same parcel in an open message, where there is room to
 * name it: the number, the carrier, the day, and the way out to both the
 * carrier's own tracking page and the reader's parcel list.
 */
export function ShipmentDetails({
  shipment,
  navigate
}: {
  shipment: MessageShipmentSummary;
  navigate: (url: string) => void;
}) {
  const today = localDay();
  return (
    <div className="shipment-details">
      <span className="shipment-details-head">
        <Icon name="package" weight="fill" />
        {shipment.carrier_label}
        {shipment.count > 1 ? <span className="shipment-details-count">{shipment.count} Pakete</span> : null}
      </span>
      <dl>
        <dt>Sendungsnummer</dt>
        <dd><code>{shipment.tracking_number}</code></dd>
        <dt>{shipment.status === "delivered" ? "Zugestellt" : "Erwartet"}</dt>
        <dd>{shipment.expected_date === "" ? statusLabels[shipment.status] : dayLabel(shipment.expected_date, today)}</dd>
      </dl>
      <div className="shipment-details-actions">
        {shipment.tracking_url ? (
          <a className="secondary" href={shipment.tracking_url} target="_blank" rel="noreferrer noopener">
            <Icon name="link" />
            Bei {shipment.carrier_label} verfolgen
          </a>
        ) : null}
        <a
          className="secondary"
          href="/deliveries"
          onClick={(event) => {
            event.preventDefault();
            navigate("/deliveries");
          }}
        >
          <Icon name="package" />
          Alle Pakete
        </a>
      </div>
    </div>
  );
}
