import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MantineProvider } from "@mantine/core";

import { CreatePage } from "./CreatePage";
import { DetailPage } from "./DetailPage";
import { ActionModal } from "./ActionModal";
import { narrow, refListURL, refOptions } from "./refPicker";
import type { AdminFieldRefSpec, AdminPageSpec, AdminSpec } from "./types";

vi.mock("./api", async () => {
  const actual = await vi.importActual<typeof import("./api")>("./api");
  return { ...actual, apiGet: vi.fn(), apiPost: vi.fn(), apiPatch: vi.fn() };
});
const { apiGet, apiPost, apiPatch } = await import("./api");

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

// A form field that names a row of another page used to be a text box the
// operator typed a uuid into. With `field_refs` it is a picker over that
// page's rows, read by their text, and the form sends the row's value.

const orgs = [
  { id: "11111111-0000-4000-8000-000000000001", name: "Acme" },
  { id: "11111111-0000-4000-8000-000000000002", name: "Globex" },
];
const roles = [
  { product: "maps", key: "geo_reader", name: "Geo reader" },
  { product: "maps", key: "geo_admin", name: "Geo admin" },
  { product: "fin", key: "ledger", name: "Ledger" },
];

const orgsPage = {
  name: "Organizations",
  model: "auth.Organization",
  list: { endpoint: "/admin/api/list/Organizations", columns: [], search: ["q"] },
  detail: null,
} as unknown as AdminPageSpec;
const rolesPage = {
  name: "ProductRoles",
  model: "auth.ProductRole",
  list: { endpoint: "/admin/api/list/ProductRoles", columns: [] },
  detail: null,
} as unknown as AdminPageSpec;

const orgRef: AdminFieldRefSpec = { page: "Organizations", value: "id" };
const productRef: AdminFieldRefSpec = {
  page: "ProductRoles",
  value: "product",
  title: "{product}",
};
const roleRef: AdminFieldRefSpec = {
  page: "ProductRoles",
  value: "key",
  title: "{name}",
  depends_on: "product",
  parent: "product",
};

const plansPage = {
  name: "OrgPlans",
  model: "app.PlanLine",
  detail: {
    read_endpoint: "/admin/api/detail/OrgPlans/{id}",
    update_endpoint: "/admin/api/detail/OrgPlans/{id}",
    create_endpoint: "/admin/api/detail/OrgPlans",
    create_fields: ["org_id", "product", "role_key"],
    fields: ["org_id", "product", "role_key"],
    field_refs: { org_id: orgRef, product: productRef, role_key: roleRef },
  },
} as unknown as AdminPageSpec;

const spec = {
  title_templates: { "auth.Organization": "{name}" },
  pages: { Organizations: orgsPage, ProductRoles: rolesPage, OrgPlans: plansPage },
} as unknown as AdminSpec;

// The picker asks each target page's list endpoint; the edit form also reads
// the row itself.
function serve(row?: Record<string, unknown>) {
  vi.mocked(apiGet).mockImplementation((url: string) => {
    if (url.startsWith("/admin/api/list/Organizations")) {
      const q = new URL(url, "http://x").searchParams.get("q") ?? "";
      return Promise.resolve({
        orgs: orgs.filter((o) => o.name.toLowerCase().includes(q.toLowerCase())),
      });
    }
    if (url.startsWith("/admin/api/list/ProductRoles")) return Promise.resolve({ roles });
    if (url.startsWith("/admin/api/detail/OrgPlans")) return Promise.resolve(row ?? {});
    return Promise.reject(new Error("unexpected " + url));
  });
}

function wrap(node: React.ReactNode) {
  return render(<MantineProvider>{node}</MantineProvider>);
}

