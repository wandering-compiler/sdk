// Locale-aware VALUE formatting — numbers, dates, times
// (docs/specs/i18n/formatting.md). The gettext track localizes
// strings; this localizes the values next to them, so a Czech
// admin stops rendering `1234.5` and `2026-07-26T14:03:00Z`.
//
// This file is the TS HALF OF A MIRRORED PAIR. Its Go twin is
// sdk/go/lib/i18n (formats.go / formatnum.go / formatdate.go),
// and the two must produce byte-identical output — that is the
// load-bearing property of the feature and the reason the format
// table is explicit data rather than a call into `Intl`, which
// disagrees with Go's x/text on group separators, the minus sign
// and rounding mode. Both halves run the SAME golden vectors
// (formatVectors.json here, testdata/format_vectors.json there,
// kept identical by `make sync-i18n-formats`).
//
// So: any change here needs the same change there, and a vector
// that pins it. Two conventions are fixed for that reason —
// rounding is HALF-UP away from zero, and the sign is ASCII
// hyphen-minus which a value rounding to zero loses.
//
// Numbers never pass through a JS number on the formatting path:
// an int64 arrives as a decimal STRING on the JSON wire (per
// docs/specs/gateway/json-dialect.md) and can exceed 2^53, so
// every step below is string arithmetic. `decimalString` is the
// one adapter in, for a MONEY field that really is a double.

import formatsDoc from "./formats.json";

/** One axis of the project's override map: formatting is remapped per kind. */
export type FormatKind = "number" | "decimal" | "percent" | "date" | "time" | "datetime";

/**
 * One locale's row of the frozen table. Field names are the wire names from
 * formats.json — no mapping layer, so the two runtimes cannot drift over a
 * rename. `first_day_of_week` (0 = Sunday) is carried for the date pickers.
 */
export interface Formats {
  decimal_separator: string;
  thousand_separator: string;
  grouping: number;
  date_format: string;
  short_date_format: string;
  time_format: string;
  datetime_format: string;
  percent_format: string;
  first_day_of_week: number;
}

/** The project-owned remap that rides in the lock: {cs: {number: "en"}}. */
export type FormatOverrides = Record<string, Partial<Record<FormatKind, string>>>;

/** The last link of every resolution chain. */
export const FALLBACK_FORMAT_LOCALE = "en";

// The table is keyed by NORMALIZED locale so a tag written any of the ways it
// is written in the wild finds its row; SPELLING keeps the canonical BCP-47
// form for anything a human reads.
const TABLE = new Map<string, Formats>();
const SPELLING = new Map<string, string>();
for (const [locale, row] of Object.entries(
  (formatsDoc as { locales: Record<string, Formats> }).locales,
)) {
  TABLE.set(normalizeFormatLocale(locale), row);
  SPELLING.set(normalizeFormatLocale(locale), locale);
}

/** Lowercases and switches `_` to `-` so `CS`, `cs` and `cs_CZ` agree. */
export function normalizeFormatLocale(locale: string): string {
  return locale.trim().toLowerCase().replace(/_/g, "-");
}

/** Every locale the frozen table carries, canonically spelled and sorted. */
export function formatLocales(): string[] {
  return [...SPELLING.values()].sort();
}

/** Whether the table has a row for exactly this locale — no fallback walking. */
export function hasFormatLocale(locale: string): boolean {
  return TABLE.has(normalizeFormatLocale(locale));
}

/**
 * The row a locale formats with: the locale itself, then its base language
 * (`pt-BR` → `pt`), then `en`. Never fails — the `en` row is always present.
 */
export function resolveFormats(locale: string): Formats {
  const l = normalizeFormatLocale(locale ?? "");
  const exact = TABLE.get(l);
  if (exact) return exact;
  const dash = l.indexOf("-");
  if (dash > 0) {
    const base = TABLE.get(l.slice(0, dash));
    if (base) return base;
  }
  return TABLE.get(FALLBACK_FORMAT_LOCALE) as Formats;
}

/**
 * resolveFormats with the project's override map applied first: a hit on
 * (language, kind) swaps the locale before the chain runs. An override naming
 * an unknown locale falls through instead of failing — the lock parser is what
 * rejects a bad override, and a runtime must not break on data that got past it.
 */
export function resolveFormatsKind(
  locale: string,
  kind: FormatKind,
  overrides?: FormatOverrides,
): Formats {
  const l = normalizeFormatLocale(locale ?? "");
  const target = overrides?.[l]?.[kind];
  if (target) {
    const row = TABLE.get(normalizeFormatLocale(target));
    if (row) return row;
  }
  return resolveFormats(l);
}

// ---- numbers ----------------------------------------------------------

