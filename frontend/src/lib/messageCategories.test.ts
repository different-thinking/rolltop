import { describe, expect, it } from "vitest";

import { messageCategoryDisplay } from "./messageCategories";
import type { MailCategorySummary } from "../types";

const categories: MailCategorySummary[] = [
  { name: "relevant", label: "Relevant", icon: "person", total: 3, unread: 1 },
  { name: "invoices", label: "Invoices & Contracts", icon: "receipt", total: 2, unread: 0 },
  { name: "forums", label: "Forums", icon: "", total: 0, unread: 0 }
];

describe("messageCategoryDisplay", () => {
  it("names a stored category with the sidebar's label and icon", () => {
    expect(messageCategoryDisplay("invoices", categories)).toEqual({
      name: "invoices",
      label: "Invoices & Contracts",
      icon: "receipt"
    });
  });

  it("reports nothing while a message has no category yet", () => {
    expect(messageCategoryDisplay("", categories)).toBeNull();
    expect(messageCategoryDisplay(undefined, categories)).toBeNull();
    expect(messageCategoryDisplay("   ", categories)).toBeNull();
  });

  it("still names a category the chrome payload does not list", () => {
    expect(messageCategoryDisplay("receipts", categories)).toEqual({
      name: "receipts",
      label: "receipts",
      icon: "label"
    });
  });

  it("falls back to a generic icon when the entry carries none", () => {
    expect(messageCategoryDisplay("forums", categories)).toEqual({
      name: "forums",
      label: "Forums",
      icon: "label"
    });
  });
});
