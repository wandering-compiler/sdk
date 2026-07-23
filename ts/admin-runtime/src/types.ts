// Spec types — mirror the JSON shape emitted by the Go-side
// admin codegen (srcgo/domains/gateway/admin/spec_gen.go). Bump
// SPEC_SCHEMA_VERSION in lockstep with the Go-side SchemaVersion
// constant whenever the shape changes in a backwards-incompatible
// way.

export const SPEC_SCHEMA_VERSION = "3";

export interface AdminSpec {
  schema_version: string;
  name: string;
  prefix: string;
  default_language?: string;
  auth: AdminAuthSpec;
  title_templates?: Record<string, string>;
  pages: Record<string, AdminPageSpec>;
  nav?: AdminNavGroup[];
  overview?: AdminOverviewSpec;
}

export interface AdminOverviewSpec {
  widgets: AdminWidgetSpec[];
}

export interface AdminWidgetSpec {
  slot: string;
  size: "SMALL" | "MEDIUM" | "LARGE";
  // Widget kind. Absent / "CUSTOM" = a JS slot dispatched at
  // `overview:widget:<slot>`. The other two are DECLARATIVE — the
  // runtime fetches `endpoint` and renders them from the spec, no
  // project JS involved. "STAT" = an aggregated-values panel read
  // from `values`. "CHART" = a plot read from `chart_kind` /
  // `rows_field` / `x_field` / `series`.
  kind?: "CUSTOM" | "STAT" | "CHART";
  // Card heading for a declarative widget (STAT / CHART). Absent for
  // CUSTOM — a custom renderer owns its own chrome.
  title?: string;
  // Data route for a declarative widget: GET <endpoint> returns the
  // source method's response, off which `values` (STAT) / `rows_field`
  // (CHART) are read. Absent for CUSTOM.
  endpoint?: string;
  // STAT: the scalar cells to render, in order.
  values?: AdminWidgetValueSpec[];
  // CHART: which plot to draw. LINE / AREA / BAR are category-vs-
  // series grids driven by `x_field` + `series`; DONUT is a
  // part-of-whole over the FIRST declared series. Codegen always
  // emits one (an unset chart_kind is refused at parse); the LINE
  // fallback below only covers a hand-edited spec.
  chart_kind?: "LINE" | "BAR" | "AREA" | "DONUT";
  // CHART: key on the response OBJECT holding the row array — the
  // response is `{ "<rows_field>": [ {…}, {…} ] }`, one entry per
  // point/slice.
  rows_field?: string;
  // CHART: row key carrying the category / time axis value.
  x_field?: string;
  // CHART: the row keys plotted as series, in order. The order is
  // load-bearing — it IS the color assignment (see chartData.ts).
  // DONUT reads only the first entry (a part-of-whole has one
  // measure).
  series?: AdminWidgetValueSpec[];
}

// AdminWidgetValueSpec is one measure read off the source response —
// a cell of a STAT widget or a series of a CHART widget: the field to
// read plus an optional display label (humanized from the field name
// when absent).
export interface AdminWidgetValueSpec {
  field: string;
  label?: string;
}

// SlotRegistry maps a slot key to a React component. The admin
// runtime ships a closed set of slot points (mostly forward-
// declared in the architecture ADR — overview widgets are the
// first one wired up). Custom JS provides implementations via
// the registry passed to bootstrap(); missing slots render as
// inert placeholders so the SPA never crashes on undeclared
// extensions.
//
// Slot key convention:
//   overview:widget:<id>               — overview widget
//   <page>:list:column:<field>         — column cell renderer
//   <page>:list:row-action             — per-row action
//   <page>:detail:tab:<id>             — extra tab next to "Detail"
//   <page>:detail:field:<field>        — field renderer override
//   global:nav:item                    — extra nav entry
export type SlotKey = string;
export type SlotRegistry = Record<SlotKey, SlotComponent>;

