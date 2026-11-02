/**
 * File overview: A ticket on a message -- a chip on the list row, and in an open
 * message a card naming every journey with the way into the calendar.
 *
 * Adding opens the calendar's own event dialog, seeded with the journey and the
 * way there and back as travel time, rather than writing anything on a click:
 * the reader picks the calendar, can correct the time, and decides whether the
 * journey to the airport is two hours or none. Once saved, the event is
 * recorded against the legs so the card, the trip list and the header all say
 * it is in the calendar.
 */

import { useCallback, useEffect, useState } from "react";
import { createPortal } from "react-dom";
import { api } from "../../api";
import type { AddToast } from "../../appTypes";
import type { CalendarEventInput, CalendarSummary, MessageTripSummary, Trip } from "../../types";
import { Icon } from "../../components/Icon";
import { messageFromError } from "../../lib/errors";
import { EventDialog } from "../calendar/EventDialog";
import { startOfDay } from "../calendar/weekModel";
import { calendarDayURL } from "../calendar/mobileModel";
import { compactDay } from "../deliveries/ShipmentChip";
import { dayLabel, localDay } from "../deliveries/DeliveriesView";
import { notifyTripsChanged } from "./revision";
import {
  clock,
  groupJourneys,
  journeyCalendarStart,
  journeyCancelled,
  journeyPrefill,
  journeyStart,
  kindIcon,
  kindLabel,
  legDay,
  nextLeg,
  tripUpcoming,
  type Journey
} from "./tripModel";

/** tripChipText is what the row's marker says: the kind and the next day. */
export function tripChipText(trip: Trip, today: string): string {
  const leg = nextLeg(trip, today);
  return `${kindLabel[trip.kind]}: ${compactDay(legDay(leg), today)}`;
}

/** TripChip marks a message in a list. A trip that is over is left unmarked,
 * for the reason a delivered parcel is: the list is scanned for what is coming. */
export function TripChip({ trip }: { trip: MessageTripSummary }) {
  const today = localDay();
  if (!tripUpcoming(trip, today)) return null;
  const leg = nextLeg(trip, today);
  const soon = legDay(leg) === today;
  return (
    <span className={`trip-chip ${soon ? "due-today" : ""}`} title={`${leg.title}${leg.from && leg.to ? `, ${leg.from} → ${leg.to}` : ""}`}>
      <Icon name={kindIcon(trip.kind)} weight={soon ? "fill" : "regular"} />
      <span className="trip-chip-label">{tripChipText(trip, today)}</span>
    </span>
  );
}

/**
 * useTripCalendar owns the event dialog for a trip's journeys. open() takes
 * one journey or several; several are added one after the other, each in its
 * own dialog, so the way there and the way back become two entries. Closing a
 * dialog drops the ones still queued behind it.
 */
export function useTripCalendar({
  csrf,
  addToast,
  onTripChanged,
  onDialogOpenChange
}: {
  csrf: string;
  addToast: AddToast;
  onTripChanged: (trip: Trip) => void;
  onDialogOpenChange?: (open: boolean) => void;
}) {
  const [calendars, setCalendars] = useState<CalendarSummary[] | null>(null);
  const [queue, setQueue] = useState<{ trip: Trip; journeys: Journey[] } | null>(null);
  const [saving, setSaving] = useState(false);
  const [problem, setProblem] = useState("");
  const dialogOpen = queue !== null && queue.journeys.length > 0;

  useEffect(() => {
    if (!dialogOpen) return;
    onDialogOpenChange?.(true);
    return () => onDialogOpenChange?.(false);
  }, [dialogOpen, onDialogOpenChange]);

  const open = useCallback(
    async (trip: Trip, journeys: Journey[]) => {
      if (journeys.length === 0) return;
      setProblem("");
      try {
        const loaded = calendars || (await api.calendars()).calendars;
        setCalendars(loaded);
        if (!loaded.some((item) => item.can_write && item.listed)) {
          addToast("There is no calendar to add this to. Connect one under Settings first.", "error");
          return;
        }
        setQueue({ trip, journeys });
      } catch (err) {
        addToast(messageFromError(err), "error");
      }
    },
    [calendars, addToast]
  );

  const save = async (input: CalendarEventInput) => {
    if (!queue || queue.journeys.length === 0) return;
    const journey = queue.journeys[0];
    setSaving(true);
    setProblem("");
    try {
      const created = await api.createCalendarEvent(csrf, input);
      const target = (calendars || []).find((item) => item.id === input.calendar_id);
      const calendarName = target?.name || "your calendar";
      for (const warning of created.warnings || []) addToast(warning, "error");
      let trip = queue.trip;
      try {
        const linked = await api.linkTripToCalendar(
          csrf,
          queue.trip.id,
          journey.segments.map((leg) => leg.id),
          created.event.id
        );
        trip = linked.trip;
        onTripChanged(linked.trip);
        notifyTripsChanged();
      } catch {
        // The event exists; only the note that it came from this ticket was
        // lost. The card offers the journey again, which is the honest state.
      }
      addToast(`Added to ${calendarName}.`, "success");
      const rest = queue.journeys.slice(1);
      setQueue(rest.length > 0 ? { trip, journeys: rest } : null);
    } catch (err) {
      setProblem(messageFromError(err));
    } finally {
      setSaving(false);
    }
  };

  const current = queue && queue.journeys.length > 0 ? queue.journeys[0] : null;
  const dialog =
    queue && current && calendars
      ? createPortal(
          <EventDialog
            key={`trip-${queue.trip.id}-${current.key}`}
            event={null}
            day={startOfDay(journeyStart(current))}
            prefill={journeyPrefill(queue.trip, current)}
            calendars={calendars}
            saving={saving}
            problem={problem}
            onSave={(input) => void save(input)}
            onDelete={() => undefined}
            onRespond={() => undefined}
            onClose={() => {
              setQueue(null);
              setProblem("");
            }}
          />,
          document.body
        )
      : null;

  return { open, dialog };
}

