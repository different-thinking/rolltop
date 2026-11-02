import { describe, expect, it } from "vitest";
import type { Trip, TripSegment } from "../../types";
import { groupJourneys, journeyCalendarStart, journeyPrefill, segmentEnd, segmentStart } from "./tripModel";
import { groupTrips } from "./TripsView";

function leg(partial: Partial<TripSegment>): TripSegment {
  return {
    id: 1,
    kind: "train",
    number: "",
    title: "",
    provider: "",
    from: "",
    from_code: "",
    to: "",
    to_code: "",
    venue: "",
    address: "",
    start: "",
    start_at: "",
    end: "",
    end_at: "",
    all_day: false,
    seat: "",
    seat_only: false,
    cancelled: false,
    calendar_event_id: 0,
    calendar_start: "",
    calendar_all_day: false,
    ...partial
  };
}

function trip(segments: TripSegment[], partial: Partial<Trip> = {}): Trip {
  return { id: 9, kind: segments[0].kind, reference: "", provider: "", segments, messages: [], ...partial };
}

const roundTrip = trip(
  [
    leg({ id: 1, number: "ICE 597", title: "ICE 597", from: "Berlin Hbf", to: "Erfurt Hbf", start: "2026-11-14T08:29", end: "2026-11-14T10:12", start_at: "2026-11-14T07:29:00Z", end_at: "2026-11-14T09:12:00Z", seat: "Coach 25, seat 45" }),
    leg({ id: 2, number: "ICE 1001", title: "ICE 1001", from: "Erfurt Hbf", to: "München Hbf", start: "2026-11-14T10:20", end: "2026-11-14T12:32", start_at: "2026-11-14T09:20:00Z", end_at: "2026-11-14T11:32:00Z" }),
    leg({ id: 3, number: "ICE 1006", title: "ICE 1006", from: "München Hbf", to: "Berlin Hbf", start: "2026-11-16T17:28", end: "2026-11-16T21:31", start_at: "2026-11-16T16:28:00Z", end_at: "2026-11-16T20:31:00Z" })
  ],
  { reference: "482910375512", provider: "Deutsche Bahn" }
);

describe("groupJourneys", () => {
  it("chains a change into one journey and labels a there-and-back", () => {
    const journeys = groupJourneys(roundTrip);
    expect(journeys.map((journey) => journey.segments.map((s) => s.id))).toEqual([[1, 2], [3]]);
    expect(journeys.map((journey) => journey.label)).toEqual(["Outbound", "Return"]);
  });

  it("keeps a seat reservation as its own journey", () => {
    const journeys = groupJourneys(trip([
      leg({ id: 1, number: "ICE 597", from: "A", to: "B", start: "2026-11-14T08:00", end: "2026-11-14T09:00" }),
      leg({ id: 2, number: "ICE 598", from: "B", to: "C", start: "2026-11-14T09:10", end: "2026-11-14T10:00", seat_only: true })
    ]));
    expect(journeys).toHaveLength(2);
  });

  it("counts a journey as in the calendar only when every leg is", () => {
    const [outbound] = groupJourneys(trip([
      { ...roundTrip.segments[0], calendar_event_id: 4, calendar_start: "2026-11-14T07:29:00Z" },
      roundTrip.segments[1]
    ]));
    expect(journeyCalendarStart(outbound)).toBe("");
  });
});

describe("journeyPrefill", () => {
  it("seeds the dialog with the whole journey and the way there and back", () => {
    const [outbound] = groupJourneys(roundTrip);
    const prefill = journeyPrefill(roundTrip, outbound);
    expect(prefill.start.toISOString()).toBe("2026-11-14T07:29:00.000Z");
    expect(prefill.end.toISOString()).toBe("2026-11-14T11:32:00.000Z");
    expect(prefill.summary).toBe("Berlin Hbf → München Hbf (ICE 597, ICE 1001)");
    expect(prefill.location).toBe("Berlin Hbf");
    expect(prefill.description).toContain("Booking: 482910375512");
    expect(prefill.description).toContain("ICE 597 · Berlin Hbf 08:29 → Erfurt Hbf 10:12 · Coach 25, seat 45");
    expect(prefill.travelBefore).toBe(30);
    expect(prefill.travelAfter).toBe(30);
    expect(prefill.allDay).toBe(false);
  });

  it("gives a flight time to reach the airport", () => {
    const flight = trip([leg({ kind: "flight", number: "LH 400", from: "Frankfurt", from_code: "FRA", to: "New York JFK", to_code: "JFK", start: "2026-11-14T10:35", start_at: "2026-11-14T09:35:00Z" })]);
    const prefill = journeyPrefill(flight, groupJourneys(flight)[0]);
    expect(prefill.summary).toBe("Flight LH 400 FRA → JFK");
    expect(prefill.location).toBe("Frankfurt (FRA)");
    expect(prefill.travelBefore).toBe(120);
    // No arrival on the ticket: the kind's usual length.
    expect(prefill.end.getTime() - prefill.start.getTime()).toBe(120 * 60_000);
  });

  it("enters a day-only event as all day, without travel time", () => {
    const festival = trip([leg({ kind: "event", title: "Festival", venue: "Park", start: "2026-12-05", end: "2026-12-06", all_day: true })]);
    const prefill = journeyPrefill(festival, groupJourneys(festival)[0]);
    expect(prefill.allDay).toBe(true);
    expect(prefill.start.toISOString()).toBe("2026-12-05T00:00:00.000Z");
    expect(prefill.end.toISOString()).toBe("2026-12-07T00:00:00.000Z");
    expect(prefill.travelBefore).toBe(0);
    expect(prefill.location).toBe("Park");
  });
});

describe("segment times", () => {
  it("reads a floating time as the wall clock in the reader's zone", () => {
    const floating = leg({ start: "2026-11-14T08:29" });
    const start = segmentStart(floating);
    expect([start.getHours(), start.getMinutes()]).toEqual([8, 29]);
    expect(segmentEnd(floating).getTime() - start.getTime()).toBe(60 * 60_000);
  });
});

describe("groupTrips", () => {
  it("puts what still has to go in the calendar first and the past last", () => {
    const added = trip([{ ...roundTrip.segments[2], calendar_event_id: 7 }], { id: 2 });
    const past = trip([leg({ id: 5, number: "RE 1", start: "2026-10-01T08:00" })], { id: 3 });
    const groups = groupTrips([roundTrip, added, past], "2026-11-02");
    expect(groups.map((group) => [group.key, group.items.map((item) => item.id)])).toEqual([
      ["add", [9]],
      ["coming", [2]],
      ["past", [3]]
    ]);
  });
});
