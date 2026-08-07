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
  Tooltip,
} from "@mantine/core";
import { DataTable } from "mantine-datatable";
import type { DataTableColumn, DataTableSortStatus } from "mantine-datatable";

import { ActionModal } from "./ActionModal";
import { PageHeader, StateView } from "./components";
import { IconSearch } from "./icons";
import { columnHeader, humanizeLabel, pageLabel } from "./format";
import { resolveChoiceLabel, translateChoiceLabel } from "./choiceLabel";
import { AdminApiError, apiGet, displayString, formatTitle, readErrorEnvelope } from "./api";
import { formatCellValue, formatContextFor } from "./cellFormat";
import { useT } from "./i18n";
import { wireNumberText } from "./wireNumber";
import type { Translate } from "./i18n";
import type { FormatContext, FormatTemplate } from "./cellFormat";
import { hasAllPermissions } from "./auth";
import { listColumnSlotKey, listRowActionSlotKey } from "./types";
import type {
  AdminActionSpec,
  AdminChoicesSpec,
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

// The direction a fresh sort cycle opens on — also the direction the
// state is parked at while unsorted, so the next column starts from the
// top of the cycle instead of inheriting the previous column's leftover
// direction (mantine-datatable carries `direction` over when the
// accessor changes).
const SORT_CYCLE_START = "asc" as const;

// Column-header sort is TRI-STATE, like Django's admin: asc → desc →
// unsorted. mantine-datatable only ever flips asc↔desc, so the third
// click arrives here as a flip back to the direction the cycle STARTED
// on — that's the reader having come full circle, and it means "drop the
// sort", not "sort ascending again". Without this the sort could be
// changed but never turned off, and a list whose server-side default
// ordering is the useful one was unreachable once any header was
// clicked. Clearing parks the direction back at the cycle start.
export function cycleSortStatus(
  prev: DataTableSortStatus<Row>,
  next: DataTableSortStatus<Row>,
): DataTableSortStatus<Row> {
  const sameColumn = !!prev.columnAccessor && next.columnAccessor === prev.columnAccessor;
  if (sameColumn && next.direction === SORT_CYCLE_START) {
    return { columnAccessor: "", direction: SORT_CYCLE_START };
  }
  // Switching columns restarts the cycle. mantine hands over the PREVIOUS
  // column's direction here, which would open the new column mid-cycle:
  // its first click would land on desc and its second would read as
  // "full circle" and clear it — a two-state column next to three-state
  // ones.
  if (!sameColumn) return { columnAccessor: next.columnAccessor, direction: SORT_CYCLE_START };
  return next;
}

export function ListPage({ spec, page, whoami, slots, onSelectRow, onAdd }: ListPageProps) {
  const t = useT();
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
    direction: SORT_CYCLE_START,
  });

  // Cursor/keyset paging state (only meaningful when
  // page.list.paging is set). `cursor` empty = page 1: the fetch
  // sends filters+search+sort + the default limit. Non-empty =
  // cursor navigation: the fetch sends the cursor token (which
  // opaquely re-encodes the original query) plus the limit (a
  // per-request param the cursor doesn't carry). `pageEnv` holds
  // the last response's Paging envelope driving the footer.
  //
  // Both are BOUND to the list that minted them. A cursor encodes the
  // minting method's request proto + ORDER BY, so it is meaningless —
  // and rejected 400 — on any other list. App.tsx keys <ListPage> per
  // page so a nav switch remounts, but a host that renders ListPage
  // itself (or a future in-place list swap) would recycle this
  // instance and carry the previous list's cursor onto the new
  // endpoint. Reading the pager through the owning page name makes
  // that leak unrepresentable rather than merely unlikely.
  const paging = page.list?.paging;
  const [pager, setPager] = useState<PagerState>({ page: page.name, cursor: "", env: null });
  const ownPager = pager.page === page.name;
  const cursor = ownPager ? pager.cursor : "";
  const pageEnv = ownPager ? pager.env : null;
  const setCursor = (token: string) =>
    setPager((prev) => ({
      page: page.name,
      cursor: token,
      env: prev.page === page.name ? prev.env : null,
    }));

  // Page size. The reader picks one off the spec's ladder; until they
  // do, the list uses the spec's default_limit. Deliberately NOT in
  // PagerState: a cursor is only valid for the list that minted it and
  // has to be dropped defensively, while a page size is just a number —
  // conflating them would make every guard reason about both. It is
  // per-mount either way (App.tsx keys <ListPage> per page), so
  // switching pages returns to that list's own default.
  const pageSizes = usablePageSizes(paging);
  const [pageSize, setPageSize] = useState<number>(paging?.default_limit ?? 0);
  // Never send a size this list can't serve — a spec whose ladder moved
  // (or whose max_limit is below the remembered pick) falls back to the
  // declared default rather than asking for a limit the server clamps.
  const limit = pageSizes.includes(pageSize) ? pageSize : (paging?.default_limit ?? 0);
  // Resizing invalidates the cursor: the token encodes a position under
  // the OLD page size, so continuing from it would skip or repeat rows.
  const changePageSize = (n: number) => {
    setPageSize(n);
    setCursor("");
  };

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
      limit,
    );
    apiGet<ListResp>(url, { response: page.list?.response_ref })
      .then((resp) => {
        if (cancelled) return;
        setRows(extractRows(resp));
        setSelectedRecords([]);
        if (paging) {
          setPager({ page: page.name, cursor, env: readPageEnvelope(resp, paging.paging_field) });
        }
      })
      .catch((err) => {
        if (cancelled) return;
        // A cursor the server won't take is a lost RESUME POINT, not a
        // broken list: the gateway restarted with a per-boot HMAC key,
        // the build's request schema moved, or the token was minted by
        // another list. Drop it and refetch page one — replacing the
        // whole page with an error card would strand the user on a
        // list that works perfectly well from the top. Can't loop:
        // the retry carries no cursor, so this branch can't re-arm.
        if (cursor !== "" && isCursorRejection(err)) {
          setPager({ page: page.name, cursor: "", env: null });
          return;
        }
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
    limit,
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
    setSortStatus((prev) => cycleSortStatus(prev, s));
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
  // Locale-aware cell formatting. The locale is the viewer's regional
  // setting (the browser's own language), falling back to the surface's
  // default_language — a Czech user running an English admin still wants
  // 26.07.2026, which is the whole point of §3 of the formatting spec.
  const columnFormats = page.list?.column_formats || {};
  const formatCtx: FormatContext = formatContextFor(spec);
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
    const header = columnHeader(col, t);
    const choices = page.list?.column_choices?.[c];
    columns.push({
      accessor: c,
      title: header,
      sortable: sortableSet.has(c),
      render: (record: Row) => {
        const val = record[c];
        // FK ref column — referenced title (or the column header) under
        // an identifying tooltip, plus an optional cross-page link. An
        // explicit custom-JS slot overrides.
        if (ref && !SlotComp) {
          const { label, tooltip, link } = resolveRefCell(ref, record, c, header);
          const body = link ? (
            <Anchor
              component="button"
              onClick={(e) => {
                e.stopPropagation();
                onSelectRow(link.page, link.id);
              }}
            >
              {label}
            </Anchor>
          ) : (
            label
          );
          // Tooltip only when it says something the cell doesn't. A
          // title-less ref shows the header and the tooltip carries the
          // id, so that case is always worth wrapping; an empty FK
          // renders blank with nothing to hover.
          if (!tooltip || tooltip === label) return body;
          return (
            <Tooltip label={tooltip} withinPortal openDelay={250}>
              <span>{body}</span>
            </Tooltip>
          );
        }
        const cellBody = SlotComp ? (
          <SlotComp row={record} field={c} value={val} page={page} />
        ) : (
          renderCell(
            choices ? translateChoiceLabel(resolveChoiceLabel(choices, val), t) : val,
            choices ? undefined : columnFormats[c],
            formatCtx,
            t,
          )
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
        <PageHeader title={pageLabel(page, t)} />
        <StateView
          kind="empty"
          title={t("No list view")}
          message={t("This page has no list configured.")}
        />
      </Stack>
    );
  }

  if (error) {
    return (
      <Stack gap="lg">
        <PageHeader title={pageLabel(page, t)} />
        <StateView kind="error" message={error} />
      </Stack>
    );
  }
  if (rows == null) {
    return (
      <Stack gap="lg">
        <PageHeader title={pageLabel(page, t)} />
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
        title={pageLabel(page, t)}
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
                  title={selectedIds.length === 0 ? t("Select at least one row") : undefined}
                >
                  {action.label ? t(action.label) : humanizeLabel(name)}
                </Button>
              ))}
              {canAdd && (
                <Button onClick={onAdd}>{t("Add {name}", { name: pageLabel(page, t) })}</Button>
              )}
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
                label={t("Search")}
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
                    choices={page.list?.filter_choices?.[f]}
                    t={t}
                    value={filterValues[f] || ""}
                    onChange={(v) => setFilterValues((prev) => ({ ...prev, [f]: v }))}
                  />
                ))}
              </Group>
            )}
            <Group justify="flex-end" gap="xs">
              <Button variant="subtle" onClick={clearFilters}>
                {t("Clear")}
              </Button>
              <Button onClick={applyFilters}>{t("Apply")}</Button>
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
        noRecordsText={t("No records found")}
        records={rows}
        columns={columns}
        idAccessor={idAccessor}
        sortStatus={sortStatus}
        onSortStatusChange={handleSortStatusChange}
        selectedRecords={wantSelection ? selectedRecords : undefined}
        onSelectedRecordsChange={wantSelection ? setSelectedRecords : undefined}
      />

      {paging && (
        <CursorPager
          env={pageEnv}
          shown={rows.length}
          pageSizes={pageSizes}
          pageSize={limit}
          onPageSize={changePageSize}
          onCursor={setCursor}
        />
      )}
    </Stack>
  );
}

