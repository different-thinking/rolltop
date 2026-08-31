import { describe, expect, it } from "vitest";

import type { Invoice } from "../../types";
import { dueLabel, formatAmount, groupInvoices, invoicesDueOn } from "./InvoicesView";
import { compactDue, invoiceChipText } from "./InvoiceChip";

const today = "2026-09-03";

function bill(overrides: Partial<Invoice>): Invoice {
  return {
    id: 1,
    issuer: "shop.example.de",
    number: "2026-4711",
    due_date: "",
    manual_due_date: "",
    amount: "149.90",
    currency: "EUR",
    status: "open",
    manual_status: "",
    settlement: "transfer",
    dunning_level: 0,
    messages: [{ id: 10, mailbox_id: 1, subject: "Ihre Rechnung", from: "billing@shop.example.de", date: "2026-09-01T10:00:00Z" }],
    ...overrides
  };
}

describe("invoicesDueOn", () => {
  // The whole difference from the parcel chip: an overdue bill is still today's
  // business, where a late parcel is not.
  it("counts what is due today or already overdue", () => {
    const invoices = [
      bill({ id: 1, due_date: today }),
      bill({ id: 2, due_date: "2026-08-20" }),
      bill({ id: 3, due_date: "2026-09-30" }),
      bill({ id: 4, due_date: "" })
    ];
    expect(invoicesDueOn(invoices, today).map((item) => item.id)).toEqual([1, 2]);
  });

  it("counts a chased bill whatever its date says, including none", () => {
    const invoices = [
      bill({ id: 1, due_date: "", dunning_level: 2 }),
      bill({ id: 2, due_date: "2026-12-01", dunning_level: 1 })
    ];
    expect(invoicesDueOn(invoices, today).map((item) => item.id)).toEqual([1, 2]);
  });

  it("ignores a settled bill", () => {
    const invoices = [bill({ id: 1, due_date: today, status: "paid" })];
    expect(invoicesDueOn(invoices, today)).toEqual([]);
  });
});

describe("groupInvoices", () => {
  it("puts what is chased first and lists each bill once", () => {
    const invoices = [
      bill({ id: 1, due_date: "2026-08-01" }),
      bill({ id: 2, due_date: today }),
      bill({ id: 3, due_date: "2026-08-01", dunning_level: 2 }),
      bill({ id: 4, due_date: "2026-09-30" }),
      bill({ id: 5, due_date: "" }),
      bill({ id: 6, due_date: "2026-08-01", status: "paid" })
    ];
    const groups = groupInvoices(invoices, today);
    expect(groups.map((group) => group.key)).toEqual([
      "chased", "overdue", "today", "upcoming", "undated", "paid"
    ]);
    // A chased bill is overdue too, and must not appear in both groups.
    expect(groups.find((group) => group.key === "chased")?.items.map((item) => item.id)).toEqual([3]);
    expect(groups.find((group) => group.key === "overdue")?.items.map((item) => item.id)).toEqual([1]);
  });

  it("drops the groups that are empty", () => {
    expect(groupInvoices([bill({ id: 1, due_date: today })], today).map((group) => group.key)).toEqual(["today"]);
  });
});

describe("dueLabel", () => {
  it("counts the days an overdue bill has been late", () => {
    expect(dueLabel("2026-08-25", today)).toBe("Overdue by 9 days");
    expect(dueLabel("2026-09-02", today)).toBe("Overdue since yesterday");
  });

  it("names the near days the way a reader would", () => {
    expect(dueLabel(today, today)).toBe("Due today");
    expect(dueLabel("2026-09-04", today)).toBe("Due tomorrow");
  });

  it("says so when nothing was readable", () => {
    expect(dueLabel("", today)).toBe("No deadline given");
  });
});

describe("formatAmount", () => {
  it("renders the stored normal form as a written sum", () => {
    expect(formatAmount("1234.56", "EUR")).toContain("1,234.56");
    expect(formatAmount("149.90", "EUR")).toContain("149.90");
  });

  it("shows nothing when no total was readable", () => {
    expect(formatAmount("", "EUR")).toBe("");
  });

  it("survives a currency code the sender mistyped", () => {
    expect(formatAmount("10.00", "EURO")).toContain("10.00");
  });
});

describe("invoiceChipText", () => {
  it("says it is being chased rather than naming a date", () => {
    const chip = invoiceChipText(
      { id: 1, issuer: "x.de", number: "1", due_date: "2026-08-01", amount: "", currency: "", status: "open", settlement: "", dunning_level: 2 },
      today
    );
    expect(chip).toBe("Overdue notice");
  });

  it("names the deadline otherwise", () => {
    const chip = invoiceChipText(
      { id: 1, issuer: "x.de", number: "1", due_date: today, amount: "", currency: "", status: "open", settlement: "", dunning_level: 0 },
      today
    );
    expect(chip).toBe("Due: today");
  });
});

describe("compactDue", () => {
  it("counts overdue days rather than naming the weekday", () => {
    expect(compactDue("2026-08-25", today)).toBe("9 days overdue");
  });

  it("says one day in the singular", () => {
    expect(compactDue("2026-09-02", today)).toBe("1 day overdue");
  });

  it("keeps the near days short", () => {
    expect(compactDue(today, today)).toBe("today");
    expect(compactDue("2026-09-04", today)).toBe("tomorrow");
  });

  it("says a bill with no readable deadline is simply open", () => {
    expect(compactDue("", today)).toBe("open");
  });
});
