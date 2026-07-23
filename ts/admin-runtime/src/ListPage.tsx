// Generic list view — mantine-datatable rendering an array of
// rows resolved from `page.list.endpoint`. Sticky headers,
// striped + bordered, sortable columns, selection checkbox
// column wired to the existing LIST-action set, custom cell
// renderers for the detail-link column + the
// <page>:list:column:<field> slot + the
// <page>:list:row-action trailing column. Filter / search /
// applied-state stay in the Paper above the table — iter-3
// could lift them into DataTable's per-column filter popovers.

import { useEffect, useMemo, useState } from "react";
import {
  Anchor,
  Button,
  Group,
  NumberInput,
  Paper,
  Select,
  Stack,
  Text,
  TextInput,
} from "@mantine/core";
import { DataTable } from "mantine-datatable";
import type { DataTableColumn, DataTableSortStatus } from "mantine-datatable";

import { ActionModal } from "./ActionModal";
import { PageHeader, StateView } from "./components";
import { IconSearch } from "./icons";
import { columnHeader, humanizeLabel, pageLabel } from "./format";
import { apiGet, displayString, formatTitle } from "./api";
import { hasAllPermissions } from "./auth";
import { listColumnSlotKey, listRowActionSlotKey } from "./types";
import type {
  AdminActionSpec,
  AdminColumnRefSpec,
  AdminListPagingSpec,
  AdminPageSpec,
  AdminSpec,
  SlotRegistry,
  WhoAmIResp,
} from "./types";

export interface ListPageProps {
  spec: AdminSpec;
  page: AdminPageSpec;
  whoami?: WhoAmIResp | null;
  slots?: SlotRegistry;
  onSelectRow: (pageName: string, rowId: string) => void;
  // Navigate to this page's create form. Optional so a host that
  // doesn't route create simply never shows the Add button.
  onAdd?: () => void;
}

interface ListResp {
  // Storage list responses carry a single repeated field with
  // the rows. Field name varies — we accept the first array-
  // typed property we find.
  [key: string]: unknown;
}

type Row = Record<string, unknown>;