interface PageEnvelope {
  // A decimal STRING — w17.Paging.total is a uint64 and the dialect renders
  // every 64-bit field that way (T2-6 pass #6 removed the number carve-out
  // that made this field the exception). Convert at the edge that needs a
  // number; never let an int64 pass through a JS number in general.
  total: string;
  next_cursor: string;
  previous_cursor: string;
}

// PagerState is the paging position TOGETHER WITH the page that owns
// it — see the guard at the useState call. `page` is the admin page
// name, `cursor` the token the next fetch resumes from ("" = page one),
// `env` the last response's Paging envelope driving the footer.
interface PagerState {
  page: string;
  cursor: string;
  env: PageEnvelope | null;
}

// isCursorRejection reports whether a failed list fetch was the SERVER
// refusing the cursor specifically — as opposed to the list being down,
// forbidden, or the request being wrong in some other way. Only the
// former is safe to recover from by dropping the token and refetching
// page one; anything else must still surface as an error.
//
// The generated admin handlers reject a cursor two ways: code
// CURSOR_EXPIRED (decoded, but not this build's/list's request schema)
// and INVALID_ARGUMENT with a "cursor: " prefix (unparseable, bad HMAC,
// empty keyset oneof). restgw wraps both as {error:{code,message}} —
// readErrorEnvelope also tolerates the re-mapped shapes a host may put
// in front of the admin API.
export function isCursorRejection(err: unknown): boolean {
  if (!(err instanceof AdminApiError) || err.status !== 400) return false;
  const { code, message } = readErrorEnvelope(err.body);
  return code === "CURSOR_EXPIRED" || message.startsWith("cursor: ");
}