describe("refOptions", () => {
  it("labels by the ref's title, then the model's title template, then the value", () => {
    expect(refOptions(orgs, orgRef, spec).map((o) => o.label)).toEqual(["Acme", "Globex"]);
    expect(
      refOptions(orgs, { page: "Organizations", value: "id", title: "#{name}!" }, spec)[0].label,
    ).toBe("#Acme!");
    const bare = { ...spec, title_templates: {} } as AdminSpec;
    expect(refOptions(orgs, orgRef, bare)[0].label).toBe(orgs[0].id);
  });

  it("offers a value once however many rows carry it", () => {
    expect(refOptions(roles, productRef, spec).map((o) => o.value)).toEqual(["maps", "fin"]);
  });

  it("keeps the row's own value for the wire, a number as a number", () => {
    const o = refOptions(
      [{ n: 7, label: "Seven" }],
      { page: "X", value: "n", title: "{label}" },
      spec,
    );
    expect(o[0]).toMatchObject({ value: "7", label: "Seven", wire: 7 });
  });

  it("narrows by the depends_on value, and offers nothing before it is chosen", () => {
    const all = refOptions(roles, roleRef, spec);
    expect(narrow(all, roleRef, "maps").map((o) => o.value)).toEqual(["geo_reader", "geo_admin"]);
    expect(narrow(all, roleRef, undefined)).toEqual([]);
  });

  it("asks a searchable page with the typed text on its search fields", () => {
    expect(refListURL(orgsPage, "acm")).toBe("/admin/api/list/Organizations?q=acm");
    expect(refListURL(rolesPage, "ignored")).toBe("/admin/api/list/ProductRoles");
  });
});

describe("a picker on the create form", () => {
  it("offers the rows by their text and sends the chosen row's value", async () => {
    serve();
    vi.mocked(apiPost).mockResolvedValue({ id: "new" });
    wrap(<CreatePage page={plansPage} spec={spec} onBack={vi.fn()} onCreated={vi.fn()} />);
    const user = userEvent.setup();

    await user.click(screen.getByRole("combobox", { name: /org id/i }));
    await user.click(await screen.findByText("Globex"));
    await user.click(screen.getByRole("combobox", { name: /^product/i }));
    await user.click(await screen.findByText("maps"));
    await user.click(screen.getByRole("combobox", { name: /role key/i }));
    // Only the chosen product's roles, by their names.
    expect(await screen.findByText("Geo reader")).toBeTruthy();
    expect(screen.queryByText("Ledger")).toBeNull();
    await user.click(screen.getByText("Geo admin"));
    await user.click(screen.getByRole("button", { name: /create/i }));

    await waitFor(() => expect(apiPost).toHaveBeenCalled());
    const [, body] = vi.mocked(apiPost).mock.calls[0];
    expect(body).toEqual({ org_id: orgs[1].id, product: "maps", role_key: "geo_admin" });
  });

  it("asks a searchable page for what the operator types", async () => {
    serve();
    wrap(<CreatePage page={plansPage} spec={spec} onBack={vi.fn()} onCreated={vi.fn()} />);
    const user = userEvent.setup();
    await user.type(screen.getByRole("combobox", { name: /org id/i }), "glo");
    await waitFor(() =>
      expect(
        vi.mocked(apiGet).mock.calls.some(([u]) => u === "/admin/api/list/Organizations?q=glo"),
      ).toBe(true),
    );
  });

  it("clears a role that the newly chosen product does not offer", async () => {
    serve();
    wrap(<CreatePage page={plansPage} spec={spec} onBack={vi.fn()} onCreated={vi.fn()} />);
    const user = userEvent.setup();
    await user.click(screen.getByRole("combobox", { name: /^product/i }));
    await user.click(await screen.findByText("maps"));
    await user.click(screen.getByRole("combobox", { name: /role key/i }));
    await user.click(await screen.findByText("Geo reader"));
    expect(screen.getByDisplayValue("Geo reader")).toBeTruthy();

    await user.click(screen.getByRole("combobox", { name: /^product/i }));
    await user.click(await screen.findByText("fin"));
    await waitFor(() => expect(screen.queryByDisplayValue("Geo reader")).toBeNull());
  });
});

