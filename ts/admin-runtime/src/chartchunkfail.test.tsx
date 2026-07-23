// The chart chunk that never arrives.
//
// Splitting the chart out buys a smaller bundle at the price of a new
// failure mode: the browser has to fetch assets/ChartWidget-*.js at
// render time, and that request can fail — offline, or a cached
// index.html pointing at an asset a redeploy replaced. React.lazy
// rethrows such a failure to the nearest error boundary, and the admin
// declares none above the overview, so WITHOUT the boundary inside
// ChartWidgetLazy one missing file blanks the whole console. This file
// pins the boundary: the tile degrades to the same quiet "Unavailable"
// a failed data fetch produces, and the page around it survives.
//
// Own file because the only way to make the dynamic import reject is
// to mock the imported module away, which is file-scoped.

import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import { MantineProvider, Text } from "@mantine/core";

import { ChartWidgetLazy } from "./components/ChartWidgetLazy";
import type { AdminWidgetSpec } from "./types";

// A factory that throws makes import("./ChartWidget") reject — the
// closest stand-in for a chunk the network could not deliver.
vi.mock("./components/ChartWidget", () => {
  throw new Error("simulated chunk fetch failure");
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

describe("ChartWidgetLazy chunk failure", () => {
  afterEach(() => {
    cleanup();
    vi.restoreAllMocks();
  });

  it("degrades to a quiet 'Unavailable' tile and leaves the page standing", async () => {
    // React logs the caught error itself; silence it so a PASSING run
    // is not full of red, and assert we logged our own diagnostic.
    const errors = vi.spyOn(console, "error").mockImplementation(() => {});
    render(
      <MantineProvider>
        <ChartWidgetLazy widget={widget} />
        <Text>sibling tile</Text>
      </MantineProvider>,
    );

    await waitFor(() => expect(screen.getByText("Unavailable")).toBeDefined());
    // Same card, same title — the operator can still see WHICH tile is
    // down, exactly as with a failed data fetch.
    expect(screen.getByText("Signups per day")).toBeDefined();
    // The rest of the overview is untouched: no boundary above us ate
    // the page.
    expect(screen.getByText("sibling tile")).toBeDefined();
    // Quiet in the UI, loud in the console — a chunk that will not
    // load is a deploy problem, and nothing else would record it.
    expect(errors.mock.calls.some((c) => String(c[0]).includes("chart chunk failed to load"))).toBe(
      true,
    );
  });
});