// usablePageSizes resolves the page-size menu for a list: the spec's
// ascending `page_sizes` ladder, or [default_limit] when the spec
// predates page-size selection (or emitted an empty ladder). Never
// empty for a paged list, so `pageSizes[0]` is always a real number.
export function usablePageSizes(paging?: AdminListPagingSpec): number[] {
  if (!paging) return [];
  const sizes = (paging.page_sizes ?? []).filter((n) => Number.isFinite(n) && n > 0);
  return sizes.length > 0 ? sizes : [paging.default_limit];
}

// pagerIsVisible decides whether the paging footer renders at all.
//
// Two independent reasons to show it:
//   - the list is ALREADY paged (a next or previous cursor exists), so
//     the reader needs the buttons; or
//   - the total exceeds the SMALLEST page size on offer. The current
//     page may well fit — 26 rows at 50-per-page has no next cursor —
//     but the reader can choose 25, and then it doesn't. Hiding the
//     footer here would hide the very control that creates the second
//     page, so a 26-row list would be stuck unpaged forever.
//
// Below that threshold the footer stays hidden: no page size on offer
// could split the list, so a "per page" select and two dead buttons
// would be pure noise on every small table.
export function pagerIsVisible(env: PageEnvelope | null, minPageSize: number): boolean {
  if (env?.next_cursor || env?.previous_cursor) return true;
  return Number(env?.total ?? "0") > minPageSize;
}