// SlotComponent is the React component shape slot implementations
// adhere to. Props vary per slot — overview widgets get
// { widget }, list column slots get { row, value, page }, etc.
// The runtime threads the right props per slot point; here we
// use a permissive `any` because TS can't narrow without per-
// slot generics (iter-3 wires per-slot typed contracts).
// eslint-disable-next-line @typescript-eslint/no-explicit-any -- intentional permissive prop type until per-slot typed contracts (iter-3); narrowing needs generics we don't have yet
export type SlotComponent = (props: any) => React.ReactNode;

// Slot key helpers — admin runtime + custom JS use these to
// build / look up slot keys consistently. Keeps the wire string
// in one place so iter-3 evolutions can rename the schema.
export function overviewWidgetSlotKey(slotId: string): string {
  return "overview:widget:" + slotId;
}

// listColumnSlotKey identifies a column-cell-renderer slot for
// a specific page+field. Consumer registers via:
//
//   bootstrap({
//     spec, root,
//     slots: {
//       [listColumnSlotKey("UserAdmin", "status")]: StatusBadge,
//     },
//   })
//
// When registered, ListPage renders the slot component instead
// of the default string formatter. Props the slot receives:
// `{ row, field, value, page }`.
export function listColumnSlotKey(pageName: string, field: string): string {
  return pageName + ":list:column:" + field;
}

// detailFieldSlotKey identifies a per-field input override on
// the detail page. When registered, DetailPage renders the slot
// component instead of the runtime's default (PasswordInput /
// type-aware TextInput / disabled readonly). Props:
//
//   { field, value, mode, disabled, page, register }
//
// - `mode`     — "editable" | "readonly"
// - `register` — react-hook-form register fn; slot may wire it
//   into a custom input via `{...register(field)}` if it
//   integrates with the runtime's form, or ignore it for
//   uncontrolled UIs that POST separately.
//
// Useful for JSON editors, markdown, file uploads, foreign-key
// pickers — anywhere the spec-driven default is too coarse.
export function detailFieldSlotKey(pageName: string, field: string): string {
  return pageName + ":detail:field:" + field;
}

// listRowActionSlotKey identifies a per-row action area on the
// list page. When registered, ListPage adds a trailing column
// rendering the slot's component for every row. Slot receives:
//
//   { row, rowId, page, onSelectRow }
//
// `onSelectRow` is the same handler the link column uses — slot
// authors can replicate it (e.g. via a button) or invoke other
// flows. Use this for quick "duplicate" / "open in new tab" /
// inline-delete shortcuts that don't warrant a full Action.
export function listRowActionSlotKey(pageName: string): string {
  return pageName + ":list:row-action";
}

// detailTabSlotKey identifies an extra detail-page tab. When at
// least one slot key matches `<page>:detail:tab:<id>`, DetailPage
// wraps its content in Mantine `<Tabs>` — the first tab "Detail"
// carries the spec-driven form + inlines; each registered slot
// becomes an additional tab. Slot props: `{ row, rowId, page,
// spec }`. Tab label is derived from the `<id>` segment by
// replacing underscores with spaces and capitalizing the first
// letter ("audit_log" → "Audit log").
//
// Use for content that fits the detail context but isn't a
// fieldset / inline: audit log, change history, related metrics
// dashboards, full-screen file previews, debug JSON dump.
export function detailTabSlotKey(pageName: string, tabId: string): string {
  return pageName + ":detail:tab:" + tabId;
}

// globalNavItemSlotKey identifies the global nav extension
// slot. When registered, App renders the slot's component at
// the END of the left navbar (below every declared page).
// Slot receives `{ spec, whoami }`; return a React Node — pack
// multiple entries into a Fragment when the consumer wants
// more than one. Use for external links (docs / status pages),
// custom views the spec can't express, or developer tools.
export function globalNavItemSlotKey(): string {
  return "global:nav:item";
}

// WidgetRenderer alias kept for backwards compatibility with
// the P16 type export; new code uses SlotComponent.
export type WidgetRenderer = SlotComponent;

export interface AdminNavGroup {
  title: string;
  pages: string[];
}

export interface AdminAuthSpec {
  login_endpoint: string;
  whoami_endpoint: string;
}