interface DecimalParts {
  neg: boolean;
  ipart: string;
  fpart: string;
}

/**
 * Renders with grouping and the locale's decimal separator at NATURAL
 * precision — the digits the value carries, trailing fraction zeros dropped.
 * The `number` filter.
 *
 * A value that is not a plain decimal is returned UNCHANGED: a malformed cell
 * renders as itself rather than as a lie (same philosophy as displayString).
 */
export function formatNumber(value: string, f: Formats): string {
  const d = parseDecimal(value);
  if (!d) return value;
  return renderDecimal(trimFractionZeros(d), f);
}

/**
 * Renders at exactly `places` decimal places, padding and rounding half-up.
 * The `decimal:N` filter, and the preset for MONEY (2) and DECIMAL (scale).
 * A negative `places` means natural precision.
 */
export function formatDecimal(value: string, places: number, f: Formats): string {
  const d = parseDecimal(value);
  if (!d) return value;
  if (places < 0) return renderDecimal(trimFractionZeros(d), f);
  return renderDecimal(roundDecimal(d, places), f);
}

/**
 * formatDecimal wrapped in the locale's percent shape — `{value} %` in Czech,
 * `{value}%` in English. The value is NOT multiplied: a PERCENTAGE field
 * carries the percentage, not the fraction.
 */
export function formatPercent(value: string, places: number, f: Formats): string {
  const num = formatDecimal(value, places, f);
  if (!f.percent_format) return num;
  return f.percent_format.replace("{value}", num);
}

/**
 * The adapter for a value that really is a JS number (a MONEY double): renders
 * the plain decimal string the formatters take, expanding JS's exponent
 * notation — which kicks in at 1e21 and 1e-7 — because the parser rejects it
 * and Go's `strconv.FormatFloat(v, 'f', -1, 64)` never produces it.
 */
export function decimalString(v: number): string {
  if (!Number.isFinite(v)) return String(v);
  return expandExponent(String(v));
}

/**
 * Rewrites exponent notation as plain decimal digits, and returns anything else
 * untouched. Split out from [decimalString] so it can be tested over inputs JS
 * itself never produces: `String(n)` only goes exponential at 1e21 and 1e-7,
 * where the point always lands outside the digit run, so the
 * digits-either-side case is unreachable through a number and would otherwise
 * be untestable code sitting in the middle of the function.
 */
export function expandExponent(s: string): string {
  const m = /^([+-]?)(\d+)(?:\.(\d+))?[eE]([+-]?\d+)$/.exec(s);
  if (!m) return s;
  const [, sign, int, frac = "", expText] = m;
  const exp = parseInt(expText, 10);
  const digits = int + frac;
  const point = int.length + exp;
  let out: string;
  if (point <= 0) {
    out = "0." + "0".repeat(-point) + digits;
  } else if (point >= digits.length) {
    out = digits + "0".repeat(point - digits.length);
  } else {
    out = digits.slice(0, point) + "." + digits.slice(point);
  }
  return sign + out;
}

/**
 * Accepts the plain decimal grammar and nothing else: an optional sign, then
 * digits with at most one dot. Exponent notation is rejected on purpose — it
 * cannot appear in a w17 payload, so accepting it would add a second rounding
 * path. Returns null when the input is not a decimal.
 */
function parseDecimal(value: string): DecimalParts | null {
  let s = (value ?? "").trim();
  if (s === "") return null;
  let neg = false;
  if (s.startsWith("-")) {
    neg = true;
    s = s.slice(1);
  } else if (s.startsWith("+")) {
    s = s.slice(1);
  }
  const dot = s.indexOf(".");
  const hasDot = dot >= 0;
  const ip = hasDot ? s.slice(0, dot) : s;
  const fp = hasDot ? s.slice(dot + 1) : "";
  if (!isDigits(ip) || !isDigits(fp)) return null;
  if (ip === "" && fp === "") return null; // "", ".", "-."
  if (hasDot && fp === "") return null; // "5."
  return { neg, ipart: ip === "" ? "0" : ip, fpart: fp };
}

function isDigits(s: string): boolean {
  for (const ch of s) {
    if (ch < "0" || ch > "9") return false;
  }
  return true;
}

/** Rounds to `places` fraction digits, half-up away from zero, padding short values. */
function roundDecimal(d: DecimalParts, places: number): DecimalParts {
  if (d.fpart.length <= places) {
    return { ...d, fpart: d.fpart + "0".repeat(places - d.fpart.length) };
  }
  let kept = d.ipart + d.fpart.slice(0, places);
  if (d.fpart.charAt(places) >= "5") kept = incrementDigits(kept);
  return {
    neg: d.neg,
    ipart: kept.slice(0, kept.length - places),
    fpart: kept.slice(kept.length - places),
  };
}