export function ListPage({ page, whoami, slots, onSelectRow, onAdd }: ListPageProps) {
  const [rows, setRows] = useState<Row[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [reloadTick, setReloadTick] = useState(0);
  const [openAction, setOpenAction] = useState<string | null>(null);
  // Row selection — DataTable owns the controlled list of
  // record references. Reset when the list refetches.
  const [selectedRecords, setSelectedRecords] = useState<Row[]>([]);

  // Filter + search values keyed by request-field name. The
  // SPA stages values locally and the user clicks "Apply" to
  // refetch (no debounce — iter-3 could lift to per-keystroke).
  const filterFieldNames = page.list?.filters || [];
  const searchFieldNames = page.list?.search || [];
  const sortableSet = useMemo(() => new Set(page.list?.sortable || []), [page.list?.sortable]);
  const [filterValues, setFilterValues] = useState<Record<string, string>>({});
  const [searchValue, setSearchValue] = useState("");
  // Applied = snapshot triggering refetch; staged = user types.
  const [appliedFilters, setAppliedFilters] = useState<Record<string, string>>({});
  const [appliedSearch, setAppliedSearch] = useState("");
  // Sort state — single-column sort. Empty string columnAccessor
  // = unsorted (DataTable accepts an undefined sortStatus, but
  // we'd lose the "default_sort" wiring; we always pass it and
  // skip the URL emit when columnAccessor is empty).
  const [sortStatus, setSortStatus] = useState<DataTableSortStatus<Row>>({
    columnAccessor: page.list?.default_sort || "",
    direction: "asc",
  });

  // Cursor/keyset paging state (only meaningful when
  // page.list.paging is set). `cursor` empty = page 1: the fetch
  // sends filters+search+sort + the default limit. Non-empty =
  // cursor navigation: the fetch sends the cursor token (which
  // opaquely re-encodes the original query) plus the limit (a
  // per-request param the cursor doesn't carry). `pageEnv` holds
  // the last response's Paging envelope driving the footer.
  const paging = page.list?.paging;
  const [cursor, setCursor] = useState("");
  const [pageEnv, setPageEnv] = useState<PageEnvelope | null>(null);

  useEffect(() => {
    if (!page.list) return;
    let cancelled = false;
    const sortBy = sortStatus.columnAccessor ? String(sortStatus.columnAccessor) : "";
    const url = buildListURL(
      page.list.endpoint,
      appliedFilters,
      searchFieldNames,
      appliedSearch,
      sortBy,
      sortBy ? sortStatus.direction : "",
      paging,
      cursor,
    );
    apiGet<ListResp>(url)
      .then((resp) => {
        if (cancelled) return;
        setRows(extractRows(resp));
        setSelectedRecords([]);
        if (paging) setPageEnv(readPageEnvelope(resp, paging.paging_field));
      })
      .catch((err) => {
        if (cancelled) return;
        setError(err instanceof Error ? err.message : String(err));
      });
    return () => {
      cancelled = true;
    };
  }, [
    page.list?.endpoint,
    reloadTick,
    appliedFilters,
    appliedSearch,
    sortStatus,
    cursor,
    searchFieldNames.join(","),
  ]);

  // Any query change — filter apply/clear, search, or sort —
  // invalidates the cursor (it encodes the OLD query), so paging
  // restarts from page 1. On an unpaged list setCursor("") is a
  // no-op string that never reaches the URL builder.
  const applyFilters = () => {
    setAppliedFilters({ ...filterValues });
    setAppliedSearch(searchValue);
    setCursor("");
  };
  const clearFilters = () => {
    setFilterValues({});
    setSearchValue("");
    setAppliedFilters({});
    setAppliedSearch("");
    setCursor("");
  };
  const handleSortStatusChange = (s: DataTableSortStatus<Row>) => {
    setSortStatus(s);
    setCursor("");
  };
  const hasFilterUI = filterFieldNames.length > 0 || searchFieldNames.length > 0;

  // LIST-target actions render as buttons above the table. BOTH-
  // target actions also render here. DETAIL-only actions appear
  // on DetailPage. Perm-aware hiding: actions whose
  // required_permissions aren't all covered by whoami's
  // permission_ids never render — backend still enforces.
  const listActions: [string, AdminActionSpec][] = Object.entries(page.actions || {}).filter(
    ([, a]) =>
      (a.target === "LIST" || a.target === "BOTH") &&
      hasAllPermissions(whoami?.permission_ids, a.required_permissions),
  );

  // "Add" is gated on three things: the page declaring a create
  // endpoint, the host wiring a route to it, and the user holding
  // the create perms. Backend still enforces — this only hides a
  // button that would 403.
  const canAdd =
    !!onAdd &&
    !!page.detail?.create_endpoint &&
    hasAllPermissions(whoami?.permission_ids, page.detail?.required_permissions_create);

  // Per-row action slot (REV-150 `<page>:list:row-action`).
  // When the consumer registers a component, a trailing
  // synthetic column renders it for every row with
  // { row, rowId, page, onSelectRow }.
  const RowActionSlot = slots?.[listRowActionSlotKey(page.name)];

  // idAccessor must return a stable React key from a record.
  // Prefer `id`; fall back to row's array index lookup. The
  // O(n) indexOf path only fires for ID-less rows (rare) on
  // ≤page-size data sets — acceptable for iter-2.
  const idAccessor = useMemo(
    () => (record: Row) => {
      if (record.id != null) return displayString(record.id);
      return rows ? rows.indexOf(record) : 0;
    },
    [rows],
  );

  // Build DataTable column descriptors. Stays in-effect-deps-
  // free (rows/slots aren't deps) because the render closures
  // capture by reference; we rebuild on each render — cheap
  // (≤dozens of columns).
  const columns: DataTableColumn<Row>[] = [];
  const linkCol = page.list?.detail_link_column || (page.list?.columns ?? [])[0]?.name || "";
  // Each AdminColumn carries its own presentation: the item field
  // `name`, an optional header `label` (Django verbose_name), and an
  // optional FK `ref`. A column with a ref renders the referenced
  // row's human title (read row-locally via the ref's `title` path)
  // and links to the target page's detail — taking precedence over
  // the detail-link treatment for that column (a registered custom-JS
  // slot still wins, as an explicit override).
  for (const col of page.list?.columns || []) {
    const c = col.name;
    const SlotComp = slots?.[listColumnSlotKey(page.name, c)];
    const ref = col.ref;
    columns.push({
      accessor: c,
      title: columnHeader(col),
      sortable: sortableSet.has(c),
      render: (record: Row) => {
        const val = record[c];
        // FK ref column — referenced title (or raw id) + optional
        // cross-page link. An explicit custom-JS slot overrides.
        if (ref && !SlotComp) {
          const { label, link } = resolveRefCell(ref, record, c);
          if (link) {
            return (
              <Anchor
                component="button"
                onClick={(e) => {
                  e.stopPropagation();
                  onSelectRow(link.page, link.id);
                }}
              >
                {label}
              </Anchor>
            );
          }
          return label;
        }
        const cellBody = SlotComp ? (
          <SlotComp row={record} field={c} value={val} page={page} />
        ) : (
          renderCell(val)
        );
        if (c === linkCol) {
          const id = String(idAccessor(record));
          return (
            <Anchor
              component="button"
              onClick={(e) => {
                // Block DataTable's row-click handler — link
                // clicks should navigate, not select.
                e.stopPropagation();
                onSelectRow(page.name, id);
              }}
            >
              {cellBody}
            </Anchor>
          );
        }
        return cellBody;
      },
    });
  }
  if (RowActionSlot) {
    columns.push({
      accessor: "__row_action__",
      title: "",
      textAlign: "right",
      render: (record: Row) => {
        const id = String(idAccessor(record));
        return RowActionSlot({ row: record, rowId: id, page, onSelectRow });
      },
    });
  }

  if (!page.list) {
    return (
      <Stack gap="lg">
        <PageHeader title={pageLabel(page)} />
        <StateView kind="empty" title="No list view" message="This page has no list configured." />
      </Stack>
    );
  }

  if (error) {
    return (
      <Stack gap="lg">
        <PageHeader title={pageLabel(page)} />
        <StateView kind="error" message={error} />
      </Stack>
    );
  }
  if (rows == null) {
    return (
      <Stack gap="lg">
        <PageHeader title={pageLabel(page)} />
        <StateView kind="loading" />
      </Stack>
    );
  }

  // Selection plumbing: when no LIST actions render, the
  // checkbox column is hidden by passing undefined for the
  // selection props (DataTable omits the column then).
  const wantSelection = listActions.length > 0;
  const selectedIds = selectedRecords.map((r) => String(idAccessor(r))).filter((id) => id !== "");

  const countLabel = `${rows.length} ${rows.length === 1 ? "record" : "records"}`;
  const subtitle =
    selectedRecords.length > 0 ? `${countLabel} · ${selectedRecords.length} selected` : countLabel;

  return (
    <Stack gap="lg">
      <PageHeader
        title={pageLabel(page)}
        subtitle={subtitle}
        actions={
          canAdd || listActions.length > 0 ? (
            <>
              {listActions.map(([name, action]) => (
                <Button
                  key={name}
                  variant="light"
                  onClick={() => setOpenAction(name)}
                  disabled={selectedIds.length === 0}
                  title={selectedIds.length === 0 ? "Select at least one row" : undefined}
                >
                  {action.label || humanizeLabel(name)}
                </Button>
              ))}
              {canAdd && <Button onClick={onAdd}>Add {pageLabel(page)}</Button>}
            </>
          ) : undefined
        }
      />
      {listActions.map(([name, action]) => (
        <ActionModal
          key={name}
          action={action}
          actionName={name}
          open={openAction === name}
          onClose={() => setOpenAction(null)}
          onSuccess={() => setReloadTick((t) => t + 1)}
          // The explicit row selection. An empty one is refused (by the
          // button above, the modal, and the handler) — there is no
          // "apply to all filtered rows" mode; nothing implements it.
          selectedIds={selectedIds}
        />
      ))}

      {hasFilterUI && (
        <Paper withBorder p="md" radius="md">
          <Stack gap="sm">
            {searchFieldNames.length > 0 && (
              <TextInput
                label="Search"
                leftSection={<IconSearch size={16} />}
                placeholder={`Search across ${searchFieldNames.map(humanizeLabel).join(", ")}`}
                value={searchValue}
                onChange={(e) => setSearchValue(e.currentTarget.value)}
              />
            )}
            {filterFieldNames.length > 0 && (
              <Group gap="xs" align="flex-end">
                {filterFieldNames.map((f) => (
                  <FilterInput
                    key={f}
                    name={f}
                    type={page.list?.filter_types?.[f]}
                    value={filterValues[f] || ""}
                    onChange={(v) => setFilterValues((prev) => ({ ...prev, [f]: v }))}
                  />
                ))}
              </Group>
            )}
            <Group justify="flex-end" gap="xs">
              <Button variant="subtle" onClick={clearFilters}>
                Clear
              </Button>
              <Button onClick={applyFilters}>Apply</Button>
            </Group>
          </Stack>
        </Paper>
      )}

      <DataTable<Row>
        withTableBorder
        borderRadius="md"
        striped
        highlightOnHover
        minHeight={160}
        verticalSpacing="sm"
        horizontalSpacing="md"
        noRecordsText="No records found"
        records={rows}
        columns={columns}
        idAccessor={idAccessor}
        sortStatus={sortStatus}
        onSortStatusChange={handleSortStatusChange}
        selectedRecords={wantSelection ? selectedRecords : undefined}
        onSelectedRecordsChange={wantSelection ? setSelectedRecords : undefined}
      />

      {paging && <CursorPager env={pageEnv} shown={rows.length} onCursor={setCursor} />}
    </Stack>
  );
}

interface PageEnvelope {
  total: number;
  next_cursor: string;
  previous_cursor: string;
}

// CursorPager renders the keyset footer: a "<n> shown of <total>"
// count plus Prev/Next. This is deliberately NOT offset/page-number
// UX — there is no jump-to-page. Next/Prev are enabled iff the last
// response carried a next_cursor / previous_cursor (an omitted key
// on the wire — EmitDefaultValues:false — reads as empty → disabled).
// Clicking sets the cursor, which the effect turns into a cursor-only
// refetch.
function CursorPager({
  env,
  shown,
  onCursor,
}: {
  env: PageEnvelope | null;
  shown: number;
  onCursor: (cursor: string) => void;
}) {
  const total = env?.total ?? 0;
  const next = env?.next_cursor ?? "";
  const prev = env?.previous_cursor ?? "";
  const countLabel = shown < total ? `${shown} shown of ${total}` : `${total} total`;
  return (
    <Group justify="space-between" align="center">
      <Text size="sm" c="dimmed">
        {countLabel}
      </Text>
      <Group gap="xs">
        <Button variant="default" disabled={!prev} onClick={() => onCursor(prev)}>
          Previous
        </Button>
        <Button variant="default" disabled={!next} onClick={() => onCursor(next)}>
          Next
        </Button>
      </Group>
    </Group>
  );
}

// readPageEnvelope pulls the w17.Paging sibling out of a paged list
// response at the spec-declared `paging_field` key. The backend
// marshals with EmitDefaultValues:false, so total/next_cursor/
// previous_cursor may be OMITTED when zero/empty — every field is
// read defensively (absent → 0 / ""). Non-string cursor values are
// coerced to "" so a malformed wire value degrades to "no more pages"
// rather than sending garbage back as a cursor.
function readPageEnvelope(resp: ListResp, pagingField: string): PageEnvelope {
  const raw = resp[pagingField];
  const env = raw && typeof raw === "object" ? (raw as Record<string, unknown>) : {};
  return {
    total: typeof env.total === "number" ? env.total : Number(env.total ?? 0) || 0,
    next_cursor: typeof env.next_cursor === "string" ? env.next_cursor : "",
    previous_cursor: typeof env.previous_cursor === "string" ? env.previous_cursor : "",
  };
}

// extractRows pulls the first array-typed property out of a
// list response. Storage list responses follow the convention
// `repeated <Msg> <items_field> = 1` — the items_field name
// varies (users / tasks / orders), so the runtime accepts any.
function extractRows(resp: ListResp): Row[] {
  for (const k of Object.keys(resp)) {
    const v = resp[k];
    if (Array.isArray(v)) {
      return v as Row[];
    }
  }
  return [];
}

// renderCell turns a JSON-decoded value into something a
// table cell can display. Strings / numbers / booleans pass
// through; null / undefined render empty; objects render as
// JSON.
function renderCell(v: unknown): string {
  return displayString(v);
}

// resolveRefCell computes what a foreign-key column cell shows
// (ADMIN-FK): the referenced row's human title — read ROW-LOCALLY
// via the ref's `title` path against the same list item — or the
// raw id (read from the owning column's `field`) as a fallback, plus
// the link target (page + id) when the ref declares a `page` and the
// id is non-empty. Pure (no React) so the decision is unit-testable
// independent of the DataTable render seam, which does not produce
// rows under jsdom.
//
// The runtime never fetches the referenced row: `title` must be a
// field the list method already projects (e.g. a DQL JOIN that
// returns `organization_name` next to `org_id`). A null / empty FK
// id yields no link (nothing to navigate to), and an unresolved /
// absent title falls back to the raw id so the cell is never blank
// when the id is present.
export function resolveRefCell(
  ref: AdminColumnRefSpec,
  row: Record<string, unknown>,
  field: string,
): { label: string; link?: { page: string; id: string } } {
  const rawId = displayString(row[field]);
  const resolved = ref.title ? formatTitle(`{${ref.title}}`, row) : "";
  const label = resolved !== "" ? resolved : rawId;
  if (ref.page && rawId !== "") {
    return { label, link: { page: ref.page, id: rawId } };
  }
  return { label };
}

// formatRowTitle resolves the spec's title template against a
// row record. Kept here for future use (FK-picker rendering,
// detail breadcrumb) — currently unused in walking-skeleton.
export const _formatRowTitle = formatTitle;

// FilterInput dispatches a Mantine input component based on the
// filter field's declared type. All inputs flow strings up to
// the caller — even numeric / date pickers emit string values
// so the URL-query-param contract stays uniform. Empty string
// = filter cleared (the URL builder skips empty values).
//
// Type → input matrix:
//   DATE                  → <TextInput type="date">  (HTML5 calendar)
//   DATETIME              → <TextInput type="datetime-local">
//   TIME                  → <TextInput type="time">
//   EMAIL                 → <TextInput type="email">
//   URL                   → <TextInput type="url">
//   IP                    → <TextInput> (plain — no browser input)
//   NUMBER / INT / FLOAT  → <NumberInput>
//   BOOL                  → <Select>  [any / true / false]
//   ENUM                  → <TextInput> (iter-3: enum values in spec)
//   default / unset       → <TextInput>
function FilterInput({
  name,
  type,
  value,
  onChange,
}: {
  name: string;
  type: string | undefined;
  value: string;
  onChange: (v: string) => void;
}) {
  switch (type) {
    case "DATE":
      return (
        <TextInput
          type="date"
          label={humanizeLabel(name)}
          value={value}
          onChange={(e) => onChange(e.currentTarget.value)}
        />
      );
    case "DATETIME":
      return (
        <TextInput
          type="datetime-local"
          label={humanizeLabel(name)}
          value={value}
          onChange={(e) => onChange(e.currentTarget.value)}
        />
      );
    case "TIME":
      return (
        <TextInput
          type="time"
          label={humanizeLabel(name)}
          value={value}
          onChange={(e) => onChange(e.currentTarget.value)}
        />
      );
    case "EMAIL":
      return (
        <TextInput
          type="email"
          label={humanizeLabel(name)}
          value={value}
          onChange={(e) => onChange(e.currentTarget.value)}
        />
      );
    case "URL":
      return (
        <TextInput
          type="url"
          label={humanizeLabel(name)}
          value={value}
          onChange={(e) => onChange(e.currentTarget.value)}
        />
      );
    case "NUMBER":
    case "INT":
    case "FLOAT":
      return (
        <NumberInput
          label={humanizeLabel(name)}
          value={value === "" ? "" : Number(value)}
          allowDecimal={type === "FLOAT" || type === "NUMBER"}
          onChange={(v) => onChange(v === "" || v == null ? "" : String(v))}
        />
      );
    case "BOOL":
      return (
        <Select
          label={humanizeLabel(name)}
          value={value || null}
          data={[
            { value: "true", label: "true" },
            { value: "false", label: "false" },
          ]}
          clearable
          onChange={(v) => onChange(v || "")}
        />
      );
    default:
      return (
        <TextInput label={name} value={value} onChange={(e) => onChange(e.currentTarget.value)} />
      );
  }
}

// buildListURL composes the list fetch URL from base endpoint
// + applied filter values + search value bound to every
// declared search field + sort_by / sort_dir when set.
// Empty values are skipped so the backend sees the storage
// method's defaults.
//
// Paging (cursor/keyset) layers on when `paging` is set:
//   - cursor NON-empty → CURSOR NAVIGATION: the token opaquely
//     re-encodes the original filters+sort, so the URL never carries
//     filters/search/sort alongside it (that would double-apply /
//     conflict) — but it DOES carry `?<limit_param>=<default_limit>`,
//     because limit is a per-request param the cursor does not encode
//     and must ride every hop to keep the page size stable.
//   - cursor empty → PAGE 1: filters+search+sort as usual PLUS
//     `?<limit_param>=<default_limit>` to request the first page.
// When `paging` is absent the function behaves exactly as before
// (no limit param, no cursor) so unpaged lists are unchanged.
function buildListURL(
  endpoint: string,
  filters: Record<string, string>,
  searchFields: string[],
  search: string,
  sortBy: string,
  sortDir: string,
  paging?: AdminListPagingSpec,
  cursor?: string,
): string {
  const params = new URLSearchParams();
  // Cursor navigation: the token opaquely carries the page-1
  // filters/search/sort, so we DON'T re-send those. But `limit` is a
  // per-request parameter — it is NOT baked into the cursor — so it
  // must ride every request or the server falls back to its own
  // default and page 2+ silently resizes. Re-send it alongside the
  // cursor to keep the page size stable across every hop.
  if (paging && cursor) {
    params.set(paging.cursor_param, cursor);
    params.set(paging.limit_param, String(paging.default_limit));
    return `${endpoint}?${params.toString()}`;
  }
  for (const [k, v] of Object.entries(filters)) {
    if (v) params.set(k, v);
  }
  if (search) {
    for (const f of searchFields) {
      params.set(f, search);
    }
  }
  if (sortBy && sortDir) {
    params.set("sort_by", sortBy);
    params.set("sort_dir", sortDir);
  }
  // Page 1 of a paged list carries the limit alongside the query.
  if (paging) {
    params.set(paging.limit_param, String(paging.default_limit));
  }
  const qs = params.toString();
  return qs ? `${endpoint}?${qs}` : endpoint;
}
