/**
 * File overview: The header's answer to "is a parcel coming today". It is a
 * hook rather than part of the chrome payload because the question is asked in
 * days and the server has no timezone for a reader: the browser's own day is
 * the only one that puts a parcel on the right day, so the browser asks.
 *
 * It rides the mail generation, which is what already bumps when a sync stores
 * messages -- exactly when a parcel can appear, move, or arrive.
 */

import { useEffect, useState } from "react";
import { api } from "../../api";
import type { Shipment } from "../../types";
import { localDay, shipmentsExpectedOn } from "./DeliveriesView";

export type ExpectedDeliveries = {
  /** Parcels due today that have not been reported delivered. */
  count: number;
  /** The single parcel's carrier, when there is exactly one. It is what lets
   * the chip say "DHL" rather than "1 Paket", which is more use at a glance. */
  carrierLabel: string;
};

export function useExpectedDeliveries(mailGeneration: number): ExpectedDeliveries {
  const [shipments, setShipments] = useState<readonly Shipment[]>([]);
  const [today] = useState(() => localDay());

  useEffect(() => {
    let cancelled = false;
    void (async () => {
      try {
        const next = await api.deliveries(today);
        if (!cancelled) setShipments(next.shipments || []);
      } catch {
        // The chip is an extra. A reader whose parcel list cannot be read is
        // told so on the parcel page, not by an error in the header.
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [today, mailGeneration]);

  const due = shipmentsExpectedOn(shipments, today);
  return { count: due.length, carrierLabel: due.length === 1 ? due[0].carrier_label : "" };
}
