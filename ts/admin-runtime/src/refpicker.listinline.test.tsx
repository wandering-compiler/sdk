import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MantineProvider } from "@mantine/core";

import { InlineSection } from "./InlineSection";
import { ListPage } from "./ListPage";
import type { AdminInlineSpec, AdminPageSpec, AdminSpec } from "./types";

vi.mock("./api", async () => {
  const actual = await vi.importActual<typeof import("./api")>("./api");
  return { ...actual, apiGet: vi.fn(), apiPost: vi.fn(), apiPatch: vi.fn() };
});
const { apiGet, apiPost } = await import("./api");

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

// The pickers of the forms (refpicker.test.tsx) on the two other places the
// admin takes a value: a list's filters and an inline's add / edit form.

const orgs = [
  { id: "o-1", slug: "acme", name: "Acme" },
  { id: "o-2", slug: "globex", name: "Globex" },
];
const roles = [
  { product: "maps", key: "geo_reader", name: "Geo reader" },
  { product: "fin", key: "ledger", name: "Ledger" },
];

const orgsPage = {
  name: "Organizations",
  model: "auth.Organization",
  list: { endpoint: "/admin/api/list/Organizations", columns: [] },
} as unknown as AdminPageSpec;
const rolesPage = {
  name: "ProductRoles",
  model: "auth.ProductRole",
  list: { endpoint: "/admin/api/list/ProductRoles", columns: [] },
} as unknown as AdminPageSpec;

// A filter by the organization's SLUG (not its id), and a role narrowed by
// the product filter.
const grantsPage = {
  name: "Grants",
  model: "auth.Grant",
  list: {
    endpoint: "/admin/api/list/Grants",
    item_type: "Grant",
    columns: [{ name: "id" }, { name: "org" }],
    filters: ["org", "product", "role_key"],
    field_refs: {
      org: { page: "Organizations", value: "slug", title: "{name}" },
      product: { page: "ProductRoles", value: "product", title: "{product}" },
      role_key: {
        page: "ProductRoles",
        value: "key",
        title: "{name}",
        depends_on: "product",
        parent: "product",
      },
    },
  },
} as unknown as AdminPageSpec;

const spec = {
  name: "admin",
  pages: { Organizations: orgsPage, ProductRoles: rolesPage, Grants: grantsPage },
} as unknown as AdminSpec;

function serve() {
  vi.mocked(apiGet).mockImplementation((url: string) => {
    if (url.startsWith("/admin/api/list/Organizations")) return Promise.resolve({ orgs });
    if (url.startsWith("/admin/api/list/ProductRoles")) return Promise.resolve({ roles });
    if (url.startsWith("/admin/api/list/Grants")) return Promise.resolve({ grants: [] });
    if (url.startsWith("/admin/api/inline/")) return Promise.resolve({ items: [] });
    return Promise.reject(new Error("unexpected " + url));
  });
}

const grantsCalls = () =>
  vi
    .mocked(apiGet)
    .mock.calls.map(([u]) => u)
    .filter((u) => u.startsWith("/admin/api/list/Grants"));

describe("a picker filter", () => {
  it("filters by the chosen row's value, chosen by its text — and clears to no filter", async () => {
    serve();
    render(
      <MantineProvider>
        <ListPage spec={spec} page={grantsPage} onSelectRow={vi.fn()} />
      </MantineProvider>,
    );
    const user = userEvent.setup();
    await waitFor(() => expect(grantsCalls().length).toBe(1));

    await user.click(screen.getByRole("combobox", { name: /^org/i }));
    await user.click(await screen.findByText("Globex"));
    await user.click(screen.getByRole("button", { name: /apply/i }));
    await waitFor(() => expect(grantsCalls().at(-1)).toContain("org=globex"));

    // Clearing the picker drops the filter, as an emptied text box does.
    await user.click(screen.getByRole("button", { name: /clear/i }));
    await waitFor(() => expect(grantsCalls().at(-1)).not.toContain("org="));
  });

  it("offers only the roles of the product filtered by", async () => {
    serve();
    render(
      <MantineProvider>
        <ListPage spec={spec} page={grantsPage} onSelectRow={vi.fn()} />
      </MantineProvider>,
    );
    const user = userEvent.setup();
    await user.click(await screen.findByRole("combobox", { name: /^product/i }));
    await user.click(await screen.findByText("maps"));
    await user.click(screen.getByRole("combobox", { name: /role key/i }));
    expect(await screen.findByText("Geo reader")).toBeTruthy();
    expect(screen.queryByText("Ledger")).toBeNull();
    await user.click(screen.getByText("Geo reader"));
    await user.click(screen.getByRole("button", { name: /apply/i }));
    await waitFor(() => expect(grantsCalls().at(-1)).toContain("role_key=geo_reader"));
  });
});

describe("a picker in an inline form", () => {
  const lendersPage = {
    name: "Lenders",
    model: "app.Lender",
    list: { columns: [{ name: "id" }, { name: "org_id" }, { name: "note" }] },
    detail: {
      read_endpoint: "/r",
      // The page's own (update) form edits the note only; the inline's add
      // form posts org_id too, from create_fields.
      fields: ["note"],
      // The inline page's own form picks org_id; the inline itself adds nothing.
      field_refs: { org_id: { page: "Organizations", value: "id", title: "{name}" } },
    },
  } as unknown as AdminPageSpec;
  const withLenders = {
    ...spec,
    pages: { ...spec.pages, Lenders: lendersPage },
  } as unknown as AdminSpec;
  const inline: AdminInlineSpec = {
    page: "Lenders",
    endpoint: "/admin/api/inline/Orgs/{id}/Lenders",
    layout: "TABULAR",
    create_endpoint: "/admin/api/inline/Orgs/{id}/Lenders",
    // The create request's own fields — org_id is not among the inline
    // page's detail fields' order, and note is.
    create_fields: ["org_id", "note"],
  };

  async function addWith(inl: AdminInlineSpec, pick: string) {
    serve();
    vi.mocked(apiPost).mockResolvedValue({});
    render(
      <MantineProvider>
        <InlineSection spec={withLenders} inline={inl} parentId="7" onSelectChild={vi.fn()} />
      </MantineProvider>,
    );
    const user = userEvent.setup();
    await user.click(await screen.findByRole("button", { name: /add lenders/i }));
    await user.click(screen.getByRole("combobox", { name: /org id/i }));
    await user.click(await screen.findByText(pick));
    await user.click(screen.getByRole("button", { name: /^create$/i }));
    await waitFor(() => expect(apiPost).toHaveBeenCalled());
    return vi.mocked(apiPost).mock.calls[0][1];
  }

  it("takes the inline page's own picker for a field the inline names none for", async () => {
    expect(await addWith(inline, "Acme")).toMatchObject({ org_id: "o-1" });
  });

  it("takes the inline's own picker over the page's", async () => {
    const own = {
      ...inline,
      field_refs: { org_id: { page: "Organizations", value: "slug", title: "{slug}" } },
    };
    expect(await addWith(own, "globex")).toMatchObject({ org_id: "globex" });
  });
});