/** Adds one to a digit string, growing it on overflow ("999" → "1000"). */
function incrementDigits(s: string): string {
  const b = [...s];
  for (let i = b.length - 1; i >= 0; i--) {
    if (b[i] !== "9") {
      b[i] = String(Number(b[i]) + 1);
      return b.join("");
    }
    b[i] = "0";
  }
  return "1" + b.join("");
}

function trimFractionZeros(d: DecimalParts): DecimalParts {
  return { ...d, fpart: d.fpart.replace(/0+$/, "") };
}

/** Sign, grouped integer digits, fraction. All-zero digits render unsigned. */
function renderDecimal(d: DecimalParts, f: Formats): string {
  const sign = d.neg && !isZeroDigits(d.ipart, d.fpart) ? "-" : "";
  const int = groupDigits(d.ipart, f.grouping, f.thousand_separator);
  return d.fpart === "" ? sign + int : sign + int + f.decimal_separator + d.fpart;
}

function isZeroDigits(...parts: string[]): boolean {
  return parts.every((p) => p.replace(/0/g, "") === "");
}

/** Inserts `sep` every `size` digits from the right; no grouping when either is absent. */
function groupDigits(digits: string, size: number, sep: string): string {
  if (size <= 0 || sep === "" || digits.length <= size) return digits;
  const lead = digits.length % size;
  const groups: string[] = [];
  if (lead > 0) groups.push(digits.slice(0, lead));
  for (let i = lead; i < digits.length; i += size) {
    groups.push(digits.slice(i, i + size));
  }
  return groups.join(sep);
}

// ---- dates and times --------------------------------------------------

/** A wall-clock instant in UTC, already stripped of zone. */
export interface CivilTime {
  year: number;
  month: number;
  day: number;
  hour: number;
  minute: number;
  second: number;
}

/** Renders a temporal value with the locale's `date_format`. */
export function formatDate(value: string, f: Formats): string {
  return formatTemporal(value, f.date_format);
}

/** Renders with `short_date_format` — the dense-table form. */
export function formatShortDate(value: string, f: Formats): string {
  return formatTemporal(value, f.short_date_format);
}

/** Renders with `time_format` — 24-hour in most locales, `h:mm A` in en-US. */
export function formatTime(value: string, f: Formats): string {
  return formatTemporal(value, f.time_format);
}

/**
 * Renders with `datetime_format`, whose `{date}` / `{time}` placeholders come
 * from the other two patterns. Keeping the composition in the table is what
 * lets a locale put the time first or separate the halves with a comma.
 */
export function formatDateTime(value: string, f: Formats): string {
  const c = parseTemporal(value);
  if (!c) return value;
  const shape = f.datetime_format || "{date} {time}";
  return shape
    .replace("{date}", renderTimePattern(f.date_format, c))
    .replace("{time}", renderTimePattern(f.time_format, c));
}

function formatTemporal(value: string, pattern: string): string {
  const c = parseTemporal(value);
  return c ? renderTimePattern(pattern, c) : value;
}

const RE_DATETIME =
  /^(\d{4})-(\d{2})-(\d{2})[Tt ](\d{2}):(\d{2})(?::(\d{2})(?:\.\d+)?)?(Z|z|[+-]\d{2}:\d{2})?$/;
const RE_DATE = /^(\d{4})-(\d{2})-(\d{2})$/;
const RE_TIME = /^(\d{2}):(\d{2})(?::(\d{2})(?:\.\d+)?)?$/;

/**
 * Parses the same forms the Go twin's layout list accepts and converts to UTC.
 * A value with no zone is taken as UTC already; a date-only or time-only value
 * fills the missing half with zeros, so a DATE rendered with a time pattern
 * yields `0:00` rather than an error. Returns null when nothing matches.
 *
 * Deliberately regex + integer arithmetic rather than `new Date(...)`: Date
 * would drag the host's local zone (and its 2-digit-year remapping) into a
 * result that has to match Go exactly.
 */
