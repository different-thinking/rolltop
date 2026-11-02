/**
 * File overview: The header's answer to "is there a ticket I have not put in my
 * calendar yet". Asked in the reader's own day, on the mail generation (a sync
 * is when a ticket arrives) and on the trips revision (adding or dismissing one
 * here is the other way the answer changes).
 */

import { useEffect, useState } from "react";
import { api } from "../../api";
import { localDay } from "../deliveries/DeliveriesView";
import { useTripsRevision } from "./revision";

export type TripsToAdd = {
  count: number;
  /** title is the single trip's first leg, when there is exactly one. */
  title: string;
};

const nothing: TripsToAdd = { count: 0, title: "" };

export function useTripsToAdd(mailGeneration: number): TripsToAdd {
  const [pending, setPending] = useState<TripsToAdd>(nothing);
  const [today] = useState(() => localDay());
  const revision = useTripsRevision();

  useEffect(() => {
    let cancelled = false;
    void (async () => {
      try {
        const next = await api.tripsToAdd(today);
        if (!cancelled) setPending({ count: next.count || 0, title: next.title || "" });
      } catch {
        // The chip is an extra; the trip page reports a fault itself.
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [today, mailGeneration, revision]);

  return pending;
}
