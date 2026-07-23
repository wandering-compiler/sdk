import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MantineProvider } from "@mantine/core";

import { ListPage, resolveRefCell } from "./ListPage";
import type { AdminColumnRefSpec, AdminListPagingSpec, AdminPageSpec, AdminSpec } from "./types";

vi.mock("./api", async () => {
  const actual = await vi.importActual<typeof import("./api")>("./api");
  return { ...actual, apiGet: vi.fn() };
});
const { apiGet } = await import("./api");

const page = (detail: Record<string, unknown> = {}): AdminPageSpec =>
  ({
    name: "Notes",
    list: {
      endpoint: "/admin/api/list/Notes",
      item_type: "Note",
      columns: [{ name: "id" }, { name: "title" }],
    },
    detail: {
      read_endpoint: "/admin/api/detail/Notes/{id}",
      create_endpoint: "/admin/api/detail/Notes",
      create_fields: ["title"],
      fields: ["title"],
      ...detail,
    },
  }) as unknown as AdminPageSpec;

function renderList(props: Record<string, unknown> = {}) {
  const onAdd = vi.fn();
  render(
    <MantineProvider>
      <ListPage
        spec={{ name: "admin", pages: {} } as unknown as AdminSpec}
        page={page()}
        onSelectRow={vi.fn()}
        onAdd={onAdd}
        {...props}
      />
    </MantineProvider>,
  );
  return { onAdd };
}

// The Add button is the only entry point to the create form, and it is
// gated on three independent things. Each of these was a real branch in
// the component; a regression in any one either hides a working feature
// or offers a form that 403s.
describe("ListPage Add button", () => {
  beforeEach(() => {
    vi.mocked(apiGet).mockResolvedValue({ notes: [] });
  });
  afterEach(cleanup);

  it("renders when the page declares create and the host routes it", async () => {
    renderList();
    await waitFor(() => expect(screen.getByRole("button", { name: /add/i })).toBeDefined());
  });

  it("is hidden when the page declares no create endpoint", async () => {
    renderList({ page: page({ create_endpoint: undefined }) });
    await waitFor(() => expect(apiGet).toHaveBeenCalled());
    expect(screen.queryByRole("button", { name: /add/i })).toBeNull();
  });

  // A host that doesn't route create must not show a dead button.
  it("is hidden when the host wires no onAdd", async () => {
    renderList({ onAdd: undefined });
    await waitFor(() => expect(apiGet).toHaveBeenCalled());
    expect(screen.queryByRole("button", { name: /add/i })).toBeNull();
  });

  // Backend enforces regardless; the SPA hides what would 403.
  it("is hidden when the caller lacks the create permission", async () => {
    renderList({
      page: page({ required_permissions_create: [7] }),
      whoami: { permission_ids: [1] },
    });
    await waitFor(() => expect(apiGet).toHaveBeenCalled());
    expect(screen.queryByRole("button", { name: /add/i })).toBeNull();
  });

  it("renders when the caller holds the create permission", async () => {
    renderList({
      page: page({ required_permissions_create: [7] }),
      whoami: { permission_ids: [7, 9] },
    });
    await waitFor(() => expect(screen.getByRole("button", { name: /add/i })).toBeDefined());
  });
});

