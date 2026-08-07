import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import { MantineProvider } from "@mantine/core";

import { ChartWidget } from "./components/ChartWidget";
import { CHART_PALETTE_LIGHT, CHART_X_KEY } from "./chartData";
import type { AdminWidgetSpec } from "./types";

vi.mock("./api", async () => {
  const actual = await vi.importActual<typeof import("./api")>("./api");
  return { ...actual, apiGet: vi.fn() };
});
const { apiGet } = await import("./api");

// The Mantine charts draw into an SVG sized by a ResizeObserver, and
// jsdom reports a zero-size box — a real render produces nothing worth
// asserting. So the four charts are stubbed down to a marker div that
// records its props. That is the right seam anyway: what this component
// OWNS is the translation from spec to chart props (which chart, which
// dataKey, which series with which colors), not recharts' geometry.
const charts = vi.hoisted(() => ({
  calls: [] as { kind: string; props: Record<string, unknown> }[],
}));

vi.mock("@mantine/charts", () => {
  const stub = (kind: string) => (props: Record<string, unknown>) => {
    charts.calls.push({ kind, props });
    return <div data-testid={`chart-${kind}`} />;
  };
  return {
    LineChart: stub("line"),
    BarChart: stub("bar"),
    AreaChart: stub("area"),
    DonutChart: stub("donut"),
  };
});

const widget = (over: Partial<AdminWidgetSpec> = {}): AdminWidgetSpec => ({
  slot: "signups_chart",
  size: "LARGE",
  kind: "CHART",
  title: "Signups per day",
  endpoint: "/admin/api/widget/signups_chart",
  chart_kind: "LINE",
  rows_field: "rows",
  x_field: "day",
  series: [{ field: "total", label: "Signups" }],
  ...over,
});

function renderWidget(w: AdminWidgetSpec = widget()) {
  render(
    <MantineProvider>
      <ChartWidget widget={w} />
    </MantineProvider>,
  );
}

// The last props the stub recorded for `kind`.
function lastProps(kind: string): Record<string, unknown> {
  const call = charts.calls.filter((c) => c.kind === kind).at(-1);
  if (!call) throw new Error(`no ${kind} chart rendered`);
  return call.props;
}

