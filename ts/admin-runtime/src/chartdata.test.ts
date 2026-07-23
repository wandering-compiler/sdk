import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import {
  CHART_OTHER_LABEL,
  CHART_PALETTE_DARK,
  CHART_PALETTE_LIGHT,
  CHART_SERIES_LIMIT,
  CHART_X_KEY,
  cappedSeries,
  chartNumber,
  chartOtherColor,
  chartPalette,
  chartRows,
  chartSeriesData,
  chartSeriesDefs,
  donutData,
  seriesColor,
} from "./chartData";
import type { AdminWidgetValueSpec } from "./types";

// The backend marshals a widget's response with protojson, whose two
// habits break naive charting: an int64/uint64 renders as a JSON
// STRING, and a zero-valued field is omitted from the object entirely.
// Every number that reaches a chart passes through here, so a
// regression turns a dashboard into NaN axes.
describe("chartNumber", () => {
  it("passes finite numbers through", () => {
    expect(chartNumber(12)).toBe(12);
    expect(chartNumber(0)).toBe(0);
    expect(chartNumber(-3.5)).toBe(-3.5);
  });

  it("parses protojson's string-rendered 64-bit integers", () => {
    expect(chartNumber("12")).toBe(12);
    expect(chartNumber("9007199254740993")).toBe(9007199254740992); // precision loss, not a crash
    expect(chartNumber(" 7 ")).toBe(7);
  });

  it("treats an omitted zero-valued field as 0", () => {
    expect(chartNumber(undefined)).toBe(0);
    expect(chartNumber(null)).toBe(0);
  });

  it("degrades anything unplottable to 0 rather than NaN", () => {
    expect(chartNumber("")).toBe(0);
    expect(chartNumber("n/a")).toBe(0);
    expect(chartNumber(NaN)).toBe(0);
    expect(chartNumber(Infinity)).toBe(0);
    expect(chartNumber({})).toBe(0);
    expect(chartNumber([1])).toBe(0);
  });
});

// A widget tile renders beside seven siblings on the first screen after
// login. A response that isn't shaped as declared must yield an empty
// chart, never an exception that blanks the whole overview.
describe("chartRows", () => {
  it("reads the declared row array", () => {
    const rows = [{ day: "2026-07-01" }, { day: "2026-07-02" }];
    expect(chartRows({ rows }, "rows")).toEqual(rows);
  });

  it("returns nothing when the rows field is absent (protojson omits an empty list)", () => {
    expect(chartRows({}, "rows")).toEqual([]);
    expect(chartRows({ other: [] }, "rows")).toEqual([]);
  });

  it("returns nothing when the rows field is not an array", () => {
    expect(chartRows({ rows: "nope" }, "rows")).toEqual([]);
    expect(chartRows({ rows: { a: 1 } }, "rows")).toEqual([]);
  });

  it("returns nothing for a body or field name it cannot use", () => {
    expect(chartRows(null, "rows")).toEqual([]);
    expect(chartRows(undefined, "rows")).toEqual([]);
    expect(chartRows("body", "rows")).toEqual([]);
    expect(chartRows({ rows: [{ a: 1 }] }, "")).toEqual([]);
  });

  it("drops entries that are not row objects", () => {
    expect(chartRows({ rows: [{ a: 1 }, null, 3, ["x"]] }, "rows")).toEqual([{ a: 1 }]);
  });
});

