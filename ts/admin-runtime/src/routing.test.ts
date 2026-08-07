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