// Cursor/keyset pagination — the SPA renders a Prev/Next footer (NOT
// offset page-numbers) and drives the list via opaque cursor tokens.
// These assert on the pager chrome (which renders outside the
// DataTable, so it survives jsdom) and on the exact URLs handed to the
// apiGet mock — the load-bearing contract with the paged Go handler.
describe("ListPage cursor pagination", () => {
  const PAGING: AdminListPagingSpec = {
    cursor_param: "cursor",
    limit_param: "limit",
    default_limit: 50,
    max_limit: 100,
    items_field: "notes",
    paging_field: "paging",
  };

  // A paged page declaring a filter + search + a sortable column so
  // the reset paths (Apply / Clear / sort) are all reachable.
  const pagedPage = (paging: AdminListPagingSpec | undefined = PAGING): AdminPageSpec =>
    ({
      name: "Notes",
      list: {
        endpoint: "/admin/api/list/Notes",
        item_type: "Note",
        columns: [{ name: "id" }, { name: "title" }],
        filters: ["status"],
        search: ["q"],
        sortable: ["title"],
        paging,
      },
      detail: { read_endpoint: "/admin/api/detail/Notes/{id}", fields: ["title"] },
    }) as unknown as AdminPageSpec;

  const renderPaged = (p: AdminPageSpec = pagedPage()) =>
    render(
      <MantineProvider>
        <ListPage
          spec={{ name: "admin", pages: {} } as unknown as AdminSpec}
          page={p}
          onSelectRow={vi.fn()}
        />
      </MantineProvider>,
    );

  // The URL of the most recent apiGet call.
  const lastURL = () => {
    const calls = vi.mocked(apiGet).mock.calls;
    return String(calls[calls.length - 1][0]);
  };

  afterEach(cleanup);

  it("renders Prev/Next + total for a paged list, and page-1 URL carries limit + filters", async () => {
    vi.mocked(apiGet).mockResolvedValue({
      notes: [{ id: 1, title: "a" }],
      paging: { total: 120, next_cursor: "NX", previous_cursor: "" },
    });
    renderPaged();

    // Footer count reads the envelope's total, not the page size.
    await waitFor(() => expect(screen.getByText(/120/)).toBeDefined());
    expect(screen.getByRole("button", { name: /next/i })).toBeDefined();
    expect(screen.getByRole("button", { name: /previous/i })).toBeDefined();

    // Page 1 with a filter applied: limit + the filter value, no cursor.
    fireEvent.change(screen.getByLabelText(/status/i), { target: { value: "open" } });
    fireEvent.click(screen.getByRole("button", { name: /^apply$/i }));
    await waitFor(() => expect(lastURL()).toContain("limit=50"));
    expect(lastURL()).toContain("status=open");
    expect(lastURL()).not.toContain("cursor=");
  });

  it("navigates by cursor carrying cursor + limit, not filters/sort (Next uses next_cursor)", async () => {
    vi.mocked(apiGet).mockResolvedValue({
      notes: [{ id: 1, title: "a" }],
      paging: { total: 120, next_cursor: "NX", previous_cursor: "PV" },
    });
    renderPaged();
    await waitFor(() => expect(screen.getByRole("button", { name: /next/i })).toBeDefined());

    fireEvent.click(screen.getByRole("button", { name: /next/i }));
    // Cursor + limit: the token carries filters/search/sort, but limit is
    // a per-request param and must ride every hop or page 2 resizes.
    await waitFor(() => expect(lastURL()).toBe("/admin/api/list/Notes?cursor=NX&limit=50"));
    // Cursor nav must still not smuggle filters/search/sort.
    expect(lastURL()).not.toContain("sort_by");
  });

  it("Prev uses previous_cursor", async () => {
    vi.mocked(apiGet).mockResolvedValue({
      notes: [{ id: 1, title: "a" }],
      paging: { total: 120, next_cursor: "NX", previous_cursor: "PV" },
    });
    renderPaged();
    await waitFor(() => expect(screen.getByRole("button", { name: /previous/i })).toBeDefined());

    fireEvent.click(screen.getByRole("button", { name: /previous/i }));
    await waitFor(() => expect(lastURL()).toBe("/admin/api/list/Notes?cursor=PV&limit=50"));
  });

  it("disables Prev/Next when the cursor keys are omitted from the wire", async () => {
    // EmitDefaultValues:false — empty cursors omitted entirely.
    vi.mocked(apiGet).mockResolvedValue({
      notes: [{ id: 1, title: "a" }],
      paging: { total: 1 },
    });
    renderPaged();
    await waitFor(() => expect(screen.getByRole("button", { name: /next/i })).toBeDefined());
    expect(screen.getByRole("button", { name: /next/i })).toHaveProperty("disabled", true);
    expect(screen.getByRole("button", { name: /previous/i })).toHaveProperty("disabled", true);
  });

  it("resets the cursor to page 1 when a filter is applied after cursor navigation", async () => {
    vi.mocked(apiGet).mockResolvedValue({
      notes: [{ id: 1, title: "a" }],
      paging: { total: 120, next_cursor: "NX", previous_cursor: "PV" },
    });
    renderPaged();
    await waitFor(() => expect(screen.getByRole("button", { name: /next/i })).toBeDefined());

    // Navigate by cursor…
    fireEvent.click(screen.getByRole("button", { name: /next/i }));
    await waitFor(() => expect(lastURL()).toBe("/admin/api/list/Notes?cursor=NX&limit=50"));

    // …then change the query — the cursor encodes the OLD query, so it
    // must be dropped and paging restart at page 1 (limit, no cursor).
    fireEvent.click(screen.getByRole("button", { name: /^apply$/i }));
    await waitFor(() => expect(lastURL()).toContain("limit=50"));
    expect(lastURL()).not.toContain("cursor=");
  });

  it("resets the cursor to page 1 when the search query changes", async () => {
    vi.mocked(apiGet).mockResolvedValue({
      notes: [{ id: 1, title: "a" }],
      paging: { total: 120, next_cursor: "NX", previous_cursor: "PV" },
    });
    renderPaged();
    await waitFor(() => expect(screen.getByRole("button", { name: /next/i })).toBeDefined());

    fireEvent.click(screen.getByRole("button", { name: /next/i }));
    await waitFor(() => expect(lastURL()).toBe("/admin/api/list/Notes?cursor=NX&limit=50"));

    fireEvent.change(screen.getByLabelText(/^search$/i), { target: { value: "hi" } });
    fireEvent.click(screen.getByRole("button", { name: /^apply$/i }));
    await waitFor(() => expect(lastURL()).toContain("q=hi"));
    expect(lastURL()).toContain("limit=50");
    expect(lastURL()).not.toContain("cursor=");
  });

  it("does not render a pager, cursor, or limit param when the list is unpaged", async () => {
    vi.mocked(apiGet).mockResolvedValue({ notes: [{ id: 1, title: "a" }] });
    // Build an explicitly-unpaged page — passing `undefined` to
    // pagedPage would trip the `= PAGING` default parameter.
    const unpaged = pagedPage();
    delete unpaged.list!.paging;
    renderPaged(unpaged);
    await waitFor(() => expect(apiGet).toHaveBeenCalled());
    expect(screen.queryByRole("button", { name: /next/i })).toBeNull();
    expect(screen.queryByRole("button", { name: /previous/i })).toBeNull();
    // Unpaged URL is unchanged — no limit, no cursor.
    expect(lastURL()).not.toContain("limit=");
    expect(lastURL()).not.toContain("cursor=");
  });
});

