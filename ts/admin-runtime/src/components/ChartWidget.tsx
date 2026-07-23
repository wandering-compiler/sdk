// ChartWidget — the declarative CHART overview widget. The spec names
// a data route, the row array on its response, an x field and the
// series to plot; the runtime fetches once and draws it. No registered
// JS slot, no per-project chart code, no chart library in the
// consumer's tree.
//
// Fetch discipline mirrors StatWidget deliberately: a spinner while in
// flight, a quiet dimmed "Unavailable" on failure. One dead tile must
// not shout over the seven healthy ones beside it — the overview is
// the first screen after login, and a red banner there reads as "the
// admin is broken". A valid response with no rows is a different,
// non-error state and says "No data".
//
// Everything the chart is MADE of — wire coercion, the palette, the
// "Other" fold — lives in ../chartData; this file is the fetch, the
// chrome, and the choice of Mantine component.

import { useEffect, useState } from "react";
import { AreaChart, BarChart, DonutChart, LineChart } from "@mantine/charts";
import { Loader, Paper, Stack, Text, useComputedColorScheme } from "@mantine/core";

import { apiGet } from "../api";
import {
  CHART_X_KEY,
  cappedSeries,
  chartOtherColor,
  chartPalette,
  chartRows,
  chartSeriesData,
  chartSeriesDefs,
  donutData,
  type ChartRow,
} from "../chartData";
import type { AdminWidgetSpec } from "../types";

export interface ChartWidgetProps {
  widget: AdminWidgetSpec;
}

// Mantine charts size to their container, and a container with no
// height collapses to nothing — the plot area needs an explicit one.
// A single fixed height (rather than one per widget size) keeps a
// SMALL, MEDIUM and LARGE tile on the same baseline when they share a
// grid row.
const CHART_HEIGHT = 220;

// Donut geometry: a ring thin enough to read as a ring, sized to leave
// room for the legend inside CHART_HEIGHT.
const DONUT_SIZE = 150;
const DONUT_THICKNESS = 26;

type ChartState = { status: "loading" } | { status: "ok"; rows: ChartRow[] } | { status: "error" };

export function ChartWidget({ widget }: ChartWidgetProps) {
  const [state, setState] = useState<ChartState>({ status: "loading" });
  const endpoint = widget.endpoint;
  const rowsField = widget.rows_field ?? "";
  useEffect(() => {
    // A CHART widget with no data route can never resolve — treat the
    // broken declaration as an unavailable tile rather than spinning
    // forever.
    if (!endpoint) {
      setState({ status: "error" });
      return;
    }
    let cancelled = false;
    apiGet<unknown>(endpoint)
      .then((resp) => {
        if (!cancelled) setState({ status: "ok", rows: chartRows(resp, rowsField) });
      })
      .catch(() => {
        if (!cancelled) setState({ status: "error" });
      });
    return () => {
      cancelled = true;
    };
  }, [endpoint, rowsField]);
  return (
    <Paper withBorder p="lg" radius="md" h="100%">
      <Stack gap="md">
        {widget.title && (
          <Text fw={600} fz="sm">
            {widget.title}
          </Text>
        )}
        {state.status === "loading" ? (
          <Loader size="sm" />
        ) : state.status === "error" ? (
          <Text c="dimmed" fz="sm">
            Unavailable
          </Text>
        ) : (
          <ChartBody widget={widget} rows={state.rows} />
        )}
      </Stack>
    </Paper>
  );
}

// ChartBody picks the Mantine chart for the declared kind and feeds it
// spec-derived data. Split from ChartWidget so the color scheme is
// read (a hook, hence unconditional) only on the path that draws.
function ChartBody({ widget, rows }: { widget: AdminWidgetSpec; rows: ChartRow[] }) {
  // The palette is mode-SELECTED, not auto-flipped, so the chart needs
  // the resolved scheme — "auto" has to become light or dark first.
  const scheme = useComputedColorScheme("light");
  if (rows.length === 0) return <NoData />;
  const xField = widget.x_field ?? "";

  if (widget.chart_kind === "DONUT") {
    // A part-of-whole has exactly one measure: the first declared
    // series. Anything after it would need a second ring, which is
    // just two charts wearing one.
    const valueField = cappedSeries(widget.series)[0]?.field ?? "";
    const cells = valueField
      ? donutData(rows, xField, valueField, chartPalette(scheme), chartOtherColor(scheme))
      : [];
    if (cells.length === 0) return <NoData />;
    // Legend always: a slice carries its identity in color alone, so
    // dropping the legend would make the chart unreadable, not tidier.
    return (
      <DonutChart
        data={cells}
        withLegend
        h={CHART_HEIGHT}
        size={DONUT_SIZE}
        thickness={DONUT_THICKNESS}
      />
    );
  }

  const series = chartSeriesDefs(widget.series, scheme);
  if (series.length === 0) return <NoData />;
  const data = chartSeriesData(rows, xField, widget.series);
  // A legend earns its space only past one series — with a single
  // series the card title already names what is plotted.
  const common = {
    data,
    dataKey: CHART_X_KEY,
    h: CHART_HEIGHT,
    withLegend: series.length >= 2,
  };
  // Tooltips stay at Mantine's default (on): they are the interaction
  // layer that lets a reader recover exact values the axis rounds off.
  switch (widget.chart_kind) {
    case "BAR":
      return <BarChart {...common} series={series} />;
    case "AREA":
      return <AreaChart {...common} series={series} />;
    case "LINE":
    default:
      return <LineChart {...common} series={series} />;
  }
}

// NoData: a valid response with zero rows is a legitimate steady state
// (a fresh install, a quiet window), not a failure. Saying so in words
// beats drawing an empty axis frame, which reads as broken.
function NoData() {
  return (
    <Text c="dimmed" fz="sm">
      No data
    </Text>
  );
}
