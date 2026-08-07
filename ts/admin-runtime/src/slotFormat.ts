/**
 * Template application — the runtime half of docs/specs/i18n/formatting.md,
 * one layer above valueFormat.ts.
 *
 * valueFormat.ts owns the frozen table and the formatters. This module owns
 * what the compiler hands a runtime: a lowered `(msgid, slots)` pair per
 * formatted field. The runtime never sees template syntax — the compiler
 * parsed it — so there is no parser here, only slot application and `{name}`
 * substitution.
 *
 * It is deliberately FREE of React and of anything admin-shaped, because it is
 * mirrored verbatim into the generated web client (the client emitter embeds
 * this file and valueFormat.ts rather than carrying a third implementation of
 * the same arithmetic — see srcgo/domains/client/emit/typescript/format.go).
 * The admin's own React context lives next door in cellFormat.ts, which
 * re-exports everything here so admin code has one import to reach for.
 */
import type { FormatKind, FormatOverrides, Formats } from "./valueFormat";
import {
  decimalString,
  formatDate,
  formatDateTime,
  formatDecimal,
  formatNumber,
  formatPercent,
  formatShortDate,
  formatTime,
  resolveFormatsKind,
} from "./valueFormat";

/** The closed filter set, as the compiler lowers it. */
export type FormatPreset =
  "number" | "decimal" | "percent" | "date" | "short_date" | "time" | "datetime";

/**
 * A substitution string carried by a slot. `translatable` records that the
 * author wrote `_("…")`; the compiler harvests those into the project's `.po`,
 * so a surface WITH a gettext catalog looks the msgid up before substituting
 * and one without renders both spellings verbatim.
 */
export interface FormatLiteral {
  text: string;
  translatable?: boolean;
}

/** One lowered `{…}` placeholder. */
export interface FormatSlot {
  name: string;
  preset?: FormatPreset;
  places?: number;
  /** Zero places is meaningful, so presence cannot be folded into the value. */
  has_places?: boolean;
  /** Substitutes when the value is null / absent. */
  default?: FormatLiteral;
  /** Substitutes when the value is numerically zero. */
  zero?: FormatLiteral;
}

/** A field's format: the msgid its text lowers to, plus its slots. */
export interface FormatTemplate {
  msgid: string;
  slots?: FormatSlot[];
}

/**
 * Everything formatting needs beyond the value itself: which locale to format
 * for, and the project's per-(language, kind) remap from the lock.
 */
export interface FormatContext {
  locale: string;
  overrides?: FormatOverrides;
}

/**
 * short_date shares the `date` override axis: a project that wants English
 * dates wants both spellings English.
 */
const PRESET_KIND: Record<FormatPreset, FormatKind> = {
  number: "number",
  decimal: "decimal",
  percent: "percent",
  date: "date",
  short_date: "date",
  time: "time",
  datetime: "datetime",
};

const NUMERIC: Record<string, boolean> = { number: true, decimal: true, percent: true };

/**
 * templateIsNumeric reports whether a template's slots are all numeric — i.e.
 * whether coercing the value to a decimal string before formatting is safe.
 *
 * A template with a `datetime` / `date` / `time` slot needs the RAW value: its
 * formatter parses a timestamp, and a value squeezed through a numeric coercion
 * first arrives as "0" (T2-6 pass #7, A2). A template with no slots at all is
 * pure literal text and needs nothing coerced either.
 */
export function templateIsNumeric(tmpl: FormatTemplate | undefined): boolean {
  const slots = tmpl?.slots ?? [];
  if (slots.length === 0) return false;
  return slots.every((s) => !s.preset || NUMERIC[s.preset]);
}

