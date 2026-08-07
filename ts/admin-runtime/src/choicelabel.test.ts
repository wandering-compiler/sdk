import { describe, expect, it } from "vitest";

import { resolveChoiceLabel, translateChoiceLabel } from "./choiceLabel";
import type { AdminChoicesSpec } from "./types";

// The JSON wire carries a proto enum's NUMBER, so every surface that
// shows one has to resolve before it renders — otherwise the column (or
// the legend) that exists precisely to say WHY a ledger row happened
// prints "1".
describe("resolveChoiceLabel", () => {
  const choices: AdminChoicesSpec = {
    enum_fqn: "billing.LedgerReason",
    carrier: "scalar_int32",
    values: [
      { value: 1, label: "Refund" },
      { value: 2, label: "Topup" },
      { value: 6, label: "Chargeback" },
    ],
  };

  it("renders the label for the wire number", () => {
    expect(resolveChoiceLabel(choices, 1)).toBe("Refund");
    expect(resolveChoiceLabel(choices, 6)).toBe("Chargeback");
  });

  it("accepts the same value as a numeric string", () => {
    // 64-bit carriers, hand-built fixtures and hosts that re-serialize
    // a row all deliver "2" rather than 2.
    expect(resolveChoiceLabel(choices, "2")).toBe("Topup");
  });

  it("maps a repeated enum element-wise", () => {
    expect(resolveChoiceLabel(choices, [1, 2])).toEqual(["Refund", "Topup"]);
  });

  it("passes an unknown value through rather than blanking the cell", () => {
    // A server that added a member this build hasn't regenerated
    // against: a number the reader can look up beats an empty cell.
    expect(resolveChoiceLabel(choices, 99)).toBe(99);
  });

  it("leaves null / undefined alone", () => {
    expect(resolveChoiceLabel(choices, null)).toBe(null);
    expect(resolveChoiceLabel(choices, undefined)).toBe(undefined);
  });

  it("maps a string-carrier catalogue by name", () => {
    const byName: AdminChoicesSpec = {
      enum_fqn: "x.Kind",
      carrier: "scalar_string",
      values: [{ value: "ACTIVE", label: "Active" }],
    };
    expect(resolveChoiceLabel(byName, "ACTIVE")).toBe("Active");
  });
});

// The catalog seam: a resolved LABEL is author prose and goes through
// gettext; the raw value it fell back to must not, or a number that
// happens to match a msgid would come back as someone else's string.
describe("translateChoiceLabel", () => {
  const shout = (m: string) => m.toUpperCase();

  it("translates a resolved label", () => {
    expect(translateChoiceLabel("Refund", shout)).toBe("REFUND");
  });

  it("leaves an unresolved wire value alone", () => {
    expect(translateChoiceLabel(99, shout)).toBe(99);
  });
});
