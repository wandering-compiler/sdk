import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import { MantineProvider } from "@mantine/core";

import { OverviewPage } from "./OverviewPage";
import type { AdminSpec, AdminWidgetSpec } from "./types";

vi.mock("./api", async () => {
  const actual = await vi.importActual<typeof import("./api")>("./api");
  return { ...actual, apiGet: vi.fn() };
});
const { apiGet } = await import("./api");

// A STAT widget's declared values are ordinary model fields, so a count
// is an int64 and reaches the SPA as the decimal STRING protojson and
// the generated codecs both produce. This test renders the real tile
// against exactly that response, because the shape is invisible to the
// unit tests of every layer above it: a widget value that fails to
// coerce does not throw and does not blank the tile — it renders "0",
// which is what a count legitimately looks like. Only a rendered tile
// carrying a KNOWN non-zero value can tell the two apart.
const statSpec = (): AdminSpec => {
  const widget: AdminWidgetSpec = {
    slot: "wallet_stats",
    size: "MEDIUM",
    kind: "STAT",
    title: "Wallets",
    endpoint: "/admin/api/widget/wallet_stats",
    values: [{ field: "wallet_count", label: "Wallets" }, { field: "balance_total" }],
  };
  return {
    name: "Admin",
    schema_version: "3",
    auth: { login_endpoint: "/login", whoami_endpoint: "/whoami" },
    pages: {},
    overview: { widgets: [widget] },
  } as AdminSpec;
};

function renderOverview(spec: AdminSpec = statSpec()) {
  render(
    <MantineProvider>
      <OverviewPage spec={spec} overview={spec.overview} slots={{}} whoami={null} />
    </MantineProvider>,
  );
}

describe("OverviewPage STAT widget", () => {
  afterEach(() => {
    cleanup();
    vi.resetAllMocks();
  });

  it("renders an int64 value that arrives as a string, grouped", async () => {
    vi.mocked(apiGet).mockResolvedValue({ wallet_count: "5", balance_total: "399400" });
    renderOverview();
    await waitFor(() => expect(screen.getByText("5")).toBeTruthy());
    expect(screen.getByText("399,400")).toBeTruthy();
    // The failure this guards is silent, so assert the wrong answer is
    // absent rather than only that the right one is present.
    expect(screen.queryByText("0")).toBeNull();
  });

  it("renders a 32-bit value that arrives as a number", async () => {
    vi.mocked(apiGet).mockResolvedValue({ wallet_count: 5, balance_total: 399400 });
    renderOverview();
    await waitFor(() => expect(screen.getByText("5")).toBeTruthy());
    expect(screen.getByText("399,400")).toBeTruthy();
  });

  // A STAT cell is not always a count. When the compiler resolved a
  // format for the field — the same one the list column gets — the tile
  // reads that way, so one number does not read two ways on one screen.
  it("renders a declared money value through its format", async () => {
    const spec = statSpec();
    spec.overview!.widgets[0].value_formats = {
      balance_total: {
        msgid: "{value}",
        slots: [{ name: "value", preset: "decimal", places: 2, has_places: true }],
      },
    };
    vi.mocked(apiGet).mockResolvedValue({ wallet_count: "5", balance_total: "399400" });
    renderOverview(spec);
    await waitFor(() => expect(screen.getByText("399,400.00")).toBeTruthy());
    // The undeclared cell keeps the plain grouped count.
    expect(screen.getByText("5")).toBeTruthy();
  });

  // The wire coercion still runs FIRST under a format: an omitted
  // zero-valued field must read as a formatted zero, not as blank.
  it("renders an omitted field as a formatted zero", async () => {
    const spec = statSpec();
    spec.overview!.widgets[0].value_formats = {
      balance_total: {
        msgid: "{value}",
        slots: [{ name: "value", preset: "decimal", places: 2, has_places: true }],
      },
    };
    vi.mocked(apiGet).mockResolvedValue({ wallet_count: "5" });
    renderOverview(spec);
    await waitFor(() => expect(screen.getByText("0.00")).toBeTruthy());
  });

  // EmitDefaultValues:false omits a zero-valued field entirely, so an
  // absent field IS a zero and must render as one.
  it("renders an omitted (zero) field as 0", async () => {
    vi.mocked(apiGet).mockResolvedValue({ wallet_count: "5" });
    renderOverview();
    await waitFor(() => expect(screen.getByText("5")).toBeTruthy());
    expect(screen.getByText("0")).toBeTruthy();
  });
});
