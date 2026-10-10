import { describe, expect, it } from "vitest";

import { parseHash, serializeView } from "./App";
import type { View } from "./App";
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

// The detail route once checked only that the page EXISTS,
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

// serializeView is the other half of the hash, and nothing was checking that the two
// halves agree. The round trip here is string -> parseHash: `navigate` writing the hash
// and the browser normalising what it stores is a different question and this does not
// answer it. parseHash returns null for anything it does not recognise and the
// caller falls back to the default view, so an asymmetry does not throw — it navigates
// somewhere else. That is the failure this section exists for.
describe("serializeView round-trips through parseHash", () => {
  // KEYED BY KIND, not an array, and that is the whole point of the shape: a
  // `Record<View["kind"], View>` is incomplete-checked, so a fifth kind added to the
  // union stops this object from compiling. An array literal does not — measured: with
  // a fifth kind added, tsc reports serializeView's missing switch case (TS2366) and
  // says nothing about an array that no longer covers the union. The first draft of
  // this test claimed both halves and only one was true.
  const views: Record<View["kind"], View> = {
    overview: { kind: "overview" },
    list: { kind: "list", pageName: "Notes" },
    detail: { kind: "detail", pageName: "Notes", rowId: "7" },
    create: { kind: "create", pageName: "Notes" },
  };

  it("returns the same view it was given, for every kind", () => {
    for (const v of Object.values(views)) {
      expect(parseHash("#" + serializeView(v), spec), `round trip failed for ${v.kind}`).toEqual(v);
    }
  });

  // The encoding half, and the reason serializeView calls encodeURIComponent at all: a
  // row id is whatever the database holds. An unencoded "/" turns one segment into two
  // and parseHash reads a THREE-part detail path as something else entirely — the same
  // class of defect as the inline endpoint that had to encode its parent id.
  it("survives ids that would otherwise change the shape of the path", () => {
    for (const rowId of ["a/b", "a b", "a#b", "a%2Fb", "ünïcødé", "a?b&c=d"]) {
      const v: View = { kind: "detail", pageName: "Notes", rowId };
      expect(parseHash("#" + serializeView(v), spec), `lost id ${rowId}`).toEqual(v);
    }
  });

  // And the same for a page name, which reaches the hash from the spec rather than
  // from a caller — a generated page name is an identifier today, so this is the
  // property holding rather than a live risk.
  it("survives a page name that needs encoding", () => {
    const pages = { ...spec.pages, "Odd/Name": { name: "Odd/Name", list: { columns: [] } } };
    const oddSpec = { ...spec, pages } as unknown as typeof spec;
    const v: View = { kind: "list", pageName: "Odd/Name" };
    expect(parseHash("#" + serializeView(v), oddSpec)).toEqual(v);
  });
});