export function parseTemporal(value: string): CivilTime | null {
  const v = (value ?? "").trim();
  if (v === "") return null;

  const dt = RE_DATETIME.exec(v);
  if (dt) {
    const c: CivilTime = {
      year: Number(dt[1]),
      month: Number(dt[2]),
      day: Number(dt[3]),
      hour: Number(dt[4]),
      minute: Number(dt[5]),
      second: Number(dt[6] ?? "0"),
    };
    const zone = dt[7];
    if (!zone || zone === "Z" || zone === "z") return c;
    const sign = zone.startsWith("-") ? -1 : 1;
    const offset = sign * (Number(zone.slice(1, 3)) * 60 + Number(zone.slice(4, 6)));
    return shiftMinutes(c, -offset);
  }

  const d = RE_DATE.exec(v);
  if (d) {
    return {
      year: Number(d[1]),
      month: Number(d[2]),
      day: Number(d[3]),
      hour: 0,
      minute: 0,
      second: 0,
    };
  }

  const t = RE_TIME.exec(v);
  if (t) {
    // Go's "15:04:05" layout yields year 0, month 1, day 1 — mirror it, so a
    // time-only value rendered with a date pattern agrees across runtimes.
    return {
      year: 0,
      month: 1,
      day: 1,
      hour: Number(t[1]),
      minute: Number(t[2]),
      second: Number(t[3] ?? "0"),
    };
  }
  return null;
}

/**
 * Shifts a civil time by `delta` minutes. An RFC 3339 offset is under 24h and
 * the clock is under 24h, so the result lands at most one day either side —
 * which is why this can be a day step rather than epoch arithmetic.
 */
function shiftMinutes(c: CivilTime, delta: number): CivilTime {
  let total = c.hour * 60 + c.minute + delta;
  let { year, month, day } = c;
  if (total < 0) {
    total += 1440;
    [year, month, day] = addDays(year, month, day, -1);
  } else if (total >= 1440) {
    total -= 1440;
    [year, month, day] = addDays(year, month, day, 1);
  }
  return {
    year,
    month,
    day,
    hour: Math.floor(total / 60),
    minute: total % 60,
    second: c.second,
  };
}

/** Steps a civil date one day forward or back, honoring month lengths + leap years. */
function addDays(
  year: number,
  month: number,
  day: number,
  delta: 1 | -1,
): [number, number, number] {
  if (delta === 1) {
    if (day < daysInMonth(year, month)) return [year, month, day + 1];
    if (month === 12) return [year + 1, 1, 1];
    return [year, month + 1, 1];
  }
  if (day > 1) return [year, month, day - 1];
  if (month === 1) return [year - 1, 12, 31];
  return [year, month - 1, daysInMonth(year, month - 1)];
}

function daysInMonth(year: number, month: number): number {
  switch (month) {
    case 2:
      return isLeapYear(year) ? 29 : 28;
    case 4:
    case 6:
    case 9:
    case 11:
      return 30;
    default:
      return 31;
  }
}

function isLeapYear(y: number): boolean {
  return (y % 4 === 0 && y % 100 !== 0) || y % 400 === 0;
}

/**
 * Substitutes the CLOSED token vocabulary shared with the Go twin:
 *
 *   YYYY 4-digit year   MM 2-digit month   M month
 *   DD 2-digit day      D day
 *   HH 2-digit hour     H hour (0-23)
 *   hh 2-digit hour     h hour (1-12)      A AM/PM
 *   mm 2-digit minute   ss 2-digit second
 *
 * Every other byte is a literal. The table is frozen, so no project-authored
 * pattern ever reaches this renderer.
 */
export function renderTimePattern(pattern: string, c: CivilTime): string {
  let out = "";
  for (let i = 0; i < pattern.length;) {
    const hit = matchToken(pattern.slice(i), c);
    if (!hit) {
      out += pattern.charAt(i);
      i += 1;
      continue;
    }
    out += hit[1];
    i += hit[0];
  }
  return out;
}

function matchToken(s: string, c: CivilTime): [number, string] | null {
  if (s.startsWith("YYYY")) return [4, pad(c.year, 4)];
  if (s.startsWith("MM")) return [2, pad(c.month, 2)];
  if (s.startsWith("DD")) return [2, pad(c.day, 2)];
  if (s.startsWith("HH")) return [2, pad(c.hour, 2)];
  if (s.startsWith("hh")) return [2, pad(hour12(c.hour), 2)];
  if (s.startsWith("mm")) return [2, pad(c.minute, 2)];
  if (s.startsWith("ss")) return [2, pad(c.second, 2)];
  if (s.startsWith("M")) return [1, String(c.month)];
  if (s.startsWith("D")) return [1, String(c.day)];
  if (s.startsWith("H")) return [1, String(c.hour)];
  if (s.startsWith("h")) return [1, String(hour12(c.hour))];
  if (s.startsWith("A")) return [1, c.hour < 12 ? "AM" : "PM"];
  return null;
}

/** Maps a 0-23 hour onto the 1-12 clock: midnight and noon are both 12. */
function hour12(h: number): number {
  if (h === 0) return 12;
  return h > 12 ? h - 12 : h;
}

function pad(v: number, width: number): string {
  const s = String(v);
  return v >= 0 && s.length < width ? "0".repeat(width - s.length) + s : s;
}
