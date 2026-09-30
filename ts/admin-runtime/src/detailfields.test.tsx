import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MantineProvider } from "@mantine/core";

import { DetailPage } from "./DetailPage";

// Mantine's PasswordInput renders more than one node matching /password/i (a visibility
// toggle with its own accessible name), so the field is reached through the CONTROL an
// operator types into — the same reason loginoptions.test.tsx does it this way.
function passwordField(): HTMLInputElement {
  const el = document.querySelector<HTMLInputElement>('input[type="password"]');
  if (!el) throw new Error("no password control rendered");
  return el;
}
import type { AdminPageSpec, AdminSpec } from "./types";

vi.mock("./api", async () => {
  const actual = await vi.importActual<typeof import("./api")>("./api");
  return { ...actual, apiGet: vi.fn(), apiPatch: vi.fn() };
});
const { apiGet, apiPatch } = await import("./api");

// Two things the detail form decides that had no test, and both are written down as
// reasons rather than as preferences:
//
//   - a PASSWORD field never round-trips. The read response carries a hash, so
//     pre-filling would PATCH the hash back as the new password, and an empty submit
//     has to mean "don't change";
//   - a readonly one renders a placeholder, never the stored value — "useless to
//     display + invites mistakes (e.g., screenshot leaks)".
//
// Plus the paths that run when the page cannot show a row at all: a failed read and a
// page whose spec has no detail. Both were unreached.

const spec = {} as AdminSpec;

function page(overrides: Partial<NonNullable<AdminPageSpec["detail"]>> = {}): AdminPageSpec {
  return {
    name: "Users",
    detail: {
      read_endpoint: "/admin/api/detail/Users/{id}",
      update_endpoint: "/admin/api/detail/Users/{id}",
      fields: ["email", "password"],
      field_types: { password: "PASSWORD" },
      ...overrides,
    },
  } as AdminPageSpec;
}

function renderDetail(p: AdminPageSpec = page()) {
  const onBack = vi.fn();
  render(
    <MantineProvider>
      <DetailPage spec={spec} page={p} rowId="1" onBack={onBack} />
    </MantineProvider>,
  );
  return { onBack };
}

beforeEach(() => {
  vi.mocked(apiGet).mockReset();
  vi.mocked(apiPatch).mockReset();
  vi.mocked(apiGet).mockResolvedValue({
    email: "root@example.com",
    // What a read response actually carries for a password column.
    password: "$2b$12$KIXQm2Q0m9jV1Q0m9jV1Qe",
  });
});

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

describe("a PASSWORD field on an editable form", () => {
  // The stored hash must not reach the input. If it did, the form would submit it as
  // the new password on the next save — hashing a hash, and locking the account out.
  it("never pre-fills with what the read returned", async () => {
    renderDetail();
    await waitFor(() => expect(screen.getByDisplayValue("root@example.com")).toBeTruthy());

    const pw = passwordField();
    expect(pw.value, "the stored hash was pre-filled into the password input").toBe("");
    expect(screen.queryByDisplayValue(/^\$2b\$/), "the hash is on the page").toBeNull();
  });

  // "Empty submit = don't change" (REV-151). The field is registered, so it IS in the
  // form's values — and the PATCH must therefore carry it as the empty string the
  // operator left, not be silently dropped or filled from the read.
  it("submits the empty string when it was left alone", async () => {
    vi.mocked(apiPatch).mockResolvedValue({ email: "root@example.com" });
    renderDetail();
    await waitFor(() => expect(screen.getByDisplayValue("root@example.com")).toBeTruthy());

    await userEvent.click(screen.getByRole("button", { name: /^save$/i }));

    await waitFor(() => expect(apiPatch).toHaveBeenCalled());
    expect(vi.mocked(apiPatch).mock.calls[0][1]).toEqual({
      email: "root@example.com",
      password: "",
    });
  });

  // The SAVE response carries the stored hash too, so re-seeding the form from it raw
  // re-arms the same bug one save later: the second Save would PATCH the hash. Two
  // saves in a row is the only shape that shows it — after the first, the form has been
  // re-seeded from the response rather than from the read.
  it("does not re-arm itself from the save response", async () => {
    vi.mocked(apiPatch).mockResolvedValue({
      email: "root@example.com",
      password: "$2b$12$KIXQm2Q0m9jV1Q0m9jV1Qe",
    });
    renderDetail();
    await waitFor(() => expect(screen.getByDisplayValue("root@example.com")).toBeTruthy());

    await userEvent.type(passwordField(), "hunter2");
    await userEvent.click(screen.getByRole("button", { name: /^save$/i }));
    await waitFor(() => expect(apiPatch).toHaveBeenCalledTimes(1));

    expect(passwordField().value, "the save response's hash was seeded back").toBe("");

    await userEvent.click(screen.getByRole("button", { name: /^save$/i }));
    await waitFor(() => expect(apiPatch).toHaveBeenCalledTimes(2));
    expect(vi.mocked(apiPatch).mock.calls[1][1]).toEqual({
      email: "root@example.com",
      password: "",
    });
  });

  it("submits what was typed when it was changed", async () => {
    vi.mocked(apiPatch).mockResolvedValue({ email: "root@example.com" });
    renderDetail();
    await waitFor(() => expect(screen.getByDisplayValue("root@example.com")).toBeTruthy());

    await userEvent.type(passwordField(), "hunter2");
    await userEvent.click(screen.getByRole("button", { name: /^save$/i }));

    await waitFor(() => expect(apiPatch).toHaveBeenCalled());
    expect(vi.mocked(apiPatch).mock.calls[0][1]).toMatchObject({ password: "hunter2" });
  });
});

