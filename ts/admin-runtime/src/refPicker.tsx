// refPicker — a form field that picks a row of another admin page
// (`AdminFieldRef`, spec `detail.field_refs` / `actions[name].field_refs`).
//
// The operator chooses a row by its TEXT and the field gets the row's VALUE:
// an organization by its name instead of its uuid, a product role by
// "scout · Geo reader" instead of `geo_reader` typed from memory. The options
// are the target page's own list — its endpoint, its search, its paging — so
// a row the operator may not read is not offered either.

import { Select } from "@mantine/core";
import { useEffect, useMemo, useRef, useState } from "react";
import { type Control, useController, useWatch } from "react-hook-form";

import { apiGet, formatTitle } from "./api";
import { humanizeLabel } from "./format";
import type { Translate } from "./i18n";
import { buildListURL, extractRows, type ListResp, type Row } from "./listQuery";
import type { AdminFieldRefSpec, AdminPageSpec, AdminSpec } from "./types";

// How long the picker waits after the last keystroke before it asks a
// searchable page for matches.
export const refSearchDebounceMs = 250;

// getPath reads a dotted path off a list item; undefined when any step is
// missing.
export function getPath(row: Row, path: string): unknown {
  let cur: unknown = row;
  for (const p of path.split(".")) {
    if (cur && typeof cur === "object" && p in (cur as Record<string, unknown>)) {
      cur = (cur as Record<string, unknown>)[p];
    } else {
      return undefined;
    }
  }
  return cur;
}

// A value as the Select's string token; null for nothing a token can hold.
function token(v: unknown): string | null {
  if (typeof v === "string") return v === "" ? null : v;
  if (typeof v === "number" || typeof v === "bigint" || typeof v === "boolean") return String(v);
  return null;
}

export interface RefOption {
  value: string;
  label: string;
  // The value the field gets on the wire: the row's own, so a numeric id
  // goes back as a number, not as the Select's string.
  wire: unknown;
  // The item's `parent` value, compared with the `depends_on` field.
  parent: string | null;
}

// refOptions turns the target page's rows into options: one per value (a page
// listing roles names each product many times), labelled by the ref's title,
// else the target model's title template, else the value itself.
export function refOptions(rows: Row[], ref: AdminFieldRefSpec, spec: AdminSpec): RefOption[] {
  const target = spec.pages?.[ref.page];
  const template = ref.title || (target ? spec.title_templates?.[target.model] : undefined) || "";
  const seen = new Set<string>();
  const out: RefOption[] = [];
  for (const row of rows) {
    const wire = getPath(row, ref.value);
    const value = token(wire);
    if (value == null || seen.has(value)) continue;
    seen.add(value);
    const label = (template ? formatTitle(template, row).trim() : "") || value;
    const parent = ref.parent ? token(getPath(row, ref.parent)) : null;
    out.push({ value, label, wire, parent });
  }
  return out;
}

// narrow keeps the options a `depends_on` value admits. No depends_on = all;
// nothing chosen in the field it depends on = none (a role of no product).
export function narrow(
  options: RefOption[],
  ref: AdminFieldRefSpec,
  dependsValue: unknown,
): RefOption[] {
  if (!ref.depends_on) return options;
  const want = token(dependsValue);
  if (want == null) return [];
  return options.filter((o) => o.parent === want);
}

// refListURL asks the target page's list for its first page, filtered by the
// typed text on every search field when the page has any. A paged list is
// asked for its largest page, so a picker over a short catalogue sees all of
// it.
export function refListURL(target: AdminPageSpec, search: string): string | null {
  const list = target.list;
  if (!list) return null;
  const paging = list.paging;
  return buildListURL(
    list.endpoint,
    {},
    list.search ?? [],
    search,
    "",
    "",
    paging,
    "",
    paging?.max_limit || paging?.default_limit,
  );
}

