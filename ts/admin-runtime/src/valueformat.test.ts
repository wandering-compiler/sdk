import { describe, expect, it } from "vitest";
import vectors from "./formatVectors.json";
import {
  FALLBACK_FORMAT_LOCALE,
  type FormatKind,
  decimalString,
  expandExponent,
  formatDate,
  formatDateTime,
  formatDecimal,
  formatLocales,
  formatNumber,
  formatPercent,
  formatShortDate,
  formatTime,
  hasFormatLocale,
  parseTemporal,
  renderTimePattern,
  resolveFormats,
  resolveFormatsKind,
} from "./valueFormat";

// The load-bearing suite: these are the SAME vectors sdk/go/lib/i18n runs
// (format_vectors_test.go over testdata/format_vectors.json, kept identical by
// `make sync-i18n-formats`). Every case passing on both sides is what makes
// "Go and TS format a value identically" a checked property rather than a hope
// — see docs/specs/i18n/formatting.md, "an explicit format table, not Intl".

interface Vector {
  name: string;
  locale: string;
  kind: string;
  places?: number;
  value?: string;
  float?: number;
  want: string;
}

const CASES = (vectors as { cases: Vector[] }).cases;

// applyVector mirrors the Go test's dispatch. Keeping the two switches the same
// shape is deliberate: a kind handled differently on one side would make the
// shared vectors prove less than they appear to.
function applyVector(v: Vector): string {
  const f = resolveFormats(v.locale);
  const raw = v.float !== undefined ? decimalString(v.float) : (v.value ?? "");
  const places = v.places ?? -1;
  switch (v.kind) {
    case "number":
      return formatNumber(raw, f);
    case "decimal":
      return formatDecimal(raw, places, f);
    case "percent":
      return formatPercent(raw, places, f);
    case "date":
      return formatDate(raw, f);
    case "short_date":
      return formatShortDate(raw, f);
    case "time":
      return formatTime(raw, f);
    case "datetime":
      return formatDateTime(raw, f);
    default:
      return `unknown kind ${v.kind}`;
  }
}

describe("shared golden vectors", () => {
  it("carries the cases the Go twin runs", () => {
    expect(CASES.length).toBeGreaterThan(20);
  });

  it.each(CASES)("$name", (v) => {
    expect(applyVector(v)).toBe(v.want);
  });
});

describe("resolveFormats", () => {
  it.each([
    { locale: "cs", sep: ",", why: "exact row" },
    { locale: "en-US", sep: ".", why: "exact regional row" },
    { locale: "cs-CZ", sep: ",", why: "base-language fallback" },
    { locale: "pt-BR", sep: ",", why: "base-language fallback to pt" },
    { locale: "xx", sep: ".", why: "unknown falls back to en" },
    { locale: "", sep: ".", why: "empty falls back to en" },
  ])("$locale → $why", ({ locale, sep }) => {
    expect(resolveFormats(locale).decimal_separator).toBe(sep);
  });

  it("normalizes case and the underscore separator", () => {
    expect(resolveFormats("CS").thousand_separator).toBe(" ");
    expect(resolveFormats("cs_CZ").thousand_separator).toBe(" ");
    expect(resolveFormats("  cs  ").thousand_separator).toBe(" ");
  });

  it("exposes the shipped locale set, canonically spelled", () => {
    const locales = formatLocales();
    expect(locales).toContain("cs");
    expect(locales).toContain("en-US");
    expect(locales).toContain(FALLBACK_FORMAT_LOCALE);
    expect([...locales]).toEqual([...locales].sort());
  });

  it("hasFormatLocale answers about the table, without fallback walking", () => {
    expect(hasFormatLocale("cs")).toBe(true);
    expect(hasFormatLocale("CS")).toBe(true);
    expect(hasFormatLocale("en-US")).toBe(true);
    // formats fine via the base-language fallback, but has no row of its own
    expect(hasFormatLocale("cs-CZ")).toBe(false);
    expect(hasFormatLocale("")).toBe(false);
  });
});

describe("resolveFormatsKind", () => {
  const overrides = { cs: { number: "en" as const } };

  it("remaps one kind and leaves the rest on the language's row", () => {
    expect(resolveFormatsKind("cs", "number", overrides).thousand_separator).toBe(",");
    expect(resolveFormatsKind("cs", "date", overrides).date_format).toBe("DD.MM.YYYY");
  });

  it("treats a missing map as the identity mapping", () => {
    expect(resolveFormatsKind("cs", "number").thousand_separator).toBe(" ");
    expect(resolveFormatsKind("cs", "number", {}).thousand_separator).toBe(" ");
  });

  it("falls through when an override names an unknown locale", () => {
    // The lock parser rejects this; the runtime must degrade, not break.
    const bad: Record<string, Partial<Record<FormatKind, string>>> = { cs: { number: "zz" } };
    expect(resolveFormatsKind("cs", "number", bad).thousand_separator).toBe(" ");
  });
});