// CursorPager renders the keyset footer: a "<n> shown of <total>"
// count, a per-page size select, and Prev/Next. This is deliberately
// NOT offset/page-number UX — there is no jump-to-page. Next/Prev are
// enabled iff the last response carried a next_cursor /
// previous_cursor (an omitted key on the wire —
// EmitDefaultValues:false — reads as empty → disabled). Clicking sets
// the cursor, which the effect turns into a cursor-only refetch.
//
// Visibility is pagerIsVisible's call — see there for why the
// threshold is the smallest page size rather than the current one.
function CursorPager({
  env,
  shown,
  pageSizes,
  pageSize,
  onPageSize,
  onCursor,
}: {
  env: PageEnvelope | null;
  shown: number;
  pageSizes: number[];
  pageSize: number;
  onPageSize: (n: number) => void;
  onCursor: (cursor: string) => void;
}) {
  const t = useT();
  const total = env?.total ?? "0";
  const next = env?.next_cursor ?? "";
  const prev = env?.previous_cursor ?? "";
  if (!pagerIsVisible(env, pageSizes[0] ?? 0)) return null;
  const countLabel = shown < Number(total) ? `${shown} shown of ${total}` : `${total} total`;
  return (
    <Group justify="space-between" align="center">
      <Group gap="xs" align="center">
        <Text size="sm" c="dimmed">
          {countLabel}
        </Text>
        {pageSizes.length > 1 && (
          <Select
            size="xs"
            w={110}
            aria-label={t("Rows per page")}
            comboboxProps={{ withinPortal: true }}
            allowDeselect={false}
            data={pageSizes.map((n) => ({ value: String(n), label: `${n} / page` }))}
            value={String(pageSize)}
            onChange={(v) => {
              const n = Number(v);
              if (Number.isFinite(n) && n > 0) onPageSize(n);
            }}
          />
        )}
      </Group>
      <Group gap="xs">
        <Button variant="default" disabled={!prev} onClick={() => onCursor(prev)}>
          {t("Previous")}
        </Button>
        <Button variant="default" disabled={!next} onClick={() => onCursor(next)}>
          {t("Next")}
        </Button>
      </Group>
    </Group>
  );
}

