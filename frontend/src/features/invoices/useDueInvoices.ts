/**
 * File overview: The header's answer to "do I owe money". It is a hook rather
 * than part of the chrome payload for the same reason the parcel one is: the
 * question is asked in days and the server has no timezone for a reader, so the
 * browser's own day is the only one that puts a bill on the right side of its
 * deadline.
 *
 * It rides the mail generation, which is what already bumps when a sync stores
 * messages -- exactly when a bill can appear, fall due, or be settled.
 */

import { useEffect, useState } from "react";
import { api } from "../../api";
import { localDay } from "../deliveries/DeliveriesView";
import { useInvoicesRevision } from "./revision";

export type DueInvoices = {
  /** Bills that want paying: due today or earlier, plus anything being chased
   * whatever its date says. */
  count: number;
  /** How many of them somebody has written about again. It is what lets the
   * chip say the one thing that changes a reader's afternoon. */
  chased: number;
  /** The single bill's sender, when there is exactly one. */
  issuer: string;
};

const nothingDue: DueInvoices = { count: 0, chased: 0, issuer: "" };

export function useDueInvoices(mailGeneration: number): DueInvoices {
  const [due, setDue] = useState<DueInvoices>(nothingDue);
  const [today] = useState(() => localDay());
  // A sync is one way the answer changes; the reader ticking a bill off or
  // entering a deadline is the other, and only this carries that one.
  const revision = useInvoicesRevision();

  useEffect(() => {
    let cancelled = false;
    void (async () => {
      try {
        const next = await api.dueInvoices(today);
        if (!cancelled) {
          setDue({ count: next.count || 0, chased: next.chased || 0, issuer: next.issuer || "" });
        }
      } catch {
        // The chip is an extra. A reader whose bills cannot be read is told so
        // by the invoice page, which is the page about them; a failed chip that
        // shouted would be an error on every route in the app.
        if (!cancelled) setDue(nothingDue);
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [today, mailGeneration, revision]);

  return due;
}