/** TripJourneys draws a trip's journeys with their legs and the calendar
 * actions. It is shared by the card in a message and the trip list. */
export function TripJourneys({
  trip,
  navigate,
  onAdd
}: {
  trip: Trip;
  navigate: (url: string) => void;
  onAdd: (journeys: Journey[]) => void;
}) {
  const today = localDay();
  const journeys = groupJourneys(trip);
  const journeyPast = (journey: Journey) => legDay(journey.segments[journey.segments.length - 1]) < today;
  const addable = journeys.filter((journey) => !journeyCalendarStart(journey) && !journeyCancelled(journey) && !journeyPast(journey));
  return (
    <div className="trip-journeys">
      {journeys.map((journey) => {
        const added = journeyCalendarStart(journey);
        const cancelled = journeyCancelled(journey);
        const past = journeyPast(journey);
        return (
          <div className={`trip-journey ${cancelled ? "cancelled" : ""}`} key={journey.key}>
            <div className="trip-journey-head">
              <span className="trip-journey-day">
                {journey.label ? <strong>{journey.label} · </strong> : null}
                {dayLabel(legDay(journey.segments[0]), today)}
              </span>
              {cancelled ? <span className="trip-badge cancelled">Cancelled</span> : null}
              {journey.segments.some((leg) => leg.seat_only) ? <span className="trip-badge">Seat reservation</span> : null}
            </div>
            <ol className="trip-legs">
              {journey.segments.map((leg) => (
                <li key={leg.id} className={leg.cancelled ? "cancelled" : ""}>
                  {leg.kind === "event" ? (
                    <>
                      <span className="trip-leg-time">{leg.all_day ? "All day" : clock(leg.start)}</span>
                      <span className="trip-leg-route">
                        <strong>{leg.title}</strong>
                        {leg.venue || leg.address ? <span className="trip-leg-place">{[leg.venue, leg.address].filter(Boolean).join(", ")}</span> : null}
                      </span>
                    </>
                  ) : (
                    <>
                      <span className="trip-leg-time">
                        {clock(leg.start)}
                        {leg.end ? <>–{clock(leg.end)}</> : null}
                      </span>
                      <span className="trip-leg-route">
                        <strong>{leg.number || leg.title}</strong>
                        <span className="trip-leg-place">
                          {leg.from || leg.from_code} → {leg.to || leg.to_code}
                        </span>
                      </span>
                    </>
                  )}
                  {leg.seat ? <span className="trip-leg-seat">{leg.seat}</span> : null}
                </li>
              ))}
            </ol>
            <div className="trip-journey-actions">
              {added ? (
                <>
                  <span className="trip-added">
                    <Icon name="check" />
                    In your calendar
                  </span>
                  <button
                    className="secondary"
                    type="button"
                    onClick={() => {
                      const at = new Date(added);
                      const day = journey.segments[0].calendar_all_day || journey.segments[0].all_day
                        ? new Date(at.getUTCFullYear(), at.getUTCMonth(), at.getUTCDate())
                        : at;
                      navigate(calendarDayURL(Number.isNaN(day.getTime()) ? journeyStart(journey) : day));
                    }}
                  >
                    <Icon name="calendar" />
                    Show in calendar
                  </button>
                </>
              ) : !cancelled && !past ? (
                <button className="secondary" type="button" onClick={() => onAdd([journey])}>
                  <Icon name="add" />
                  Add to calendar
                </button>
              ) : null}
            </div>
          </div>
        );
      })}
      {addable.length > 1 ? (
        <div className="trip-journeys-all">
          <button className="secondary" type="button" onClick={() => onAdd(addable)}>
            <Icon name="calendar" />
            Add all {addable.length} to calendar
          </button>
        </div>
      ) : null}
    </div>
  );
}

/**
 * TripDetails is the ticket in an open message, above the message itself: the
 * mail around a booking is a page of conditions and advertising, and the
 * departure is what the reader opened it for.
 */
export function TripDetails({
  trip: initial,
  csrf,
  navigate,
  addToast,
  onDialogOpenChange
}: {
  trip: MessageTripSummary;
  csrf: string;
  navigate: (url: string) => void;
  addToast: AddToast;
  onDialogOpenChange?: (open: boolean) => void;
}) {
  const [trip, setTrip] = useState<Trip>(initial);
  useEffect(() => setTrip(initial), [initial]);
  const { open, dialog } = useTripCalendar({ csrf, addToast, onTripChanged: setTrip, onDialogOpenChange });
  const provider = trip.provider || trip.segments[0]?.provider || "";
  return (
    <div className="trip-details">
      <span className="trip-details-head">
        <Icon name={kindIcon(trip.kind)} weight="fill" />
        {kindLabel[trip.kind] === "Ticket" ? "Ticket" : `${kindLabel[trip.kind]} ticket`}
        {provider ? <span className="trip-details-provider">{provider}</span> : null}
        {trip.reference ? <code className="trip-details-reference" title="Booking reference">{trip.reference}</code> : null}
        {initial.count > 1 ? <span className="trip-details-count">{initial.count} bookings</span> : null}
      </span>
      <TripJourneys trip={trip} navigate={navigate} onAdd={(journeys) => void open(trip, journeys)} />
      <div className="trip-details-actions">
        <a
          className="secondary"
          href="/trips"
          onClick={(event) => {
            event.preventDefault();
            navigate("/trips");
          }}
        >
          <Icon name="ticket" />
          All trips &amp; tickets
        </a>
      </div>
      {dialog}
    </div>
  );
}
