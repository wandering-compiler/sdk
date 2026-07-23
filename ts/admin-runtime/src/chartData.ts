// chartData — the pure data layer behind the declarative CHART
// overview widget (components/ChartWidget.tsx). No JSX, no React, no
// fetching: it takes a decoded response body plus the field names the
// spec declared and returns exactly what @mantine/charts consumes.
//
// Split out for two reasons. First, everything genuinely hard about a
// chart tile lives here — protojson's wire quirks and the palette
// rules — and all of it is unit-testable without a render harness.
// Second, it has to be TOTAL: a widget renders next to seven siblings
// on the very first screen after login, so a malformed response must
// degrade to an empty chart, never throw and blank the whole overview.
// Every entry point below therefore tolerates undefined / null /
// wrong-typed input instead of trusting the wire.

import { displayString } from "./api";
import { humanizeLabel } from "./format";
import type { AdminWidgetValueSpec } from "./types";

export type ChartColorScheme = "light" | "dark";

// One source row as it arrives on the wire — values are `unknown`
// because protojson's rendering of a field depends on its proto type
// (see chartNumber).
export type ChartRow = Record<string, unknown>;

// ---- palette ----

// The categorical series palette: eight hues in a FIXED order.
//
// The light and dark arrays are a VALIDATED PAIR — together they clear
// the lightness band, the chroma floor, adjacent-pair CVD separation,
// the normal-vision separation floor, and mark-vs-surface contrast,
// each against its own surface. Dark is its own stepping of the same
// hues, deliberately not an automatic flip of light (a flipped light
// palette loses contrast on a dark surface).
//
// Treat the two arrays as ONE unit: do not edit, reorder, extend or
// "improve" a single entry — every hex participates in pair checks
// with its neighbours, so a piecemeal change silently invalidates the
// validation for the whole set. Re-run the palette validator if the
// set ever has to move.
export const CHART_PALETTE_LIGHT: readonly string[] = [
  "#2a78d6",
  "#eb6834",
  "#1baf7a",
  "#eda100",
  "#e87ba4",
  "#008300",
  "#4a3aa7",
  "#e34948",
];

export const CHART_PALETTE_DARK: readonly string[] = [
  "#3987e5",
  "#d95926",
  "#199e70",
  "#c98500",
  "#d55181",
  "#008300",
  "#9085e9",
  "#e66767",
];

// CHART_SERIES_LIMIT is the palette width, and therefore the hard cap
// on plotted series. There is no ninth hue to hand out: a ninth series
// either folds into "Other" (donut) or is a declaration to split.
export const CHART_SERIES_LIMIT = CHART_PALETTE_LIGHT.length;

// Label of the slice everything past the palette folds into.
export const CHART_OTHER_LABEL = "Other";

// The neutral that fold wears — grey on purpose, so "Other" reads as
// the residue it is and never competes with a real category. Mantine
// theme colors (not palette hexes) because the grey only has to sit
// quietly against the surface, and Mantine's greys already do per
// scheme.
const OTHER_COLOR_LIGHT = "gray.6";
const OTHER_COLOR_DARK = "gray.5";

// chartPalette returns the hue column for a color scheme.
export function chartPalette(scheme: ChartColorScheme): readonly string[] {
  return scheme === "dark" ? CHART_PALETTE_DARK : CHART_PALETTE_LIGHT;
}

// chartOtherColor returns the "Other" grey for a color scheme.
export function chartOtherColor(scheme: ChartColorScheme): string {
  return scheme === "dark" ? OTHER_COLOR_DARK : OTHER_COLOR_LIGHT;
}

// seriesColor picks the hue for the series at `index`. Assignment is
// by INDEX and nothing else — never cycled, never by rank — so a
// series keeps its color when a sibling's values move or a filter
// changes what else is on screen. Out-of-range indices clamp to the
// last slot rather than wrapping (wrapping would repeat a hue, which
// reads as "same series"); callers cap at CHART_SERIES_LIMIT first.
export function seriesColor(index: number, scheme: ChartColorScheme): string {
  const palette = chartPalette(scheme);
  if (!Number.isFinite(index) || index < 0) return palette[0];
  return palette[Math.min(Math.trunc(index), palette.length - 1)];
}

// ---- wire decoding ----

// chartNumber coerces one wire value into a plottable number.
//
// The backend marshals with protojson, which (a) renders int64/uint64
// as JSON STRINGS ("12", not 12) and (b) OMITS zero-valued fields
// entirely. So the same series key legitimately arrives as 12, as
// "12", or not at all. Anything that isn't a finite number — absent,
// null, "", "n/a", an object — plots as 0: a hole in one series must
// not become a NaN that takes the axis (and the tile) down with it.
export function chartNumber(v: unknown): number {
  if (typeof v === "number") return Number.isFinite(v) ? v : 0;
  if (typeof v === "string") {
    const trimmed = v.trim();
    if (trimmed === "") return 0;
    const n = Number(trimmed);
    return Number.isFinite(n) ? n : 0;
  }
  return 0;
}

// chartRows reads the row array off the fetched response. Anything
// other than an array under `rowsField` — an absent key (protojson
// omits an empty repeated field), a scalar, a null body — means "no
// rows", which the widget renders as "No data". Non-object entries are
// dropped rather than plotted as blanks.
export function chartRows(resp: unknown, rowsField: string): ChartRow[] {
  if (!resp || typeof resp !== "object" || !rowsField) return [];
  const raw: unknown = (resp as Record<string, unknown>)[rowsField];
  if (!Array.isArray(raw)) return [];
  const rows: ChartRow[] = [];
  for (const entry of raw as unknown[]) {
    if (entry && typeof entry === "object" && !Array.isArray(entry)) {
      rows.push(entry as ChartRow);
    }
  }
  return rows;
}

