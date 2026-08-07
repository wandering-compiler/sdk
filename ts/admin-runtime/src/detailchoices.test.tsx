import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MantineProvider } from "@mantine/core";

import { DetailPage } from "./DetailPage";
import type { AdminChoicesSpec, AdminPageSpec, AdminSpec } from "./types";

vi.mock("./api", async () => {
  const actual = await vi.importActual<typeof import("./api")>("./api");
  return { ...actual, apiGet: vi.fn(), apiPatch: vi.fn() };
});
const { apiGet, apiPatch } = await import("./api");

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

// The detail form and `field_choices`.
//
// The compiler emitted this catalogue for the detail form all along
// (admin/spec_gen.go, `json:"field_choices"`), the list had been reading its
// twins (`column_choices` for cells, `filter_choices` for the filter panel)
// — and the detail form read NEITHER the spec field nor, until this change,
// even declared it in AdminDetailSpec. So an enum rendered as the wire NUMBER
// on the one page whose job is to show one record properly, and EDITING it
// meant typing that number into a text box.

const reasonChoices: AdminChoicesSpec = {
  enum_fqn: "billing.v1.Reason",
  carrier: "scalar_int32",
  values: [
    { value: 1, label: "Refund" },
    { value: 2, label: "Topup" },
    { value: 3, label: "Retired", deprecated: true },
  ],
};

const tagChoices: AdminChoicesSpec = {
  enum_fqn: "billing.v1.Tag",
  carrier: "repeated_int32",
  values: [
    { value: 7, label: "Urgent" },
    { value: 8, label: "Manual" },
  ],
};

const spec = { default_language: "cs" } as AdminSpec;

function page(overrides: Partial<AdminPageSpec["detail"]> = {}): AdminPageSpec {
  return {
    name: "Entries",
    detail: {
      read_endpoint: "/admin/api/detail/Entries/{id}",
      update_endpoint: "/admin/api/detail/Entries/{id}",
      fields: ["reason"],
      readonly_fields: [],
      field_choices: { reason: reasonChoices },
      ...overrides,
    },
  } as AdminPageSpec;
}

function renderDetail(p: AdminPageSpec) {
  return render(
    <MantineProvider>
      <DetailPage spec={spec} page={p} rowId="1" onBack={() => {}} />
    </MantineProvider>,
  );
}

describe("DetailPage field_choices", () => {
  it("shows the LABEL, not the wire number, on a readonly enum field", async () => {
    vi.mocked(apiGet).mockResolvedValue({ reason: 1 });
    renderDetail(page({ fields: [], readonly_fields: ["reason"] }));

    await waitFor(() => {
      expect(screen.getByDisplayValue("Refund")).toBeTruthy();
    });
    // The decoy that makes the assertion mean something: "Refund" could also
    // appear because the row happened to carry that string. It does not — the
    // row carries 1, and 1 is what the field rendered before this change.
    expect(screen.queryByDisplayValue("1")).toBeNull();
  });

  it("renders an editable enum field as a Select over the catalogue", async () => {
    vi.mocked(apiGet).mockResolvedValue({ reason: 2 });
    renderDetail(page());

    // Mantine's Select is an input showing the LABEL of the current value.
    await waitFor(() => {
      expect(screen.getByDisplayValue("Topup")).toBeTruthy();
    });
    // Asserted by BEHAVIOUR rather than by a DOM attribute: opening it offers
    // the catalogue's members. A `readonly` check would have been a fact about
    // Mantine's markup — and a wrong one, since a searchable Select keeps its
    // input typeable for filtering.
    const user = userEvent.setup();
    await user.click(screen.getByDisplayValue("Topup"));
    expect(await screen.findByText("Refund")).toBeTruthy();
  });

  // The one that would have shipped a silent data bug. The catalogue is keyed
  // by the WIRE value — a NUMBER for an int32 carrier — while a <Select>
  // speaks only strings. Submitting the token as typed sends "1" where the API
  // expects 1, and JSON has no way to tell the reader which one it got.
  it("PATCHes the wire NUMBER, not the Select's string token", async () => {
    vi.mocked(apiGet).mockResolvedValue({ reason: 1 });
    vi.mocked(apiPatch).mockResolvedValue({ reason: 2 });
    renderDetail(page());

    await waitFor(() => expect(screen.getByDisplayValue("Refund")).toBeTruthy());
    const user = userEvent.setup();
    await user.click(screen.getByDisplayValue("Refund"));
    await user.click(await screen.findByText("Topup"));
    await user.click(screen.getByRole("button", { name: /save/i }));

    await waitFor(() => expect(apiPatch).toHaveBeenCalled());
    const [, payload] = vi.mocked(apiPatch).mock.calls[0];
    // toEqual is type-strict on primitives, so `{ reason: "2" }` fails this —
    // which is the entire point of the case, not an incidental detail of it.
    expect(payload).toEqual({ reason: 2 });
  });

  it("hides a deprecated member — unless the row is the one holding it", async () => {
    // Not holding it: the retired member is not offerable.
    vi.mocked(apiGet).mockResolvedValue({ reason: 1 });
    const { unmount } = renderDetail(page());
    await waitFor(() => expect(screen.getByDisplayValue("Refund")).toBeTruthy());
    const user = userEvent.setup();
    await user.click(screen.getByDisplayValue("Refund"));
    expect(screen.queryByText("Retired")).toBeNull();
    unmount();
    cleanup();

    // Holding it: dropping it from the list would render the field blank and
    // let a save quietly rewrite a value nobody touched.
    vi.mocked(apiGet).mockResolvedValue({ reason: 3 });
    renderDetail(page());
    await waitFor(() => expect(screen.getByDisplayValue("Retired")).toBeTruthy());
  });

  it("renders a repeated enum as a MultiSelect showing every label", async () => {
    vi.mocked(apiGet).mockResolvedValue({ tags: [7, 8] });
    renderDetail(page({ fields: ["tags"], field_choices: { tags: tagChoices } }));

    // getAllByText, not getByText: a MultiSelect renders the selected member
    // as a pill AND keeps it in the option list, so the label legitimately
    // appears more than once. What matters is that it appears at all…
    await waitFor(() => {
      expect(screen.getAllByText("Urgent").length).toBeGreaterThan(0);
    });
    expect(screen.getAllByText("Manual").length).toBeGreaterThan(0);
    // …and that the wire numbers are nowhere on the screen, which is the
    // whole complaint this change answers.
    expect(screen.queryByText("7")).toBeNull();
    expect(screen.queryByText("8")).toBeNull();
  });

  it("leaves a field with no catalogue exactly as it was", async () => {
    vi.mocked(apiGet).mockResolvedValue({ note: "hello" });
    renderDetail(page({ fields: ["note"], field_choices: {} }));

    await waitFor(() => {
      const input = screen.getByDisplayValue("hello");
      // A plain text box — writable, no Select behaviour grafted onto it.
      expect(input.getAttribute("readonly")).toBeNull();
    });
  });
});
