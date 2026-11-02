// listQuery — how the admin asks a page for its rows and reads them back,
// shared by the list table and the form pickers that offer another page's rows.

import type { AdminListPagingSpec } from "./types";

export interface ListResp {
  // Storage list responses carry a single repeated field with
  // the rows. Field name varies — we accept the first array-
  // typed property we find.
  [key: string]: unknown;
}

export type Row = Record<string, unknown>;

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
export function buildListURL(
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

// extractRows pulls the first array-typed property out of a
// list response. Storage list responses follow the convention
// `repeated <Msg> <items_field> = 1` — the items_field name
// varies (users / tasks / orders), so the runtime accepts any. No body
// at all is no rows, not a failure.
export function extractRows(resp: ListResp | null | undefined): Row[] {
  if (!resp || typeof resp !== "object") return [];
  for (const k of Object.keys(resp)) {
    const v = resp[k];
    if (Array.isArray(v)) {
      return v as Row[];
    }
  }
  return [];
}
