import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MantineProvider } from "@mantine/core";

import { AdminApiError } from "./api";
import {
  ListPage,
  cycleSortStatus,
  isCursorRejection,
  pagerIsVisible,
  resolveRefCell,
  usablePageSizes,
} from "./ListPage";
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

  it("renders NO pager when the list fits in one page (no cursor on the wire)", async () => {
    // EmitDefaultValues:false — empty cursors omitted entirely. A list
    // shorter than the page size has nothing to page through, so the
    // footer stays away rather than showing two dead buttons: paging
    // becomes visible exactly when it starts doing something.
    // Driven as a TRANSITION (paged response first, then a filtered one
    // that fits in a page) so the assertion watches the footer go away
    // instead of racing an absence that would also hold before the first
    // fetch resolved. The DataTable itself doesn't render rows under
    // jsdom, so the pager chrome is the only post-fetch signal there is.
    vi.mocked(apiGet).mockResolvedValue({
      notes: [{ id: 1, title: "a" }],
      paging: { total: 120, next_cursor: "NX", previous_cursor: "" },
    });
    renderPaged();
    await waitFor(() => expect(screen.getByRole("button", { name: /next/i })).toBeDefined());

    vi.mocked(apiGet).mockResolvedValue({
      notes: [{ id: 1, title: "a" }],
      paging: { total: 1 },
    });
    fireEvent.click(screen.getByRole("button", { name: /^apply$/i }));
    await waitFor(() => expect(screen.queryByRole("button", { name: /next/i })).toBeNull());
    expect(screen.queryByRole("button", { name: /previous/i })).toBeNull();
  });

  it("keeps the pager on a short LAST page (previous_cursor alone brings it back)", async () => {
    // The tail page can hold fewer rows than the page size and carries no
    // next_cursor — but the reader is mid-walk and still needs Previous,
    // so one cursor is enough to render the footer.
    vi.mocked(apiGet).mockResolvedValue({
      notes: [{ id: 1, title: "a" }],
      paging: { total: 120, previous_cursor: "PV" },
    });
    renderPaged();
    await waitFor(() => expect(screen.getByRole("button", { name: /previous/i })).toBeDefined());
    expect(screen.getByRole("button", { name: /previous/i })).toHaveProperty("disabled", false);
    expect(screen.getByRole("button", { name: /next/i })).toHaveProperty("disabled", true);
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

  // A cursor is minted for ONE method's request proto + ORDER BY, so
  // it is invalid on any other list — the server answers 400
  // CURSOR_EXPIRED. Reported from the field: paging Ledger, then
  // switching the nav to Topup, sent Ledger's cursor to Topup and
  // replaced the list with an error card. App.tsx now keys <ListPage>
  // per page so the switch remounts; this covers the component itself
  // for any host that renders it without a key (or swaps the page in
  // place), by rerendering the SAME element with a different page.
  it("never carries a cursor from one list onto another (recycled instance)", async () => {
    vi.mocked(apiGet).mockResolvedValue({
      notes: [{ id: 1, title: "a" }],
      paging: { total: 120, next_cursor: "NX", previous_cursor: "PV" },
    });
    const ledger = pagedPage();
    const { rerender } = renderPaged(ledger);
    await waitFor(() => expect(screen.getByRole("button", { name: /next/i })).toBeDefined());
    fireEvent.click(screen.getByRole("button", { name: /next/i }));
    await waitFor(() => expect(lastURL()).toBe("/admin/api/list/Notes?cursor=NX&limit=50"));

    const topup = pagedPage();
    topup.name = "Topups";
    topup.list!.endpoint = "/admin/api/list/Topups";
    rerender(
      <MantineProvider>
        <ListPage
          spec={{ name: "admin", pages: {} } as unknown as AdminSpec}
          page={topup}
          onSelectRow={vi.fn()}
        />
      </MantineProvider>,
    );

    await waitFor(() => expect(lastURL()).toContain("/admin/api/list/Topups"));
    expect(lastURL()).not.toContain("cursor=");
    expect(lastURL()).toContain("limit=50");
  });

  // A cursor the server refuses is a lost resume point, not a broken
  // list — the top of the list still works. Falling back to page one
  // beats stranding the reader on an error card they can only escape
  // by navigating away.
  it("drops a rejected cursor and refetches page one instead of erroring", async () => {
    vi.mocked(apiGet).mockResolvedValue({
      notes: [{ id: 1, title: "a" }],
      paging: { total: 120, next_cursor: "NX", previous_cursor: "PV" },
    });
    renderPaged();
    await waitFor(() => expect(screen.getByRole("button", { name: /next/i })).toBeDefined());

    vi.mocked(apiGet).mockRejectedValueOnce(
      new AdminApiError(400, { error: { code: "CURSOR_EXPIRED", message: "refetch" } }, "HTTP 400"),
    );
    fireEvent.click(screen.getByRole("button", { name: /next/i }));

    // The retry drops the token — page one of the SAME list, no card.
    await waitFor(() => expect(lastURL()).toBe("/admin/api/list/Notes?limit=50"));
    expect(screen.queryByText(/something went wrong/i)).toBeNull();
  });

  // The degrade is scoped to cursor rejections: a list that is down or
  // forbidden must still say so rather than silently retrying page one.
  it("still shows an error card when the failure is not a cursor rejection", async () => {
    vi.mocked(apiGet).mockResolvedValue({
      notes: [{ id: 1, title: "a" }],
      paging: { total: 120, next_cursor: "NX", previous_cursor: "PV" },
    });
    renderPaged();
    await waitFor(() => expect(screen.getByRole("button", { name: /next/i })).toBeDefined());

    vi.mocked(apiGet).mockRejectedValueOnce(
      new AdminApiError(403, { error: { code: "PERMISSION_DENIED" } }, "HTTP 403"),
    );
    fireEvent.click(screen.getByRole("button", { name: /next/i }));
    await waitFor(() => expect(screen.getByText(/something went wrong/i)).toBeDefined());
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

// The predicate that decides whether a failed list fetch is safe to
// retry without its cursor. Too loose and a real failure gets papered
// over as a page-one refetch; too tight and the reported cross-list
// cursor still lands on an error card.
describe("isCursorRejection", () => {
  const err = (status: number, body: unknown) => new AdminApiError(status, body, "boom");

  it("recognises the CURSOR_EXPIRED code in the restgw envelope", () => {
    expect(isCursorRejection(err(400, { error: { code: "CURSOR_EXPIRED", message: "x" } }))).toBe(
      true,
    );
  });

  it("recognises a flat {code} body from a host that re-maps the envelope", () => {
    expect(isCursorRejection(err(400, { code: "CURSOR_EXPIRED" }))).toBe(true);
  });

  it("recognises the malformed-cursor path (INVALID_ARGUMENT, 'cursor: ' prefix)", () => {
    expect(
      isCursorRejection(
        err(400, {
          error: { code: "INVALID_ARGUMENT", message: "cursor: paging: cursor_malformed" },
        }),
      ),
    ).toBe(true);
  });

  it("rejects a different 400 — a bad filter must not be retried as page one", () => {
    expect(
      isCursorRejection(err(400, { error: { code: "INVALID_ARGUMENT", message: "status: bad" } })),
    ).toBe(false);
  });

  it("rejects non-400 statuses and non-API errors", () => {
    expect(isCursorRejection(err(500, { error: { code: "CURSOR_EXPIRED" } }))).toBe(false);
    expect(isCursorRejection(new Error("network down"))).toBe(false);
    expect(isCursorRejection(null)).toBe(false);
  });
});

// ADMIN-FK — the decision a foreign-key column cell makes: which text
// to show (referenced title, read row-locally, vs the column header),
// what its tooltip identifies it as, and whether to link (target page
// + id). Tested through the pure resolveRefCell rather than the
// rendered table: mantine-datatable does not produce row cells under
// jsdom (the Add-button tests above assert on chrome outside the table
// for the same reason), so the component render seam can't exercise
// cell logic — the pure function can, and end-to-end row rendering is
// covered by the example e2e.
describe("resolveRefCell (FK column decision)", () => {
  const ref = (over: Partial<AdminColumnRefSpec>): AdminColumnRefSpec => ({ ...over });
  // The FK id comes from the owning column's `name` field, passed as
  // the third arg — here always "org_id". The fourth arg is the
  // column's rendered header, used when the ref resolves no title.
  const FIELD = "org_id";
  const HEADER = "Organization";
  const row = { org_id: "org-123", organization_name: "Acme Inc" };

  it("shows the referenced title (not the raw id) and links to the target page by FK id", () => {
    const out = resolveRefCell(
      ref({ title: "organization_name", page: "Organizations" }),
      row,
      FIELD,
      HEADER,
    );
    expect(out.label).toBe("Acme Inc");
    // Link carries the FOREIGN page + the FK id (not this row's own id).
    expect(out.link).toEqual({ page: "Organizations", id: "org-123" });
  });

  it("identifies the exact referenced row in the tooltip — title plus id", () => {
    const out = resolveRefCell(
      ref({ title: "organization_name", page: "Organizations" }),
      row,
      FIELD,
      HEADER,
    );
    expect(out.tooltip).toBe("Acme Inc #org-123");
  });

  it("resolves a dotted title path against a nested projected object", () => {
    const out = resolveRefCell(
      ref({ title: "organization.name", page: "Organizations" }),
      { org_id: "org-9", organization: { name: "Globex" } },
      FIELD,
      HEADER,
    );
    expect(out.label).toBe("Globex");
    expect(out.link).toEqual({ page: "Organizations", id: "org-9" });
  });

  // The no-title case is the whole point of the header fallback: four
  // FK columns of a user's technical child tables (wallet / limits /
  // subscription) each read as their own column name instead of four
  // identical placeholders or four raw UUIDs.
  it("falls back to the COLUMN HEADER when no title path is set, keeping the id in the tooltip", () => {
    const out = resolveRefCell(ref({ page: "Organizations" }), row, FIELD, HEADER);
    expect(out.label).toBe("Organization");
    expect(out.tooltip).toBe("#org-123");
    expect(out.link).toEqual({ page: "Organizations", id: "org-123" });
  });

  it("falls back to the column header when the title path resolves empty", () => {
    const out = resolveRefCell(
      ref({ title: "organization_name", page: "Organizations" }),
      { org_id: "org-123" },
      FIELD,
      HEADER,
    );
    expect(out.label).toBe("Organization");
    expect(out.tooltip).toBe("#org-123");
  });

  it("emits no link when the ref declares no target page", () => {
    const out = resolveRefCell(ref({ title: "organization_name" }), row, FIELD, HEADER);
    expect(out.label).toBe("Acme Inc");
    expect(out.link).toBeUndefined();
  });

  it("emits no link when the FK id is null / empty — nothing to navigate to", () => {
    const out = resolveRefCell(
      ref({ title: "organization_name", page: "Organizations" }),
      { org_id: null, organization_name: "Acme Inc" },
      FIELD,
      HEADER,
    );
    expect(out.label).toBe("Acme Inc");
    // No id to identify, so the tooltip is just the title — the cell
    // already shows it, and the render seam drops a tooltip that only
    // repeats the label.
    expect(out.tooltip).toBe("Acme Inc");
    expect(out.link).toBeUndefined();
  });

  it("renders BLANK for an empty FK — neither a title nor an id to stand for", () => {
    const out = resolveRefCell(ref({ page: "Organizations" }), { org_id: "" }, FIELD, HEADER);
    // Showing the bare header here would read as a link to a row that
    // isn't referenced at all.
    expect(out.label).toBe("");
    expect(out.tooltip).toBe("");
    expect(out.link).toBeUndefined();
  });
});

// Paging footer visibility + the page-size menu. Both are pure
// decisions lifted out of <CursorPager> for the same jsdom reason as
// resolveRefCell above.
describe("usablePageSizes", () => {
  const paging = (over: Partial<AdminListPagingSpec>): AdminListPagingSpec => ({
    cursor_param: "cursor",
    limit_param: "limit",
    default_limit: 50,
    max_limit: 100,
    items_field: "items",
    paging_field: "paging",
    ...over,
  });

  it("uses the spec's ladder", () => {
    expect(usablePageSizes(paging({ page_sizes: [25, 50, 75, 100] }))).toEqual([25, 50, 75, 100]);
  });

  it("falls back to [default_limit] for a spec emitted before page-size selection", () => {
    expect(usablePageSizes(paging({}))).toEqual([50]);
  });

  it("ignores a garbage ladder rather than offering a zero page size", () => {
    expect(usablePageSizes(paging({ page_sizes: [0, -5] }))).toEqual([50]);
  });

  it("is empty for an unpaged list", () => {
    expect(usablePageSizes(undefined)).toEqual([]);
  });
});

describe("pagerIsVisible", () => {
  // total is a decimal STRING, like every 64-bit field in the dialect — the
  // helper takes a number for readability and converts, which also documents
  // that pagerIsVisible must not compare strings (T2-6 pass #6).
  const env = (over: Partial<{ total: number; next_cursor: string; previous_cursor: string }>) => ({
    total: String(over.total ?? 0),
    next_cursor: over.next_cursor ?? "",
    previous_cursor: over.previous_cursor ?? "",
  });

  it("hides on a list no page size on offer could split", () => {
    expect(pagerIsVisible(env({ total: 25 }), 25)).toBe(false);
    expect(pagerIsVisible(env({ total: 12 }), 25)).toBe(false);
  });

  // The case that drove the change: 26 rows at 50-per-page has no next
  // cursor, but the reader can pick 25 — and the only control that
  // does so lives in the footer. Hiding it would strand the list.
  it("shows at one row past the SMALLEST page size, even with no next cursor", () => {
    expect(pagerIsVisible(env({ total: 26 }), 25)).toBe(true);
  });

  it("shows whenever the response carried a cursor either way", () => {
    expect(pagerIsVisible(env({ total: 10, next_cursor: "abc" }), 25)).toBe(true);
    // A short LAST page: no next, but previous keeps the reader's way back.
    expect(pagerIsVisible(env({ total: 10, previous_cursor: "abc" }), 25)).toBe(true);
  });

  it("hides when there is no envelope at all (list not yet loaded)", () => {
    expect(pagerIsVisible(null, 25)).toBe(false);
  });
});

// Tri-state column sort. The DataTable renders no headers under jsdom, so
// the cycle is driven through the same seam the table uses: mantine's own
// click rule (below, copied from mantine-datatable) produces the proposed
// status, and cycleSortStatus decides what the list actually adopts.
describe("cycleSortStatus", () => {
  type Status = { columnAccessor: string; direction: "asc" | "desc" };
  const START: Status = { columnAccessor: "", direction: "asc" };

  // mantine-datatable's header click: same column flips the direction,
  // a different column inherits the current one.
  const click = (status: Status, accessor: string): Status =>
    cycleSortStatus(status, {
      columnAccessor: accessor,
      direction:
        status.columnAccessor === accessor
          ? status.direction === "asc"
            ? "desc"
            : "asc"
          : status.direction,
    });

  it("cycles asc → desc → unsorted on repeated clicks of one column", () => {
    const asc = click(START, "title");
    expect(asc).toEqual({ columnAccessor: "title", direction: "asc" });
    const desc = click(asc, "title");
    expect(desc).toEqual({ columnAccessor: "title", direction: "desc" });
    // The third click is the whole point: sorting can be turned OFF, so a
    // list falls back to the server's own ordering.
    expect(click(desc, "title")).toEqual({ columnAccessor: "", direction: "asc" });
  });

  it("re-arms after being cleared — a fourth click starts the cycle over", () => {
    const cleared = { columnAccessor: "", direction: "asc" } as Status;
    expect(click(cleared, "title")).toEqual({ columnAccessor: "title", direction: "asc" });
  });

  it("restarts at asc when the reader switches columns mid-cycle", () => {
    // Without the restart mantine would carry `desc` onto the new column,
    // whose next click would read as full-circle and clear it — a
    // two-state column sitting next to three-state ones.
    const desc = { columnAccessor: "title", direction: "desc" } as Status;
    expect(click(desc, "id")).toEqual({ columnAccessor: "id", direction: "asc" });
  });

  it("turns off a default_sort column too (its cycle is already at asc)", () => {
    const dflt = { columnAccessor: "created_at", direction: "asc" } as Status;
    const desc = click(dflt, "created_at");
    expect(desc.direction).toBe("desc");
    expect(click(desc, "created_at").columnAccessor).toBe("");
  });
});