/**
 * Applies one slot to one raw value.
 *
 * Order matters and is the spec's: an ABSENT value takes `default` before
 * anything looks at its type, and a ZERO value takes `zero` before the number
 * is formatted — otherwise `zero:_("none")` would render "0" for the case it
 * exists to replace.
 *
 * `translate` is how a surface with a gettext catalog reaches one; a literal
 * the author spelled `_("…")` goes through it and a bare one never does, which
 * is what the flag exists for — "—" and "0" must not reach a translator.
 *
 * A value the formatter cannot parse passes through as its own string. A
 * malformed value must render as itself rather than as a lie — the same
 * philosophy as the admin's displayString.
 */
export function formatSlotValue(
  raw: unknown,
  slot: FormatSlot,
  ctx: FormatContext,
  translate?: (msgid: string) => string,
): string {
  if (raw == null || raw === "") {
    return slot.default ? literalText(slot.default, translate) : "";
  }
  const preset = slot.preset;
  const text = rawText(raw);
  if (!preset) return text;

  if (slot.zero && NUMERIC[preset] && isNumericZero(text)) {
    return literalText(slot.zero, translate);
  }
  const f = resolveFormatsKind(ctx.locale, PRESET_KIND[preset], ctx.overrides);
  return renderPreset(text, preset, slot, f);
}

function literalText(lit: FormatLiteral, translate?: (msgid: string) => string): string {
  return translate && lit.translatable ? translate(lit.text) : lit.text;
}

function renderPreset(text: string, preset: FormatPreset, slot: FormatSlot, f: Formats): string {
  // An absent `has_places` means natural precision, which the formatters
  // spell as a NEGATIVE places — not zero places, which would round away
  // every fraction digit.
  const places = slot.has_places ? (slot.places ?? 0) : -1;
  switch (preset) {
    case "number":
      return formatNumber(text, f);
    case "decimal":
      return formatDecimal(text, places, f);
    case "percent":
      return formatPercent(text, places, f);
    case "date":
      return formatDate(text, f);
    case "short_date":
      return formatShortDate(text, f);
    case "time":
      return formatTime(text, f);
    case "datetime":
      return formatDateTime(text, f);
  }
}

/**
 * The decimal string a raw JSON value formats from.
 *
 * A number goes through the float adapter (a MONEY carrier really is a
 * double); everything else is already a string on the wire. An object cannot
 * be formatted at all, and renders as JSON rather than as "[object Object]",
 * which reads like a bug in the data rather than in the declared type.
 */
function rawText(raw: unknown): string {
  if (typeof raw === "number") return decimalString(raw);
  if (typeof raw === "string") return raw;
  if (typeof raw === "boolean" || typeof raw === "bigint") return raw.toString();
  return JSON.stringify(raw) ?? "";
}

/**
 * Numerically zero, decided on the DECIMAL STRING rather than by parseFloat:
 * the formatting path never touches a float, and an int64 past 2^53 would
 * round on the way through one.
 */
function isNumericZero(text: string): boolean {
  const body = text.trim().replace(/^[+-]/, "");
  return /^[0-9]*\.?[0-9]*$/.test(body) && body !== "" && body !== "." && !/[1-9]/.test(body);
}

/**
 * Formats one value through its template: fills every slot, then substitutes
 * into the msgid.
 *
 * `translate` is how a surface with a gettext catalog reaches it — the admin
 * has none and passes nothing, the generated web client passes its own `t`.
 * Substitution itself matches what `i18n.T` / the client's `t` already do, so
 * a surface that gains a catalog changes where the string comes from and
 * nothing else.
 *
 * A template with no slots formats nothing; the caller falls back to its raw
 * rendering.
 */
export function formatTemplateValue(
  raw: unknown,
  tmpl: FormatTemplate | undefined,
  ctx: FormatContext,
  translate?: (msgid: string) => string,
): string | undefined {
  if (!tmpl || !tmpl.slots || tmpl.slots.length === 0) return undefined;
  let out = translate ? translate(tmpl.msgid) : tmpl.msgid;
  for (const slot of tmpl.slots) {
    out = out.split("{" + slot.name + "}").join(formatSlotValue(raw, slot, ctx, translate));
  }
  return out;
}