// Color is assigned by series INDEX and never by rank, so a series
// keeps its hue when a sibling's values move or a filter changes what
// else is on screen. The two palettes are a validated pair — pinning
// the exact hexes makes an accidental edit a test failure.
describe("palette", () => {
  it("is eight hues wide in both modes", () => {
    expect(CHART_PALETTE_LIGHT).toHaveLength(8);
    expect(CHART_PALETTE_DARK).toHaveLength(8);
    expect(CHART_SERIES_LIMIT).toBe(8);
  });

  it("assigns hues in fixed order per mode", () => {
    expect(seriesColor(0, "light")).toBe("#2a78d6");
    expect(seriesColor(1, "light")).toBe("#eb6834");
    expect(seriesColor(7, "light")).toBe("#e34948");
    expect(seriesColor(0, "dark")).toBe("#3987e5");
    expect(seriesColor(1, "dark")).toBe("#d95926");
    expect(seriesColor(7, "dark")).toBe("#e66767");
  });

  it("is mode-selected, not an auto flip — dark has its own steps", () => {
    // Only the green slot is shared; every other slot is re-stepped.
    const shared = CHART_PALETTE_LIGHT.filter((c, i) => c === CHART_PALETTE_DARK[i]);
    expect(shared).toEqual(["#008300"]);
  });

  it("clamps rather than wraps past the last slot — a ninth hue never exists", () => {
    expect(seriesColor(8, "light")).toBe(seriesColor(7, "light"));
    expect(seriesColor(99, "dark")).toBe(seriesColor(7, "dark"));
  });

  it("clamps a nonsense index to the first slot", () => {
    expect(seriesColor(-1, "light")).toBe(CHART_PALETTE_LIGHT[0]);
    expect(seriesColor(NaN, "light")).toBe(CHART_PALETTE_LIGHT[0]);
  });

  it("keeps the Other fold neutral in both modes", () => {
    expect(chartOtherColor("light")).toBe("gray.6");
    expect(chartOtherColor("dark")).toBe("gray.5");
    expect(chartPalette("dark")).toBe(CHART_PALETTE_DARK);
  });
});

describe("cappedSeries", () => {
  const many = (n: number): AdminWidgetValueSpec[] =>
    Array.from({ length: n }, (_, i) => ({ field: `s${i}` }));

  beforeEach(() => {
    vi.spyOn(console, "warn").mockImplementation(() => {});
  });
  afterEach(() => {
    vi.restoreAllMocks();
  });

  it("passes a declaration within the palette width through untouched", () => {
    const declared = many(8);
    expect(cappedSeries(declared)).toEqual(declared);
    expect(console.warn).not.toHaveBeenCalled();
  });

  it("drops series past the cap and tells the developer once", () => {
    const got = cappedSeries(many(9));
    expect(got).toHaveLength(8);
    expect(got[7].field).toBe("s7");
    expect(console.warn).toHaveBeenCalledTimes(1);
    expect(console.warn).toHaveBeenCalledWith(expect.stringContaining("9 series"));
    // Re-rendering the same over-declaration must not spam the console.
    cappedSeries(many(9));
    expect(console.warn).toHaveBeenCalledTimes(1);
  });

  it("tolerates a missing or unusable declaration", () => {
    expect(cappedSeries(undefined)).toEqual([]);
    expect(cappedSeries([{ field: "" }])).toEqual([]);
  });
});

describe("chartSeriesDefs", () => {
  it("resolves label from the spec, else humanizes the field", () => {
    const defs = chartSeriesDefs(
      [{ field: "total", label: "Signups" }, { field: "org_id" }],
      "light",
    );
    expect(defs).toEqual([
      { name: "total", label: "Signups", color: "#2a78d6" },
      { name: "org_id", label: "Org ID", color: "#eb6834" },
    ]);
  });

  it("ignores a blank label override", () => {
    expect(chartSeriesDefs([{ field: "total", label: "   " }], "dark")[0].label).toBe("Total");
  });
});