// readPageEnvelope pulls the w17.Paging sibling out of a paged list
// response at the spec-declared `paging_field` key. The backend
// marshals with EmitDefaultValues:false, so total/next_cursor/
// previous_cursor may be OMITTED when zero/empty — every field falls
// back on ABSENCE (→ 0 / ""). It does not fall back on the wrong TYPE:
// `total` arrives as a DECIMAL STRING, like every 64-bit field in the
// dialect, and the cursors as strings too. It was briefly the dialect's one
// number carve-out; that exception broke this reader on the binary wire and
// narrowed formatCount for its other caller, so it was removed rather than
// propagated (T2-6 pass #6).
function readPageEnvelope(resp: ListResp, pagingField: string): PageEnvelope {
  const raw = resp[pagingField];
  const env = raw && typeof raw === "object" ? (raw as Record<string, unknown>) : {};
  return {
    total: wireNumberText(env.total) ?? "0",
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

// renderCell turns a JSON-decoded value into something a table cell can
// display. A column carrying a `column_formats` entry goes through the
// locale-aware formatter — the compiler already decided this cell is MONEY
// or a DATETIME and lowered it to a msgid plus one slot. Everything else
// falls through to displayString: strings / numbers / booleans pass through,
// null / undefined render empty, objects render as JSON.
//
// The formatter can decline (a template with no slots), and then the raw
// rendering stands. A cell must never come back blank because its format
// had nothing to say.
function renderCell(
  v: unknown,
  tmpl: FormatTemplate | undefined,
  fmt: FormatContext,
  t: Translate,
): string {
  const formatted = formatCellValue(v, tmpl, fmt, t);
  return formatted !== undefined ? formatted : displayString(v);
}

// resolveRefCell computes what a foreign-key column cell shows
// (ADMIN-FK): its visible text, its hover tooltip, and the link target
// (page + id) when the ref declares a `page` and the id is non-empty.
// Pure (no React) so the decision is unit-testable independent of the
// DataTable render seam, which does not produce rows under jsdom.
//
// The runtime never fetches the referenced row: `title` must be a
// field the list method already projects (e.g. a DQL JOIN that
// returns `organization_name` next to `org_id`).
//
// TEXT — the ref's `title` resolved ROW-LOCALLY when the author
// declared one ("Acme"); otherwise the COLUMN HEADER ("Organization",
// "Wallet"). The header fallback is what makes the no-title case
// usable: several FK columns side by side used to render an identical
// placeholder (or a raw UUID), so thirty rows down — with the real
// headers scrolled off — there was no telling which link went where.
// Falling back to the header also means a technical child table that
// has no name of its own (a user's wallet / limits / subscription)
// simply declares no `title` and reads as its own column name, instead
// of repeating the owner's name in four adjacent cells.
//
// TOOLTIP — always identifies the exact row behind that text:
// "<title> #<id>" when a title resolved, "#<id>" when only the id is
// known. That is the other half of the trade: the cell may now show
// static text, so hovering has to reveal what is behind it.
//
// A ref with NEITHER a resolved title NOR an id is an empty FK — it
// renders blank, never the bare header, which would read as a link to
// something that isn't there.
export function resolveRefCell(
  ref: AdminColumnRefSpec,
  row: Record<string, unknown>,
  field: string,
  header: string,
): { label: string; tooltip: string; link?: { page: string; id: string } } {
  const rawId = displayString(row[field]);
  const resolved = ref.title ? formatTitle(`{${ref.title}}`, row) : "";
  if (resolved === "" && rawId === "") return { label: "", tooltip: "" };
  const label = resolved !== "" ? resolved : header;
  const idPart = rawId !== "" ? `#${rawId}` : "";
  const tooltip = resolved !== "" && idPart !== "" ? `${resolved} ${idPart}` : resolved || idPart;
  if (ref.page && rawId !== "") {
    return { label, tooltip, link: { page: ref.page, id: rawId } };
  }
  return { label, tooltip };
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
//   ENUM                  → <Select> from the spec's filter_choices
//   default / unset       → <TextInput>
function FilterInput({
  name,
  type,
  choices,
  value,
  onChange,
  t,
}: {
  name: string;
  type: string | undefined;
  choices?: AdminChoicesSpec;
  value: string;
  onChange: (v: string) => void;
  t: (msgid: string) => string;
}) {
  // An ENUM filter is the one place the operator TYPES a value, so it is the
  // one place a wire token is least usable — and the spec has carried the
  // catalogue for it all along.
  if (choices && choices.values?.length) {
    return (
      <Select
        label={humanizeLabel(name)}
        value={value || null}
        data={choices.values
          .filter((c) => !c.deprecated)
          .map((c) => ({ value: String(c.value), label: t(c.label) }))}
        clearable
        searchable
        onChange={(v) => onChange(v || "")}
      />
    );
  }
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
        <TextInput
          label={humanizeLabel(name)}
          value={value}
          onChange={(e) => onChange(e.currentTarget.value)}
        />
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
//     conflict) — but it DOES carry `?<limit_param>=<limit>`, because
//     limit is a per-request param the cursor does not encode and must
//     ride every hop to keep the page size stable.
//   - cursor empty → PAGE 1: filters+search+sort as usual PLUS
//     `?<limit_param>=<limit>` to request the first page.
// `limit` is the reader's selected page size; it falls back to the
// spec's default_limit when unset. When `paging` is absent the function
// behaves exactly as before (no limit param, no cursor) so unpaged
// lists are unchanged.
function buildListURL(
  endpoint: string,
  filters: Record<string, string>,
  searchFields: string[],
  search: string,
  sortBy: string,
  sortDir: string,
  paging?: AdminListPagingSpec,
  cursor?: string,
  limit?: number,
): string {
  const pageLimit = limit && limit > 0 ? limit : paging?.default_limit;
  const params = new URLSearchParams();
  // Cursor navigation: the token opaquely carries the page-1
  // filters/search/sort, so we DON'T re-send those. But `limit` is a
  // per-request parameter — it is NOT baked into the cursor — so it
  // must ride every request or the server falls back to its own
  // default and page 2+ silently resizes. Re-send it alongside the
  // cursor to keep the page size stable across every hop.
  if (paging && cursor) {
    params.set(paging.cursor_param, cursor);
    params.set(paging.limit_param, String(pageLimit));
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
    params.set(paging.limit_param, String(pageLimit));
  }
  const qs = params.toString();
  return qs ? `${endpoint}?${qs}` : endpoint;
}
