import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import { MantineProvider } from "@mantine/core";

import { DetailPage } from "./DetailPage";
import type { AdminPageSpec, AdminSpec } from "./types";
import type { FormatTemplate } from "./cellFormat";

vi.mock("./api", async () => {
  const actual = await vi.importActual<typeof import("./api")>("./api");
  return { ...actual, apiGet: vi.fn() };
});
const { apiGet } = await import("./api");

// The language override below is a GLOBAL: navigator is one object for the whole
// worker, so a test that pins "cs" and walks away leaves every later test in the
// same worker formatting in Czech. That is invisible under the default pool, which
// gives each file its own worker, and it is what made four OverviewPage tests fail
// ~37% of the time under a single worker: they assert "399,400" and got "399 400"
// — a narrow no-break space and a comma decimal, which is Czech.
// DELETE the override rather than restoring a captured descriptor. In a shared
// worker the descriptor read at module load may already BE another file's
// override, so "restoring" it would put that language back. navigator.language
// lives on Navigator.prototype, so removing the own property un-shadows the real
// getter — which is the only value that is correct no matter who ran first.
function clearNavigatorLanguage() {
  delete (window.navigator as { language?: string }).language;
}

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
  clearNavigatorLanguage();
});

// A detail view formats the same value the
// list formats, but ONLY where that value is display-only. The distinction is
// the whole design: an editable field's value round-trips through the form and
// back out on save, so formatting it would PATCH a string the user never typed.

const moneyFormat: FormatTemplate = {
  msgid: "{value}",
  slots: [{ name: "value", preset: "decimal", places: 2, has_places: true }],
};
const dateFormat: FormatTemplate = {
  msgid: "{value}",
  slots: [{ name: "value", preset: "datetime" }],
};

const spec = { default_language: "cs" } as AdminSpec;

function page(overrides: Partial<AdminPageSpec["detail"]> = {}): AdminPageSpec {
  return {
    name: "Invoices",
    detail: {
      read_endpoint: "/admin/api/detail/Invoices/{id}",
      update_endpoint: "/admin/api/detail/Invoices/{id}",
      fields: ["amount"],
      readonly_fields: ["created_at"],
      field_formats: { amount: moneyFormat, created_at: dateFormat },
      ...overrides,
    },
  } as AdminPageSpec;
}

function renderDetail(p: AdminPageSpec) {
  return render(
    <MantineProvider>
      <DetailPage spec={spec} page={p} rowId="1" onBack={() => {}} />
    </MantineProvider>,
  );
}

// The admin formats for the BROWSER's locale (§3: formatting follows the
// region, not the UI language), so the test pins one rather than trusting the
// runner's.
function withNavigatorLanguage(lang: string) {
  Object.defineProperty(window.navigator, "language", { value: lang, configurable: true });
}

describe("DetailPage value formatting", () => {
  it("formats a READONLY field and leaves an editable one raw", async () => {
    withNavigatorLanguage("cs");
    vi.mocked(apiGet).mockResolvedValue({
      amount: "1234.5",
      created_at: "2026-07-26T14:03:00Z",
    });
    renderDetail(page());

    // The readonly field is display-only: Czech date order, dot-separated.
    await waitFor(() => {
      expect(screen.getByDisplayValue("26.07.2026 14:03")).toBeTruthy();
    });
    // The editable one is what the form will PATCH back, so it stays exactly
    // as the wire delivered it.
    expect(screen.getByDisplayValue("1234.5")).toBeTruthy();
    expect(screen.queryByDisplayValue("1 234,50")).toBeNull();
  });

  // A form nobody can submit has no value to protect, so every field on it is
  // display-only. Without this, a read-only-by-permission detail page would
  // show a raw ISO timestamp next to a formatted one — the same value
  // rendering two ways on one screen.
  it("formats an editable field when the form cannot be submitted at all", async () => {
    withNavigatorLanguage("cs");
    vi.mocked(apiGet).mockResolvedValue({
      amount: "1234.5",
      created_at: "2026-07-26T14:03:00Z",
    });
    renderDetail(page({ update_endpoint: undefined }));

    await waitFor(() => {
      expect(screen.getByDisplayValue("1 234,50")).toBeTruthy();
    });
  });

  // The locale is the viewer's, not the project's — the same spec renders
  // differently for a differently-configured browser, which is the point of
  // the whole table.
  it("follows the viewer's locale", async () => {
    withNavigatorLanguage("en");
    vi.mocked(apiGet).mockResolvedValue({
      amount: "1234.5",
      created_at: "2026-07-26T14:03:00Z",
    });
    renderDetail(page({ update_endpoint: undefined }));

    await waitFor(() => {
      expect(screen.getByDisplayValue("1,234.50")).toBeTruthy();
    });
    // `en` is day-first; month-first is `en-US`, which has its own row.
    expect(screen.getByDisplayValue("26/07/2026 14:03")).toBeTruthy();
  });

  // A spec with no field_formats is the pre-feature world, and it has to keep
  // rendering exactly what it rendered.
  it("leaves everything raw when the compiler resolved no format", async () => {
    withNavigatorLanguage("cs");
    vi.mocked(apiGet).mockResolvedValue({
      amount: "1234.5",
      created_at: "2026-07-26T14:03:00Z",
    });
    renderDetail(page({ field_formats: undefined, update_endpoint: undefined }));

    await waitFor(() => {
      expect(screen.getByDisplayValue("1234.5")).toBeTruthy();
    });
    expect(screen.getByDisplayValue("2026-07-26T14:03:00Z")).toBeTruthy();
  });
});

// The row heading comes from the model's (w17.admin_title), which the compiler
// writes into title_templates under the model ref (page.model). It was read
// under the page's name instead, so no admin ever showed one: the page name is
// an identifier ("OrgPlans"), the key a model ref ("billing.PlanLine").
describe("DetailPage row heading", () => {
  const titled = {
    ...spec,
    title_templates: { "billing.PlanLine": "{org_name} → {product}" },
  } as AdminSpec;

  it("formats the model's title template for the row", async () => {
    vi.mocked(apiGet).mockResolvedValue({ org_name: "Acme", product: "maps", amount: "1" });
    render(
      <MantineProvider>
        <DetailPage
          spec={titled}
          page={{ ...page(), name: "OrgPlans", model: "billing.PlanLine", title: "Organization plans" }}
          rowId="1"
          onBack={() => {}}
        />
      </MantineProvider>,
    );
    await waitFor(() => {
      expect(screen.getByText("Acme → maps")).toBeTruthy();
    });
    // The page's own title is its label, shown beside the row's id.
    expect(screen.getByText("Organization plans · 1")).toBeTruthy();
  });

  it("falls back to the page label when the model has no template", async () => {
    vi.mocked(apiGet).mockResolvedValue({ amount: "1" });
    render(
      <MantineProvider>
        <DetailPage
          spec={titled}
          page={{ ...page(), name: "Invoices", model: "billing.Invoice", title: "Invoices" }}
          rowId="1"
          onBack={() => {}}
        />
      </MantineProvider>,
    );
    await waitFor(() => {
      expect(screen.getByText("ID: 1")).toBeTruthy();
    });
    expect(screen.getByText("Invoices")).toBeTruthy();
  });
});
