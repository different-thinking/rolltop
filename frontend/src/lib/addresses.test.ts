import { describe, expect, it } from "vitest";
import { formatAddress, parseAddress, parseAddressList, splitAddressList } from "./addresses";

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

  it("unescapes a quoted name the way Go wrote it and drops comments outside quotes", () => {
    expect(parseAddress('"Ann \\"the boss\\" Lee" (Sales) <ann@example.test>')).toEqual({ name: 'Ann "the boss" Lee', email: "ann@example.test" });
    expect(parseAddress('"Ann\\u00a0Lee" <ann@example.test>')).toEqual({ name: "Ann\u00a0Lee", email: "ann@example.test" });
    expect(parseAddress('"M\\xfcller" <m@example.test>')).toEqual({ name: "Müller", email: "m@example.test" });
    expect(parseAddress('"Smith, John (Acme)" <j@acme.test>')).toEqual({ name: "Smith, John (Acme)", email: "j@acme.test" });
  });

  it("reads a bare address beside a comment and the members of a group", () => {
    expect(parseAddress("bob@example.test (Bob Smith)")).toEqual({ name: "", email: "bob@example.test" });
    expect(parseAddressList("Team: alice@x.test, bob@x.test;")).toEqual([
      { name: "", email: "alice@x.test" },
      { name: "", email: "bob@x.test" }
    ]);
  });

  it("answers nothing for whitespace or an empty pair, and keeps a label without an address", () => {
    expect(parseAddressList(" , ")).toEqual([]);
    expect(parseAddressList("<>, bob@example.test")).toEqual([{ name: "", email: "bob@example.test" }]);
    expect(parseAddressList("undisclosed-recipients:;")).toEqual([{ name: "undisclosed-recipients:", email: "" }]);
  });

  it("splits the raw entries for the composer's chips", () => {
    expect(splitAddressList('"Doe, John" <john@x.test>; bob@x.test,, ')).toEqual(['"Doe, John" <john@x.test>', "bob@x.test"]);
  });
});

describe("formatAddress", () => {
  it("writes a bare address when there is no name", () => {
    expect(formatAddress({ name: "", email: "ann@example.test" })).toBe("ann@example.test");
    expect(formatAddress({ name: "ann@example.test", email: "ann@example.test" })).toBe("ann@example.test");
  });

  it("quotes a name that needs it, without escapes the chip would show", () => {
    expect(formatAddress({ name: "Ann Lee", email: "ann@example.test" })).toBe("Ann Lee <ann@example.test>");
    expect(formatAddress({ name: "Lee, Ann", email: "ann@example.test" })).toBe('"Lee, Ann" <ann@example.test>');
    expect(formatAddress({ name: 'Ann "Boss"', email: "ann@example.test" })).toBe("\"Ann 'Boss'\" <ann@example.test>");
  });
});
