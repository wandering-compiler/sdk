/**
 * Template application — the runtime half of locale-aware value formatting,
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
 * the same arithmetic).
 * The admin's own React context lives next door in cellFormat.ts, which
 * re-exports everything here so admin code has one import to reach for.
 */
import type { FormatKind, FormatOverrides, Formats } from "./valueFormat";
import {
  decimalString,
  formatDate,
  formatDateTime,
  formatDecimal,
  formatMoney,
  formatNumber,
  formatPercent,
  formatShortDate,
  formatTime,
  normalizeCurrencyCode,
  resolveFormatsKind,
} from "./valueFormat";

/** The closed filter set, as the compiler lowers it. */
export type FormatPreset =
  | "number"
  | "decimal"
  | "percent"
  | "money"
  | "date"
  | "short_date"
  | "time"
  | "datetime";

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
  /**
   * `money` only: the minor-unit scale of the value (12345 at scale 2 is
   * 123.45). Set on every money slot the compiler emits.
   */
  scale?: number;
  /** `money` only: a fixed ISO 4217 code, uppercase. Wins over the row's. */
  currency?: string;
  /**
   * `money` only: the field of the SAME row/object that carries the code,
   * when the currency is per row. The caller reads it off the row into
   * FormatContext.currency (see withRowCurrency).
   */
  currency_field?: string;
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
  /**
   * The currency of the row being rendered — the value of a money slot's
   * `currency_field` on that row, resolved by the caller (withRowCurrency).
   * A slot's own fixed `currency` wins over it.
   */
  currency?: string;
}

/**
 * short_date shares the `date` override axis: a project that wants English
 * dates wants both spellings English.
 */
const PRESET_KIND: Record<FormatPreset, FormatKind> = {
  number: "number",
  decimal: "decimal",
  percent: "percent",
  // Money remaps with the decimal axis: "English-looking numbers" means the
  // amounts too, and the lock has no separate money kind.
  money: "decimal",
  date: "date",
  short_date: "date",
  time: "time",
  datetime: "datetime",
};

const NUMERIC: Record<string, boolean> = {
  number: true,
  decimal: true,
  percent: true,
  money: true,
};

/**
 * The `currency_field` of a template's money slot — the row field the caller
 * has to read the currency from — or undefined when the template is not per-row
 * money (no money slot, or one with a fixed currency).
 */
export function templateCurrencyField(tmpl: FormatTemplate | undefined): string | undefined {
  for (const s of tmpl?.slots ?? []) {
    if (s.preset === "money" && !s.currency && s.currency_field) return s.currency_field;
  }
  return undefined;
}

/**
 * The currency a row carries for a template: `row[currency_field]` when it is
 * a non-empty string, else undefined (the value then renders without a
 * symbol rather than in a guessed currency).
 */
export function rowCurrency(
  tmpl: FormatTemplate | undefined,
  row: Record<string, unknown> | undefined | null,
): string | undefined {
  const field = templateCurrencyField(tmpl);
  if (!field || !row) return undefined;
  const v = row[field];
  return typeof v === "string" && v.trim() !== "" ? v : undefined;
}

/**
 * `ctx` with the row's currency filled in for `tmpl` — what every surface that
 * renders a value off a row (a list cell, a detail field, a STAT tile) passes
 * to formatTemplateValue. Returns `ctx` itself when nothing changes.
 */
export function withRowCurrency(
  ctx: FormatContext,
  tmpl: FormatTemplate | undefined,
  row: Record<string, unknown> | undefined | null,
): FormatContext {
  const currency = rowCurrency(tmpl, row);
  return currency === undefined ? ctx : { ...ctx, currency };
}

/**
 * The one currency a SET of rows agrees on for `tmpl` — for a chart, whose
 * value formatter sees a number and no row. Rows that disagree (or a row with
 * no code) give undefined, so a mixed-currency chart shows bare amounts rather
 * than one currency's symbol on another's numbers.
 */
export function sharedRowCurrency(
  tmpl: FormatTemplate | undefined,
  rows: readonly Record<string, unknown>[] | undefined,
): string | undefined {
  if (!templateCurrencyField(tmpl) || !rows || rows.length === 0) return undefined;
  let agreed: string | undefined;
  for (const row of rows) {
    const c = rowCurrency(tmpl, row);
    if (c === undefined) return undefined;
    const norm = normalizeCurrencyCode(c);
    if (agreed === undefined) agreed = norm;
    else if (agreed !== norm) return undefined;
  }
  return agreed;
}

/**
 * templateIsNumeric reports whether a template's slots are all numeric — i.e.
 * whether coercing the value to a decimal string before formatting is safe.
 *
 * A template with a `datetime` / `date` / `time` slot needs the RAW value: its
 * formatter parses a timestamp, and a value squeezed through a numeric coercion
 * first arrives as "0". A template with no slots at all is
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
  return renderPreset(text, preset, slot, f, ctx);
}

function literalText(lit: FormatLiteral, translate?: (msgid: string) => string): string {
  return translate && lit.translatable ? translate(lit.text) : lit.text;
}

function renderPreset(
  text: string,
  preset: FormatPreset,
  slot: FormatSlot,
  f: Formats,
  ctx: FormatContext,
): string {
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
    case "money":
      return formatMoney(text, slot.scale ?? 0, slot.currency || ctx.currency, f);
    case "date":
      return formatDate(text, f);
    case "short_date":
      return formatShortDate(text, f);
    case "time":
      return formatTime(text, f);
    case "datetime":
      return formatDateTime(text, f);
  }
  // A preset this module has never heard of — which happens whenever the
  // code that WROTE the spec is newer than the copy of this file rendering
  // it, and nothing holds those two versions together.
  //
  // This file has two homes and the gap exists in both: the admin runtime
  // (vendored into a project out of whichever w17ctl binary the consumer
  // ran) and the mirror the TypeScript client emitter bakes into every
  // generated client. Neither is resolved against the console that emitted
  // the thing being rendered.
  //
  // The switch is exhaustive against FormatPreset, so TypeScript is satisfied
  // and the declared `string` return looks safe. It is not: an unknown preset
  // fell out of the bottom as `undefined`, and a caller interpolating it put
  // the word "undefined" in the cell — MEASURED, not reasoned about. That
  // reads as a bug in the data rather than as a version the runtime cannot
  // render, which is the more expensive of the two to chase.
  //
  // Rendering the raw text is what this file already promises one paragraph
  // up: "a value the formatter cannot parse passes through as its own
  // string ... a malformed value must render as itself rather than as a lie."
  return text;
}

/**
 * The decimal string a raw JSON value formats from.
 *
 * A number goes through the float adapter (a NUMBER / PERCENTAGE / RATIO
 * carrier really is a double; for an integer — a money amount a chart handed
 * back as a number — the adapter yields its plain digits, no fraction);
 * everything else, an int64 MONEY amount included, is already a string on
 * the wire. An object cannot
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