describe("chartSeriesData", () => {
  const series: AdminWidgetValueSpec[] = [{ field: "total" }, { field: "failed" }];

  it("stringifies the x value and coerces every series value", () => {
    const rows = [
      { day: "2026-07-01", total: "12", failed: 3 },
      // `failed` omitted — protojson dropped a zero.
      { day: "2026-07-02", total: 4 },
    ];
    expect(chartSeriesData(rows, "day", series)).toEqual([
      { [CHART_X_KEY]: "2026-07-01", total: 12, failed: 3 },
      { [CHART_X_KEY]: "2026-07-02", total: 4, failed: 0 },
    ]);
  });

  it("labels the axis even when the x field is absent or non-string", () => {
    expect(chartSeriesData([{ total: 1 }, { day: 5 }], "day", series)).toEqual([
      { [CHART_X_KEY]: "", total: 1, failed: 0 },
      { [CHART_X_KEY]: "5", total: 0, failed: 0 },
    ]);
  });

  it("keeps source order — the backing query owns the sort", () => {
    const rows = [
      { day: "b", total: 1 },
      { day: "a", total: 9 },
    ];
    expect(chartSeriesData(rows, "day", [{ field: "total" }]).map((p) => p[CHART_X_KEY])).toEqual([
      "b",
      "a",
    ]);
  });

  it("emits bare axis points when nothing usable is declared", () => {
    expect(chartSeriesData([{ day: "a" }], "day", [])).toEqual([{ [CHART_X_KEY]: "a" }]);
    expect(chartSeriesData([], "day", series)).toEqual([]);
  });
});

describe("donutData", () => {
  const palette = CHART_PALETTE_LIGHT;
  const rowsOf = (...values: number[]) =>
    values.map((v, i) => ({ name: `c${i}`, count: String(v) }));

  it("builds one slice per row in palette order", () => {
    const cells = donutData(rowsOf(3, 1), "name", "count", palette);
    expect(cells).toEqual([
      { name: "c0", value: 3, color: palette[0] },
      { name: "c1", value: 1, color: palette[1] },
    ]);
  });

  it("fills the palette exactly at eight rows — no Other slice", () => {
    const cells = donutData(rowsOf(1, 2, 3, 4, 5, 6, 7, 8), "name", "count", palette);
    expect(cells).toHaveLength(8);
    expect(cells.some((c) => c.name === CHART_OTHER_LABEL)).toBe(false);
  });

  it("folds the tail into one grey Other at nine rows", () => {
    // c0 is the smallest, so it is the one that folds.
    const cells = donutData(
      rowsOf(1, 20, 30, 40, 50, 60, 70, 80, 90),
      "name",
      "count",
      palette,
      "gray.6",
    );
    expect(cells).toHaveLength(9);
    expect(cells[8]).toEqual({ name: CHART_OTHER_LABEL, value: 1, color: "gray.6" });
    expect(cells.slice(0, 8).map((c) => c.name)).toEqual([
      "c1",
      "c2",
      "c3",
      "c4",
      "c5",
      "c6",
      "c7",
      "c8",
    ]);
  });

  it("sums every folded row into the single Other slice", () => {
    const cells = donutData(
      rowsOf(1, 2, 3, 100, 100, 100, 100, 100, 100, 100, 100),
      "name",
      "count",
      palette,
    );
    expect(cells).toHaveLength(9);
    expect(cells[8]).toEqual({ name: CHART_OTHER_LABEL, value: 6, color: "gray.6" });
  });

  it("keeps survivors in SOURCE order so a slice's color follows its row, not its rank", () => {
    // The largest row is last; it must not steal palette slot 0.
    const cells = donutData(
      rowsOf(10, 20, 30, 40, 50, 60, 70, 80, 5, 900),
      "name",
      "count",
      palette,
    );
    expect(cells.slice(0, 8).map((c) => c.name)).toEqual([
      "c1",
      "c2",
      "c3",
      "c4",
      "c5",
      "c6",
      "c7",
      "c9",
    ]);
    expect(cells[0].color).toBe(palette[0]);
    expect(cells[7].color).toBe(palette[7]);
  });

  it("degrades to nothing on an unusable declaration", () => {
    expect(donutData([], "name", "count", palette)).toEqual([]);
    expect(donutData(rowsOf(1), "name", "count", [])).toEqual([]);
  });

  it("names a slice even when the x field is missing", () => {
    expect(donutData([{ count: 2 }], "name", "count", palette)).toEqual([
      { name: "", value: 2, color: palette[0] },
    ]);
  });
});
