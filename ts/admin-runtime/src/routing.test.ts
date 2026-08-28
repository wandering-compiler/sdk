import { describe, expect, it } from "vitest";

import { parseHash } from "./App";
import type { AdminSpec } from "./types";

// The hash is the source of truth for navigation, so parseHash decides
// what a deep link or a hand-typed URL resolves to. The create route is
// the one with a guard: a page without a create endpoint must not open
// a form that cannot submit.
const spec = {
  name: "admin",
  overview: { widgets: [] },
  pages: {
    Notes: {
      name: "Notes",
      detail: { read_endpoint: "/x", create_endpoint: "/admin/api/detail/Notes", fields: [] },
    },
    ReadOnly: {
      name: "ReadOnly",
      detail: { read_endpoint: "/y", fields: [] },
    },
    // A list-only page: a model with no single-column identity cannot be
    // keyed by a row id, so it declares no detail and Go serialises the
    // field as a literal null.
    TaskTags: {
      name: "TaskTags",
      list: { endpoint: "/admin/api/list/TaskTags", columns: [{ name: "task_id" }] },
      detail: null,
    },
  },
} as unknown as AdminSpec;

describe("parseHash", () => {
  it("resolves the documented routes", () => {
    expect(parseHash("#/overview", spec)).toEqual({ kind: "overview" });
    expect(parseHash("#/list/Notes", spec)).toEqual({ kind: "list", pageName: "Notes" });
    expect(parseHash("#/detail/Notes/7", spec)).toEqual({
      kind: "detail",
      pageName: "Notes",
      rowId: "7",
    });
    expect(parseHash("#/create/Notes", spec)).toEqual({ kind: "create", pageName: "Notes" });
  });

  // The guard: a page with no create_endpoint has no create form, so
  // the route must fall through to the default view rather than render
  // one that can't submit.
  it("refuses a create route for a page that declares no create endpoint", () => {
    expect(parseHash("#/create/ReadOnly", spec)).toBeNull();
  });

  it("refuses routes naming an unknown page", () => {
    expect(parseHash("#/create/Ghost", spec)).toBeNull();
    expect(parseHash("#/list/Ghost", spec)).toBeNull();
    expect(parseHash("#/detail/Ghost/1", spec)).toBeNull();
  });

  it("returns null for empty or unrecognised hashes", () => {
    expect(parseHash("", spec)).toBeNull();
    expect(parseHash("#", spec)).toBeNull();
    expect(parseHash("#/", spec)).toBeNull();
    expect(parseHash("#/nonsense", spec)).toBeNull();
    expect(parseHash("#/create", spec)).toBeNull();
  });

  // Page names and row ids round-trip through encodeURIComponent, so a
  // non-ASCII id or a name with a slash must survive.
  it("decodes percent-encoded segments", () => {
    expect(parseHash("#/detail/Notes/" + encodeURIComponent("a/b"), spec)).toEqual({
      kind: "detail",
      pageName: "Notes",
      rowId: "a/b",
    });
  });
});

// T2-6 pass #9, B9-1. The detail route checked only that the page EXISTS,
// while the sibling create route one branch below checked that the page
// declares one. So a hand-typed or stale-bookmarked #/detail/<list-only
// page>/<id> resolved, mounted DetailPage, and dereferenced a null detail
// spec — a crash, not a 404.
describe("parseHash — a list-only page has no detail route", () => {
  it("refuses #/detail for a page whose detail is null", () => {
    expect(parseHash("#/detail/TaskTags/0", spec)).toBeNull();
  });

  it("still resolves the list route for that page", () => {
    expect(parseHash("#/list/TaskTags", spec)).toEqual({ kind: "list", pageName: "TaskTags" });
  });
});
