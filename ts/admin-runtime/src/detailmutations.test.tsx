import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MantineProvider } from "@mantine/core";

import { DetailPage } from "./DetailPage";
import type { AdminPageSpec, AdminSpec } from "./types";

vi.mock("./api", async () => {
  const actual = await vi.importActual<typeof import("./api")>("./api");
  return { ...actual, apiGet: vi.fn(), apiPatch: vi.fn(), apiDelete: vi.fn() };
});
const { apiGet, apiPatch, apiDelete } = await import("./api");

// The two MUTATIONS a detail view performs, neither of which had a test.
// `detailpage.test.tsx` covers formatting — which value is display-only and which
// round-trips through the form — and stops at the read. So the form that PATCHes and
// the button that deletes were reached by nothing, and `onDelete` was one of the 45
// never-executed functions in this runtime.
//
// What that leaves unpinned is the shape of the request (only editable fields go up,
// the row id is encoded into the path) and what happens when either call is refused.

const spec = {} as AdminSpec;

function page(overrides: Partial<NonNullable<AdminPageSpec["detail"]>> = {}): AdminPageSpec {
  return {
    name: "Invoices",
    detail: {
      read_endpoint: "/admin/api/detail/Invoices/{id}",
      update_endpoint: "/admin/api/detail/Invoices/{id}",
      delete_endpoint: "/admin/api/detail/Invoices/{id}",
      fields: ["amount"],
      readonly_fields: ["created_at"],
      ...overrides,
    },
  } as AdminPageSpec;
}

function renderDetail(p: AdminPageSpec = page(), rowId = "1") {
  const onBack = vi.fn();
  render(
    <MantineProvider>
      <DetailPage spec={spec} page={p} rowId={rowId} onBack={onBack} />
    </MantineProvider>,
  );
  return { onBack };
}

async function loaded() {
  await waitFor(() => expect(screen.getByDisplayValue("1234.5")).toBeTruthy());
}

beforeEach(() => {
  vi.mocked(apiGet).mockReset();
  vi.mocked(apiPatch).mockReset();
  vi.mocked(apiDelete).mockReset();
  vi.mocked(apiGet).mockResolvedValue({ amount: "1234.5", created_at: "2026-07-26T14:03:00Z" });
});

afterEach(() => {
  cleanup();
  // restoreAllMocks and not a per-test `confirm.mockRestore()`: a test that FAILS
  // before its restore line leaves `window.confirm` stubbed for every test after it,
  // in this file and in every later file of the same worker. That is the cross-file
  // global this suite has already been bitten by through navigator.language.
  vi.restoreAllMocks();
});

