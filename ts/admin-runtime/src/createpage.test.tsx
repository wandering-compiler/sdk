import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MantineProvider } from "@mantine/core";

import { CreatePage, extractCreatedId } from "./CreatePage";
import type { AdminPageSpec } from "./types";

vi.mock("./api", async () => {
  const actual = await vi.importActual<typeof import("./api")>("./api");
  return { ...actual, apiPost: vi.fn() };
});
const { apiPost } = await import("./api");

// extractCreatedId is a heuristic over a CONSUMER-defined response
// shape — the create mutation returns whatever the project's proto
// says. These pin which shapes it recognises, and that an unrecognised
// one degrades to undefined (caller falls back to the list) rather
// than throwing or inventing an id.
describe("extractCreatedId", () => {
  it("reads a top-level string or numeric id", () => {
    expect(extractCreatedId({ id: "abc" })).toBe("abc");
    expect(extractCreatedId({ id: 42 })).toBe("42");
  });

  it("reads the <model>_id convention", () => {
    expect(extractCreatedId({ user_id: "u1" })).toBe("u1");
    expect(extractCreatedId({ note_id: 7 })).toBe("7");
  });

  it("reads a nested { <model>: { id } }", () => {
    expect(extractCreatedId({ user: { id: "nested" } })).toBe("nested");
  });

  it("prefers the top-level id over a nested one", () => {
    expect(extractCreatedId({ id: "top", user: { id: "nested" } })).toBe("top");
  });

  it("gives up quietly on shapes it doesn't recognise", () => {
    expect(extractCreatedId({ created: true })).toBeUndefined();
    expect(extractCreatedId({})).toBeUndefined();
    expect(extractCreatedId(null)).toBeUndefined();
    expect(extractCreatedId(undefined)).toBeUndefined();
    // An empty string is not a usable id.
    expect(extractCreatedId({ id: "" })).toBeUndefined();
  });
});

const page = (overrides: Partial<AdminPageSpec["detail"]> = {}): AdminPageSpec =>
  ({
    name: "Notes",
    detail: {
      read_endpoint: "/admin/api/detail/Notes/{id}",
      create_endpoint: "/admin/api/detail/Notes",
      create_fields: ["title", "body"],
      fields: ["title"],
      ...overrides,
    },
  }) as AdminPageSpec;

function renderCreate(props: Partial<Parameters<typeof CreatePage>[0]> = {}) {
  const onCreated = vi.fn();
  const onBack = vi.fn();
  render(
    <MantineProvider>
      <CreatePage page={page()} onBack={onBack} onCreated={onCreated} {...props} />
    </MantineProvider>,
  );
  return { onCreated, onBack };
}

describe("CreatePage", () => {
  beforeEach(() => {
    vi.mocked(apiPost).mockReset();
  });
  afterEach(cleanup);

  // The form is built from create_fields (the CREATE request), NOT
  // fields (which describes the update form and comes from a different
  // message). Rendering the wrong list is the whole reason the two are
  // separate in the spec.
  it("renders an input per create_field", () => {
    renderCreate();
    expect(screen.getByLabelText(/title/i)).toBeDefined();
    expect(screen.getByLabelText(/body/i)).toBeDefined();
  });

  it("POSTs the create endpoint with only the declared create fields", async () => {
    vi.mocked(apiPost).mockResolvedValue({ id: "new-1" });
    const { onCreated } = renderCreate();

    await userEvent.type(screen.getByLabelText(/title/i), "hello");
    await userEvent.type(screen.getByLabelText(/body/i), "world");
    await userEvent.click(screen.getByRole("button", { name: /create/i }));

    await waitFor(() => expect(apiPost).toHaveBeenCalledTimes(1));
    expect(vi.mocked(apiPost).mock.calls[0][0]).toBe("/admin/api/detail/Notes");
    expect(vi.mocked(apiPost).mock.calls[0][1]).toEqual({ title: "hello", body: "world" });
    await waitFor(() => expect(onCreated).toHaveBeenCalledWith("new-1"));
  });

  // A create response with no recognisable id must still complete —
  // the caller navigates to the list instead of a nonexistent row.
  it("completes with an undefined id when the response carries none", async () => {
    vi.mocked(apiPost).mockResolvedValue({ ok: true });
    const { onCreated } = renderCreate();
    await userEvent.click(screen.getByRole("button", { name: /create/i }));
    await waitFor(() => expect(onCreated).toHaveBeenCalledWith(undefined));
  });

  it("surfaces a failed create and does not navigate away", async () => {
    vi.mocked(apiPost).mockRejectedValue(new Error("boom"));
    const { onCreated } = renderCreate();
    await userEvent.click(screen.getByRole("button", { name: /create/i }));
    await waitFor(() => expect(screen.getByText(/boom/)).toBeDefined());
    expect(onCreated).not.toHaveBeenCalled();
  });

  // Backend enforces regardless; the SPA must not offer a form the
  // caller cannot submit.
  it("refuses to render a form without the create permission", () => {
    renderCreate({
      page: page({ required_permissions_create: [7] }),
      whoami: { permission_ids: [1, 2] },
    });
    expect(screen.queryByRole("button", { name: /create/i })).toBeNull();
    expect(screen.getByText(/cannot create/i)).toBeDefined();
  });

  it("refuses to render a form when the page declares no create endpoint", () => {
    renderCreate({ page: page({ create_endpoint: undefined }) });
    expect(screen.queryByRole("button", { name: /create/i })).toBeNull();
  });
});