// useRefRows fetches the target page's rows: once for a page without search,
// again (debounced) as the operator types for a page with search.
function useRefRows(
  target: AdminPageSpec | undefined,
  search: string,
): { rows: Row[]; loaded: boolean; failed: boolean } {
  const searchable = !!target?.list?.search?.length;
  const [state, setState] = useState<{ rows: Row[]; loaded: boolean; failed: boolean }>({
    rows: [],
    loaded: false,
    failed: false,
  });
  const [query, setQuery] = useState("");
  useEffect(() => {
    if (!searchable) return;
    const h = setTimeout(() => setQuery(search), refSearchDebounceMs);
    return () => clearTimeout(h);
  }, [search, searchable]);
  useEffect(() => {
    if (!target) return;
    const url = refListURL(target, searchable ? query : "");
    if (!url) return;
    let cancelled = false;
    apiGet<ListResp>(url, { response: target.list?.response_ref })
      .then((resp) => {
        if (!cancelled) setState({ rows: extractRows(resp), loaded: true, failed: false });
      })
      .catch(() => {
        // A picker that cannot load its options degrades to showing the value
        // it holds; it never blocks the form.
        if (!cancelled) setState((s) => ({ ...s, loaded: true, failed: true }));
      });
    return () => {
      cancelled = true;
    };
  }, [target, searchable, query]);
  return state;
}

export interface RefPickerProps {
  field: string;
  fieldRef: AdminFieldRefSpec;
  spec: AdminSpec;
  value: unknown;
  onChange: (v: unknown) => void;
  // The current value of the `depends_on` field, when the ref has one.
  dependsValue?: unknown;
  disabled?: boolean;
  t?: Translate;
}

// RefPicker is the controlled picker; RefField binds it to a react-hook-form
// field, and the action form drives it from its own state.
export function RefPicker({
  field,
  fieldRef,
  spec,
  value,
  onChange,
  dependsValue,
  disabled,
  t,
}: RefPickerProps) {
  const target = spec.pages?.[fieldRef.page];
  const [search, setSearch] = useState("");
  const { rows, loaded, failed } = useRefRows(target, search);
  const all = useMemo(() => refOptions(rows, fieldRef, spec), [rows, fieldRef, spec]);
  const offered = useMemo(() => narrow(all, fieldRef, dependsValue), [all, fieldRef, dependsValue]);

  // The depends_on field moved: a value its new parent does not offer goes.
  // Only on a CHANGE of that field, after the options are in — an edit form
  // opening on a stored pair must not lose its value while it loads.
  const prevDepends = useRef(token(dependsValue));
  useEffect(() => {
    const now = token(dependsValue);
    if (!fieldRef.depends_on || now === prevDepends.current) return;
    prevDepends.current = now;
    if (!loaded) return;
    const held = token(value);
    if (held != null && !offered.some((o) => o.value === held)) onChange(null);
  }, [dependsValue, fieldRef.depends_on, loaded, offered, value, onChange]);

  const held = token(value);
  // The value the field holds stays visible even when no option carries it
  // (not loaded yet, filtered out, or the list failed): the raw value, never
  // a blank that a save would then write back.
  const data = offered.map((o) => ({ value: o.value, label: o.label }));
  if (held != null && !data.some((d) => d.value === held)) {
    const known = all.find((o) => o.value === held);
    data.unshift({ value: held, label: known ? known.label : held });
  }
  const tr = t ?? ((s: string) => s);
  return (
    <Select
      label={humanizeLabel(field)}
      disabled={disabled}
      data={data}
      value={held}
      onChange={(tok) => {
        if (tok == null) {
          onChange(null);
          return;
        }
        const hit = all.find((o) => o.value === tok);
        onChange(hit ? hit.wire : tok);
      }}
      searchable
      clearable
      searchValue={search}
      onSearchChange={setSearch}
      nothingFoundMessage={failed ? tr("Could not load the options") : tr("Nothing found")}
      // A searchable page answers the typed text itself; filtering its answer
      // again by label would hide a match on a field the label does not show.
      filter={target?.list?.search?.length ? ({ options }) => options : undefined}
    />
  );
}

// RefField is RefPicker bound to a react-hook-form field, watching its
// depends_on field in the same form.
export function RefField({
  field,
  fieldRef,
  spec,
  control,
  defaultValue,
  disabled,
  t,
}: {
  field: string;
  fieldRef: AdminFieldRefSpec;
  spec: AdminSpec;
  control: Control<Record<string, unknown>>;
  defaultValue: unknown;
  disabled?: boolean;
  t?: Translate;
}) {
  const { field: bound } = useController({ name: field, control, defaultValue });
  const dependsValue = useWatch({ control, name: fieldRef.depends_on || "__w17_no_field__" });
  return (
    <RefPicker
      field={field}
      fieldRef={fieldRef}
      spec={spec}
      value={bound.value}
      onChange={bound.onChange}
      dependsValue={fieldRef.depends_on ? dependsValue : undefined}
      disabled={disabled}
      t={t}
    />
  );
}
