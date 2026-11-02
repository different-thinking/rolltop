/**
 * File overview: The arithmetic behind the ticket views -- which legs make one
 * journey, when a leg starts and ends as the browser draws it, and what the
 * calendar's event dialog is seeded with when a journey is added.
 *
 * A journey is what a reader puts in the calendar as one entry: the way there
 * with its change in Erfurt is one afternoon, not two appointments. So legs are
 * chained while each one leaves from where the last one arrived, soon after it
 * arrived; the way back, days later, is a second journey, and an event is always
 * its own.
 */

import type { EventPrefill } from "../calendar/EventDialog";
import type { Trip, TripKind, TripSegment } from "../../types";

/** Journey is one calendar entry's worth of a trip. */
export type Journey = {
  key: string;
  segments: TripSegment[];
  /** label is "Outbound" or "Return" when the trip is a there-and-back, empty
   * otherwise. */
  label: string;
};

/** maxChangeMinutes is how long a change between two legs may take and still
 * be one journey. A missed connection rebooked onto a later train is still the
 * same afternoon; an overnight stop is two days of travel. */
const maxChangeMinutes = 6 * 60;

/** travelDefaults are the minutes of journey to the departure and from the
 * arrival a new entry starts with. A flight needs the way to the airport and
 * check-in before it; everything else gets half an hour either side. The
 * reader edits or unticks them in the dialog. */
export const travelDefaults: Record<TripKind, { before: number; after: number }> = {
  flight: { before: 120, after: 60 },
  train: { before: 30, after: 30 },
  bus: { before: 30, after: 30 },
  event: { before: 30, after: 30 }
};

/** defaultMinutes is how long a leg is assumed to take when the ticket names
 * no end. */
const defaultMinutes: Record<TripKind, number> = { flight: 120, train: 60, bus: 60, event: 180 };

/** segmentStart is the leg's start as the browser draws it. An all-day leg is
 * the UTC midnight of its date, which is how the calendar stores one; a timed
 * leg with no stated zone is the wall clock in the reader's own zone. */
export function segmentStart(segment: TripSegment): Date {
  if (segment.all_day) return utcDay(segment.start);
  if (segment.start_at) return new Date(segment.start_at);
  return new Date(segment.start);
}

/** segmentEnd is the leg's end, or the kind's usual length after its start. An
 * all-day leg ends at the UTC midnight after its last day. */
export function segmentEnd(segment: TripSegment): Date {
  if (segment.all_day) {
    const last = segment.end ? utcDay(segment.end) : utcDay(segment.start);
    return new Date(last.getTime() + 86_400_000);
  }
  const start = segmentStart(segment);
  let end: Date | null = null;
  if (segment.end_at) end = new Date(segment.end_at);
  else if (segment.end) end = new Date(segment.end);
  if (!end || Number.isNaN(end.getTime()) || end <= start) {
    end = new Date(start.getTime() + defaultMinutes[segment.kind] * 60_000);
  }
  return end;
}

function utcDay(day: string): Date {
  const [year, month, date] = day.slice(0, 10).split("-").map(Number);
  return new Date(Date.UTC(year, (month || 1) - 1, date || 1));
}

/** sameStop compares two stations or airports loosely: "Berlin Hbf" and
 * "Berlin Hbf (tief)" are one place, and an airport code decides when both
 * legs carry one. */
function sameStop(arrivalName: string, arrivalCode: string, departureName: string, departureCode: string): boolean {
  if (arrivalCode && departureCode) return arrivalCode === departureCode;
  const a = arrivalName.toLowerCase().replace(/\(.*?\)/g, "").trim();
  const b = departureName.toLowerCase().replace(/\(.*?\)/g, "").trim();
  if (!a || !b) return true;
  return a === b || a.startsWith(b) || b.startsWith(a);
}

/** groupJourneys chains a trip's legs into journeys. */
export function groupJourneys(trip: Trip): Journey[] {
  const legs = [...trip.segments].sort((a, b) => segmentStart(a).getTime() - segmentStart(b).getTime());
  const journeys: Journey[] = [];
  for (const leg of legs) {
    const current = journeys[journeys.length - 1];
    const previous = current?.segments[current.segments.length - 1];
    if (previous && leg.kind !== "event" && previous.kind === leg.kind && !previous.seat_only && !leg.seat_only) {
      const gap = (segmentStart(leg).getTime() - segmentEnd(previous).getTime()) / 60_000;
      if (gap >= 0 && gap <= maxChangeMinutes && sameStop(previous.to, previous.to_code, leg.from, leg.from_code)) {
        current.segments.push(leg);
        continue;
      }
    }
    journeys.push({ key: `${leg.id}`, segments: [leg], label: "" });
  }
  // A there-and-back is labelled as one: two journeys of the same kind, the
  // second ending where the first began.
  if (journeys.length === 2 && trip.kind !== "event") {
    const [out, back] = journeys;
    const origin = out.segments[0];
    const end = back.segments[back.segments.length - 1];
    if (sameStop(end.to, end.to_code, origin.from, origin.from_code) && (end.to || end.to_code)) {
      out.label = "Outbound";
      back.label = "Return";
    }
  }
  return journeys;
}

