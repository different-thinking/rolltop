/**
 * File overview: The trip list. The tickets the mail carried -- flights, train
 * journeys, coaches, concerts -- in the order they happen, each with its
 * journeys and the way into the calendar.
 *
 * It is a view of bookings, not of mail, for the parcel list's reason: the
 * booking, the ticket and the seat reservation are three messages and one trip.
 * The link back into the mail is what makes it a view of the mailbox all the
 * same.
 */

import { useCallback, useEffect, useMemo, useState } from "react";
import { api } from "../../api";
import type { AddToast, DatePrefs } from "../../appTypes";
import type { Trip } from "../../types";
import { Icon } from "../../components/Icon";
import { messageURL } from "../../lib/routes";
import { messageFromError } from "../../lib/errors";
import { displayDateTime } from "../../lib/format";
import { localDay } from "../deliveries/DeliveriesView";
import { TripJourneys, useTripCalendar } from "./TripCard";
import { notifyTripsChanged } from "./revision";
import { kindIcon, kindLabel, tripHasLegToAdd, tripUpcoming } from "./tripModel";

export type TripGroup = { key: string; title: string; hint: string; items: Trip[] };

/** groupTrips splits the list the way a reader reads it: what still has to go
 * in the calendar, what is coming and already there, and what is over. */
export function groupTrips(trips: readonly Trip[], today: string): TripGroup[] {
  const toAdd = trips.filter((trip) => tripUpcoming(trip, today) && tripHasLegToAdd(trip, today));
  const coming = trips.filter((trip) => tripUpcoming(trip, today) && !tripHasLegToAdd(trip, today));
  const past = trips.filter((trip) => !tripUpcoming(trip, today)).reverse();
  return [
    { key: "add", title: "Not in your calendar yet", hint: "Tickets for something still to come", items: toAdd },
    { key: "coming", title: "Coming up", hint: "Already in your calendar, or cancelled", items: coming },
    { key: "past", title: "Past", hint: "The past two weeks", items: past }
  ].filter((group) => group.items.length > 0);
}

export function TripsView({
  csrf,
  datePrefs,
  mailGeneration,
  navigate,
  addToast
}: {
  csrf: string;
  datePrefs: DatePrefs;
  mailGeneration: number;
  navigate: (url: string) => void;
  addToast: AddToast;
}) {
  const [trips, setTrips] = useState<Trip[] | null>(null);
  const [error, setError] = useState("");
  const [busy, setBusy] = useState<Set<number>>(() => new Set());
  const [today] = useState(() => localDay());

  const refresh = useCallback(async () => {
    try {
      const next = await api.trips(today);
      setTrips(next.trips || []);
      setError("");
    } catch (err) {
      setError(messageFromError(err));
    }
  }, [today]);

  useEffect(() => {
    void refresh();
  }, [refresh, mailGeneration]);

  const replaceTrip = useCallback((updated: Trip) => {
    setTrips((current) => (current || []).map((item) => (item.id === updated.id ? { ...updated, messages: updated.messages.length ? updated.messages : item.messages } : item)));
  }, []);
  const { open, dialog } = useTripCalendar({ csrf, addToast, onTripChanged: replaceTrip });

  const groups = useMemo(() => groupTrips(trips || [], today), [trips, today]);
  const upcomingCount = (trips || []).filter((trip) => tripUpcoming(trip, today)).length;

  async function dismiss(trip: Trip) {
    setBusy((current) => new Set(current).add(trip.id));
    try {
      await api.setTripManualStatus(csrf, trip.id, "dismissed");
      setTrips((current) => (current || []).filter((item) => item.id !== trip.id));
      notifyTripsChanged();
      addToast("No longer listed as a ticket.", "success", {
        label: "Undo",
        onCommit: () => undefined,
        onUndo: () => {
          void api
            .setTripManualStatus(csrf, trip.id, "")
            .then(() => {
              notifyTripsChanged();
              return refresh();
            })
            .catch((err) => addToast(messageFromError(err), "error"));
        }
      });
    } catch (err) {
      addToast(messageFromError(err), "error");
    } finally {
      setBusy((current) => {
        const next = new Set(current);
        next.delete(trip.id);
        return next;
      });
    }
  }

  return (
    <>
      <div className="content-head">
        <div>
          <h1>Trips &amp; tickets</h1>
          <span className="label-pill">{upcomingCount.toLocaleString()} coming up</span>
        </div>
      </div>

      {error ? <div className="error">{error}</div> : null}

      {trips !== null && groups.length === 0 && !error ? (
        <section className="panel deliveries-idle">
          <Icon name="ticket" />
          <div>
            <strong>No tickets found.</strong>
            <p>
              Rolltop reads flights, train journeys, coach trips and event tickets out of the mail: from
              the booking data airlines, rail operators and ticket shops embed in their messages, from
              Wallet passes, from Deutsche Bahn ticket and seat reservation PDFs, and from calendar files
              that come with a ticket. As soon as one arrives, it stands here, ready for your calendar.
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
          <div className="trip-rows">
            {group.items.map((trip) => {
              const latest = trip.messages[0];
              const provider = trip.provider || trip.segments[0]?.provider || "";
              return (
                <article className="trip-row" key={trip.id}>
                  <header className="trip-row-head">
                    <span className="trip-row-kind">
                      <Icon name={kindIcon(trip.kind)} weight="fill" />
                      {kindLabel[trip.kind]}
                    </span>
                    {provider ? <span className="trip-details-provider">{provider}</span> : null}
                    {trip.reference ? <code className="trip-details-reference" title="Booking reference">{trip.reference}</code> : null}
                    <button
                      className="ghost trip-row-dismiss"
                      type="button"
                      disabled={busy.has(trip.id)}
                      title="This is not a ticket of mine - stop showing it"
                      onClick={() => void dismiss(trip)}
                    >
                      <Icon name="close" />
                    </button>
                  </header>
                  <TripJourneys trip={trip} navigate={navigate} onAdd={(journeys) => void open(trip, journeys)} />
                  {latest ? (
                    <div className="trip-row-mail">
                      {trip.messages.map((message) => (
                        <a
                          key={message.id}
                          href={messageURL(message.id, "", [], "/trips")}
                          onClick={(event) => {
                            event.preventDefault();
                            navigate(messageURL(message.id, "", [], "/trips"));
                          }}
                        >
                          <Icon name="mail" />
                          <span className="trip-row-mail-subject">{message.subject || "(no subject)"}</span>
                          <span className="trip-row-mail-date">{displayDateTime(message.date, datePrefs)}</span>
                        </a>
                      ))}
                    </div>
                  ) : null}
                </article>
              );
            })}
          </div>
        </section>
      ))}
      {dialog}
    </>
  );
}
