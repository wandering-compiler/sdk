// ChartWidgetLazy — the code-split door to the CHART widget.
//
// What is worth asserting here is NOT the chart (chartwidget.test.tsx
// owns spec→chart-props translation, rendering the widget directly).
// It is the two things the split adds:
//   1. the wait is invisible — the placeholder is the SAME card the
//      loaded widget renders, with the same small Loader the rest of
//      the overview uses, so the chunk arriving causes no layout jump;
//   2. the chunk really does arrive and hand over to the real widget.
// The failure path (a chunk that never loads) is chartchunkfail.test
// .tsx — it has to mock the lazily-imported module away, which is a
// whole-file decision.

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import { MantineProvider } from "@mantine/core";

import { ChartWidgetLazy } from "./components/ChartWidgetLazy";
import type { AdminWidgetSpec } from "./types";

vi.mock("./api", async () => {
  const actual = await vi.importActual<typeof import("./api")>("./api");
  return { ...actual, apiGet: vi.fn() };
});
const { apiGet } = await import("./api");

// Same stub seam as chartwidget.test.tsx: jsdom gives the charts a
// zero-size box, so the real ones draw nothing assertable. Here the
// stub only has to prove WHICH module got loaded.
vi.mock("@mantine/charts", () => {
  const stub = (kind: string) => () => <div data-testid={`chart-${kind}`} />;
  return {
    LineChart: stub("line"),
    BarChart: stub("bar"),
    AreaChart: stub("area"),
    DonutChart: stub("donut"),
  };
});

const widget: AdminWidgetSpec = {
  slot: "signups_chart",
  size: "LARGE",
  kind: "CHART",
  title: "Signups per day",
  endpoint: "/admin/api/widget/signups_chart",
  chart_kind: "LINE",
  rows_field: "rows",
  x_field: "day",
  series: [{ field: "total" }],
};

function renderLazy() {
  return render(
    <MantineProvider>
      <ChartWidgetLazy widget={widget} />
    </MantineProvider>,
  );
}

describe("ChartWidgetLazy", () => {
  beforeEach(() => {
    vi.mocked(apiGet).mockResolvedValue({ rows: [{ day: "2026-07-01", total: 12 }] });
  });
  afterEach(() => {
    cleanup();
    vi.resetAllMocks();
  });

  it("shows the widget's own card and spinner while the chart chunk loads", () => {
    const { container } = renderLazy();
    // The title is up immediately — the tile occupies its slot in the
    // grid at full size before the chunk lands, so nothing below it
    // moves when it does.
    expect(screen.getByText("Signups per day")).toBeDefined();
    // The quiet placeholder discipline of StatWidget/CountBadge: a
    // small Mantine Loader, not a skeleton, not an error, not text.
    expect(container.querySelector(".mantine-Loader-root[data-size='sm']")).not.toBeNull();
    expect(screen.queryByText("Unavailable")).toBeNull();
    // Nothing has been fetched yet: the data route belongs to the
    // lazily-loaded widget, so the chunk is genuinely not resolved.
    expect(apiGet).not.toHaveBeenCalled();
  });

  it("hands over to the real ChartWidget once the chunk resolves", async () => {
    renderLazy();
    await waitFor(() => expect(screen.getByTestId("chart-line")).toBeDefined());
    expect(apiGet).toHaveBeenCalledWith("/admin/api/widget/signups_chart");
    // The placeholder is gone and the title never flickered.
    expect(screen.getByText("Signups per day")).toBeDefined();
  });
});