/** journeyStart and journeyEnd are the journey's bounds as the dialog takes them. */
export function journeyStart(journey: Journey): Date {
  return segmentStart(journey.segments[0]);
}

export function journeyEnd(journey: Journey): Date {
  return segmentEnd(journey.segments[journey.segments.length - 1]);
}

/** journeyInCalendar is the event a journey was added as, when every one of
 * its legs was: one event covers the whole journey, and a leg added on its own
 * before a change was rebooked does not make the journey added. */
export function journeyCalendarStart(journey: Journey): string {
  if (journey.segments.some((leg) => !leg.calendar_event_id)) return "";
  return journey.segments[0].calendar_start || journey.segments[0].start_at || journey.segments[0].start;
}

/** journeyCancelled is a journey every leg of which was cancelled. */
export function journeyCancelled(journey: Journey): boolean {
  return journey.segments.every((leg) => leg.cancelled);
}

/** stop names a station or an airport the way a ticket does. */
function stop(name: string, code: string): string {
  if (name && code && !name.includes(code)) return `${name} (${code})`;
  return name || code;
}

/** journeyTitle is the entry's title: where to where, and on what. */
export function journeyTitle(journey: Journey): string {
  const first = journey.segments[0];
  const last = journey.segments[journey.segments.length - 1];
  if (first.kind === "event") return first.title;
  const numbers = journey.segments.map((leg) => leg.number || leg.title).filter(Boolean);
  const from = first.from_code || first.from;
  const to = last.to_code || last.to;
  const route = from && to ? `${from} → ${to}` : "";
  if (first.kind === "flight") {
    return [`Flight ${numbers.join(" / ")}`.trim(), route].filter(Boolean).join(" ");
  }
  if (!route) return numbers.join(", ");
  return numbers.length > 0 ? `${route} (${numbers.join(", ")})` : route;
}

/** clock is the time part of a ticket's wall-clock time. */
export function clock(value: string): string {
  return value.length >= 16 ? value.slice(11, 16) : "";
}

/** legLine describes one leg in a line, as the entry's notes carry it. */
export function legLine(leg: TripSegment): string {
  if (leg.kind === "event") {
    return [leg.title, leg.all_day ? "" : clock(leg.start), leg.seat].filter(Boolean).join(" · ");
  }
  const departure = [stop(leg.from, leg.from_code), clock(leg.start)].filter(Boolean).join(" ");
  const arrival = [stop(leg.to, leg.to_code), clock(leg.end)].filter(Boolean).join(" ");
  const route = departure && arrival ? `${departure} → ${arrival}` : departure || arrival;
  return [leg.number || leg.title, route, leg.seat, leg.seat_only ? "seat reservation only" : ""]
    .filter(Boolean)
    .join(" · ");
}

/** journeyPrefill seeds the event dialog with a journey: its bounds, title,
 * the place it starts from, what the ticket said, and the way there and back
 * as travel time the reader can change or untick. */
export function journeyPrefill(trip: Trip, journey: Journey): EventPrefill {
  const first = journey.segments[0];
  const allDay = first.all_day;
  const location =
    first.kind === "event"
      ? [first.venue, first.address].filter(Boolean).join(", ")
      : stop(first.from, first.from_code);
  const notes: string[] = [];
  const provider = first.provider || trip.provider;
  if (provider) notes.push(provider);
  if (trip.reference) notes.push(`Booking: ${trip.reference}`);
  for (const leg of journey.segments) notes.push(legLine(leg));
  const travel = travelDefaults[first.kind];
  return {
    start: journeyStart(journey),
    end: journeyEnd(journey),
    allDay,
    summary: journeyTitle(journey),
    location,
    description: notes.join("\n"),
    travelBefore: allDay ? 0 : travel.before,
    travelAfter: allDay ? 0 : travel.after
  };
}

/** kindIcon is the glyph for a kind of ticket. */
export function kindIcon(kind: TripKind): string {
  switch (kind) {
    case "flight":
      return "travel";
    case "train":
      return "train";
    case "bus":
      return "bus";
    default:
      return "ticket";
  }
}

/** kindLabel names a kind of ticket in a chip. */
export const kindLabel: Record<TripKind, string> = {
  flight: "Flight",
  train: "Train",
  bus: "Coach",
  event: "Ticket"
};

/** legDay is the plain day a leg starts on, as the ticket printed it. */
export function legDay(leg: TripSegment): string {
  return leg.start.slice(0, 10);
}

/** tripUpcoming reports a trip with a leg on or after today. */
export function tripUpcoming(trip: Trip, today: string): boolean {
  return trip.segments.some((leg) => legDay(leg) >= today || (leg.end && leg.end.slice(0, 10) >= today));
}

/** nextLeg is the first leg still to come, or the last one when all are past. */
export function nextLeg(trip: Trip, today: string): TripSegment {
  return trip.segments.find((leg) => legDay(leg) >= today && !leg.cancelled) || trip.segments[trip.segments.length - 1];
}

/** tripHasLegToAdd matches the server's count for the header chip. */
export function tripHasLegToAdd(trip: Trip, today: string): boolean {
  return trip.segments.some((leg) => !leg.cancelled && !leg.calendar_event_id && legDay(leg) >= today);
}