// ADMIN-FK — the decision a foreign-key column cell makes: which
// text to show (referenced title, read row-locally, vs raw id) and
// whether to link (target page + id). Tested through the pure
// resolveRefCell rather than the rendered table: mantine-datatable
// does not produce row cells under jsdom (the Add-button tests above
// assert on chrome outside the table for the same reason), so the
// component render seam can't exercise cell logic — the pure function
// can, and end-to-end row rendering is covered by the example e2e.
describe("resolveRefCell (FK column decision)", () => {
  const ref = (over: Partial<AdminColumnRefSpec>): AdminColumnRefSpec => ({ ...over });
  // The FK id comes from the owning column's `name` field, passed as
  // the third arg — here always "org_id".
  const FIELD = "org_id";
  const row = { org_id: "org-123", organization_name: "Acme Inc" };

  it("shows the referenced title (not the raw id) and links to the target page by FK id", () => {
    const out = resolveRefCell(
      ref({ title: "organization_name", page: "Organizations" }),
      row,
      FIELD,
    );
    expect(out.label).toBe("Acme Inc");
    // Link carries the FOREIGN page + the FK id (not this row's own id).
    expect(out.link).toEqual({ page: "Organizations", id: "org-123" });
  });

  it("resolves a dotted title path against a nested projected object", () => {
    const out = resolveRefCell(
      ref({ title: "organization.name", page: "Organizations" }),
      { org_id: "org-9", organization: { name: "Globex" } },
      FIELD,
    );
    expect(out.label).toBe("Globex");
    expect(out.link).toEqual({ page: "Organizations", id: "org-9" });
  });

  it("falls back to the raw id when no title path is set", () => {
    const out = resolveRefCell(ref({ page: "Organizations" }), row, FIELD);
    expect(out.label).toBe("org-123");
    expect(out.link).toEqual({ page: "Organizations", id: "org-123" });
  });

  it("falls back to the raw id when the title path resolves empty", () => {
    const out = resolveRefCell(
      ref({ title: "organization_name", page: "Organizations" }),
      { org_id: "org-123" },
      FIELD,
    );
    expect(out.label).toBe("org-123");
  });

  it("emits no link when the ref declares no target page", () => {
    const out = resolveRefCell(ref({ title: "organization_name" }), row, FIELD);
    expect(out.label).toBe("Acme Inc");
    expect(out.link).toBeUndefined();
  });

  it("emits no link when the FK id is null / empty — nothing to navigate to", () => {
    const out = resolveRefCell(
      ref({ title: "organization_name", page: "Organizations" }),
      { org_id: null, organization_name: "Acme Inc" },
      FIELD,
    );
    expect(out.label).toBe("Acme Inc");
    expect(out.link).toBeUndefined();
  });
});