describe("decimalString", () => {
  it.each([
    { v: 1234.5, want: "1234.5" },
    { v: 1234, want: "1234" },
    { v: -0.005, want: "-0.005" },
    { v: 0, want: "0" },
    { v: 1e21, want: "1000000000000000000000" },
    { v: 1e-7, want: "0.0000001" },
    { v: -1.5e-7, want: "-0.00000015" },
    { v: 1.5e22, want: "15000000000000000000000" },
  ])("expands $v to a plain decimal", ({ v, want }) => {
    expect(decimalString(v)).toBe(want);
  });

  it("passes non-finite values through as themselves", () => {
    expect(decimalString(NaN)).toBe("NaN");
    expect(decimalString(Infinity)).toBe("Infinity");
  });
});

describe("expandExponent", () => {
  it.each([
    { s: "1e21", want: "1000000000000000000000" },
    { s: "1.5e-7", want: "0.00000015" },
    // Digits on both sides of the point — unreachable via String(number),
    // which only goes exponential where the point falls outside the digits.
    { s: "1.234e2", want: "123.4" },
    { s: "-9.87654e3", want: "-9876.54" },
    { s: "1E3", want: "1000" },
    { s: "not-a-number", want: "not-a-number" },
    { s: "1234.5", want: "1234.5" },
  ])("expands $s", ({ s, want }) => {
    expect(expandExponent(s)).toBe(want);
  });
});

describe("parseTemporal", () => {
  it("takes an offset off the clock and can move the date", () => {
    expect(parseTemporal("2026-07-27T00:30:00+02:00")).toEqual({
      year: 2026,
      month: 7,
      day: 26,
      hour: 22,
      minute: 30,
      second: 0,
    });
  });

  it("steps back into a 30-day month", () => {
    // June has 30 days — the daysInMonth branch a 31-day month never reaches.
    expect(parseTemporal("2026-07-01T00:30:00+02:00")).toMatchObject({
      year: 2026,
      month: 6,
      day: 30,
      hour: 22,
    });
  });

  it("steps forward over a year boundary", () => {
    expect(parseTemporal("2026-12-31T23:30:00-02:00")).toMatchObject({
      year: 2027,
      month: 1,
      day: 1,
      hour: 1,
    });
  });

  it("steps back over a month boundary", () => {
    expect(parseTemporal("2026-08-01T00:30:00+02:00")).toMatchObject({
      year: 2026,
      month: 7,
      day: 31,
      hour: 22,
    });
  });

  it("steps back over a year boundary", () => {
    expect(parseTemporal("2027-01-01T00:30:00+02:00")).toMatchObject({
      year: 2026,
      month: 12,
      day: 31,
      hour: 22,
    });
  });

  it("steps forward over a leap day", () => {
    expect(parseTemporal("2028-02-28T23:30:00-02:00")).toMatchObject({
      year: 2028,
      month: 2,
      day: 29,
      hour: 1,
    });
  });

  it("steps forward over a non-leap February", () => {
    expect(parseTemporal("2026-02-28T23:30:00-02:00")).toMatchObject({
      year: 2026,
      month: 3,
      day: 1,
      hour: 1,
    });
  });

  it("returns null for a value that is not temporal", () => {
    expect(parseTemporal("not-a-date")).toBeNull();
    expect(parseTemporal("")).toBeNull();
    expect(parseTemporal("26/07/2026")).toBeNull();
  });
});

describe("renderTimePattern", () => {
  const c = { year: 2026, month: 7, day: 6, hour: 9, minute: 3, second: 4 };

  it.each([
    { pattern: "YYYY-MM-DD", want: "2026-07-06" },
    { pattern: "D/M/YYYY", want: "6/7/2026" },
    { pattern: "HH:mm:ss", want: "09:03:04" },
    { pattern: "H:mm", want: "9:03" },
    { pattern: "h:mm A", want: "9:03 AM" },
    { pattern: "hh:mm A", want: "09:03 AM" },
    { pattern: "literal text", want: "literal text" }, // no tokens to hit
  ])("renders $pattern", ({ pattern, want }) => {
    expect(renderTimePattern(pattern, c)).toBe(want);
  });

  it("renders the afternoon meridiem", () => {
    expect(renderTimePattern("h:mm A", { ...c, hour: 15 })).toBe("3:03 PM");
  });
});

describe("grouping", () => {
  it("honours a non-three group size from the row", () => {
    const row = { ...resolveFormats("cs"), grouping: 4, thousand_separator: "_" };
    expect(formatNumber("123456789", row)).toBe("1_2345_6789");
  });

  it("skips grouping when the row has no separator", () => {
    const row = { ...resolveFormats("cs"), thousand_separator: "" };
    expect(formatNumber("123456789", row)).toBe("123456789");
  });
});
