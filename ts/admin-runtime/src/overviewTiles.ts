// Default overview tiles — the "aby tam něco bylo" landing shown when
// the admin declares no custom overview widgets. A tile per nav group
// lists the group's pages as quick-links; a page whose list method is
// PAGED also shows a row count, because a paged list already computes
// COUNT(*) for its pager total (REV-148) — so the number is free. An
// unpaged list carries no cheap total, so it shows just the link (no
// number). No pg_class estimate, no synthesized count query: the tile
// reuses exactly what the paged list endpoint already returns.
//
// This module is the PURE part (spec → tile model + the count request
// descriptor); the React OverviewPage does the actual fetch + render.
// Kept JSX-free so the grouping + countability rules are unit-testable
// without a render harness.

import { pageVisibleTo } from "./auth";
import { pageLabel } from "./format";
import type { AdminSpec, AdminNavGroup } from "./types";

// OverviewCountRequest describes how to fetch one page's row count off
// its paged list endpoint: GET <endpoint>?<limitParam>=1, then read
// `<pagingField>.total` from the JSON response. Present only for paged
// lists; the limit=1 keeps the payload to a single row while the
// server still computes the full COUNT(*) for the pager total.
export interface OverviewCountRequest {
  endpoint: string;
  limitParam: string;
  pagingField: string;
}

export interface OverviewTilePage {
  name: string;
  label: string;
  // Present iff the page has a list view — the tile links here.
  listEndpoint?: string;
  // Present iff the list is paged — the tile can show a count.
  count?: OverviewCountRequest;
}

export interface OverviewTileGroup {
  title: string;
  pages: OverviewTilePage[];
}

// defaultTileGroups builds the default-overview tile model from the
// spec: groups follow the declared nav (spec.nav) when present, else a
// single implicit group holds every page in declaration order. Pages
// the user can't see (per list permissions) are dropped, and a group
// that ends up empty is omitted — so the overview never shows a tile
// the user can't act on, mirroring the nav's own perm filter.
export function defaultTileGroups(
  spec: AdminSpec,
  userPerms: number[] | undefined,
): OverviewTileGroup[] {
  const groups = navGroupsOrFallback(spec);
  const out: OverviewTileGroup[] = [];
  for (const g of groups) {
    const pages: OverviewTilePage[] = [];
    for (const pName of g.pages) {
      const page = spec.pages[pName];
      if (!page || !pageVisibleTo(page, userPerms)) continue;
      pages.push(tilePage(spec, pName));
    }
    if (pages.length > 0) out.push({ title: g.title, pages });
  }
  return out;
}

// navGroupsOrFallback returns the declared nav groups, or a single
// implicit group (titled by the admin name) containing every page in
// declaration order when no nav is declared. A flat admin still gets a
// populated overview.
function navGroupsOrFallback(spec: AdminSpec): AdminNavGroup[] {
  if (spec.nav && spec.nav.length > 0) return spec.nav;
  return [{ title: spec.name, pages: Object.keys(spec.pages) }];
}

// tilePage projects one page name into its tile model: its label, its
// list endpoint (when it has a list), and a count request (when that
// list is paged).
function tilePage(spec: AdminSpec, name: string): OverviewTilePage {
  const page = spec.pages[name];
  const tile: OverviewTilePage = { name, label: pageLabel(page) };
  const list = page.list;
  if (!list) return tile;
  tile.listEndpoint = list.endpoint;
  if (list.paging) {
    tile.count = {
      endpoint: list.endpoint,
      limitParam: list.paging.limit_param,
      pagingField: list.paging.paging_field,
    };
  }
  return tile;
}

// overviewIsAvailable reports whether the overview view can render
// anything: either the admin declared an overview (custom widgets) or
// there is at least one page to build default tiles from. App uses
// this to decide the default landing view + whether to show the
// Overview nav entry. A truly empty admin (no pages) has no overview.
export function overviewIsAvailable(spec: AdminSpec): boolean {
  if (spec.overview) return true;
  return Object.keys(spec.pages).length > 0;
}

// hasDeclaredWidgets reports whether the declared overview carries at
// least one widget (of any kind — CUSTOM or STAT). When true the
// overview renders ONLY those widgets (the design's "widget declared →
// default tiles disappear" rule); when false the default group tiles
// render instead.
export function hasDeclaredWidgets(spec: AdminSpec): boolean {
  return !!spec.overview && spec.overview.widgets.length > 0;
}