describe("a PASSWORD field on a form nobody can submit", () => {
  // Readonly, so the field renders display-only — and the rule is that a password
  // shows a placeholder rather than its value. The reason at the code is a screenshot
  // leak, which makes this a security claim and not a layout preference.
  it("shows a placeholder, never the stored value", async () => {
    renderDetail(page({ update_endpoint: undefined }));
    await waitFor(() => expect(screen.getByDisplayValue("root@example.com")).toBeTruthy());

    expect(screen.getByDisplayValue("••••••••")).toBeTruthy();
    expect(screen.queryByDisplayValue(/^\$2b\$/), "the hash is on the page").toBeNull();
  });

  // ...and the placeholder is inert. A disabled+readOnly input is what keeps it out of
  // the form and out of a copy-paste.
  it("renders that placeholder as an inert input", async () => {
    renderDetail(page({ update_endpoint: undefined }));
    const dots = await screen.findByDisplayValue("••••••••");

    expect((dots as HTMLInputElement).readOnly).toBe(true);
    expect((dots as HTMLInputElement).disabled).toBe(true);
  });
});

describe("when the row cannot be shown at all", () => {
  // The read's catch. A detail page whose GET fails has to say so — the alternative is
  // the spinner or an empty form, and an empty form invites a save that would blank
  // the row.
  it("reports a failed read instead of showing an empty form", async () => {
    vi.mocked(apiGet).mockRejectedValue(new Error("row 1 not found"));
    renderDetail();

    expect(await screen.findByText("row 1 not found")).toBeTruthy();
    expect(screen.queryByRole("button", { name: /^save$/i })).toBeNull();
  });

  it("reports a rejection that is not an Error at all", async () => {
    vi.mocked(apiGet).mockRejectedValue("gateway exploded");
    renderDetail();

    expect(await screen.findByText("gateway exploded")).toBeTruthy();
  });

  // The guard the file calls unreachable-but-necessary: "App.tsx no longer routes to a
  // detail a page has not got, and ListPage no longer links into one. But a hand-typed
  // URL or a stale bookmark must land on a message, not a crash." Nothing was checking
  // that it lands on the message.
  it("says so when the page declares no detail view", () => {
    renderDetail({ name: "TaskTags", detail: null } as unknown as AdminPageSpec);

    expect(screen.getByText(/no detail view/i)).toBeTruthy();
    expect(apiGet, "a page with no detail still fetched something").not.toHaveBeenCalled();
  });
});

