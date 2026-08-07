// wireNumber — the ONE coercion from a wire value to a number, shared
// by every overview surface that has to render one.
//
// It exists because the same numeric field reaches the SPA in more
// than one shape. Both wires agree on which (json-dialect.md, and the
// generated codecs keep 64-bit fields as decimal strings so the binary
// wire decodes to the same thing):
//
//   - an int64/uint64 is a decimal STRING — "12", not 12,
//   - a 32-bit field is a JSON number,
//   - a zero-valued field is OMITTED entirely (EmitDefaultValues:false).
//
// There is no longer an exception. `w17.Paging.total` used to be one —
// restgw unquoted it back into a JSON number — and that single carve-out
// produced two independent bugs before it was removed (T2-6 pass #6): the pb
// codec never inherited it, so admin lists reported 0 rows on the default
// wire, and formatCount was narrowed to numbers-only for it, which broke
// StatWidget's other caller. It was also never applied consistently inside
// the JSON leg itself: the fast path returns before the unquoting step, so
// REST gateway responses carried the string all along.
//
// So one declared widget value legitimately arrives as 12, as "12", or
// not at all, and a coercion narrowed to one of those shapes renders
// every value of the other shape as 0 — silently, because 0 is a
// plausible count. That regression shipped once (formatCount narrowed
// to numbers-only when Paging.total became a number, while its other
// caller reads whatever int64 a project declared); this module is why
// it can no longer happen in one consumer without happening in both.
//
// The DECIMAL STRING is the primitive here, not the number: the
// formatting path never passes a value through a JS number, because an
// int64 past 2^53 rounds on the way through one (see valueFormat.ts).
// A caller that genuinely needs a JS number — a chart plots into one —
// converts at its own edge (chartData.ts::chartNumber).

import { decimalString, expandExponent } from "./valueFormat";

// wireNumberText normalises one wire value to a plain decimal string,
// or undefined when the value is not a number at all — absent, null,
// "", "n/a", an object. Callers decide what that means for them: the
// overview renders "0" (an omitted field IS a zero), a chart plots 0
// rather than letting a NaN take the axis down with it.
//
// Exponent notation is expanded on the way through, so a double large
// enough for protojson to render as "1e+21" still reaches a formatter
// that only speaks digits.
export function wireNumberText(v: unknown): string | undefined {
  if (typeof v === "number") return Number.isFinite(v) ? decimalString(v) : undefined;
  // Not a shape the generated codecs produce (they keep 64-bit fields
  // as strings), but a hand-written slot renderer may hand over what a
  // protobuf-es-style decoder would give it, and dropping that to 0
  // would be the same bug in a new place.
  if (typeof v === "bigint") return v.toString();
  if (typeof v === "string") {
    const text = v.trim();
    if (text === "" || !Number.isFinite(Number(text))) return undefined;
    return expandExponent(text);
  }
  return undefined;
}
