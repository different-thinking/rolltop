import { describe, expect, it } from "vitest";

import type { Shipment } from "../../types";
import { dayLabel, groupShipments, shipmentsExpectedOn } from "./DeliveriesView";
import { compactDay, shipmentChipText } from "./ShipmentChip";

const today = "2026-09-03";

function parcel(overrides: Partial<Shipment>): Shipment {
  return {
    id: 1,
    carrier: "dhl",
    carrier_label: "DHL",
    tracking_number: "00340434212345678901",
    tracking_url: "https://example.test/track",
    expected_date: "",
    window_start: "",
    window_end: "",
    status: "announced",
    messages: [{ id: 10, mailbox_id: 1, subject: "Versand", from: "noreply@dhl.de", date: "2026-09-01T10:00:00Z" }],
    ...overrides
  };
}

describe("shipmentsExpectedOn", () => {
  it("counts what is due today and has not arrived", () => {
    const shipments = [
      parcel({ id: 1, expected_date: today, status: "out_for_delivery" }),
      parcel({ id: 2, expected_date: today, status: "announced" }),
      parcel({ id: 3, expected_date: today, status: "delivered" }),
      parcel({ id: 4, expected_date: "2026-09-04" })
    ];
    expect(shipmentsExpectedOn(shipments, today).map((item) => item.id)).toEqual([1, 2]);
  });
});

describe("groupShipments", () => {
  it("puts today first and leaves out the groups with nothing in them", () => {
    const shipments = [
      parcel({ id: 1, expected_date: "2026-09-05" }),
      parcel({ id: 2, expected_date: today }),
      parcel({ id: 3, expected_date: "2026-09-01", status: "delivered" })
    ];
    expect(groupShipments(shipments, today).map((group) => group.key)).toEqual(["today", "upcoming", "delivered"]);
  });

  // A day that has passed with no delivery report is the one state a reader
  // has to act on, so it is its own group rather than buried under "on its way".
  it("separates a parcel whose announced day has passed", () => {
    const shipments = [parcel({ id: 1, expected_date: "2026-09-01", status: "announced" })];
    const groups = groupShipments(shipments, today);
    expect(groups.map((group) => group.key)).toEqual(["overdue"]);
  });

  it("keeps an undated parcel out of the dated groups", () => {
    const groups = groupShipments([parcel({ id: 1, expected_date: "" })], today);
    expect(groups.map((group) => group.key)).toEqual(["undated"]);
  });

  it("never lists one parcel twice", () => {
    const shipments = [
      parcel({ id: 1, expected_date: today }),
      parcel({ id: 2, expected_date: "2026-09-01", status: "announced" }),
      parcel({ id: 3, expected_date: "" }),
      parcel({ id: 4, expected_date: "2026-09-09" }),
      parcel({ id: 5, expected_date: "2026-08-30", status: "delivered" })
    ];
    const seen = groupShipments(shipments, today).flatMap((group) => group.items.map((item) => item.id));
    expect(seen.slice().sort()).toEqual([1, 2, 3, 4, 5]);
  });
});

describe("dayLabel", () => {
  it("says the days around today in words", () => {
    expect(dayLabel(today, today)).toBe("Heute");
    expect(dayLabel("2026-09-04", today)).toBe("Morgen");
    expect(dayLabel("2026-09-02", today)).toBe("Gestern");
  });

  it("names a weekday inside the coming week and a date beyond it", () => {
    expect(dayLabel("2026-09-07", today)).toContain("Montag");
    expect(dayLabel("2026-10-01", today)).toBe("01.10.2026");
  });

  it("says so when nothing was announced", () => {
    expect(dayLabel("", today)).toBe("Kein Termin genannt");
  });
});

describe("compactDay", () => {
  it("keeps the near days as the adverbs they are", () => {
    expect(compactDay(today, today)).toBe("heute");
    expect(compactDay("2026-09-04", today)).toBe("morgen");
  });

  // A weekday is a noun in German, so it is capitalised where "heute" is not.
  it("names a weekday inside the coming week", () => {
    expect(compactDay("2026-09-07", today)).toMatch(/^Mo\.? 07\.09\.$/);
  });

  it("gives only a date past the coming week, so the chip stays short", () => {
    expect(compactDay("2026-10-01", today)).toBe("01.10.");
  });
});

describe("shipmentChipText", () => {
  const base = {
    id: 1, carrier: "dhl", carrier_label: "DHL", tracking_number: "1", tracking_url: "",
    expected_date: today, status: "announced" as const, count: 1
  };

  it("leads with the day, which is what a reader scans for", () => {
    expect(shipmentChipText(base, today)).toBe("Paket: heute");
  });

  it("counts several parcels in one message", () => {
    expect(shipmentChipText({ ...base, count: 3 }, today)).toBe("3 Pakete: heute");
  });

  it("falls back to the status when no day was announced", () => {
    expect(shipmentChipText({ ...base, expected_date: "" }, today)).toBe("Paket angekündigt");
  });
});
