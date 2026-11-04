// Editing a MONEY field.
//
// A MONEY value is an int64 count of MINOR units (12345 at scale 2 is
// 123.45), and on the JSON wire it is a decimal STRING. A form that let the
// operator edit that integer raw would show "12345" for 123.45 and store 1.23
// when they type "123" meaning 123 — the amount 100× off with nothing on the
// page to say so. The edit form therefore shows and accepts MAJOR units and
// converts both ways here, with string arithmetic only: an int64 past 2^53
// keeps every digit, and no float is involved at any step.
//
// The text is plain — no grouping, no symbol — because it is what the
// operator edits. Either `.` or `,` is accepted as the decimal separator, so
// a Czech "123,45" and an English "123.45" both mean 12345 at scale 2.

import type { FormatTemplate } from "./slotFormat";

/**
 * The minor-unit scale of a template's money slot, or undefined when the
 * template does not render money (the field is then not a MONEY field, or
 * declares no format the runtime can read a scale from).
 */
export function moneyInputScale(tmpl: FormatTemplate | undefined): number | undefined {
  for (const s of tmpl?.slots ?? []) {
    if (s.preset === "money") return s.scale !== undefined && s.scale > 0 ? Math.floor(s.scale) : 0;
  }
  return undefined;
}

/** The fixed currency code of a template's money slot, if it has one. */
export function moneyInputCurrency(tmpl: FormatTemplate | undefined): string | undefined {
  for (const s of tmpl?.slots ?? []) {
    if (s.preset === "money" && s.currency) return s.currency;
  }
  return undefined;
}

/**
 * The editable text of an amount of minor units: `"12345"` at scale 2 is
 * `"123.45"`, `"-5"` is `"-0.05"`. Empty for an absent value. A value that is
 * not a plain integer is returned as its string, unchanged, so a malformed
 * cell shows as itself rather than as a different amount.
 */
export function minorToMajorText(raw: unknown, scale: number): string {
  if (raw === null || raw === undefined) return "";
  if (typeof raw !== "string" && typeof raw !== "number" && typeof raw !== "bigint") {
    return JSON.stringify(raw);
  }
  const s = String(raw).trim();
  const m = /^([+-]?)(\d+)$/.exec(s);
  if (!m) return s;
  let digits = m[2].replace(/^0+/, "");
  const zero = digits === "";
  if (scale <= 0) return (m[1] === "-" && !zero ? "-" : "") + (zero ? "0" : digits);
  digits = digits.padStart(scale + 1, "0");
  const ipart = digits.slice(0, digits.length - scale);
  const fpart = digits.slice(digits.length - scale);
  return (m[1] === "-" && !zero ? "-" : "") + ipart + "." + fpart;
}

/**
 * The minor units an operator's typed amount means, as a decimal string —
 * `"123,45"` and `"123.45"` are `"12345"` at scale 2, `"123"` is `"12300"`.
 * Spaces (including the no-break spaces locales group with) are ignored.
 * Empty text is `""` (the field is cleared). Anything else — two separators,
 * a letter, more decimals than the scale holds — is `null`: not an amount,
 * and never rounded into one.
 */
export function majorTextToMinor(text: string, scale: number): string | null {
  const t = text.replace(/[\s\u00a0\u202f]/g, "");
  if (t === "") return "";
  const m = /^([+-]?)(\d*)(?:[.,](\d*))?$/.exec(t);
  if (!m) return null;
  const ipart = m[2];
  const fpart = m[3] ?? "";
  if (ipart === "" && fpart === "") return null;
  if (fpart.length > Math.max(scale, 0)) return null;
  const digits = ((ipart || "0") + fpart.padEnd(Math.max(scale, 0), "0")).replace(/^0+/, "");
  if (digits === "") return "0";
  return (m[1] === "-" ? "-" : "") + digits;
}