describe("saving a detail row", () => {
  // "Send only the editable fields; readonly + auto-generated fields stay where they
  // were read from." A PATCH carrying `created_at` back would write a value the
  // operator never typed — and on a server that trusts the payload, would overwrite an
  // audit timestamp with the one the page happened to read.
  it("patches the editable fields and nothing else", async () => {
    vi.mocked(apiPatch).mockResolvedValue({ amount: "99", created_at: "2026-07-26T14:03:00Z" });
    renderDetail();
    await loaded();

    await userEvent.clear(screen.getByDisplayValue("1234.5"));
    await userEvent.type(screen.getByRole("textbox", { name: /amount/i }), "99");
    await userEvent.click(screen.getByRole("button", { name: /^save$/i }));

    await waitFor(() => expect(apiPatch).toHaveBeenCalled());
    expect(vi.mocked(apiPatch).mock.calls[0][1]).toEqual({ amount: "99" });
  });

  // The RESPONSE has to be adopted, not just sent. `setRow(updated); reset(updated)`
  // is what puts the server's version of the row back into the form, and deleting
  // both of those broke NO test before this one existed — measured. A save that
  // succeeds on the wire and keeps showing what the operator typed hides any
  // normalisation the server did, and the NEXT save would PATCH the stale value.
  it("shows the server's version of the row after saving, not the typed one", async () => {
    vi.mocked(apiPatch).mockResolvedValue({ amount: "99.00", created_at: "2026-07-26T14:03:00Z" });
    renderDetail();
    await loaded();

    await userEvent.clear(screen.getByDisplayValue("1234.5"));
    await userEvent.type(screen.getByRole("textbox", { name: /amount/i }), "99");
    await userEvent.click(screen.getByRole("button", { name: /^save$/i }));

    expect(await screen.findByDisplayValue("99.00")).toBeTruthy();
    expect(screen.queryByDisplayValue("99"), "the form kept the typed value").toBeNull();
  });

  // The row id reaches the server as part of the PATH, so an id that is not URL-safe
  // addresses a different resource unencoded — the same class of defect the inline
  // endpoint's parent id carries a test for.
  it("encodes the row id into the update path", async () => {
    vi.mocked(apiPatch).mockResolvedValue({ amount: "1234.5" });
    renderDetail(page(), "a/b c");
    await loaded();

    await userEvent.click(screen.getByRole("button", { name: /^save$/i }));

    await waitFor(() => expect(apiPatch).toHaveBeenCalled());
    expect(vi.mocked(apiPatch).mock.calls[0][0]).toBe("/admin/api/detail/Invoices/a%2Fb%20c");
  });

  // A refused save has to say why and leave the form standing: the operator's edit is
  // still in the inputs and their next move is to change it and try again.
  it("reports a refusal and keeps the edit", async () => {
    vi.mocked(apiPatch).mockRejectedValue(new Error("amount exceeds the approved limit"));
    renderDetail();
    await loaded();

    await userEvent.clear(screen.getByDisplayValue("1234.5"));
    await userEvent.type(screen.getByRole("textbox", { name: /amount/i }), "99");
    await userEvent.click(screen.getByRole("button", { name: /^save$/i }));

    expect(await screen.findByText("amount exceeds the approved limit")).toBeTruthy();
  });

  it("reports a rejection that is not an Error at all", async () => {
    vi.mocked(apiPatch).mockRejectedValue("gateway exploded");
    renderDetail();
    await loaded();

    await userEvent.click(screen.getByRole("button", { name: /^save$/i }));

    expect(await screen.findByText("gateway exploded")).toBeTruthy();
  });
});

describe("deleting a detail row", () => {
  // `window.confirm` is the walking-skeleton gate the file says will become a modal.
  // Whatever it becomes, "declined" must mean nothing was sent — a delete that fires
  // on the way to asking is not recoverable.
  it("sends nothing when the confirmation is declined", async () => {
    const confirm = vi.spyOn(window, "confirm").mockReturnValue(false);
    renderDetail();
    await loaded();

    await userEvent.click(screen.getByRole("button", { name: /^delete$/i }));

    expect(confirm).toHaveBeenCalled();
    expect(apiDelete, "the row was deleted without a confirmation").not.toHaveBeenCalled();
  });

  it("deletes the confirmed row and navigates back", async () => {
    vi.spyOn(window, "confirm").mockReturnValue(true);
    vi.mocked(apiDelete).mockResolvedValue({});
    const { onBack } = renderDetail(page(), "a/b c");
    await loaded();

    await userEvent.click(screen.getByRole("button", { name: /^delete$/i }));

    await waitFor(() => expect(apiDelete).toHaveBeenCalled());
    // Encoded, for the same reason the update path is.
    expect(vi.mocked(apiDelete).mock.calls[0][0]).toBe("/admin/api/detail/Invoices/a%2Fb%20c");
    expect(onBack, "a deleted row left the operator on its own detail page").toHaveBeenCalled();
  });

  // A refused delete must NOT navigate away: leaving the page would read as success,
  // and the row is still there. The reason belongs on the screen the operator is on.
  it("stays on the row when the delete is refused", async () => {
    vi.spyOn(window, "confirm").mockReturnValue(true);
    vi.mocked(apiDelete).mockRejectedValue(new Error("row is referenced by 3 payments"));
    const { onBack } = renderDetail();
    await loaded();

    await userEvent.click(screen.getByRole("button", { name: /^delete$/i }));

    expect(await screen.findByText("row is referenced by 3 payments")).toBeTruthy();
    expect(onBack, "a refused delete navigated away as if it had worked").not.toHaveBeenCalled();
  });

  // The guard at the top of onDelete. A spec with no delete endpoint renders no
  // button, so this is the belt behind the braces — but the function is exported to
  // the button either way, and a spec that grows a Delete button without an endpoint
  // would otherwise call `replace` on undefined.
  it("offers no delete at all without an endpoint", async () => {
    renderDetail(page({ delete_endpoint: undefined }));
    await loaded();

    expect(screen.queryByRole("button", { name: /^delete$/i })).toBeNull();
  });
});