describe("ChartWidget", () => {
  beforeEach(() => {
    charts.calls.length = 0;
  });
  afterEach(() => {
    cleanup();
    vi.resetAllMocks();
  });

  // The whole point of the widget: a declared endpoint + field names
  // become a plotted chart with no project JS in between.
  it("fetches the data route and plots the declared series", async () => {
    let resolve: (v: unknown) => void = () => {};
    vi.mocked(apiGet).mockReturnValue(
      new Promise((r) => {
        resolve = r;
      }),
    );
    renderWidget();

    // In flight: the title is up, the chart is not.
    expect(screen.getByText("Signups per day")).toBeDefined();
    expect(screen.queryByTestId("chart-line")).toBeNull();

    resolve({
      rows: [
        { day: "2026-07-01", total: "12" },
        // protojson omits a zero-valued field entirely.
        { day: "2026-07-02" },
      ],
    });

    await waitFor(() => expect(screen.getByTestId("chart-line")).toBeDefined());
    expect(apiGet).toHaveBeenCalledWith("/admin/api/widget/signups_chart");
    const props = lastProps("line");
    expect(props.dataKey).toBe(CHART_X_KEY);
    expect(props.data).toEqual([
      { [CHART_X_KEY]: "2026-07-01", total: 12 },
      { [CHART_X_KEY]: "2026-07-02", total: 0 },
    ]);
    // Series carry the spec's label and the fixed palette slot.
    expect(props.series).toEqual([
      { name: "total", label: "Signups", color: CHART_PALETTE_LIGHT[0] },
    ]);
    // One series → no legend; the card title already names it.
    expect(props.withLegend).toBe(false);
  });

  it("shows a legend once there are two series", async () => {
    vi.mocked(apiGet).mockResolvedValue({ rows: [{ day: "d", total: 1, failed: 2 }] });
    renderWidget(widget({ series: [{ field: "total" }, { field: "failed" }] }));
    await waitFor(() => expect(screen.getByTestId("chart-line")).toBeDefined());
    const props = lastProps("line");
    expect(props.withLegend).toBe(true);
    expect(props.series).toEqual([
      { name: "total", label: "Total", color: CHART_PALETTE_LIGHT[0] },
      { name: "failed", label: "Failed", color: CHART_PALETTE_LIGHT[1] },
    ]);
  });

  it.each([
    { kind: "BAR" as const, testid: "chart-bar" },
    { kind: "AREA" as const, testid: "chart-area" },
  ])("dispatches chart_kind $kind to its Mantine chart", async ({ kind, testid }) => {
    vi.mocked(apiGet).mockResolvedValue({ rows: [{ day: "d", total: 1 }] });
    renderWidget(widget({ chart_kind: kind }));
    await waitFor(() => expect(screen.getByTestId(testid)).toBeDefined());
  });

  // A donut is part-of-whole over the FIRST declared series, and its
  // legend is never optional — a slice's identity is color-only.
  it("plots a donut over the first declared series, always with a legend", async () => {
    vi.mocked(apiGet).mockResolvedValue({
      rows: [
        { plan: "free", users: "40" },
        { plan: "pro", users: 10 },
      ],
    });
    renderWidget(widget({ chart_kind: "DONUT", x_field: "plan", series: [{ field: "users" }] }));
    await waitFor(() => expect(screen.getByTestId("chart-donut")).toBeDefined());
    const props = lastProps("donut");
    expect(props.data).toEqual([
      { name: "free", value: 40, color: CHART_PALETTE_LIGHT[0] },
      { name: "pro", value: 10, color: CHART_PALETTE_LIGHT[1] },
    ]);
    expect(props.withLegend).toBe(true);
  });

  // The report this feature came from: a donut split by an enum drew a
  // legend of raw wire numbers. A slice's NAME is its whole identity —
  // the legend, the tooltip heading, the colour key — so resolving it is
  // not decoration.
  it("names donut slices by their enum label, not the wire number", async () => {
    vi.mocked(apiGet).mockResolvedValue({
      rows: [
        { reason: 1, count: 5 },
        { reason: 2, count: 20 },
        // A member this build hasn't regenerated against still plots —
        // a number the reader can look up beats a blank slice.
        { reason: 99, count: 1 },
      ],
    });
    renderWidget(
      widget({
        chart_kind: "DONUT",
        x_field: "reason",
        series: [{ field: "count" }],
        x_choices: {
          enum_fqn: "billing.LedgerReason",
          carrier: "scalar_int32",
          values: [
            { value: 1, label: "Topup" },
            { value: 2, label: "Purchase" },
          ],
        },
      }),
    );
    await waitFor(() => expect(screen.getByTestId("chart-donut")).toBeDefined());
    expect((lastProps("donut").data as { name: string }[]).map((c) => c.name)).toEqual([
      "Topup",
      "Purchase",
      "99",
    ]);
  });

  // The other half of the same rule: a time axis reads as a date.
  it("formats the axis through the declared value format", async () => {
    vi.mocked(apiGet).mockResolvedValue({ rows: [{ day: "2026-07-26T00:00:00Z", total: 3 }] });
    renderWidget(
      widget({
        chart_kind: "AREA",
        x_format: { msgid: "{value}", slots: [{ name: "value", preset: "short_date" }] },
      }),
    );
    await waitFor(() => expect(screen.getByTestId("chart-area")).toBeDefined());
    expect(lastProps("area").data).toEqual([{ [CHART_X_KEY]: "26/7/2026", total: 3 }]);
  });

  // Measures follow their declaration too — and a chart takes ONE
  // formatter, so it passes none while the series disagree.
  it("passes a value formatter only when every series shares one format", async () => {
    const money = {
      msgid: "{value}",
      slots: [{ name: "value", preset: "decimal" as const, places: 2, has_places: true }],
    };
    vi.mocked(apiGet).mockResolvedValue({ rows: [{ day: "d", total: 1, failed: 2 }] });
    renderWidget(widget({ series: [{ field: "total" }], value_formats: { total: money } }));
    await waitFor(() => expect(screen.getByTestId("chart-line")).toBeDefined());
    expect((lastProps("line").valueFormatter as (v: number) => string)(1234.5)).toBe("1,234.50");

    renderWidget(
      widget({
        series: [{ field: "total" }, { field: "failed" }],
        value_formats: { total: money },
      }),
    );
    await waitFor(() => expect(charts.calls.filter((c) => c.kind === "line").length).toBe(2));
    expect(lastProps("line").valueFormatter).toBeUndefined();
  });

  // Reported from a live console: the donut used a narrow strip of a
  // wide card and its legend entries collided. Mantine sizes the whole
  // component from `size`, so the ring's radius was also deciding the
  // widget's width — the box has to be freed from it while the ring
  // keeps it.
  it("lets the donut fill its card while the ring keeps its size", async () => {
    vi.mocked(apiGet).mockResolvedValue({ rows: [{ plan: "free", users: 1 }] });
    renderWidget(widget({ chart_kind: "DONUT", x_field: "plan", series: [{ field: "users" }] }));
    await waitFor(() => expect(screen.getByTestId("chart-donut")).toBeDefined());
    const props = lastProps("donut");
    expect(props.w).toBe("100%");
    // Mantine's root pins min-width/min-height to `size + 80`; left in
    // place they would re-impose the box the width override just lifted.
    expect(props.miw).toBe(0);
    expect(props.mih).toBe(0);
    expect(props.size).toBe(150);
    // One slice per hover, not a cramped copy of the legend.
    expect(props.tooltipDataSource).toBe("segment");
  });

  // Recharts paints the legend after the tooltip, so without this the
  // legend shows THROUGH the panel hovering above it.
  it.each([
    { kind: "LINE" as const, testid: "chart-line" },
    { kind: "AREA" as const, testid: "chart-area" },
    { kind: "BAR" as const, testid: "chart-bar" },
    { kind: "DONUT" as const, testid: "chart-donut" },
  ])("keeps the $kind tooltip above the legend", async ({ kind, testid }) => {
    vi.mocked(apiGet).mockResolvedValue({ rows: [{ day: "d", total: 1 }] });
    renderWidget(widget({ chart_kind: kind, series: [{ field: "total" }] }));
    await waitFor(() => expect(screen.getByTestId(testid)).toBeDefined());
    const tooltipProps = lastProps(testid.replace("chart-", "")).tooltipProps as {
      wrapperStyle?: { zIndex?: number };
    };
    expect(tooltipProps?.wrapperStyle?.zIndex).toBeGreaterThan(0);
  });

  // A formatted date label is wide; recharts only drops colliding ticks
  // if it is told how much room one needs.
  it("gives the category axis room for a formatted label", async () => {
    vi.mocked(apiGet).mockResolvedValue({ rows: [{ day: "d", total: 1 }] });
    renderWidget(widget({ chart_kind: "AREA" }));
    await waitFor(() => expect(screen.getByTestId("chart-area")).toBeDefined());
    expect((lastProps("area").xAxisProps as { minTickGap?: number })?.minTickGap).toBeGreaterThan(
      5,
    );
  });

  // A dashboard tile must not shout: a failed fetch is one quiet line,
  // not an error banner over the first screen after login.
  it("degrades to a quiet 'Unavailable' when the fetch fails", async () => {
    vi.mocked(apiGet).mockRejectedValue(new Error("boom"));
    renderWidget();
    await waitFor(() => expect(screen.getByText("Unavailable")).toBeDefined());
    expect(screen.queryByTestId("chart-line")).toBeNull();
    // The title stays — the operator can still see which tile is down.
    expect(screen.getByText("Signups per day")).toBeDefined();
  });

  // A widget whose spec carries no data route can never resolve; it is
  // an unavailable tile, not an eternal spinner.
  it("is unavailable when the spec declares no endpoint", async () => {
    renderWidget(widget({ endpoint: undefined }));
    await waitFor(() => expect(screen.getByText("Unavailable")).toBeDefined());
    expect(apiGet).not.toHaveBeenCalled();
  });

  // An empty result set is a steady state (fresh install, quiet
  // window), not a failure — and not an empty axis frame either.
  it.each([
    { name: "an empty row array", body: { rows: [] } },
    { name: "an omitted rows field", body: {} },
    { name: "a non-array rows field", body: { rows: "nope" } },
  ])("says 'No data' for $name", async ({ body }) => {
    vi.mocked(apiGet).mockResolvedValue(body);
    renderWidget();
    await waitFor(() => expect(screen.getByText("No data")).toBeDefined());
    expect(screen.queryByTestId("chart-line")).toBeNull();
  });
});