export interface AdminPageSpec {
  name: string;
  model: string;
  title?: string;
  list?: AdminListSpec;
  detail: AdminDetailSpec;
  actions?: Record<string, AdminActionSpec>;
  inlines?: AdminInlineSpec[];
}

export interface AdminInlineSpec {
  page: string; // target page name
  endpoint: string; // "/<prefix>/api/inline/<parent>/{id}/<inline>"
  layout: "TABULAR" | "STACKED";
  label?: string;
  create_endpoint?: string;
  update_endpoint?: string;
  delete_endpoint?: string;
}

export interface AdminActionSpec {
  endpoint: string;
  fields: string[];
  label?: string;
  confirm?: string;
  target: "LIST" | "DETAIL" | "BOTH";
  // Cascade-stamped permission IDs required to invoke this
  // action. SPA hides the action button when whoami's
  // permission_ids don't include all of these. UX-only —
  // backend enforces.
  required_permissions?: number[];
}

export interface AdminListSpec {
  endpoint: string;
  item_type: string;
  // Columns to render, in order — one AdminColumnSpec per column
  // carrying the item-field `name` plus optional `label` (header
  // override) and `ref` (FK link). Unifies the former string
  // columns + refs + column_labels into a single per-column object.
  columns: AdminColumnSpec[];
  detail_link_column?: string;
  default_sort?: string;
  // Filter field names from the source request — SPA renders
  // a TextInput per entry above the table. Values land as URL
  // query params (?<field>=<value>) on every list fetch.
  filters?: string[];
  // Search field names from the source request — SPA renders
  // one search box bound to the FIRST search field; values
  // forward to that field via query params.
  search?: string[];
  // Sortable item-field names from the response item — iter-3
  // wires the column-header sort UI. Iter-2 emits but ignores.
  sortable?: string[];
  // FilterTypes maps a filter field name to its semantic /
  // scalar type ("DATE" / "DATETIME" / "TIME" / "EMAIL" / "URL"
  // / "IP" / "NUMBER" / "INT" / "FLOAT" / "BOOL" / "ENUM" / …).
  // ListPage's filter panel dispatches typed inputs from this
  // map; fields without an entry fall through to plain TextInput.
  filter_types?: Record<string, string>;
  // Cascade-stamped permission IDs required to invoke the list
  // method. SPA hides the page from nav when whoami's
  // permission_ids don't include all of these (REV-150 iter-3).
  // UX hint only — backend handler still enforces (P21).
  required_permissions?: number[];
  // Cursor/keyset pagination descriptor. Present iff the backing
  // list method is paged — the SPA then renders a cursor pager
  // (Prev/Next + total, NO jump-to-page) and drives the list via
  // opaque cursor tokens instead of offsets. Absent = unpaged
  // (whole result set in one fetch, no pager).
  paging?: AdminListPagingSpec;
}

// AdminListPagingSpec mirrors the Go-side paging block emitted onto
// a paged list's AdminListSpec. It carries the wire names the SPA
// needs to (a) build requests — the cursor + limit query-param
// names and default page size — and (b) read the response — the
// snake_case field holding the repeated items and the sibling
// w17.Paging envelope. The cursor token opaquely encodes the
// original filters+sort, so cursor navigation sends ONLY the cursor
// param (never filters/search/sort/limit alongside it).
export interface AdminListPagingSpec {
  // Query-param name carrying the opaque cursor token, e.g. "cursor".
  cursor_param: string;
  // Query-param name carrying the page-size limit, e.g. "limit".
  limit_param: string;
  // Page size the SPA requests for page 1 (typically 50).
  default_limit: number;
  // Server-enforced ceiling on default_limit (informational for the SPA).
  max_limit: number;
  // snake_case name of the response's repeated items field.
  items_field: string;
  // snake_case name of the response's sibling w17.Paging object —
  // shaped { total, limit, next_cursor, previous_cursor }. The
  // backend marshals with EmitDefaultValues:false, so empty
  // next_cursor/previous_cursor and a zero total may be OMITTED
  // entirely; the SPA treats an absent key as empty/none.
  paging_field: string;
}

