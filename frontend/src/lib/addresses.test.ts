import { describe, expect, it } from "vitest";
import { formatAddress, parseAddress, parseAddressList } from "./addresses";

describe("parseAddressList", () => {
  it("splits plain and named entries", () => {
    expect(parseAddressList("Ann <ann@example.test>, bob@example.test")).toEqual([
      { name: "Ann", email: "ann@example.test" },
      { name: "", email: "bob@example.test" }
    ]);
  });

  it("keeps a comma inside a quoted name", () => {
    expect(parseAddressList('"Doe, John" <John@Example.test>, "Roe; Jane" <jane@example.test>')).toEqual([
      { name: "Doe, John", email: "john@example.test" },
      { name: "Roe; Jane", email: "jane@example.test" }
    ]);
  });

  it("unescapes a quoted name and drops comments", () => {
    expect(parseAddress('"Ann \\"the boss\\" Lee" (Sales) <ann@example.test>')).toEqual({ name: 'Ann "the boss" Lee', email: "ann@example.test" });
  });

  it("answers nothing for whitespace and keeps a group label without an address", () => {
    expect(parseAddressList(" , ")).toEqual([]);
    expect(parseAddressList("undisclosed-recipients:;")).toEqual([{ name: "undisclosed-recipients:", email: "" }]);
  });
});

describe("formatAddress", () => {
  it("writes a bare address when there is no name", () => {
    expect(formatAddress({ name: "", email: "ann@example.test" })).toBe("ann@example.test");
    expect(formatAddress({ name: "ann@example.test", email: "ann@example.test" })).toBe("ann@example.test");
  });

  it("quotes a name that needs it", () => {
    expect(formatAddress({ name: "Ann Lee", email: "ann@example.test" })).toBe("Ann Lee <ann@example.test>");
    expect(formatAddress({ name: "Lee, Ann", email: "ann@example.test" })).toBe('"Lee, Ann" <ann@example.test>');
    expect(formatAddress({ name: 'Ann "Boss"', email: "ann@example.test" })).toBe('"Ann \\"Boss\\"" <ann@example.test>');
  });
});
