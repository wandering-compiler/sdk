import { afterEach, describe, expect, it, vi } from "vitest";
import type { Mock } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { MantineProvider } from "@mantine/core";

import { TranslateProvider, translatorFor } from "./i18n";
import { chartSeriesDefs } from "./chartData";
import { columnHeader, pageLabel } from "./format";
import type { AdminPageSpec, AdminWidgetValueSpec } from "./types";

vi.mock("./api", async () => {
  const actual = await vi.importActual<typeof import("./api")>("./api");
  return { ...actual, apiGet: vi.fn() };
});
const { apiGet } = await import("./api");

// Every string the compiler RESOLVED — a widget title, a series label, a
// column header override, a page or nav-group title, a fieldset title — is
// harvested into the project's .po as a msgid (admin/catalog.go: "every string
// the admin SPA will render that a translator should see"). So each of them
// has to reach the translator at render time, or the translator's work sits in
// the catalog and is never shown.
//
// Eight of them didn't (T2-6 pass #7, A1). The sharpest proof was inside one
// component: DetailPage renders `t(fs.title)` in its accordion branch and
// `{fs.title}` raw in the flat branch — the same value, from the same map,
// picked by whether a NEIGHBOURING fieldset happens to be collapsible.
//
// These are RENDER tests on purpose. The vocab gate added in pass #6 scans
// source lines for untranslated prose and skips any line containing `{}()<>`,
// so `{widget.title}` is invisible to it by construction — no amount of
// widening a line scanner sees this class. Asking the rendered DOM whether a
// marker translation came through does, and it fails for the right reason: not
// "you wrote a bare string" but "the user would have seen English".
//
// The mirror matters too: a HUMANIZED fallback is derived, not authored, and
// deliberately is not harvested — so it must NOT be sent through the
// translator, or a project could "translate" a string no catalog will ever
// contain.

const MARKER = "PŘELOŽENO";

function translate(msgids: string[]) {
  const cs = Object.fromEntries(msgids.map((m) => [m, `${MARKER}:${m}`]));
  return translatorFor({ default_language: "cs", catalogs: { cs } });
}

function renderWith(msgids: string[], node: React.ReactNode) {
  return render(
    <MantineProvider>
      <TranslateProvider value={translate(msgids)}>{node}</TranslateProvider>
    </MantineProvider>,
  );
}

afterEach(cleanup);

describe("spec-resolved prose reaches the translator", () => {
  it("translates a declared column header, and leaves a humanized name alone", () => {
    const t = translate(["Заголовок", "id"]);
    expect(columnHeader({ name: "title", label: "Заголовок" }, t)).toBe(`${MARKER}:Заголовок`);
    // `id` has no declared label: humanizeLabel wins and never goes through
    // the catalog, even though a catalog entry for it exists here.
    expect(columnHeader({ name: "id" }, t)).toBe("ID");
  });

  it("translates a declared page title, and leaves a humanized page name alone", () => {
    const t = translate(["Poznámky", "notes"]);
    const titled = { name: "notes", title: "Poznámky" } as AdminPageSpec;
    const untitled = { name: "notes" } as AdminPageSpec;
    expect(pageLabel(titled, t)).toBe(`${MARKER}:Poznámky`);
    expect(pageLabel(untitled, t)).toBe("Notes");
  });

  it("translates a declared chart series label, and leaves a humanized field alone", () => {
    const t = translate(["Součet", "row_count"]);
    const series: AdminWidgetValueSpec[] = [
      { field: "total", label: "Součet" },
      { field: "row_count" },
    ];
    const defs = chartSeriesDefs(series, "light", t);
    expect(defs[0].label).toBe(`${MARKER}:Součet`);
    expect(defs[1].label).toBe("Row count");
  });

  it("renders a whitespace-only label as its humanized field, untranslated", () => {
    const t = translate(["   "]);
    expect(chartSeriesDefs([{ field: "total", label: "   " }], "light", t)[0].label).toBe("Total");
    expect(columnHeader({ name: "total", label: "  " }, t)).toBe("Total");
  });

  it("keeps the translator out of the humanized path even when it would match", () => {
    // A catalog that happens to contain the humanized form must not change it:
    // the humanized string is not a msgid, so translating it would be a
    // coincidence the .po never promised.
    const t = translate(["Total"]);
    expect(columnHeader({ name: "total" }, t)).toBe("Total");
  });
});

// A rendered guard for the shape that started this: the same title, one branch
// translated and one not, chosen by whether a NEIGHBOURING fieldset is
// collapsible.
describe("DetailPage fieldset titles", () => {
  it("translates a fieldset title in both the collapsible and the flat branch", async () => {
    const { DetailPage } = await import("./DetailPage");
    (apiGet as unknown as Mock).mockResolvedValue({ id: 1 });

    const pageFor = (collapsed: boolean) =>
      ({
        name: "Notes",
        title: "Notes",
        detail: {
          endpoint: "/admin/api/detail/Notes",
          read_endpoint: "/admin/api/detail/Notes/{id}",
          fields: ["id"],
          fieldsets: [
            { title: "Základ", fields: ["id"], ...(collapsed ? { collapsed: true } : {}) },
          ],
        },
      }) as unknown as AdminPageSpec;

    for (const collapsed of [true, false]) {
      cleanup();
      renderWith(
        ["Základ"],
        <DetailPage
          spec={{ pages: {} } as never}
          page={pageFor(collapsed)}
          rowId="1"
          onBack={() => {}}
          onSelectInlineRow={() => {}}
        />,
      );
      const hits = await screen.findAllByText(new RegExp(MARKER));
      expect(
        hits.length,
        `fieldset title must be translated in the ${collapsed ? "collapsible" : "flat"} branch`,
      ).toBeGreaterThan(0);
    }
  });
});