// ---- series ----

// CHART_X_KEY is the key the x-axis value lands under in the rows fed
// to Mantine. Deliberately NOT the spec's `x_field` name: the same row
// object also carries one key per series field, and a hyphenated key
// can never collide with a proto/JSON field name (identifiers cannot
// contain "-"), so no declaration can shadow the axis.
export const CHART_X_KEY = "x-label";

// ChartSeriesDef is one plotted series in Mantine's shape: `name` is
// the row key the chart reads, `label` the legend/tooltip text, and
// `color` the fixed palette slot for its position.
export interface ChartSeriesDef {
  name: string;
  label: string;
  color: string;
}

// cappedSeries trims a declaration to the palette width, dropping
// entries with no usable field name.
//
// Past eight series a chart stops being readable AND there is no ninth
// hue to give the extras, so they are dropped — and warned about,
// because a nine-series declaration is a spec bug for the developer to
// fix (fold to an "Other" measure, facet, or split the widget), not a
// runtime condition an end user can do anything about. The warning is
// deduped so a re-render doesn't spam the console.
export function cappedSeries(series: AdminWidgetValueSpec[] | undefined): AdminWidgetValueSpec[] {
  const declared = (series ?? []).filter(
    (s) => !!s && typeof s.field === "string" && s.field !== "",
  );
  if (declared.length <= CHART_SERIES_LIMIT) return declared;
  warnSeriesCap(declared.length);
  return declared.slice(0, CHART_SERIES_LIMIT);
}

// chartSeriesDefs maps the declared series onto Mantine's descriptors,
// resolving each label the way STAT resolves its cell labels (spec
// override, else humanized from the field name) and each color from
// its index.
export function chartSeriesDefs(
  series: AdminWidgetValueSpec[] | undefined,
  scheme: ChartColorScheme,
): ChartSeriesDef[] {
  return cappedSeries(series).map((s, i) => ({
    name: s.field,
    label: s.label && s.label.trim() ? s.label : humanizeLabel(s.field),
    color: seriesColor(i, scheme),
  }));
}

// chartSeriesData builds the row array LineChart / AreaChart /
// BarChart consume: one object per source row carrying the x value as
// a STRING under CHART_X_KEY (an axis labels with text, and a Date or
// an int64-as-string must never reach it as "[object Object]") plus
// one NUMBER per declared series field.
//
// Source order is preserved — the backing query owns the ordering
// (that is what its DQL sort is for); the SPA never re-sorts what it
// was handed.
export function chartSeriesData(
  rows: ChartRow[],
  xField: string,
  series: AdminWidgetValueSpec[] | undefined,
): Record<string, string | number>[] {
  const fields = cappedSeries(series).map((s) => s.field);
  return (rows ?? []).map((row) => {
    const point: Record<string, string | number> = {
      [CHART_X_KEY]: displayString(row[xField]),
    };
    for (const field of fields) point[field] = chartNumber(row[field]);
    return point;
  });
}

// ---- donut ----

// ChartDonutCell is one slice in Mantine's DonutChart shape.
export interface ChartDonutCell {
  name: string;
  value: number;
  color: string;
}

// donutData builds the part-of-whole cells DonutChart consumes: one
// slice per row, named by the row's x value and sized by `valueField`.
//
// The ≥9 rule: a donut may never show a ninth hue, so once the row set
// outgrows the palette the LARGEST `colors.length` slices keep their
// palette slots and every remaining row folds into a single neutral
// "Other" slice carrying their summed value. Selection is by value
// (the small tail is what nobody can read anyway), but the survivors
// stay in SOURCE order so a slice's color follows the row it belongs
// to rather than its rank — a value shuffle must not repaint the
// chart.
export function donutData(
  rows: ChartRow[],
  xField: string,
  valueField: string,
  colors: readonly string[],
  otherColor: string = OTHER_COLOR_LIGHT,
): ChartDonutCell[] {
  if (colors.length === 0) return [];
  const slices = (rows ?? []).map((row) => ({
    name: displayString(row[xField]),
    value: chartNumber(row[valueField]),
  }));
  if (slices.length <= colors.length) {
    return slices.map((s, i) => ({ ...s, color: colors[i] }));
  }
  // Rank a copy to pick the survivors (ties broken by source index so
  // the selection is deterministic), then walk the rows in source
  // order so colors are handed out by position, not by rank.
  const keep = new Set(
    slices
      .map((s, i) => ({ i, value: s.value }))
      .sort((a, b) => b.value - a.value || a.i - b.i)
      .slice(0, colors.length)
      .map((e) => e.i),
  );
  const cells: ChartDonutCell[] = [];
  let other = 0;
  slices.forEach((s, i) => {
    if (keep.has(i)) cells.push({ ...s, color: colors[cells.length] });
    else other += s.value;
  });
  cells.push({ name: CHART_OTHER_LABEL, value: other, color: otherColor });
  return cells;
}

// warnSeriesCap logs the over-declaration once per distinct series
// count. Developer-facing only — never surfaced in the UI, where an
// end user would just see framework scolding they cannot act on.
const warnedSeriesCounts = new Set<number>();
function warnSeriesCap(declared: number): void {
  if (warnedSeriesCounts.has(declared)) return;
  warnedSeriesCounts.add(declared);
  console.warn(
    `[admin] chart widget declares ${declared} series but only ${CHART_SERIES_LIMIT} can be ` +
      `told apart; the extras are not plotted. Fold the tail into one "other" measure, ` +
      `split the widget, or facet it.`,
  );
}