// SECRET / CRYPTED_SECRET are the OTHER half of the masking rule, and they are not the
// same question as the password. The generator keeps all three out of an admin's default
// list columns and detail fields (`masked` in srcgo/domains/gateway/admin/spec_gen.go);
// these tests are what happens when an author names one explicitly anyway.
//
// CRYPTED_SECRET is the one that was missing from that generator predicate AND from the
// spec's type vocabulary, so it reached the runtime as an ordinary string: a plain
// TextInput printing a value the DATABASE is encrypted specifically so as not to hold.
describe("a SECRET field is masked but still seeded", () => {
  const secretPage = (over: Record<string, unknown> = {}) =>
    ({
      name: "Apps",
      detail: {
        read_endpoint: "/admin/api/detail/Apps/{id}",
        update_endpoint: "/admin/api/detail/Apps/{id}",
        fields: ["email", "client_secret"],
        field_types: { client_secret: "SECRET" },
        ...over,
      },
    }) as unknown as AdminPageSpec;

  beforeEach(() => {
    vi.mocked(apiGet).mockResolvedValue({
      email: "root@example.com",
      client_secret: "sk_live_7Qm2Q0m9jV1",
    });
  });

  it("renders as a masked control, not as text", async () => {
    renderDetail(secretPage());
    await waitFor(() => expect(screen.getByDisplayValue("root@example.com")).toBeTruthy());

    expect(passwordField(), "the secret rendered as a plain text input").not.toBeNull();
  });

  // The distinction from PASSWORD, and the reason it is worth its own test: a secret
  // reads back its REAL value, so blanking it the way formSeed blanks a hash would make
  // an untouched save wipe it.
  it("keeps its value, because an untouched save must not wipe it", async () => {
    vi.mocked(apiPatch).mockResolvedValue({ email: "root@example.com" });
    renderDetail(secretPage());
    await waitFor(() => expect(screen.getByDisplayValue("root@example.com")).toBeTruthy());

    expect(passwordField().value).toBe("sk_live_7Qm2Q0m9jV1");

    await userEvent.click(screen.getByRole("button", { name: /^save$/i }));
    await waitFor(() => expect(apiPatch).toHaveBeenCalled());
    expect(vi.mocked(apiPatch).mock.calls[0][1]).toEqual({
      email: "root@example.com",
      client_secret: "sk_live_7Qm2Q0m9jV1",
    });
  });

  it("shows dots rather than the value when the form cannot be submitted", async () => {
    renderDetail(secretPage({ update_endpoint: undefined }));
    await waitFor(() => expect(screen.getByDisplayValue("root@example.com")).toBeTruthy());

    expect(screen.getByDisplayValue("••••••••")).toBeTruthy();
    expect(screen.queryByDisplayValue(/^sk_live_/), "the secret is on the page").toBeNull();
  });

  // CRYPTED_SECRET behaves identically — same application-side contract, and the type
  // the generator did not know about.
  // ⚠️ On an EDITABLE form the value is in the control — it has to be, or an untouched
  // save would wipe it — so "masked" here means the control is type=password, not that
  // the value is absent. The first cut of this test asserted both and contradicted
  // itself. The value being absent is the READONLY case, above.
  it("treats CRYPTED_SECRET the same way", async () => {
    renderDetail(secretPage({ field_types: { client_secret: "CRYPTED_SECRET" } }));
    await waitFor(() => expect(screen.getByDisplayValue("root@example.com")).toBeTruthy());

    const pw = passwordField();
    expect(pw.value).toBe("sk_live_7Qm2Q0m9jV1");
    expect(pw.type, "a crypted secret rendered as a readable input").toBe("password");
  });

  it("shows dots for a readonly CRYPTED_SECRET too", async () => {
    renderDetail(
      secretPage({
        update_endpoint: undefined,
        field_types: { client_secret: "CRYPTED_SECRET" },
      }),
    );
    await waitFor(() => expect(screen.getByDisplayValue("root@example.com")).toBeTruthy());

    expect(screen.getByDisplayValue("••••••••")).toBeTruthy();
    expect(screen.queryByDisplayValue(/^sk_live_/), "the secret is on the page").toBeNull();
  });
});
