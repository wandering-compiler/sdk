import { describe, expect, it } from "vitest";

import { wireNumberText } from "./wireNumber";

// The shapes under test are the wire's, not JS's: an int64 is a decimal
// string, a 32-bit field is a number, a zero-valued field is absent.
// Every consumer of a numeric widget value goes through this one
// function precisely so those three cannot be handled differently in
// two places (see the module comment).
describe("wireNumberText", () => {
  it("passes a 32-bit field's JSON number through as digits", () => {
    expect(wireNumberText(0)).toBe("0");
    expect(wireNumberText(512)).toBe("512");
    expect(wireNumberText(-7)).toBe("-7");
    expect(wireNumberText(12.5)).toBe("12.5");
  });

  it("keeps an int64's decimal string exact, past 2^53", () => {
    expect(wireNumberText("5")).toBe("5");
    expect(wireNumberText("9007199254740993")).toBe("9007199254740993");
    expect(wireNumberText(" 42 ")).toBe("42");
  });

  it("expands exponent notation into digits a formatter can read", () => {
    expect(wireNumberText("1e3")).toBe("1000");
    expect(wireNumberText(1e21)).toBe("1000000000000000000000");
  });

  it("accepts a bigint (a decoder that hands one over is not a 0)", () => {
    expect(wireNumberText(10n)).toBe("10");
  });

  it("is undefined for anything that is not a number on the wire", () => {
    expect(wireNumberText(undefined)).toBeUndefined();
    expect(wireNumberText(null)).toBeUndefined();
    expect(wireNumberText("")).toBeUndefined();
    expect(wireNumberText("   ")).toBeUndefined();
    expect(wireNumberText("n/a")).toBeUndefined();
    expect(wireNumberText({})).toBeUndefined();
    expect(wireNumberText([1])).toBeUndefined();
    expect(wireNumberText(Number.NaN)).toBeUndefined();
    expect(wireNumberText(Number.POSITIVE_INFINITY)).toBeUndefined();
  });
});