// A2 — a STAT tile is not always a count. The emitter lowers a DATETIME format
// for a timestamp field and the parser accepts any scalar in `values` (unlike
// chart series, which it checks for numeric), so a tile can legitimately carry
// a timestamp. Squeezing that through wireNumberText first yields undefined,
// the `?? "0"` turns it into a zero, and the tile renders "0" for a date the
// formatter handles perfectly from the raw value — which is what the sibling
// paths already do: axisLabeler and renderCell both receive it untouched.
describe("STAT values that are not numbers", () => {
  it("formats a timestamp through its declared datetime format", async () => {
    const { formatStatValue } = await import("./OverviewPage");
    const tmpl = { msgid: "{value}", slots: [{ name: "value", preset: "datetime" as const }] };
    const out = formatStatValue("2026-07-28T12:34:56Z", tmpl, { locale: "en" });
    expect(out).not.toBe("0");
    expect(out).toMatch(/2026/);
  });

  it("still coerces for a numeric format, so an omitted count reads as 0", async () => {
    const { formatStatValue } = await import("./OverviewPage");
    const tmpl = { msgid: "{value}", slots: [{ name: "value", preset: "number" as const }] };
    expect(formatStatValue(undefined, tmpl, { locale: "en" })).toBe("0");
    expect(formatStatValue("42", tmpl, { locale: "en" })).toMatch(/42/);
  });
});

// A4 — a widget's data route is permission-gated server-side, and the spec now
// carries those perms so the SPA can hide a card the caller could never open.
// Without it the overview showed permanently "Unavailable" tiles to anyone
// without the perm, while pages, actions and default tiles were hidden
// properly. No leak either way — the server gate is what enforces.
describe("widget permissions", () => {
  it("hides a widget whose perms the user lacks, and keeps the ones they have", async () => {
    const { renderableWidgets } = await import("./OverviewPage");
    const widgets = [
      { slot: "a", size: "SMALL", kind: "STAT", required_permissions: [7] },
      { slot: "b", size: "SMALL", kind: "STAT", required_permissions: [7, 9] },
      { slot: "c", size: "SMALL", kind: "CHART" },
    ] as never[];
    const kept = renderableWidgets(widgets, {}, [7]).map((w) => w.slot);
    expect(kept).toEqual(["a", "c"]);
  });

  it("keeps every widget when the spec declares no perms", async () => {
    const { renderableWidgets } = await import("./OverviewPage");
    const widgets = [{ slot: "a", size: "SMALL", kind: "STAT" }] as never[];
    expect(renderableWidgets(widgets, {}, []).map((w) => w.slot)).toEqual(["a"]);
  });
});

// A3 — the emitter has always produced `filter_choices` for an enum-typed
// filter, and the catalog harvest has always sent their labels to translators.
// The runtime never read them, so an enum filter was a free-text box: the
// operator saw "Active" in the column and had to guess the wire token to
// filter by it, and the translated labels sat in the .po with no consumer.
describe("enum filters", () => {
  it("renders the spec's choices as options, translated, skipping deprecated ones", async () => {
    const { ListPage } = await import("./ListPage");
    (apiGet as unknown as Mock).mockResolvedValue({ items: [], paging: {} });

    const page = {
      name: "Tasks",
      list: {
        endpoint: "/admin/api/list/Tasks",
        item_type: "Task",
        columns: [{ name: "id" }],
        filters: ["status"],
        filter_types: { status: "ENUM" },
        filter_choices: {
          status: {
            enum_fqn: "app.Status",
            carrier: "scalar_int32",
            values: [
              { value: 1, label: "Aktivní" },
              { value: 2, label: "Hotovo" },
              { value: 3, label: "Zrušeno", deprecated: true },
            ],
          },
        },
      },
    } as unknown as AdminPageSpec;

    renderWith(
      ["Aktivní"],
      <ListPage spec={{ name: "admin", pages: {} } as never} page={page} onSelectRow={() => {}} />,
    );
    // Mantine renders a Select as an input carrying the chosen label; the
    // options live behind a click, so assert on the data the component got by
    // opening it.
    // Mantine renders a Select as a combobox input; the options appear on
    // click. Asserting on the rendered options is what proves the runtime
    // READ the catalogue rather than falling back to a free-text box.
    const [input] = await screen.findAllByRole("combobox");
    fireEvent.click(input);
    expect(await screen.findByText(/PŘELOŽENO:Aktivní/)).toBeTruthy();
    expect(screen.queryByText("Zrušeno")).toBeNull();
  });
});