// AdminColumnSpec mirrors one AdminColumn in admin.json — a single
// list column. `name` is the response-item field; `label` overrides
// the humanized header (Django verbose_name) when set; `ref`
// promotes the column to a foreign-key link. A bare `{ name }`
// renders a humanized header + raw scalar cell.
export interface AdminColumnSpec {
  name: string;
  // Header text used verbatim (no humanization). Absent = header
  // humanized from `name` (`org_id` -> "Org ID").
  label?: string;
  // Foreign-key reference (ADMIN-FK): ListPage renders the
  // referenced row's human title (read row-locally via `ref.title`)
  // in place of the raw id held by `name`, and links the cell to
  // `ref.page`'s detail view. Resolution is row-local — the list
  // method must project the title; the runtime never fetches the
  // referenced row. Absent = the column renders its raw scalar.
  ref?: AdminColumnRefSpec;
}

// AdminColumnRefSpec mirrors one AdminColumnRef in admin.json — the
// FK annotation on a column. The id comes from the owning column's
// `name` field; this carries only the display title + link target.
export interface AdminColumnRefSpec {
  // Row-local display path for the referenced title (bare, no
  // braces) — e.g. "organization_name" or "organization.name".
  // Resolved against the list row via formatTitle. Absent = show
  // the raw id value of the owning column's `name` field.
  title?: string;
  // Target page name to link the cell to (its detail, via the
  // same onSelectRow the detail-link column uses). Absent = no
  // link — title / id renders as plain text.
  page?: string;
}

export interface AdminDetailSpec {
  read_endpoint: string;
  update_endpoint?: string;
  delete_endpoint?: string;
  // POSTed to for a root-level create. Absent = no "Add" button
  // on the list. Note it targets the COLLECTION root (no /{id}) —
  // there is no row id until the mutation returns one.
  create_endpoint?: string;
  fields: string[];
  // The create form's own field list — every client-settable field
  // of the CREATE request. Deliberately separate from `fields`,
  // which describes the update form and is derived from a
  // different message (no id, possibly missing create-only
  // fields). Absent when no create endpoint.
  create_fields?: string[];
  readonly_fields?: string[];
  fieldsets?: AdminFieldsetSpec[];
  // FieldTypes maps a field name to its semantic type
  // (PASSWORD / EMAIL / DATE / DATETIME / ...). DetailPage
  // dispatches type-aware inputs from this map — PasswordInput
  // for PASSWORD, plain TextInput as fallback. Only fields with
  // a non-default semantic type appear here.
  field_types?: Record<string, string>;
  // Cascade-stamped perms required per endpoint. SPA hides the
  // edit form when read perms missing (no point editing what
  // you can't see), the submit button when update perms
  // missing, and the Delete button when delete perms missing
  // (REV-150 iter-3). Backend still enforces.
  required_permissions_read?: number[];
  required_permissions_update?: number[];
  required_permissions_delete?: number[];
  required_permissions_create?: number[];
}

export interface AdminFieldsetSpec {
  title?: string;
  fields: string[];
  collapsed?: boolean;
}

// WhoAmIResp — the admin SPA expects the user_lookup method to
// return a body shaped roughly like this. Codegen doesn't pin
// the wire shape; the runtime is defensive (reads
// `permission_ids` + `id` when present, falls back to
// "anonymous" when absent). `permission_ids` are the numeric
// lock-allocated IDs the backend enforces on — the SPA compares
// them against each page/action's `required_permissions[]`
// (also numeric IDs), never against perm-name strings.
export interface WhoAmIResp {
  id?: string;
  permission_ids?: number[];
  // Free-form extras the storage method may add (display name,
  // tenant, …). Runtime passes them through to slot props.
  [key: string]: unknown;
}

// BootstrapOptions — the inputs `bootstrap()` accepts. The
// generated `spa/src/main.tsx` calls bootstrap with these.
export interface BootstrapOptions {
  spec: AdminSpec;
  root: HTMLElement;
  // Optional slot registry — developer-provided React components
  // keyed by slot id. Walking-skeleton iter-2 wires only the
  // overview:widget:<id> slot point; iter-3 fans out to list /
  // detail slot points declared in the architecture ADR.
  slots?: SlotRegistry;
}
