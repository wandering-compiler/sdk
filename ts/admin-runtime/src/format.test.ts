import { describe, expect, it } from "vitest";

import { columnHeader, humanizeLabel, pageLabel } from "./format";
import type { AdminPageSpec } from "./types";

// humanizeLabel is the admin UI's de-jargon layer — every field /
// column / page / widget label passes through it so raw wire names
// never reach end users. These pin the sentence-case + acronym
// behaviour the screens rely on.
describe("humanizeLabel", () => {
  it("humanizes snake_case, upper-casing known acronyms", () => {
    expect(humanizeLabel("org_id")).toBe("Org ID");
    expect(humanizeLabel("wallet_id")).toBe("Wallet ID");
    expect(humanizeLabel("amount_units")).toBe("Amount units");
    expect(humanizeLabel("auth_user_count")).toBe("Auth user count");
  });

  it("splits camelCase / PascalCase into sentence case", () => {
    expect(humanizeLabel("createdAt")).toBe("Created at");
    expect(humanizeLabel("TopupAdmin")).toBe("Topup admin");
  });

  it("splits letter→digit runs", () => {
    expect(humanizeLabel("sha256")).toBe("Sha 256");
  });

  it("returns empty string for empty / whitespace input", () => {
    expect(humanizeLabel("")).toBe("");
    expect(humanizeLabel("   ")).toBe("");
  });
});

describe("pageLabel", () => {
  const base: AdminPageSpec = {
    name: "TopupAdmin",
    model: "Topup",
    detail: { read_endpoint: "/x", fields: [] },
  };

  it("prefers the author-declared title", () => {
    expect(pageLabel({ ...base, title: "Top-ups" })).toBe("Top-ups");
  });

  it("falls back to the humanized page name", () => {
    expect(pageLabel(base)).toBe("Topup admin");
  });

  it("ignores a blank title", () => {
    expect(pageLabel({ ...base, title: "   " })).toBe("Topup admin");
  });
});

// columnHeader is the list table's header resolver — a column's
// declared `label` (Django verbose_name) wins over field-name
// humanization, with a defensive fall-back so a blank override never
// renders an empty header.
describe("columnHeader", () => {
  it("uses a declared label override verbatim", () => {
    expect(columnHeader({ name: "owner", label: "Account owner" })).toBe("Account owner");
  });

  it("humanizes the field name when no label is declared", () => {
    expect(columnHeader({ name: "org_id" })).toBe("Org ID");
    expect(columnHeader({ name: "created_at" })).toBe("Created at");
  });

  it("falls back to humanization for a blank label", () => {
    expect(columnHeader({ name: "owner", label: "   " })).toBe("Owner");
  });
});
