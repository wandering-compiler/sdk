import { describe, expect, it } from "vitest";

import { formatCellValue } from "./cellFormat";
import { axisLabeler, measureFormatter } from "./chartData";
import { columnText } from "./listCell";
import { formatStatValue } from "./OverviewPage";
import {
  formatSlotValue,
  rowCurrency,
  sharedRowCurrency,
  templateCurrencyField,
  withRowCurrency,
} from "./slotFormat";
import type { FormatContext, FormatTemplate } from "./slotFormat";
import type { AdminPageSpec, AdminWidgetSpec } from "./types";
import { currencyCodes, formatMoney, lookupCurrency, normalizeCurrencyCode, resolveFormats } from "./valueFormat";

// MONEY end to end on the admin side: an int64 of minor units, a scale, and a
// currency that is either fixed on the slot or read off the row being shown.
// The arithmetic itself is pinned by the shared golden vectors
// (valueformat.test.ts); this file pins where the currency comes FROM.

const cs: FormatContext = { locale: "cs" };
const en: FormatContext = { locale: "en" };

const perRow: FormatTemplate = {
  msgid: "{value}",
  slots: [{ name: "value", preset: "money", scale: 2, currency_field: "currency" }],
};
const fixedCZK: FormatTemplate = {
  msgid: "{value}",
  slots: [{ name: "value", preset: "money", scale: 2, currency: "CZK" }],
};
const bare: FormatTemplate = {
  msgid: "{value}",
  slots: [{ name: "value", preset: "money", scale: 2 }],
};

describe("formatMoney", () => {
  it("shifts minor units by the scale and rounds to the currency", () => {
    expect(formatMoney("123450", 2, "CZK", resolveFormats("cs"))).toBe("1 234,50 Kč");
    expect(formatMoney("123450", 2, "JPY", resolveFormats("en"))).toBe("JP¥1,235");
  });

  it("never touches a float: an int64 past 2^53 keeps every digit", () => {
    expect(formatMoney("9007199254740993", 2, undefined, resolveFormats("en"))).toBe(
      "90,071,992,547,409.93",
    );
  });

  it("exposes the currencies table", () => {
    expect(lookupCurrency("kwd")?.decimals).toBe(3);
    expect(lookupCurrency("USD")?.symbol).toBe("US$");
    expect(lookupCurrency("ABC")).toBeUndefined();
    expect(normalizeCurrencyCode(" eur ")).toBe("EUR");
    const codes = currencyCodes();
    expect(codes.length).toBeGreaterThan(150);
    expect([...codes].sort()).toEqual(codes);
  });
});

describe("money slots", () => {
  it("a fixed currency wins over the row's", () => {
    const ctx = withRowCurrency(cs, fixedCZK, { currency: "EUR" });
    expect(formatSlotValue("123450", fixedCZK.slots![0], ctx)).toBe("1 234,50 Kč");
  });

  it("reads a per-row currency off the row", () => {
    expect(formatCellValue("123450", perRow, cs, undefined, { currency: "EUR" })).toBe(
      "1 234,50 €",
    );
    expect(formatCellValue("123450", perRow, en, undefined, { currency: "usd" })).toBe(
      "US$1,234.50",
    );
  });

  it("renders the bare scaled amount when the row carries no code", () => {
    expect(formatCellValue("123450", perRow, cs, undefined, { currency: "" })).toBe("1 234,50");
    expect(formatCellValue("123450", perRow, cs)).toBe("1 234,50");
    expect(formatCellValue("123450", bare, en)).toBe("1,234.50");
  });

  it("applies zero: to money", () => {
    const tmpl: FormatTemplate = {
      msgid: "{value}",
      slots: [{ name: "value", preset: "money", scale: 2, zero: { text: "free" } }],
    };
    expect(formatCellValue("0", tmpl, en)).toBe("free");
  });

  it("remaps with the decimal axis", () => {
    const ctx: FormatContext = { locale: "cs", overrides: { cs: { decimal: "en" } } };
    expect(formatCellValue("123450", bare, ctx)).toBe("1,234.50");
  });

  it("a JSON number of minor units renders its digits, no fraction invented", () => {
    expect(formatCellValue(123450, fixedCZK, cs)).toBe("1 234,50 Kč");
  });

  it("names the currency field only for per-row money", () => {
    expect(templateCurrencyField(perRow)).toBe("currency");
    expect(templateCurrencyField(fixedCZK)).toBeUndefined();
    expect(templateCurrencyField(bare)).toBeUndefined();
    expect(rowCurrency(perRow, { currency: 5 })).toBeUndefined();
    expect(withRowCurrency(cs, bare, { currency: "EUR" })).toBe(cs);
  });

  it("a set of rows shares a currency only when every row agrees", () => {
    expect(sharedRowCurrency(perRow, [{ currency: "eur" }, { currency: "EUR" }])).toBe("EUR");
    expect(sharedRowCurrency(perRow, [{ currency: "EUR" }, { currency: "CZK" }])).toBeUndefined();
    expect(sharedRowCurrency(perRow, [{ currency: "EUR" }, {}])).toBeUndefined();
    expect(sharedRowCurrency(perRow, [])).toBeUndefined();
  });
});

describe("money on admin surfaces", () => {
  it("a list column reads the row's currency", () => {
    const page = {
      name: "orders",
      list: { column_formats: { total: perRow } },
    } as unknown as AdminPageSpec;
    const t = (m: string) => m;
    expect(columnText(page, "total", "990", cs, t, { total: "990", currency: "CZK" })).toBe(
      "9,90 Kč",
    );
    expect(columnText(page, "total", "990", cs, t)).toBe("9,90");
  });

  it("a STAT tile reads the currency off the response it came from", () => {
    expect(formatStatValue("500000", perRow, en, undefined, { total: "500000", currency: "EUR" })).toBe(
      "€5,000.00",
    );
    expect(formatStatValue("500000", fixedCZK, cs)).toBe("5 000,00 Kč");
    // An omitted zero-valued total still reads as money.
    expect(formatStatValue(undefined, fixedCZK, cs)).toBe("0,00 Kč");
  });

  it("a chart's measures take the currency every row agrees on", () => {
    const widget = {
      kind: "CHART",
      series: [{ field: "total" }],
      value_formats: { total: perRow },
    } as unknown as AdminWidgetSpec;
    const one = measureFormatter(widget, en, undefined, [
      { total: "100", currency: "USD" },
      { total: "250", currency: "USD" },
    ]);
    expect(one?.(250)).toBe("US$2.50");
    const mixed = measureFormatter(widget, en, undefined, [
      { total: "100", currency: "USD" },
      { total: "250", currency: "EUR" },
    ]);
    expect(mixed?.(250)).toBe("2.50");
  });

  it("a money axis takes the shared currency too", () => {
    const label = axisLabeler(undefined, fixedCZK, cs);
    expect(label("1000")).toBe("10,00 Kč");
    const perRowLabel = axisLabeler(undefined, perRow, en, undefined, [{ currency: "GBP" }]);
    expect(perRowLabel("1000")).toBe("£10.00");
  });
});
