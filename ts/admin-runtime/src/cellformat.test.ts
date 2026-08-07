import { describe, expect, it } from "vitest";

import {
  adminFormatLocale,
  formatCellValue,
  formatContextFor,
  formatSlotValue,
} from "./cellFormat";
import type { FormatContext, FormatSlot, FormatTemplate } from "./cellFormat";

// docs/specs/i18n/formatting.md — the runtime half of cell formatting: apply
// a compiler-lowered slot, then substitute into the msgid. There is no parser
// here on purpose; the compiler already did that.

const en: FormatContext = { locale: "en" };
const cs: FormatContext = { locale: "cs" };

const money: FormatSlot = { name: "value", preset: "decimal", places: 2, has_places: true };

describe("formatSlotValue", () => {
  it("formats through the frozen table, per locale", () => {
    expect(formatSlotValue("1234.5", money, en)).toBe("1,234.50");
    expect(formatSlotValue("1234.5", money, cs)).toBe("1 234,50");
  });

  it("takes a MONEY double through the decimal-string adapter", () => {
    expect(formatSlotValue(1234.5, money, en)).toBe("1,234.50");
  });

  // An int64 past 2^53 must survive: the formatting path never touches a
  // float, and the zero check reads the decimal string for the same reason.
  it("keeps every digit of a large integer", () => {
    const n: FormatSlot = { name: "value", preset: "number" };
    expect(formatSlotValue("9007199254740993", n, en)).toBe("9,007,199,254,740,993");
  });

  it("applies the project's remap per kind", () => {
    const ctx: FormatContext = { locale: "cs", overrides: { cs: { number: "en" } } };
    expect(formatSlotValue("1234.5", { name: "value", preset: "number" }, ctx)).toBe("1,234.5");
    // The remap named `number`, so dates stay Czech.
    expect(formatSlotValue("2026-07-26", { name: "value", preset: "date" }, ctx)).toBe(
      "26.07.2026",
    );
  });

  it("renders short_date through the date row", () => {
    expect(formatSlotValue("2026-07-26", { name: "value", preset: "short_date" }, cs)).toBe(
      "26.7.2026",
    );
  });

  it("renders the temporal presets", () => {
    const at = "2026-07-26T14:03:00Z";
    expect(formatSlotValue(at, { name: "value", preset: "time" }, cs)).toBe("14:03");
    expect(formatSlotValue(at, { name: "value", preset: "datetime" }, cs)).toBe("26.07.2026 14:03");
    expect(formatSlotValue(at, { name: "value", preset: "date" }, cs)).toBe("26.07.2026");
  });

  // An absent value takes `default` before anything looks at its type; with
  // no default it renders empty rather than "null".
  it("substitutes default for an absent value", () => {
    const slot: FormatSlot = { ...money, default: { text: "—" } };
    expect(formatSlotValue(null, slot, en)).toBe("—");
    expect(formatSlotValue(undefined, slot, en)).toBe("—");
    expect(formatSlotValue("", slot, en)).toBe("—");
    expect(formatSlotValue(null, money, en)).toBe("");
  });

  // `zero` replaces the number BEFORE it is formatted — otherwise the filter
  // that exists to hide "0" would render "0.00".
  it("substitutes zero for a numerically zero value, in every spelling", () => {
    const slot: FormatSlot = { ...money, zero: { text: "none", translatable: true } };
    for (const v of ["0", "0.00", "-0", "+0.0", 0]) {
      expect(formatSlotValue(v, slot, en)).toBe("none");
    }
    expect(formatSlotValue("0.01", slot, en)).toBe("0.01");
  });

  // A date has no numeric zero, and the compiler refuses `zero:` on one — but
  // the runtime must not invent a match if a hand-edited spec carries one.
  it("never applies zero to a temporal preset", () => {
    const slot: FormatSlot = { name: "value", preset: "date", zero: { text: "never" } };
    // `en` is day-first in the frozen table; `en-US` is the month-first row.
    expect(formatSlotValue("2026-07-26", slot, en)).toBe("26/07/2026");
    expect(formatSlotValue("2026-07-26", slot, { locale: "en-US" })).toBe("07/26/2026");
  });

  it("passes a malformed value through rather than lying about it", () => {
    expect(formatSlotValue("n/a", money, en)).toBe("n/a");
    expect(formatSlotValue("not-a-date", { name: "value", preset: "date" }, en)).toBe("not-a-date");
    // An object renders as JSON, the way displayString does — never
    // "[object Object]", which reads like a bug in the row rather than in
    // the column's declared type.
    expect(formatSlotValue({ nested: 1 }, money, en)).toBe('{"nested":1}');
  });

  // has_places absent means natural precision. Reading it as zero places
  // would round 12.3456 to 12 and look deliberate.
  it("treats a missing has_places as natural precision, not zero places", () => {
    expect(formatSlotValue("12.3456", { name: "value", preset: "percent" }, cs)).toBe("12,3456 %");
    expect(
      formatSlotValue("12.3456", { name: "value", preset: "percent", has_places: true }, cs),
    ).toBe("12 %");
  });

  it("leaves a slot with no preset alone", () => {
    expect(formatSlotValue("raw", { name: "value" }, en)).toBe("raw");
  });
});

describe("formatCellValue", () => {
  it("substitutes the formatted value into the msgid", () => {
    const tmpl: FormatTemplate = {
      msgid: "{value} of quota",
      slots: [{ name: "value", preset: "percent", places: 1, has_places: true }],
    };
    expect(formatCellValue("12.34", tmpl, cs)).toBe("12,3 % of quota");
  });

  it("uses the slot's own name, not a fixed one", () => {
    const tmpl: FormatTemplate = {
      msgid: "{qty}",
      slots: [{ name: "qty", preset: "number" }],
    };
    expect(formatCellValue("1234", tmpl, en)).toBe("1,234");
  });

  // Declining is how the caller knows to fall back to displayString. A cell
  // must never come back blank because its format had nothing to say.
  it("declines a template with nothing to fill", () => {
    expect(formatCellValue("x", undefined, en)).toBeUndefined();
    expect(formatCellValue("x", { msgid: "{value}" }, en)).toBeUndefined();
    expect(formatCellValue("x", { msgid: "{value}", slots: [] }, en)).toBeUndefined();
  });
});

describe("formatContextFor", () => {
  it("carries the spec's remap and resolves the viewer's locale", () => {
    const ctx = formatContextFor({
      default_language: "cs",
      format_overrides: { cs: { number: "en" } },
    });
    expect(ctx.overrides).toEqual({ cs: { number: "en" } });
    expect(ctx.locale).toBe(navigator.language || "cs");
  });
});

describe("adminFormatLocale", () => {
  // The browser's language IS the viewer's regional setting, and §3 is
  // explicit that formatting follows the region rather than the UI language.
  it("prefers the browser over the surface default", () => {
    expect(adminFormatLocale("cs")).toBe(navigator.language || "cs");
  });
});
