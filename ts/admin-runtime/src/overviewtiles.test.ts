import { describe, expect, it } from "vitest";

import { defaultTileGroups, hasDeclaredWidgets, overviewIsAvailable } from "./overviewTiles";
import { formatCount } from "./OverviewPage";
import type { AdminSpec } from "./types";

// A spec builder: pages keyed by name, an optional nav + overview.
function makeSpec(over: Partial<AdminSpec>): AdminSpec {
  return {
    name: "Admin",
    schema_version: "1",
    auth: { login_endpoint: "/login", whoami_endpoint: "/whoami" },
    pages: {},
    ...over,
  } as AdminSpec;
}

const usersPage = {
  name: "Users",
  model: "auth.User",
  detail: { read_endpoint: "/d", fields: [] },
  list: {
    endpoint: "/admin/api/list/Users",
    item_type: "auth.User",
    columns: [{ name: "id" }],
    // paged → countable
    paging: {
      cursor_param: "cursor",
      limit_param: "limit",
      default_limit: 50,
      max_limit: 100,
      items_field: "users",
      paging_field: "paging",
    },
  },
};

const rolesPage = {
  name: "Roles",
  model: "auth.Role",
  detail: { read_endpoint: "/d", fields: [] },
  // unpaged list → no count
  list: { endpoint: "/admin/api/list/Roles", item_type: "auth.Role", columns: [{ name: "id" }] },
};

describe("defaultTileGroups", () => {
  it("uses declared nav groups and marks paged pages countable", () => {
    const spec = makeSpec({
      pages: { Users: usersPage, Roles: rolesPage },
      nav: [{ title: "Accounts", pages: ["Users", "Roles"] }],
    });
    const groups = defaultTileGroups(spec, undefined);
    expect(groups).toHaveLength(1);
    expect(groups[0].title).toBe("Accounts");
    const [users, roles] = groups[0].pages;
    expect(users.name).toBe("Users");
    expect(users.count).toEqual({
      endpoint: "/admin/api/list/Users",
      limitParam: "limit",
      pagingField: "paging",
    });
    // Unpaged Roles has a link but no count.
    expect(roles.listEndpoint).toBe("/admin/api/list/Roles");
    expect(roles.count).toBeUndefined();
  });

  it("falls back to a single group of all pages when no nav is declared", () => {
    const spec = makeSpec({ pages: { Users: usersPage, Roles: rolesPage } });
    const groups = defaultTileGroups(spec, undefined);
    expect(groups).toHaveLength(1);
    expect(groups[0].title).toBe("Admin");
    expect(groups[0].pages.map((p) => p.name)).toEqual(["Users", "Roles"]);
  });

  it("drops pages the user lacks permission for, then omits an empty group", () => {
    const gated = {
      ...rolesPage,
      list: { ...rolesPage.list, required_permissions: [7] },
    };
    const spec = makeSpec({
      pages: { Roles: gated },
      nav: [{ title: "Accounts", pages: ["Roles"] }],
    });
    // user has no perms → gated page hidden → group empty → dropped
    expect(defaultTileGroups(spec, [])).toHaveLength(0);
    // user has perm 7 → visible
    expect(defaultTileGroups(spec, [7])).toHaveLength(1);
  });
});

describe("overviewIsAvailable", () => {
  it("is true when a custom overview is declared", () => {
    expect(overviewIsAvailable(makeSpec({ overview: { widgets: [] } }))).toBe(true);
  });
  it("is true when there is at least one page (default tiles can render)", () => {
    expect(overviewIsAvailable(makeSpec({ pages: { Users: usersPage } }))).toBe(true);
  });
  it("is false for an empty admin with no pages and no overview", () => {
    expect(overviewIsAvailable(makeSpec({}))).toBe(false);
  });
});

describe("hasDeclaredWidgets", () => {
  it("is false when overview is absent or has no widgets", () => {
    expect(hasDeclaredWidgets(makeSpec({}))).toBe(false);
    expect(hasDeclaredWidgets(makeSpec({ overview: { widgets: [] } }))).toBe(false);
  });
  it("is true when the overview declares at least one widget", () => {
    expect(
      hasDeclaredWidgets(makeSpec({ overview: { widgets: [{ slot: "x", size: "SMALL" }] } })),
    ).toBe(true);
  });
});

describe("formatCount", () => {
  it("renders a protojson uint64 string with thousands separators", () => {
    expect(formatCount("1234")).toBe((1234).toLocaleString());
  });
  it("renders a number", () => {
    expect(formatCount(512)).toBe((512).toLocaleString());
  });
  it("treats absent/null as 0 (EmitDefaultValues:false omits a zero total)", () => {
    expect(formatCount(undefined)).toBe("0");
    expect(formatCount(null)).toBe("0");
  });
  it("passes through an unparseable value rather than showing NaN", () => {
    expect(formatCount("n/a")).toBe("n/a");
  });
});