describe("a picker on the edit form", () => {
  it("shows the stored value's text, not its id", async () => {
    serve({ org_id: orgs[0].id, product: "maps", role_key: "geo_reader" });
    vi.mocked(apiPatch).mockResolvedValue({});
    wrap(<DetailPage spec={spec} page={plansPage} rowId="1" onBack={vi.fn()} />);

    // The visible combobox reads the row's text; the id rides in Mantine's
    // hidden form input, which is not what the operator sees.
    const shown = (name: RegExp) => screen.getByRole<HTMLInputElement>("combobox", { name }).value;
    await waitFor(() => expect(shown(/org id/i)).toBe("Acme"));
    expect(shown(/role key/i)).toBe("Geo reader");
  });

  it("shows the raw value when no option carries it, never a blank", async () => {
    serve({
      org_id: "99999999-dead-4000-8000-000000000000",
      product: "maps",
      role_key: "geo_reader",
    });
    wrap(<DetailPage spec={spec} page={plansPage} rowId="1" onBack={vi.fn()} />);
    const shown = () => screen.getByRole<HTMLInputElement>("combobox", { name: /org id/i }).value;
    await waitFor(() => expect(shown()).toBe("99999999-dead-4000-8000-000000000000"));
  });
});

describe("a ref field the form does not submit", () => {
  // org_id readonly on the edit form; a detail with no update shows every
  // field display-only. Both read the picked row's text, not its id.
  const readonlyPage = {
    ...plansPage,
    detail: { ...plansPage.detail, readonly_fields: ["org_id"], fields: ["product", "role_key"] },
  } as unknown as AdminPageSpec;
  const viewOnlyPage = {
    ...plansPage,
    detail: { ...plansPage.detail, update_endpoint: "" },
  } as unknown as AdminPageSpec;
  const shown = (name: RegExp) => screen.getByRole<HTMLInputElement>("textbox", { name }).value;

  it("shows a readonly field's row by its text", async () => {
    serve({ org_id: orgs[1].id, product: "maps", role_key: "geo_reader" });
    wrap(<DetailPage spec={spec} page={readonlyPage} rowId="1" onBack={vi.fn()} />);
    await waitFor(() => expect(shown(/org id/i)).toBe("Globex"));
  });

  it("shows every ref of a view-only detail by its text", async () => {
    serve({ org_id: orgs[0].id, product: "maps", role_key: "geo_admin" });
    wrap(<DetailPage spec={spec} page={viewOnlyPage} rowId="1" onBack={vi.fn()} />);
    await waitFor(() => expect(shown(/org id/i)).toBe("Acme"));
    expect(shown(/role key/i)).toBe("Geo admin");
  });

  it("shows the value itself when no row carries it", async () => {
    serve({
      org_id: "99999999-dead-4000-8000-000000000000",
      product: "maps",
      role_key: "geo_reader",
    });
    wrap(<DetailPage spec={spec} page={readonlyPage} rowId="1" onBack={vi.fn()} />);
    await waitFor(() => expect(shown(/org id/i)).toBe("99999999-dead-4000-8000-000000000000"));
  });
});

describe("a picker on an action form", () => {
  it("sends the chosen row's value with the action", async () => {
    serve();
    vi.mocked(apiPost).mockResolvedValue({});
    const action = {
      endpoint: "/admin/api/action/Members/move",
      fields: ["org_id"],
      target: "DETAIL",
      field_refs: { org_id: orgRef },
    } as unknown as Parameters<typeof ActionModal>[0]["action"];
    wrap(
      <ActionModal
        action={action}
        actionName="move"
        open
        onClose={vi.fn()}
        onSuccess={vi.fn()}
        selectedIds={["m1"]}
        spec={spec}
      />,
    );
    const user = userEvent.setup();
    await user.click(screen.getByRole("combobox", { name: /org id/i }));
    await user.click(await screen.findByText("Acme"));
    await user.click(screen.getByRole("button", { name: /move/i }));
    await waitFor(() => expect(apiPost).toHaveBeenCalled());
    expect(vi.mocked(apiPost).mock.calls[0][1]).toEqual({ ids: ["m1"], org_id: orgs[0].id });
  });
});
